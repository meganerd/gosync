package transport_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gbjohnso/gosync/pkg/ranged"
	"github.com/gbjohnso/gosync/pkg/transport"
	"github.com/quic-go/quic-go"
)

// fanoutPeer is a protocol-conformant QUIC receiver for the fan-out wire
// contract: a control listener that answers PING/CAPS/FANOUT/COMMIT/ABORT and
// the existing SEND commands, plus a second listener on the advertised port
// that serves ranged sends. It presents the same certificate on both listeners,
// so a client that weakened verification for data sockets would still connect,
// while a client that dialed a different port or host would not.
//
// The production receiver lives in pkg/server and its fan-out support was still
// in flight, so these tests pin the sender against the specified wire contract.
type fanoutPeer struct {
	certPEM     []byte
	controlAddr *net.UDPAddr
	dataPort    int

	// fanoutLine builds the FANOUT reply; nil means "older receiver".
	fanoutLine func(dataPort int) string
	// failRangeAt gets ERROR replies for its first failRangeTimes attempts.
	failRangeAt    int64
	failRangeTimes int
	// corruptDigestAt gets a wrong per-range digest in its acknowledgement.
	corruptDigestAt int64

	mu            sync.Mutex
	commands      []string
	file          []byte
	ranges        []ranged.Chunk
	attempts      map[int64]int
	dataPeers     map[string]int
	sendPayloads  []int64
	committed     bool
	commitDigest  string
	aborted       bool
	protocolError string
}

// newFanoutPeer applies every setup function before starting the listeners, so
// test knobs are never written while peer goroutines could read them.
func newFanoutPeer(t *testing.T, total int64, setup ...func(*fanoutPeer)) *fanoutPeer {
	t.Helper()
	cert := quicUDPTestCertificate(t)
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"gosync"},
		MinVersion:   tls.VersionTLS13,
	}
	control, err := quic.ListenAddr("127.0.0.1:0", tlsConfig, &quic.Config{MaxIdleTimeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	data, err := quic.ListenAddr("127.0.0.1:0", tlsConfig, &quic.Config{MaxIdleTimeout: 30 * time.Second})
	if err != nil {
		control.Close()
		t.Fatal(err)
	}
	peer := &fanoutPeer{
		certPEM:         pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}),
		controlAddr:     control.Addr().(*net.UDPAddr),
		dataPort:        data.Addr().(*net.UDPAddr).Port,
		fanoutLine:      func(port int) string { return fmt.Sprintf("OK FANOUT 8 %d\n", port) },
		failRangeAt:     -1,
		corruptDigestAt: -1,
		file:            make([]byte, total),
		attempts:        map[int64]int{},
		dataPeers:       map[string]int{},
	}
	for _, apply := range setup {
		apply(peer)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var listeners sync.WaitGroup
	listeners.Add(2)
	go func() { defer listeners.Done(); peer.accept(ctx, control, false) }()
	go func() { defer listeners.Done(); peer.accept(ctx, data, true) }()
	t.Cleanup(func() {
		cancel()
		control.Close()
		data.Close()
		listeners.Wait()
	})
	return peer
}

// accept never calls t.*: stray peer goroutines must not report after the test.
func (p *fanoutPeer) accept(ctx context.Context, listener *quic.Listener, isData bool) {
	var conns sync.WaitGroup
	defer conns.Wait()
	for {
		conn, err := listener.Accept(ctx)
		if err != nil {
			return
		}
		if isData {
			p.mu.Lock()
			p.dataPeers[conn.RemoteAddr().String()]++
			p.mu.Unlock()
		}
		conns.Add(1)
		go func() {
			defer conns.Done()
			defer conn.CloseWithError(0, "peer done")
			var streams sync.WaitGroup
			defer streams.Wait()
			for {
				stream, err := conn.AcceptStream(ctx)
				if err != nil {
					return
				}
				streams.Add(1)
				go func() {
					defer streams.Done()
					defer stream.Close()
					defer stream.CancelRead(0)
					stream.SetDeadline(time.Now().Add(30 * time.Second))
					p.handle(stream)
				}()
			}
		}()
	}
}

