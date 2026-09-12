package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gbjohnso/gosync/pkg/ranged"
	"github.com/quic-go/quic-go"
)

// rangeClient speaks the protocol to handleClient over an in-memory pipe, which
// keeps the ranged tests independent of any transport.
type rangeClient struct {
	t      *testing.T
	conn   net.Conn
	reader *bufio.Reader
}

func newRangeClient(t *testing.T, s *Server) *rangeClient {
	t.Helper()
	client, serverSide := net.Pipe()
	go s.handleClient(serverSide)
	t.Cleanup(func() { client.Close() })
	return &rangeClient{t: t, conn: client, reader: bufio.NewReader(client)}
}

// do sends one command plus its payload and returns the reply line. It waits
// for the payload write only when the server accepted the command: a rejected
// command never reads the payload, and waiting would deadlock the pipe.
func (c *rangeClient) do(command string, payload []byte) string {
	c.t.Helper()
	c.conn.SetDeadline(time.Now().Add(20 * time.Second))
	written := make(chan error, 1)
	go func() {
		_, err := io.Copy(c.conn, io.MultiReader(strings.NewReader(command+"\n"), bytes.NewReader(payload)))
		written <- err
	}()
	line, err := c.reader.ReadString('\n')
	if err != nil {
		c.t.Fatalf("%.60s: read reply: %v", command, err)
	}
	if strings.HasPrefix(line, "OK ") {
		if err := <-written; err != nil {
			c.t.Fatalf("%.60s: write: %v", command, err)
		}
	}
	return line
}

func (c *rangeClient) expect(command string, payload []byte, want string) {
	c.t.Helper()
	if got := c.do(command, payload); got != want {
		c.t.Fatalf("%.60s: got %q, want %q", command, got, want)
	}
}

func (c *rangeClient) expectError(command string, payload []byte, contains string) {
	c.t.Helper()
	got := c.do(command, payload)
	if !strings.HasPrefix(got, "ERROR ") || !strings.Contains(got, contains) {
		c.t.Fatalf("%.60s: got %q, want an ERROR containing %q", command, got, contains)
	}
}

func testToken(t *testing.T) string {
	t.Helper()
	token, err := ranged.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// rangeServer builds a server that is never started, for handler-level tests.
func rangeServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer("127.0.0.1:0", t.TempDir())
	t.Cleanup(func() { s.cleanupTokens() })
	return s
}

func sendRange(token, path string, offset, length, total int64, withChecksum bool) string {
	command := ranged.CmdSendRange
	if withChecksum {
		command = ranged.CmdSendRangeHash
	}
	return fmt.Sprintf("%s %s %d %d %d %s", command, token, offset, length, total,
		base64.StdEncoding.EncodeToString([]byte(path)))
}

func testPayload(size int) []byte {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i*31 + i/251)
	}
	return payload
}

func (s *Server) tokenCount(t *testing.T) int {
	t.Helper()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.tokens)
}