func (p *fanoutPeer) fail(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.protocolError == "" {
		p.protocolError = fmt.Sprintf(format, args...)
	}
}

func (p *fanoutPeer) handle(stream *quic.Stream) {
	reader := bufio.NewReader(stream)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	line = strings.TrimSuffix(line, "\n")
	fields := strings.Split(line, " ")
	p.mu.Lock()
	p.commands = append(p.commands, fields[0])
	p.mu.Unlock()

	switch fields[0] {
	case "PING":
		io.WriteString(stream, "OK PONG "+encodedPath("/receiver base")+"\n")
	case "CAPS":
		io.WriteString(stream, "OK CAPS NOHASH\n")
	case ranged.CmdFanout:
		if p.fanoutLine == nil {
			io.WriteString(stream, "ERROR unknown command: FANOUT\n")
			return
		}
		io.WriteString(stream, p.fanoutLine(p.dataPort))
	case ranged.CmdSendRange, ranged.CmdSendRangeHash:
		p.handleRange(stream, reader, fields)
	case ranged.CmdCommit, ranged.CmdCommitHash:
		p.handleCommit(stream, fields)
	case ranged.CmdAbort:
		p.mu.Lock()
		p.aborted = true
		p.mu.Unlock()
		io.WriteString(stream, "OK ABORT\n")
	case "SEND", "SEND-NOHASH":
		p.handleSend(stream, reader, fields)
	default:
		io.WriteString(stream, "ERROR unknown command: "+fields[0]+"\n")
	}
}

func (p *fanoutPeer) handleRange(stream *quic.Stream, reader *bufio.Reader, fields []string) {
	if len(fields) != 6 {
		p.fail("malformed ranged command: %v", fields)
		io.WriteString(stream, "ERROR malformed SEND-RANGE\n")
		return
	}
	if err := ranged.ValidateToken(fields[1]); err != nil {
		p.fail("invalid token %q: %v", fields[1], err)
		io.WriteString(stream, "ERROR invalid token\n")
		return
	}
	offset, offErr := strconv.ParseInt(fields[2], 10, 64)
	length, lenErr := strconv.ParseInt(fields[3], 10, 64)
	total, totalErr := strconv.ParseInt(fields[4], 10, 64)
	if offErr != nil || lenErr != nil || totalErr != nil {
		p.fail("non-numeric range framing: %v", fields)
		io.WriteString(stream, "ERROR malformed range\n")
		return
	}
	if err := ranged.ValidateRange(offset, length, total); err != nil {
		p.fail("invalid range: %v", err)
		io.WriteString(stream, "ERROR invalid range\n")
		return
	}
	if fields[5] != encodedPath(quicTestPath) {
		p.fail("unexpected range path %q", fields[5])
		io.WriteString(stream, "ERROR unexpected path\n")
		return
	}
	p.mu.Lock()
	p.attempts[offset]++
	attempt := p.attempts[offset]
	if total != int64(len(p.file)) {
		p.mu.Unlock()
		p.fail("range total %d, want %d", total, len(p.file))
		io.WriteString(stream, "ERROR wrong total\n")
		return
	}
	target := p.file[offset : offset+length]
	p.mu.Unlock()

	// Read the payload before any refusal, so a rejected range does not leave
	// the sender blocked writing into a full stream buffer.
	if _, err := io.ReadFull(reader, target); err != nil {
		return
	}
	if offset == p.failRangeAt && attempt <= p.failRangeTimes {
		io.WriteString(stream, "ERROR staged write failed\n")
		return
	}
	p.mu.Lock()
	p.ranges = append(p.ranges, ranged.Chunk{Offset: offset, Length: length})
	p.mu.Unlock()
	if fields[0] == ranged.CmdSendRangeHash {
		digest := digestHex(target)
		if offset == p.corruptDigestAt {
			digest = digestHex(append([]byte("corrupt"), target...))
		}
		fmt.Fprintf(stream, "OK RANGE %d %d %s\n", offset, length, digest)
		return
	}
	fmt.Fprintf(stream, "OK RANGE %d %d\n", offset, length)
}

func (p *fanoutPeer) handleCommit(stream *quic.Stream, fields []string) {
	hashed := fields[0] == ranged.CmdCommitHash
	if (hashed && len(fields) != 4) || (!hashed && len(fields) != 3) {
		p.fail("malformed commit: %v", fields)
		io.WriteString(stream, "ERROR malformed COMMIT\n")
		return
	}
	total, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || total != int64(len(p.file)) {
		p.fail("commit total %q, want %d", fields[2], len(p.file))
		io.WriteString(stream, "ERROR wrong total\n")
		return
	}
	p.mu.Lock()
	var coverage ranged.Coverage
	for _, span := range p.ranges {
		if err := coverage.Add(span.Offset, span.Length, total); err != nil {
			p.mu.Unlock()
			p.fail("coverage rejected %d+%d: %v", span.Offset, span.Length, err)
			io.WriteString(stream, "ERROR overlapping ranges\n")
			return
		}
	}
	if !coverage.Covers(total) {
		p.mu.Unlock()
		io.WriteString(stream, "ERROR missing ranges: "+coverage.Missing(total)+"\n")
		return
	}
	// Whole-file read-back of what was persisted, as the design specifies.
	staged := digestHex(p.file)
	p.committed = true
	p.commitDigest = staged
	p.mu.Unlock()
	if hashed {
		if fields[3] != staged {
			io.WriteString(stream, "ERROR checksum mismatch\n")
			return
		}
		fmt.Fprintf(stream, "OK COMMIT %d %s\n", total, staged)
		return
	}
	fmt.Fprintf(stream, "OK COMMIT %d\n", total)
}

func (p *fanoutPeer) handleSend(stream *quic.Stream, reader *bufio.Reader, fields []string) {
	if len(fields) != 3 {
		p.fail("malformed send: %v", fields)
		return
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		p.fail("malformed send size %q", fields[1])
		return
	}
	hash := sha256.New()
	if _, err := io.CopyN(hash, reader, size); err != nil {
		return
	}
	p.mu.Lock()
	p.sendPayloads = append(p.sendPayloads, size)
	p.mu.Unlock()
	if fields[0] == "SEND" {
		fmt.Fprintf(stream, "OK %x %d\n", hash.Sum(nil), size)
		return
	}
	fmt.Fprintf(stream, "OK NONE %d\n", size)
}

func (p *fanoutPeer) sawCommand(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, command := range p.commands {
		if command == name {
			return true
		}
	}
	return false
}

func (p *fanoutPeer) snapshot() ([]string, []ranged.Chunk, bool, bool, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.commands...), append([]ranged.Chunk(nil), p.ranges...), p.committed, p.aborted, p.protocolError
}

// fanoutSource writes a deterministic payload whose every byte depends on its
// offset, so a range written to the wrong offset cannot go unnoticed.
func fanoutSource(t *testing.T, size int64) (*os.File, []byte) {
	t.Helper()
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i*31 + i/1024)
	}
	path := filepath.Join(t.TempDir(), "source.bin")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file, payload
}

func connectFanoutClient(t *testing.T, peer *fanoutPeer, config transport.Config) *transport.QUICTransport {
	t.Helper()
	config.CertificatePEM = peer.certPEM
	if config.Timeout == 0 {
		config.Timeout = 15
	}
	client := transport.NewQUICTransport(config)
	t.Cleanup(func() { client.Close() })
	if err := client.Connect(peer.controlAddr.IP.String(), peer.controlAddr.Port); err != nil {
		t.Fatal(err)
	}
	return client
}