// waitInFlight blocks until the registry reports the expected number of ranges
// in flight for a token, which is the only observable handoff between a range
// handler and a commit on another connection.
func (s *Server) waitInFlight(t *testing.T, token string, want int) {
	t.Helper()
	until := time.Now().Add(5 * time.Second)
	for {
		s.mu.RLock()
		got := 0
		if transfer := s.tokens[token]; transfer != nil {
			got = transfer.inFlight
		}
		s.mu.RUnlock()
		if got == want {
			return
		}
		if time.Now().After(until) {
			t.Fatalf("in-flight ranges = %d, want %d", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFanoutCapabilityReply(t *testing.T) {
	for _, protocol := range []string{"tcp", "quic"} {
		t.Run(protocol, func(t *testing.T) {
			s := NewServer("127.0.0.1:0", t.TempDir())
			if err := s.SetTransport(protocol); err != nil {
				t.Fatal(err)
			}
			if err := s.SetMaxConnections(4); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- s.Start() }()
			t.Cleanup(func() { s.Stop() })
			until := time.Now().Add(5 * time.Second)
			for s.Addr() == nil {
				if time.Now().After(until) {
					t.Fatal("listener not ready")
				}
				time.Sleep(time.Millisecond)
			}
			port := 0
			switch addr := s.Addr().(type) {
			case *net.UDPAddr:
				port = addr.Port
			case *net.TCPAddr:
				port = addr.Port
			}
			client := newRangeClient(t, s)
			client.expect(ranged.CmdFanout, nil, fmt.Sprintf("OK FANOUT 4 %d\n", port))
			// CAPS is compared byte for byte by existing clients.
			client.expect("CAPS", nil, "OK CAPS NOHASH\n")
			client.expectError("FANOUT 2", nil, "FANOUT takes no arguments")
		})
	}
}

func TestFanoutDefaultsToOneConnection(t *testing.T) {
	s := rangeServer(t)
	if s.maxConnections != 1 {
		t.Fatalf("default maxConnections = %d, want 1", s.maxConnections)
	}
	client := newRangeClient(t, s)
	// Not started, so no port is bound yet; the limit must still be advertised.
	client.expect(ranged.CmdFanout, nil, "OK FANOUT 1 0\n")
}

func TestSetMaxConnectionsValidation(t *testing.T) {
	s := NewServer("127.0.0.1:0", t.TempDir())
	for _, connections := range []int{0, -1, ranged.MaxConnections + 1, 1000} {
		if err := s.SetMaxConnections(connections); err == nil {
			t.Fatalf("accepted %d connections", connections)
		}
	}
	if s.maxConnections != 1 {
		t.Fatalf("rejected value changed the default: %d", s.maxConnections)
	}
	for _, connections := range []int{1, 2, ranged.MaxConnections} {
		if err := s.SetMaxConnections(connections); err != nil {
			t.Fatalf("rejected %d connections: %v", connections, err)
		}
	}
	if err := s.SetTokenTimeout(time.Second); err != nil {
		t.Fatal(err)
	}
	if s.tokenTimeout != minTokenTimeout {
		t.Fatalf("token timeout = %v, want the %v floor", s.tokenTimeout, minTokenTimeout)
	}
	if err := s.SetTokenTimeout(5 * time.Minute); err != nil || s.tokenTimeout != 5*time.Minute {
		t.Fatalf("token timeout = %v (%v)", s.tokenTimeout, err)
	}
	s.Stop()
	if s.SetMaxConnections(2) == nil || s.SetTokenTimeout(time.Hour) == nil {
		t.Fatal("configured a stopped server")
	}
}

func TestRangedTransferCoverageAndCommit(t *testing.T) {
	for _, withChecksum := range []bool{false, true} {
		t.Run(fmt.Sprintf("checksum=%v", withChecksum), func(t *testing.T) {
			s := rangeServer(t)
			client := newRangeClient(t, s)
			token := testToken(t)
			name := "nested folder/ranged file.bin"
			payload := testPayload(3 * rangeChunk)
			total := int64(len(payload))
			// Deliberately out of order, with an unaligned final range.
			for _, span := range []ranged.Chunk{
				{Offset: 2 * rangeChunk, Length: total - 2*rangeChunk},
				{Offset: 0, Length: rangeChunk},
				{Offset: rangeChunk, Length: rangeChunk},
			} {
				command := sendRange(token, name, span.Offset, span.Length, total, withChecksum)
				want := fmt.Sprintf("OK RANGE %d %d\n", span.Offset, span.Length)
				if withChecksum {
					digest := sha256.Sum256(payload[span.Offset : span.Offset+span.Length])
					want = fmt.Sprintf("OK RANGE %d %d %x\n", span.Offset, span.Length, digest)
				}
				client.expect(command, payload[span.Offset:span.Offset+span.Length], want)
			}
			staged := stagedPath(filepath.Join(s.baseDir, name), token)
			if info, err := os.Stat(staged); err != nil || info.Size() != total {
				t.Fatalf("staged file: %v", err)
			}
			if _, err := os.Stat(filepath.Join(s.baseDir, name)); !os.IsNotExist(err) {
				t.Fatalf("ranged data reached the destination path before COMMIT: %v", err)
			}
			if withChecksum {
				digest := sha256.Sum256(payload)
				client.expect(fmt.Sprintf("%s %s %d %x", ranged.CmdCommitHash, token, total, digest), nil,
					fmt.Sprintf("OK COMMIT %d %x\n", total, digest))
			} else {
				client.expect(fmt.Sprintf("%s %s %d", ranged.CmdCommit, token, total), nil,
					fmt.Sprintf("OK COMMIT %d\n", total))
			}
			stored, err := os.ReadFile(filepath.Join(s.baseDir, name))
			if err != nil || !bytes.Equal(stored, payload) {
				t.Fatalf("committed file differs: %v", err)
			}
			if _, err := os.Stat(staged); !os.IsNotExist(err) {
				t.Fatalf("staged file survived COMMIT: %v", err)
			}
		})
	}
}

// rangeChunk is a range size deliberately unrelated to the copy buffer size, so
// ranges neither start nor end on a buffer boundary.
const rangeChunk = 40_000

func TestRangeRejections(t *testing.T) {
	s := rangeServer(t)
	client := newRangeClient(t, s)
	token := testToken(t)
	other := testToken(t)
	name := "reject/file.bin"
	encoded := base64.StdEncoding.EncodeToString([]byte(name))
	const total = 100

	// Framing and validation failures, none of which may create any state.
	for _, tc := range []struct{ command, contains string }{
		{ranged.CmdSendRange, "requires token, offset, length, total and path"},
		{fmt.Sprintf("%s %s 0 10 %d", ranged.CmdSendRange, token, total), "requires token, offset, length, total and path"},
		{fmt.Sprintf("%s %s 0 10 %d %s extra", ranged.CmdSendRange, token, total, encoded), "requires token, offset, length, total and path"},
		{fmt.Sprintf("%s deadbeef 0 10 %d %s", ranged.CmdSendRange, total, encoded), "invalid token length"},
		{fmt.Sprintf("%s %s 0 10 %d %s", ranged.CmdSendRange, strings.ToUpper(token), total, encoded), "lowercase hex"},
		{fmt.Sprintf("%s %s 0 10 %d %s", ranged.CmdSendRange, strings.Repeat("z", 32), total, encoded), "not hex"},
		{fmt.Sprintf("%s %s -1 10 %d %s", ranged.CmdSendRange, token, total, encoded), "requires decimal offset"},
		{fmt.Sprintf("%s %s 0 -10 %d %s", ranged.CmdSendRange, token, total, encoded), "requires decimal offset"},
		{fmt.Sprintf("%s %s 0 10 -1 %s", ranged.CmdSendRange, token, encoded), "requires decimal offset"},
		{fmt.Sprintf("%s %s +0 10 %d %s", ranged.CmdSendRange, token, total, encoded), "requires decimal offset"},
		{fmt.Sprintf("%s %s x 10 %d %s", ranged.CmdSendRange, token, total, encoded), "requires decimal offset"},
		{fmt.Sprintf("%s %s 0 10 9223372036854775808 %s", ranged.CmdSendRange, token, encoded), "requires decimal offset"},
		{fmt.Sprintf("%s %s 0 0 %d %s", ranged.CmdSendRange, token, total, encoded), "invalid length 0"},
		{fmt.Sprintf("%s %s 100 1 %d %s", ranged.CmdSendRange, token, total, encoded), "exceeds total"},
		{fmt.Sprintf("%s %s 101 1 %d %s", ranged.CmdSendRange, token, total, encoded), "exceeds total"},
		{fmt.Sprintf("%s %s 9223372036854775807 2 %d %s", ranged.CmdSendRange, token, total, encoded), "overflows"},
		{fmt.Sprintf("%s %s 0 10 %d !!!not-base64", ranged.CmdSendRange, token, total), "path not base64 encoded"},
	} {
		client.expectError(tc.command, nil, tc.contains)
	}
	if count := s.tokenCount(t); count != 0 {
		t.Fatalf("rejected ranges created %d tokens", count)
	}
	entries, err := os.ReadDir(s.baseDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected ranges touched the destination directory: %v %v", entries, err)
	}

	// A first accepted range fixes total and path for the token.
	payload := testPayload(total)
	client.expect(sendRange(token, name, 0, 10, total, false), payload[:10], "OK RANGE 0 10\n")
	for _, tc := range []struct{ command, contains string }{
		{sendRange(token, name, 0, 10, total, false), "overlaps"},
		{sendRange(token, name, 5, 10, total, false), "overlaps"},
		{sendRange(token, name, 9, 1, total, false), "overlaps"},
		{sendRange(token, name, 20, 10, total+1, false), "does not match"},
		{sendRange(token, "reject/other.bin", 20, 10, total, false), "path does not match"},
	} {
		client.expectError(tc.command, nil, tc.contains)
	}
	// An adjacent range is not an overlap.
	client.expect(sendRange(token, name, 10, 10, total, false), payload[10:20], "OK RANGE 10 10\n")

	// A different token for the same path stages separately.
	client.expect(sendRange(other, name, 0, 5, total, false), payload[:5], "OK RANGE 0 5\n")
	if count := s.tokenCount(t); count != 2 {
		t.Fatalf("tokens = %d, want 2", count)
	}
	if _, err := os.Stat(stagedPath(filepath.Join(s.baseDir, name), other)); err != nil {
		t.Fatalf("second token has no staged file: %v", err)
	}
}

func TestRangeLimitPerToken(t *testing.T) {
	s := rangeServer(t)
	client := newRangeClient(t, s)
	token := testToken(t)
	name := "limits/ranges.bin"
	total := int64(ranged.MaxRangesPerToken + 8)
	one := []byte{7}
	for offset := int64(0); offset < int64(ranged.MaxRangesPerToken); offset++ {
		client.expect(sendRange(token, name, offset, 1, total, false), one,
			fmt.Sprintf("OK RANGE %d 1\n", offset))
	}
	client.expectError(sendRange(token, name, int64(ranged.MaxRangesPerToken), 1, total, false), nil,
		fmt.Sprintf("too many ranges for one transfer (limit %d)", ranged.MaxRangesPerToken))
}

func TestTokenLimit(t *testing.T) {
	s := rangeServer(t)
	client := newRangeClient(t, s)
	one := []byte{9}
	for i := 0; i < maxLiveTokens; i++ {
		token := testToken(t)
		client.expect(sendRange(token, fmt.Sprintf("limits/file-%d.bin", i), 0, 1, 2, false), one, "OK RANGE 0 1\n")
	}
	client.expectError(sendRange(testToken(t), "limits/one-too-many.bin", 0, 1, 2, false), nil,
		fmt.Sprintf("too many concurrent ranged transfers (limit %d)", maxLiveTokens))
	// Aborting one live transfer makes room again.
	s.mu.RLock()
	var victim string
	for token := range s.tokens {
		victim = token
		break
	}
	s.mu.RUnlock()
	client.expect(ranged.CmdAbort+" "+victim, nil, "OK ABORT\n")
	client.expect(sendRange(testToken(t), "limits/after-abort.bin", 0, 1, 2, false), one, "OK RANGE 0 1\n")
}

func TestCommittedTokensDoNotBlockNewTransfers(t *testing.T) {
	s := rangeServer(t)
	client := newRangeClient(t, s)
	one := []byte{3}
	for i := 0; i < maxCommittedTokens+2; i++ {
		token := testToken(t)
		name := fmt.Sprintf("committed/file-%d.bin", i)
		client.expect(sendRange(token, name, 0, 1, 1, false), one, "OK RANGE 0 1\n")
		client.expect(fmt.Sprintf("%s %s 1", ranged.CmdCommit, token), nil, "OK COMMIT 1\n")
		stored, err := os.ReadFile(filepath.Join(s.baseDir, name))
		if err != nil || !bytes.Equal(stored, one) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if count := s.tokenCount(t); count > maxCommittedTokens {
		t.Fatalf("kept %d commit tombstones, limit is %d", count, maxCommittedTokens)
	}
}

func TestCommitRejections(t *testing.T) {
	s := rangeServer(t)
	client := newRangeClient(t, s)
	token := testToken(t)
	name := "commit/gap.bin"
	payload := testPayload(30)

	for _, tc := range []struct{ command, contains string }{
		{ranged.CmdCommit, "requires token and total"},
		{fmt.Sprintf("%s %s", ranged.CmdCommit, token), "requires token and total"},
		{fmt.Sprintf("%s %s 30 extra", ranged.CmdCommit, token), "requires token and total"},
		{fmt.Sprintf("%s nothex 30", ranged.CmdCommit), "invalid token"},
		{fmt.Sprintf("%s %s notanumber", ranged.CmdCommit, token), "requires a decimal total"},
		{fmt.Sprintf("%s %s 30", ranged.CmdCommit, token), "unknown transfer token"},
		{fmt.Sprintf("%s %s 30", ranged.CmdCommitHash, token), "requires token, total and checksum"},
		{fmt.Sprintf("%s %s 30 nothex", ranged.CmdCommitHash, token), "requires a hex SHA-256 checksum"},
		{fmt.Sprintf("%s %s 30 %s", ranged.CmdCommitHash, token, strings.Repeat("a", 63)), "requires a hex SHA-256 checksum"},
	} {
		client.expectError(tc.command, nil, tc.contains)
	}

	// Coverage with a hole must not commit, and a failed commit discards the
	// transfer: no staged file, no destination file, no token.
	client.expect(sendRange(token, name, 0, 10, 30, false), payload[:10], "OK RANGE 0 10\n")
	client.expect(sendRange(token, name, 20, 10, 30, false), payload[20:30], "OK RANGE 20 10\n")
	reply := client.do(fmt.Sprintf("%s %s 30", ranged.CmdCommit, token), nil)
	if !strings.Contains(reply, "incomplete coverage") || !strings.Contains(reply, "missing 10-20") {
		t.Fatalf("COMMIT with a gap: %q", reply)
	}
	assertNoFiles(t, s, name, token)
	client.expectError(fmt.Sprintf("%s %s 30", ranged.CmdCommit, token), nil, "unknown transfer token")

	// A total that disagrees with the staged transfer also discards it.
	mismatched := testToken(t)
	client.expect(sendRange(mismatched, name, 0, 30, 30, false), payload, "OK RANGE 0 30\n")
	client.expectError(fmt.Sprintf("%s %s 31", ranged.CmdCommit, mismatched), nil, "commit total 31 does not match 30")
	assertNoFiles(t, s, name, mismatched)

	// Full coverage commits, and the staged file goes away.
	complete := testToken(t)
	for _, span := range []ranged.Chunk{{Offset: 0, Length: 10}, {Offset: 20, Length: 10}, {Offset: 10, Length: 10}} {
		client.expect(sendRange(complete, name, span.Offset, span.Length, 30, false),
			payload[span.Offset:span.Offset+span.Length],
			fmt.Sprintf("OK RANGE %d %d\n", span.Offset, span.Length))
	}
	client.expect(fmt.Sprintf("%s %s 30", ranged.CmdCommit, complete), nil, "OK COMMIT 30\n")
	stored, err := os.ReadFile(filepath.Join(s.baseDir, name))
	if err != nil || !bytes.Equal(stored, payload) {
		t.Fatalf("committed file differs: %v", err)
	}
}

// assertNoFiles checks that a discarded transfer left neither a staged file nor
// a destination file, and forgot its token.
func assertNoFiles(t *testing.T, s *Server, name, token string) {
	t.Helper()
	if _, err := os.Stat(stagedPath(filepath.Join(s.baseDir, name), token)); !os.IsNotExist(err) {
		t.Fatalf("staged file survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, name)); !os.IsNotExist(err) {
		t.Fatalf("destination file appeared: %v", err)
	}
	s.mu.RLock()
	_, live := s.tokens[token]
	s.mu.RUnlock()
	if live {
		t.Fatal("token survived a discarded transfer")
	}
}

func TestCommitRejectsWrongStagedSize(t *testing.T) {
	s := rangeServer(t)
	client := newRangeClient(t, s)
	token := testToken(t)
	name := "commit/size.bin"
	payload := testPayload(40)
	client.expect(sendRange(token, name, 0, 40, 40, false), payload, "OK RANGE 0 40\n")
	staged := stagedPath(filepath.Join(s.baseDir, name), token)
	// Simulate storage losing bytes: coverage still tiles, the file does not.
	if err := os.Truncate(staged, 39); err != nil {
		t.Fatal(err)
	}
	client.expectError(fmt.Sprintf("%s %s 40", ranged.CmdCommit, token), nil, "staged size 39 does not match total 40")
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("failed commit kept the staged file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, name)); !os.IsNotExist(err) {
		t.Fatalf("failed commit created a destination file: %v", err)
	}
	if count := s.tokenCount(t); count != 0 {
		t.Fatalf("failed commit kept %d tokens", count)
	}
}

func TestCommitRejectsRangeInFlight(t *testing.T) {
	s := rangeServer(t)
	sender := newRangeClient(t, s)
	control := newRangeClient(t, s)
	token := testToken(t)
	name := "commit/inflight.bin"
	payload := testPayload(20)
	sender.expect(sendRange(token, name, 0, 10, 20, false), payload[:10], "OK RANGE 0 10\n")
	// Announce a range and never send its payload: it stays in flight.
	stalled := newRangeClient(t, s)
	stalled.conn.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.WriteString(stalled.conn, sendRange(token, name, 10, 10, 20, false)+"\n"); err != nil {
		t.Fatal(err)
	}
	s.waitInFlight(t, token, 1)
	// Committing before every range is acknowledged is a protocol error, so it
	// fails and discards the transfer rather than racing the writer.
	control.expectError(fmt.Sprintf("%s %s 20", ranged.CmdCommit, token), nil, "still in flight")
	assertNoFiles(t, s, name, token)

	// The stalled range now has nowhere to land and must report failure.
	if _, err := stalled.conn.Write(payload[10:20]); err != nil {
		t.Fatal(err)
	}
	line, err := stalled.reader.ReadString('\n')
	if err != nil {
		t.Fatalf("stalled range reply: %v", err)
	}
	if !strings.HasPrefix(line, "ERROR ") {
		t.Fatalf("stalled range reply %q, want an ERROR", line)
	}
	control.expectError(fmt.Sprintf("%s %s 20", ranged.CmdCommit, token), nil, "unknown transfer token")
	assertNoFiles(t, s, name, token)
}

func TestFailedRangeStaysRetryable(t *testing.T) {
	s := rangeServer(t)
	client := newRangeClient(t, s)
	token := testToken(t)
	name := "retry/file.bin"
	payload := testPayload(20)
	client.expect(sendRange(token, name, 0, 10, 20, false), payload[:10], "OK RANGE 0 10\n")
	// A truncated payload fails the range; the offset must stay free so the
	// sender can retry it on another connection.
	failing := newRangeClient(t, s)
	failing.conn.SetDeadline(time.Now().Add(20 * time.Second))
	if _, err := io.WriteString(failing.conn, sendRange(token, name, 10, 10, 20, false)+"\nshort"); err != nil {
		t.Fatal(err)
	}
	failing.conn.Close()
	s.waitInFlight(t, token, 0)
	retry := newRangeClient(t, s)
	retry.expect(sendRange(token, name, 10, 10, 20, false), payload[10:20], "OK RANGE 10 10\n")
	retry.expect(fmt.Sprintf("%s %s 20", ranged.CmdCommit, token), nil, "OK COMMIT 20\n")
	stored, err := os.ReadFile(filepath.Join(s.baseDir, name))
	if err != nil || !bytes.Equal(stored, payload) {
		t.Fatalf("committed file differs: %v", err)
	}
}

func TestCommitIdempotence(t *testing.T) {
	s := rangeServer(t)
	client := newRangeClient(t, s)
	token := testToken(t)
	name := "idempotent/file.bin"
	payload := testPayload(16)
	digest := sha256.Sum256(payload)
	client.expect(sendRange(token, name, 0, 16, 16, false), payload, "OK RANGE 0 16\n")
	for i := 0; i < 3; i++ {
		client.expect(fmt.Sprintf("%s %s 16", ranged.CmdCommit, token), nil, "OK COMMIT 16\n")
	}
	// A plain commit records no digest, so COMMIT-HASH cannot be answered.
	client.expectError(fmt.Sprintf("%s %s 16 %x", ranged.CmdCommitHash, token, digest), nil,
		"committed without checksum verification")
	// Ranges for a committed token are refused rather than corrupting it.
	client.expectError(sendRange(token, name, 0, 16, 16, false), nil, "already committed")
	client.expectError(ranged.CmdAbort+" "+token, nil, "already committed")
	stored, err := os.ReadFile(filepath.Join(s.baseDir, name))
	if err != nil || !bytes.Equal(stored, payload) {
		t.Fatalf("committed file differs: %v", err)
	}

	hashed := testToken(t)
	client.expect(sendRange(hashed, name, 0, 16, 16, true), payload,
		fmt.Sprintf("OK RANGE 0 16 %x\n", sha256.Sum256(payload)))
	for i := 0; i < 3; i++ {
		client.expect(fmt.Sprintf("%s %s 16 %x", ranged.CmdCommitHash, hashed, digest), nil,
			fmt.Sprintf("OK COMMIT 16 %x\n", digest))
	}
	client.expectError(fmt.Sprintf("%s %s 16 %x", ranged.CmdCommitHash, hashed, sha256.Sum256([]byte("other"))), nil,
		"checksum mismatch")
}

func TestCommitHashMismatchLeavesNoDestination(t *testing.T) {
	s := rangeServer(t)
	client := newRangeClient(t, s)
	token := testToken(t)
	name := "verify/corrupt.bin"
	payload := testPayload(4096)
	total := int64(len(payload))
	digest := sha256.Sum256(payload)
	half := total / 2
	client.expect(sendRange(token, name, 0, half, total, true), payload[:half],
		fmt.Sprintf("OK RANGE 0 %d %x\n", half, sha256.Sum256(payload[:half])))
	client.expect(sendRange(token, name, half, total-half, total, true), payload[half:],
		fmt.Sprintf("OK RANGE %d %d %x\n", half, total-half, sha256.Sum256(payload[half:])))

	// Corrupt the staged bytes without changing its size, so only the
	// commit-time read back can catch it.
	staged := stagedPath(filepath.Join(s.baseDir, name), token)
	corrupt, err := os.OpenFile(staged, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corrupt.WriteAt([]byte{^payload[1024]}, 1024); err != nil {
		t.Fatal(err)
	}
	if err := corrupt.Close(); err != nil {
		t.Fatal(err)
	}

	reply := client.do(fmt.Sprintf("%s %s %d %x", ranged.CmdCommitHash, token, total, digest), nil)
	if !strings.HasPrefix(reply, "ERROR ") || !strings.Contains(reply, "checksum mismatch") {
		t.Fatalf("COMMIT-HASH of corrupt data: %q", reply)
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, name)); !os.IsNotExist(err) {
		t.Fatalf("failed verification produced a destination file: %v", err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("failed verification kept the staged file: %v", err)
	}
	if count := s.tokenCount(t); count != 0 {
		t.Fatalf("failed verification kept %d tokens", count)
	}
}

func TestAbortCleanup(t *testing.T) {
	s := rangeServer(t)
	client := newRangeClient(t, s)
	token := testToken(t)
	name := "abort/file.bin"
	payload := testPayload(50)
	client.expect(sendRange(token, name, 0, 10, 50, false), payload[:10], "OK RANGE 0 10\n")
	staged := stagedPath(filepath.Join(s.baseDir, name), token)
	if _, err := os.Stat(staged); err != nil {
		t.Fatal(err)
	}
	client.expect(ranged.CmdAbort+" "+token, nil, "OK ABORT\n")
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("ABORT kept the staged file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, name)); !os.IsNotExist(err) {
		t.Fatalf("ABORT created a destination file: %v", err)
	}
	if count := s.tokenCount(t); count != 0 {
		t.Fatalf("ABORT kept %d tokens", count)
	}
	// Aborting an unknown token is not an error: it may already have expired.
	client.expect(ranged.CmdAbort+" "+token, nil, "OK ABORT\n")
	client.expectError(ranged.CmdAbort, nil, "ABORT requires a token")
	client.expectError(ranged.CmdAbort+" "+token+" extra", nil, "ABORT requires a token")
	client.expectError(ranged.CmdAbort+" nothex", nil, "invalid token")
	// COMMIT after ABORT fails, and nothing appears at the destination.
	client.expectError(fmt.Sprintf("%s %s 50", ranged.CmdCommit, token), nil, "unknown transfer token")
	if _, err := os.Stat(filepath.Join(s.baseDir, name)); !os.IsNotExist(err) {
		t.Fatalf("destination file appeared after ABORT: %v", err)
	}
}

func TestTokenExpiry(t *testing.T) {
	s := rangeServer(t)
	client := newRangeClient(t, s)
	token := testToken(t)
	name := "expiry/file.bin"
	client.expect(sendRange(token, name, 0, 4, 8, false), []byte("abcd"), "OK RANGE 0 4\n")
	staged := stagedPath(filepath.Join(s.baseDir, name), token)

	// Not yet idle for the timeout.
	s.tokenTimeout = time.Hour
	if removed := s.expireTokens(time.Now()); removed != 0 {
		t.Fatalf("expired %d live tokens", removed)
	}
	if _, err := os.Stat(staged); err != nil {
		t.Fatalf("staged file removed early: %v", err)
	}
	// Idle past the timeout: the token and its staged file must both go.
	if removed := s.expireTokens(time.Now().Add(2 * time.Hour)); removed != 1 {
		t.Fatalf("expired %d tokens, want 1", removed)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("expiry kept the staged file: %v", err)
	}
	if count := s.tokenCount(t); count != 0 {
		t.Fatalf("expiry kept %d tokens", count)
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, name)); !os.IsNotExist(err) {
		t.Fatalf("expiry created a destination file: %v", err)
	}
	client.expectError(fmt.Sprintf("%s %s 8", ranged.CmdCommit, token), nil, "unknown transfer token")
}

func TestTokenExpirySweeper(t *testing.T) {
	s := NewServer("127.0.0.1:0", t.TempDir())
	// Set the field directly: SetTokenTimeout enforces the 60s floor, which no
	// test can wait for.
	s.tokenTimeout = 40 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- s.Start() }()
	t.Cleanup(func() { s.Stop() })
	until := time.Now().Add(5 * time.Second)
	for s.Addr() == nil {
		if time.Now().After(until) {
			t.Fatal("listener not ready")
		}
		time.Sleep(time.Millisecond)
	}
	client := newRangeClient(t, s)
	token := testToken(t)
	name := "sweeper/file.bin"
	client.expect(sendRange(token, name, 0, 4, 8, false), []byte("abcd"), "OK RANGE 0 4\n")
	staged := stagedPath(filepath.Join(s.baseDir, name), token)
	until = time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(staged); os.IsNotExist(err) {
			break
		}
		if time.Now().After(until) {
			t.Fatal("sweeper never expired the idle token")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if count := s.tokenCount(t); count != 0 {
		t.Fatalf("sweeper kept %d tokens", count)
	}
}

func TestStopRemovesStagedFiles(t *testing.T) {
	s := NewServer("127.0.0.1:0", t.TempDir())
	done := make(chan error, 1)
	go func() { done <- s.Start() }()
	until := time.Now().Add(5 * time.Second)
	for s.Addr() == nil {
		if time.Now().After(until) {
			t.Fatal("listener not ready")
		}
		time.Sleep(time.Millisecond)
	}
	client := newRangeClient(t, s)
	token := testToken(t)
	name := "stop/file.bin"
	client.expect(sendRange(token, name, 0, 4, 64, false), []byte("abcd"), "OK RANGE 0 4\n")
	staged := stagedPath(filepath.Join(s.baseDir, name), token)
	// The first range preallocates the staged file to the full total.
	if info, err := os.Stat(staged); err != nil || info.Size() != 64 {
		t.Fatalf("staged file size %v: %v", info, err)
	}
	stopped := make(chan struct{})
	go func() { s.Stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop deadlocked")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Fatalf("Stop kept the staged file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, name)); !os.IsNotExist(err) {
		t.Fatalf("Stop created a destination file: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return")
	}
}

func TestConcurrentRangesProduceExactFile(t *testing.T) {
	s := rangeServer(t)
	token := testToken(t)
	name := "concurrent/big.bin"
	const workers = 8
	payload := testPayload(workers * 9973)
	total := int64(len(payload))
	chunk := total / workers
	var wg sync.WaitGroup
	failures := make(chan string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			offset := int64(i) * chunk
			length := chunk
			if i == workers-1 {
				length = total - offset
			}
			// One connection per range, as fan-out does with one socket each.
			client := newRangeClient(t, s)
			reply := client.do(sendRange(token, name, offset, length, total, true), payload[offset:offset+length])
			want := fmt.Sprintf("OK RANGE %d %d %x\n", offset, length, sha256.Sum256(payload[offset:offset+length]))
			if reply != want {
				failures <- fmt.Sprintf("range %d+%d: got %q, want %q", offset, length, reply, want)
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	if t.Failed() {
		return
	}
	digest := sha256.Sum256(payload)
	control := newRangeClient(t, s)
	control.expect(fmt.Sprintf("%s %s %d %x", ranged.CmdCommitHash, token, total, digest), nil,
		fmt.Sprintf("OK COMMIT %d %x\n", total, digest))
	stored, err := os.ReadFile(filepath.Join(s.baseDir, name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, payload) {
		t.Fatalf("committed %d bytes, want %d byte-exact", len(stored), len(payload))
	}
}

func TestQUICReusePortGroup(t *testing.T) {
	config, err := ephemeralTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	single, err := listenQUICGroup("127.0.0.1:0", config, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(single.listeners) != 1 || len(single.sockets) != 0 || len(single.transports) != 0 {
		t.Fatal("count 1 must take the original single-listener path")
	}
	single.close()

	group, err := listenQUICGroup("127.0.0.1:0", config, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer group.close()
	if len(group.listeners) == 0 {
		t.Fatal("no listener bound")
	}
	if len(group.listeners) == 1 {
		t.Log("SO_REUSEPORT unavailable; fell back to a single socket")
		return
	}
	port := group.listeners[0].Addr().(*net.UDPAddr).Port
	for i, listener := range group.listeners {
		if got := listener.Addr().(*net.UDPAddr).Port; got != port {
			t.Fatalf("listener %d bound port %d, want the shared %d", i, got, port)
		}
	}
	if len(group.sockets) != len(group.listeners) || len(group.transports) != len(group.listeners) {
		t.Fatal("group bookkeeping mismatch")
	}
}

// startFanoutQUICServer starts a QUIC receiver with an SO_REUSEPORT group.
func startFanoutQUICServer(t *testing.T, connections int) *Server {
	t.Helper()
	s := NewServer("127.0.0.1:0", t.TempDir())
	if err := s.SetTransport("quic"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMaxConnections(connections); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Start() }()
	t.Cleanup(func() {
		stopped := make(chan struct{})
		go func() { s.Stop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Error("Stop deadlocked")
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Start: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("accept loops did not exit")
		}
	})
	until := time.Now().Add(5 * time.Second)
	for s.Addr() == nil {
		select {
		case err := <-done:
			t.Fatalf("Start: %v", err)
		default:
		}
		if time.Now().After(until) {
			t.Fatal("listener not ready")
		}
		time.Sleep(time.Millisecond)
	}
	return s
}

func TestQUICFanoutEndToEnd(t *testing.T) {
	const connections = 4
	s := startFanoutQUICServer(t, connections)
	port := s.Addr().(*net.UDPAddr).Port

	// Every data connection dials the single advertised port.
	conns := make([]*quic.Conn, 0, connections)
	for i := 0; i < connections; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := quic.DialAddr(ctx, fmt.Sprintf("127.0.0.1:%d", port), &tls.Config{
			InsecureSkipVerify: true, // Authentication is covered in tls_test.go.
			NextProtos:         []string{"gosync"},
		}, nil)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		defer conn.CloseWithError(0, "test complete")
		conns = append(conns, conn)
	}

	reply, err := rawOperation(conns[0], ranged.CmdFanout, nil, false)
	if err != nil || string(reply) != fmt.Sprintf("OK FANOUT %d %d\n", connections, port) {
		t.Fatalf("FANOUT: %q, %v", reply, err)
	}

	token := testToken(t)
	name := "quic fanout/payload.bin"
	payload := testPayload(connections * 257 * 1024)
	total := int64(len(payload))
	chunks := ranged.Plan(total, connections, 32*1024)
	if len(chunks) == 0 {
		t.Fatal("no chunks planned")
	}
	if len(chunks) == 1 {
		// Below ranged.MinFanoutSize, Plan returns one chunk; split by hand so
		// the test still exercises several concurrent connections.
		chunks = chunks[:0]
		size := total / connections
		for i := 0; i < connections; i++ {
			length := size
			if i == connections-1 {
				length = total - int64(i)*size
			}
			chunks = append(chunks, ranged.Chunk{Offset: int64(i) * size, Length: length})
		}
	}

	var wg sync.WaitGroup
	failures := make(chan string, len(chunks))
	for i, chunk := range chunks {
		wg.Add(1)
		go func(i int, chunk ranged.Chunk) {
			defer wg.Done()
			conn := conns[i%len(conns)]
			command := sendRange(token, name, chunk.Offset, chunk.Length, total, true)
			got, err := rawOperation(conn, command, payload[chunk.Offset:chunk.Offset+chunk.Length], false)
			if err != nil {
				failures <- fmt.Sprintf("range %d+%d: %v", chunk.Offset, chunk.Length, err)
				return
			}
			want := fmt.Sprintf("OK RANGE %d %d %x\n", chunk.Offset, chunk.Length,
				sha256.Sum256(payload[chunk.Offset:chunk.Offset+chunk.Length]))
			if string(got) != want {
				failures <- fmt.Sprintf("range %d+%d: got %q, want %q", chunk.Offset, chunk.Length, got, want)
			}
		}(i, chunk)
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	if t.Failed() {
		return
	}

	// Commit on the control connection, as the sender does.
	digest := sha256.Sum256(payload)
	command := fmt.Sprintf("%s %s %d %x", ranged.CmdCommitHash, token, total, digest)
	got, err := rawOperation(conns[0], command, nil, false)
	if err != nil || string(got) != fmt.Sprintf("OK COMMIT %d %x\n", total, digest) {
		t.Fatalf("COMMIT-HASH: %q, %v", got, err)
	}
	stored, err := os.ReadFile(filepath.Join(s.baseDir, name))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, payload) {
		t.Fatalf("received %d bytes, want %d byte-exact", len(stored), len(payload))
	}
	entries, err := os.ReadDir(filepath.Join(s.baseDir, "quic fanout"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".gosync-") {
			t.Fatalf("staged file %s survived the commit", entry.Name())
		}
	}
	if count := s.tokenCount(t); count != 1 {
		t.Fatalf("tokens = %d, want the single commit tombstone", count)
	}
	// A repeated commit on another connection must still succeed.
	got, err = rawOperation(conns[1], command, nil, false)
	if err != nil || string(got) != fmt.Sprintf("OK COMMIT %d %x\n", total, digest) {
		t.Fatalf("repeated COMMIT-HASH: %q, %v", got, err)
	}
}

// TestQUICFanoutAcceptsOnEverySocket dials many connections at one port served
// by a socket group. The kernel spreads them unevenly across the group, so a
// socket whose accept loop never ran would leave its share of connections
// completing their handshake and then never answered.
func TestQUICFanoutAcceptsOnEverySocket(t *testing.T) {
	s := startFanoutQUICServer(t, 4)
	s.mu.RLock()
	listeners := len(s.quicGroup.listeners)
	s.mu.RUnlock()
	if listeners == 1 {
		t.Skip("SO_REUSEPORT unavailable; single socket covered elsewhere")
	}
	var wg sync.WaitGroup
	failures := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			conn, err := quic.DialAddr(ctx, s.Addr().String(), &tls.Config{
				InsecureSkipVerify: true, // Authentication is covered in tls_test.go.
				NextProtos:         []string{"gosync"},
			}, nil)
			if err != nil {
				failures <- err
				return
			}
			defer conn.CloseWithError(0, "test complete")
			reply, err := rawOperation(conn, "PING", nil, false)
			if err != nil || string(reply) != "OK PONG\n" {
				failures <- fmt.Errorf("PING: %q, %v", reply, err)
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}

func TestQUICRangeAcrossConnectionsRejectsOverlap(t *testing.T) {
	s := startFanoutQUICServer(t, 2)
	first := dialRawQUIC(t, s)
	second := dialRawQUIC(t, s)
	token := testToken(t)
	name := "quic overlap/file.bin"
	payload := testPayload(2048)
	total := int64(len(payload))
	reply, err := rawOperation(first, sendRange(token, name, 0, 1024, total, false), payload[:1024], false)
	if err != nil || string(reply) != "OK RANGE 0 1024\n" {
		t.Fatalf("first range: %q, %v", reply, err)
	}
	// The registry is shared across connections and sockets, so the overlap is
	// refused even though it arrives on a different connection.
	reply, err = rawOperation(second, sendRange(token, name, 512, 1024, total, false), nil, false)
	if err != nil || !strings.HasPrefix(string(reply), "ERROR ") || !strings.Contains(string(reply), "overlaps") {
		t.Fatalf("overlapping range: %q, %v", reply, err)
	}
	reply, err = rawOperation(second, sendRange(token, name, 1024, 1024, total+1, false), nil, false)
	if err != nil || !strings.Contains(string(reply), "does not match") {
		t.Fatalf("mismatched total: %q, %v", reply, err)
	}
	reply, err = rawOperation(second, sendRange(token, name, 1024, 1024, total, false), payload[1024:], false)
	if err != nil || string(reply) != "OK RANGE 1024 1024\n" {
		t.Fatalf("second range: %q, %v", reply, err)
	}
	reply, err = rawOperation(second, fmt.Sprintf("%s %s %d", ranged.CmdCommit, token, total), nil, false)
	if err != nil || string(reply) != fmt.Sprintf("OK COMMIT %d\n", total) {
		t.Fatalf("COMMIT: %q, %v", reply, err)
	}
	stored, err := os.ReadFile(filepath.Join(s.baseDir, name))
	if err != nil || !bytes.Equal(stored, payload) {
		t.Fatalf("committed file differs: %v", err)
	}
}

func TestStagedFileNameAndDigestHelpers(t *testing.T) {
	if got := stagedPath("/dest/dir/file.bin", "abc"); got != "/dest/dir/file.bin.gosync-abc.part" {
		t.Fatalf("stagedPath = %q", got)
	}
	s := rangeServer(t)
	payload := testPayload(5000)
	path := filepath.Join(s.baseDir, "digest.bin")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	digest, err := s.stagedDigest(path, int64(len(payload)))
	if err != nil || digest != hex.EncodeToString(sha256Sum(payload)) {
		t.Fatalf("stagedDigest = %q, %v", digest, err)
	}
	if _, err := s.stagedDigest(path+".missing", 1); err == nil {
		t.Fatal("digest of a missing file succeeded")
	}
	if _, err := s.stagedDigest(path, int64(len(payload))+1); err == nil {
		t.Fatal("digest past the end of the file succeeded")
	}
}

func sha256Sum(payload []byte) []byte {
	digest := sha256.Sum256(payload)
	return digest[:]
}