func TestQUICFanoutTransfersRangesAndCommits(t *testing.T) {
	const size = int64(ranged.MinFanoutSize)
	peer := newFanoutPeer(t, size)
	client := connectFanoutClient(t, peer, transport.Config{Connections: 4, MaxRetries: 2})
	source, payload := fanoutSource(t, size)

	var progress atomic.Int64
	client.SetProgressCallback(func(n int64) { progress.Add(n) })
	if err := client.SendStream(source, quicTestPath, size); err != nil {
		t.Fatalf("fan-out send failed: %v", err)
	}

	commands, spans, committed, aborted, protocolErr := peer.snapshot()
	if protocolErr != "" {
		t.Fatalf("receiver rejected the sender's framing: %s", protocolErr)
	}
	if !committed || aborted {
		t.Fatalf("committed=%v aborted=%v (commands %v)", committed, aborted, commands)
	}
	if len(spans) != 4 {
		t.Fatalf("ranges = %d, want 4: %v", len(spans), spans)
	}
	var coverage ranged.Coverage
	for _, span := range spans {
		if err := coverage.Add(span.Offset, span.Length, size); err != nil {
			t.Fatalf("ranges overlap: %v", err)
		}
	}
	if !coverage.Covers(size) {
		t.Fatalf("ranges do not tile [0,%d): missing %s", size, coverage.Missing(size))
	}
	peer.mu.Lock()
	sockets := len(peer.dataPeers)
	stored := append([]byte(nil), peer.file...)
	peer.mu.Unlock()
	// Sender socket count is the dimension that scales, so assert one socket
	// per connection rather than several streams on one socket.
	if sockets != 4 {
		t.Fatalf("distinct sender sockets = %d, want 4", sockets)
	}
	if !bytes.Equal(stored, payload) {
		t.Fatal("assembled file differs from the source")
	}
	for _, forbidden := range []string{"SEND", "SEND-NOHASH", ranged.CmdCommitHash} {
		if peer.sawCommand(forbidden) {
			t.Fatalf("unexpected %s during checksum-off fan-out: %v", forbidden, commands)
		}
	}
	if progress.Load() != size {
		t.Fatalf("progress = %d, want %d", progress.Load(), size)
	}
}

func TestQUICFanoutChecksumUsesRangeAndCommitDigests(t *testing.T) {
	const size = int64(ranged.MinFanoutSize)
	peer := newFanoutPeer(t, size)
	client := connectFanoutClient(t, peer, transport.Config{Connections: 2, MaxRetries: 1, Checksum: true})
	source, payload := fanoutSource(t, size)

	if err := client.SendFile(source.Name(), quicTestPath); err != nil {
		t.Fatalf("checksum fan-out send failed: %v", err)
	}
	commands, spans, committed, aborted, protocolErr := peer.snapshot()
	if protocolErr != "" {
		t.Fatalf("receiver rejected the sender's framing: %s", protocolErr)
	}
	if !committed || aborted || len(spans) != 2 {
		t.Fatalf("committed=%v aborted=%v ranges=%v commands=%v", committed, aborted, spans, commands)
	}
	if !peer.sawCommand(ranged.CmdSendRangeHash) || !peer.sawCommand(ranged.CmdCommitHash) {
		t.Fatalf("checksum fan-out used the wrong commands: %v", commands)
	}
	if peer.sawCommand(ranged.CmdSendRange) || peer.sawCommand("SEND") {
		t.Fatalf("checksum fan-out used unhashed commands: %v", commands)
	}
	peer.mu.Lock()
	digest, stored := peer.commitDigest, append([]byte(nil), peer.file...)
	peer.mu.Unlock()
	if digest != digestHex(payload) {
		t.Fatalf("commit digest = %s, want whole-file %s", digest, digestHex(payload))
	}
	if !bytes.Equal(stored, payload) {
		t.Fatal("assembled file differs from the source")
	}
}

func TestQUICFanoutRangeDigestMismatchAborts(t *testing.T) {
	const size = int64(ranged.MinFanoutSize)
	// The second of two chunks gets a digest the sender cannot have produced.
	peer := newFanoutPeer(t, size, func(p *fanoutPeer) { p.corruptDigestAt = size / 2 })
	client := connectFanoutClient(t, peer, transport.Config{Connections: 2, MaxRetries: 1, Checksum: true})
	source, _ := fanoutSource(t, size)

	err := client.SendStream(source, quicTestPath, size)
	if err == nil {
		t.Fatal("corrupt per-range digest was accepted")
	}
	if !strings.Contains(err.Error(), "RANGE checksum mismatch") {
		t.Fatalf("error = %v, want a range checksum mismatch", err)
	}
	_, _, committed, aborted, _ := peer.snapshot()
	if committed {
		t.Fatal("committed despite a range digest mismatch")
	}
	if !aborted {
		t.Fatal("no ABORT after a range digest mismatch")
	}
}

func TestQUICFanoutRangeFailureAbortsWithoutCommit(t *testing.T) {
	const size = int64(ranged.MinFanoutSize)
	// Offset 0 never succeeds, so the retry budget is exhausted.
	peer := newFanoutPeer(t, size, func(p *fanoutPeer) { p.failRangeAt, p.failRangeTimes = 0, 100 })
	client := connectFanoutClient(t, peer, transport.Config{Connections: 2, MaxRetries: 2})
	source, _ := fanoutSource(t, size)

	err := client.SendStream(source, quicTestPath, size)
	if err == nil {
		t.Fatal("permanently failing range reported success")
	}
	if !strings.Contains(err.Error(), "range 0+") || !strings.Contains(err.Error(), "attempts") {
		t.Fatalf("error = %v, want the failing range and its attempts", err)
	}
	_, _, committed, aborted, _ := peer.snapshot()
	if committed {
		t.Fatal("committed despite a failed range")
	}
	if !aborted {
		t.Fatal("no ABORT after a failed range")
	}
	peer.mu.Lock()
	attempts := peer.attempts[0]
	peer.mu.Unlock()
	if attempts != 3 { // one attempt plus MaxRetries
		t.Fatalf("attempts for offset 0 = %d, want 3", attempts)
	}
}

func TestQUICFanoutRetriesFailedRangeAndCommits(t *testing.T) {
	const size = int64(ranged.MinFanoutSize)
	// One transient failure at offset 0, which another connection must retry.
	peer := newFanoutPeer(t, size, func(p *fanoutPeer) { p.failRangeAt, p.failRangeTimes = 0, 1 })
	client := connectFanoutClient(t, peer, transport.Config{Connections: 4, MaxRetries: 2})
	source, payload := fanoutSource(t, size)

	if err := client.SendStream(source, quicTestPath, size); err != nil {
		t.Fatalf("transient range failure was not retried: %v", err)
	}
	_, spans, committed, aborted, protocolErr := peer.snapshot()
	if protocolErr != "" {
		t.Fatalf("receiver rejected the sender's framing: %s", protocolErr)
	}
	if !committed || aborted {
		t.Fatalf("committed=%v aborted=%v", committed, aborted)
	}
	if len(spans) != 4 {
		t.Fatalf("ranges = %d, want 4", len(spans))
	}
	peer.mu.Lock()
	attempts, stored := peer.attempts[0], append([]byte(nil), peer.file...)
	peer.mu.Unlock()
	if attempts != 2 {
		t.Fatalf("attempts for offset 0 = %d, want 2", attempts)
	}
	if !bytes.Equal(stored, payload) {
		t.Fatal("retried range did not land at its original offset")
	}
}

// An older receiver answers FANOUT with its unknown-command error; the sender
// must fall back silently, with no retry loop and no ranged commands.
func TestQUICFanoutFallsBackToSingleConnectionOnOldReceiver(t *testing.T) {
	const size = int64(ranged.MinFanoutSize)
	peer := newFanoutPeer(t, size, func(p *fanoutPeer) { p.fanoutLine = nil })
	client := connectFanoutClient(t, peer, transport.Config{Connections: 8})
	source, _ := fanoutSource(t, size)

	if err := client.SendStream(source, quicTestPath, size); err != nil {
		t.Fatalf("fallback transfer failed: %v", err)
	}
	commands, _, committed, aborted, _ := peer.snapshot()
	if !peer.sawCommand("SEND-NOHASH") {
		t.Fatalf("fallback did not use the existing SEND path: %v", commands)
	}
	for _, forbidden := range []string{ranged.CmdSendRange, ranged.CmdSendRangeHash, ranged.CmdCommit, ranged.CmdAbort} {
		if peer.sawCommand(forbidden) {
			t.Fatalf("fallback sent %s: %v", forbidden, commands)
		}
	}
	if committed || aborted {
		t.Fatalf("fallback committed=%v aborted=%v", committed, aborted)
	}
	var fanoutQueries int
	for _, command := range commands {
		if command == ranged.CmdFanout {
			fanoutQueries++
		}
	}
	if fanoutQueries != 1 {
		t.Fatalf("FANOUT queried %d times, want exactly 1", fanoutQueries)
	}
}

// A syntactically valid but unusable advertisement (here: a port nothing
// listens on) must not fail the transfer; nothing has been sent yet, so the
// sender falls back to the single-connection path.
func TestQUICFanoutFallsBackWhenDataPortIsUnreachable(t *testing.T) {
	const size = int64(ranged.MinFanoutSize)
	closed, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	deadPort := closed.LocalAddr().(*net.UDPAddr).Port
	closed.Close()
	peer := newFanoutPeer(t, size, func(p *fanoutPeer) {
		p.fanoutLine = func(int) string { return fmt.Sprintf("OK FANOUT 4 %d\n", deadPort) }
	})
	client := connectFanoutClient(t, peer, transport.Config{Connections: 4, Timeout: 2})
	source, _ := fanoutSource(t, size)

	if err := client.SendStream(source, quicTestPath, size); err != nil {
		t.Fatalf("unreachable data port failed the transfer: %v", err)
	}
	if !peer.sawCommand("SEND-NOHASH") {
		t.Fatal("unreachable data port did not fall back to a single connection")
	}
	if peer.sawCommand(ranged.CmdSendRange) {
		t.Fatal("ranges were sent to an unreachable port")
	}
}

func TestQUICFanoutMalformedAdvertisementFallsBack(t *testing.T) {
	for _, reply := range []string{
		"ERROR fan-out disabled\n",
		"OK FANOUT\n",
		"OK FANOUT 4\n",
		"OK FANOUT 0 9444\n",
		fmt.Sprintf("OK FANOUT %d 9444\n", ranged.MaxConnections+1),
		"OK FANOUT 4 0\n",
		"OK FANOUT 4 65536\n",
		"OK FANOUT four 9444\n",
		"OK FANOUT 4 9444 extra\n",
		"OK CAPS NOHASH\n",
	} {
		t.Run(strings.TrimSpace(reply), func(t *testing.T) {
			// Below-threshold size keeps these cases cheap: the assertion is
			// that no ranged command is attempted and the transfer succeeds.
			const size = int64(64 << 10)
			peer := newFanoutPeer(t, size, func(p *fanoutPeer) {
				p.fanoutLine = func(int) string { return reply }
			})
			client := connectFanoutClient(t, peer, transport.Config{Connections: 4, Timeout: 5})
			source, _ := fanoutSource(t, size)
			if err := client.SendStream(source, quicTestPath, size); err != nil {
				t.Fatalf("transfer failed after %q: %v", reply, err)
			}
			if !peer.sawCommand("SEND-NOHASH") || peer.sawCommand(ranged.CmdSendRange) {
				commands, _, _, _, _ := peer.snapshot()
				t.Fatalf("commands = %v", commands)
			}
		})
	}
}

// Plan owns the fan-out threshold: one byte below MinFanoutSize must use the
// existing single-connection path even though the receiver advertised fan-out.
func TestQUICFanoutSkippedBelowThreshold(t *testing.T) {
	for _, size := range []int64{1, int64(ranged.MinFanoutSize) - 1} {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			peer := newFanoutPeer(t, size)
			client := connectFanoutClient(t, peer, transport.Config{Connections: 8})
			source, _ := fanoutSource(t, size)
			if err := client.SendStream(source, quicTestPath, size); err != nil {
				t.Fatalf("below-threshold transfer failed: %v", err)
			}
			if !peer.sawCommand("SEND-NOHASH") {
				t.Fatal("below-threshold transfer did not use the single-connection path")
			}
			if peer.sawCommand(ranged.CmdSendRange) || peer.sawCommand(ranged.CmdCommit) {
				commands, _, _, _, _ := peer.snapshot()
				t.Fatalf("fan-out used below the threshold: %v", commands)
			}
			peer.mu.Lock()
			sockets := len(peer.dataPeers)
			peer.mu.Unlock()
			if sockets != 0 {
				t.Fatalf("data sockets opened below the threshold: %d", sockets)
			}
		})
	}
}

// nonReaderAt hides ReadAt, like a pipe or a decompressing reader. Such a
// source must never be seeked, so fan-out is skipped.
type nonReaderAt struct{ reader io.Reader }

func (r nonReaderAt) Read(p []byte) (int, error) { return r.reader.Read(p) }

func TestQUICFanoutRequiresReaderAtSource(t *testing.T) {
	const size = int64(ranged.MinFanoutSize)
	peer := newFanoutPeer(t, size)
	client := connectFanoutClient(t, peer, transport.Config{Connections: 4})
	source, _ := fanoutSource(t, size)

	if err := client.SendStream(nonReaderAt{source}, quicTestPath, size); err != nil {
		t.Fatalf("stream-only source failed: %v", err)
	}
	if !peer.sawCommand("SEND-NOHASH") {
		t.Fatal("stream-only source did not use the single-connection path")
	}
	if peer.sawCommand(ranged.CmdSendRange) || peer.sawCommand(ranged.CmdSendRangeHash) {
		t.Fatal("ranges were sent for a stream-only source")
	}
	peer.mu.Lock()
	sockets := len(peer.dataPeers)
	peer.mu.Unlock()
	if sockets != 0 {
		t.Fatalf("data sockets opened for a stream-only source: %d", sockets)
	}
}

// The default configuration must reproduce today's wire behavior exactly: no
// FANOUT, no ranged commands, no data sockets.
func TestQUICSingleConnectionWireIsUnchanged(t *testing.T) {
	for _, connections := range []int{0, 1} {
		t.Run(strconv.Itoa(connections), func(t *testing.T) {
			const size = int64(ranged.MinFanoutSize)
			peer := newFanoutPeer(t, size)
			client := connectFanoutClient(t, peer, transport.Config{Connections: connections})
			source, _ := fanoutSource(t, size)
			if err := client.SendStream(source, quicTestPath, size); err != nil {
				t.Fatalf("single-connection transfer failed: %v", err)
			}
			commands, _, _, _, _ := peer.snapshot()
			want := []string{"PING", "CAPS", "SEND-NOHASH"}
			sorted := append([]string(nil), commands...)
			sort.Strings(sorted)
			sort.Strings(want)
			if strings.Join(sorted, ",") != strings.Join(want, ",") {
				t.Fatalf("commands = %v, want exactly %v", commands, want)
			}
			peer.mu.Lock()
			sockets, payloads := len(peer.dataPeers), append([]int64(nil), peer.sendPayloads...)
			peer.mu.Unlock()
			if sockets != 0 {
				t.Fatalf("data sockets = %d, want 0", sockets)
			}
			if len(payloads) != 1 || payloads[0] != size {
				t.Fatalf("single SEND payload = %v, want [%d]", payloads, size)
			}
		})
	}
}

// Close must tear down every data socket and transport, and an in-flight
// fan-out must fail rather than hang when the control connection goes away.
func TestQUICCloseDuringFanoutFailsTransfer(t *testing.T) {
	const size = int64(ranged.MinFanoutSize)
	peer := newFanoutPeer(t, size)
	client := connectFanoutClient(t, peer, transport.Config{Connections: 4, MaxRetries: 2, Timeout: 10})
	source, _ := fanoutSource(t, size)

	done := make(chan error, 1)
	go func() { done <- client.SendStream(source, quicTestPath, size) }()
	// Close as soon as the receiver has seen ranged traffic, so the transfer is
	// genuinely in flight.
	deadline := time.Now().Add(10 * time.Second)
	for !peer.sawCommand(ranged.CmdSendRange) {
		if time.Now().After(deadline) {
			t.Fatal("fan-out never reached the receiver")
		}
		time.Sleep(time.Millisecond)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("transfer succeeded after the control connection was closed")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("closing the control connection left the transfer hanging")
	}
	_, _, committed, _, _ := peer.snapshot()
	if committed {
		t.Fatal("committed after Close")
	}
}
