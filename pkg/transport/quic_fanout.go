package transport

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	"github.com/gbjohnso/gosync/pkg/checksum"
	"github.com/gbjohnso/gosync/pkg/ranged"
	"github.com/quic-go/quic-go"
)

// Measurement showed that the sender's socket count carries QUIC throughput
// scaling, so fan-out means several sender UDP sockets, each with its own
// quic.Transport and connection, writing disjoint ranges of one file to the
// receiver's single advertised port. See docs/multi-connection-quic-design.md.

// errFanoutUnavailable means fan-out was not attempted and no payload byte was
// written, so the caller may fall back to the single-connection path. It is
// never returned once ranges have been sent.
var errFanoutUnavailable = errors.New("quic fan-out unavailable")

// fanoutConn is one sender socket: a UDP socket, the quic.Transport that owns
// it, and the single connection dialed over it.
type fanoutConn struct {
	udp       *net.UDPConn
	transport *quic.Transport
	conn      *quic.Conn
	// A transfer tears down its own sockets, and Close tears down whatever is
	// still registered, so teardown must be safe to call twice.
	closeOnce sync.Once
	closeErr  error
}

func (c *fanoutConn) dead() bool { return c.conn == nil || c.conn.Context().Err() != nil }

// close tears down connection, transport and socket. quic.Transport.Close does
// not close a caller-supplied Conn, so the socket is closed explicitly; Close
// also joins the transport's read loop, so no goroutine outlives this call.
func (c *fanoutConn) close() error {
	c.closeOnce.Do(func() {
		var errs []error
		if c.conn != nil {
			errs = append(errs, c.conn.CloseWithError(0, ""))
		}
		if c.transport != nil {
			errs = append(errs, c.transport.Close())
		}
		if c.udp != nil {
			errs = append(errs, c.udp.Close())
		}
		for _, err := range errs {
			// An already-closed socket is a successful teardown, not a failure.
			if err != nil && !errors.Is(err, net.ErrClosed) {
				c.closeErr = errors.Join(c.closeErr, err)
			}
		}
	})
	return c.closeErr
}

// SetConnections configures QUIC data connections (sender sockets) per file.
// It takes effect on the next Connect, which is where fan-out support is
// negotiated. 1 keeps today's single-connection wire behavior exactly.
func (q *QUICTransport) SetConnections(connections int) error {
	if connections < 1 || connections > ranged.MaxConnections {
		return fmt.Errorf("connections must be between 1 and %d", ranged.MaxConnections)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.config.Connections = connections
	return nil
}

func (q *QUICTransport) fanoutRequested() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.config.Connections > 1
}

func (q *QUICTransport) copyBufferSize() int {
	if q.config.BufferSize > 0 {
		return q.config.BufferSize
	}
	return checksum.DefaultBufferSize
}

// queryFanout asks the control connection once, after PING/CAPS. An older
// receiver answers with its unknown-command error; the caller then transfers
// over one connection without further attempts.
func (q *QUICTransport) queryFanout(ctx context.Context, conn *quic.Conn) (maxConnections, port int, err error) {
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { finishQUICStream(stream, err) }()
	deadline, _ := ctx.Deadline()
	if err = stream.SetDeadline(deadline); err != nil {
		return 0, 0, err
	}
	if _, err = io.WriteString(stream, ranged.CmdFanout+"\n"); err != nil {
		return 0, 0, err
	}
	if err = stream.Close(); err != nil {
		return 0, 0, err
	}
	line, err := readProtocolLine(bufio.NewReader(stream))
	if err != nil {
		return 0, 0, err
	}
	return parseFanoutReply(line)
}

// parseFanoutReply accepts only "OK FANOUT <maxConnections> <port>" with
// plausible values. Anything else, including an ERROR reply from a receiver
// without fan-out, is an error and selects single-connection transfer.
func parseFanoutReply(line string) (maxConnections, port int, err error) {
	parts := strings.Split(line, " ")
	if len(parts) != 4 || parts[0] != "OK" || parts[1] != ranged.CmdFanout {
		return 0, 0, fmt.Errorf("invalid FANOUT response: %q", line)
	}
	// parseSize is digit-only, so signs, whitespace and junk are rejected here.
	count, err := parseSize(parts[2])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid FANOUT connection count: %q", parts[2])
	}
	advertised, err := parseSize(parts[3])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid FANOUT port: %q", parts[3])
	}
	if count < 1 || count > ranged.MaxConnections {
		return 0, 0, fmt.Errorf("implausible FANOUT connection count %d (want 1-%d)", count, ranged.MaxConnections)
	}
	if advertised < 1 || advertised > 65535 {
		return 0, 0, fmt.Errorf("implausible FANOUT port %d", advertised)
	}
	return int(count), int(advertised), nil
}

// reserveDataSockets bounds concurrent data sockets across all files in flight,
// so -workers times -connections cannot exhaust file descriptors. It returns
// how many sockets the caller may use, which may be fewer than requested.
func (q *QUICTransport) reserveDataSockets(want int) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	free := ranged.MaxConnections - q.dataSockets
	if want > free {
		want = free
	}
	if want < 0 {
		want = 0
	}
	q.dataSockets += want
	return want
}

func (q *QUICTransport) releaseDataSockets(count int) {
	if count <= 0 {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.dataSockets -= count
	if q.dataSockets < 0 {
		q.dataSockets = 0
	}
}

// registerDataConns publishes the sockets so Close can tear them down while a
// transfer is in flight. It reports false if the transport closed meanwhile.
func (q *QUICTransport) registerDataConns(conns []*fanoutConn) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.conn == nil {
		return false
	}
	if q.dataConns == nil {
		q.dataConns = make(map[*fanoutConn]struct{})
	}
	for _, conn := range conns {
		q.dataConns[conn] = struct{}{}
	}
	return true
}

func (q *QUICTransport) unregisterDataConns(conns []*fanoutConn) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, conn := range conns {
		delete(q.dataConns, conn)
	}
}

func closeFanoutConns(conns []*fanoutConn) {
	for _, conn := range conns {
		_ = conn.close()
	}
}

// sendFanout transfers size bytes of src as disjoint ranges over several sender
// sockets and commits on the control connection. It returns
// errFanoutUnavailable only before any payload byte is sent.
func (q *QUICTransport) sendFanout(src io.ReaderAt, remotePath string, size int64) error {
	q.mu.Lock()
	control, callback := q.conn, q.progressCallback
	requested, maxConnections, port := q.config.Connections, q.fanoutMax, q.fanoutPort
	q.mu.Unlock()
	if control == nil {
		return fmt.Errorf("not connected")
	}
	if requested < 2 || maxConnections < 1 || port < 1 {
		return errFanoutUnavailable
	}

	// Plan owns both fan-out rules: connection count and the minimum size below
	// which fan-out cannot pay for itself.
	chunks := ranged.Plan(size, min(requested, maxConnections), q.copyBufferSize())
	if len(chunks) < 2 {
		return errFanoutUnavailable
	}
	granted := q.reserveDataSockets(len(chunks))
	defer q.releaseDataSockets(granted)
	if granted < 2 {
		return errFanoutUnavailable
	}
	if granted < len(chunks) {
		if chunks = ranged.Plan(size, granted, q.copyBufferSize()); len(chunks) < 2 {
			return errFanoutUnavailable
		}
	}

	token, err := ranged.NewToken()
	if err != nil {
		return err
	}
	conns, err := q.dialDataConns(control, len(chunks), port)
	if err != nil {
		return fmt.Errorf("%w: %v", errFanoutUnavailable, err)
	}
	defer closeFanoutConns(conns)
	if !q.registerDataConns(conns) {
		return fmt.Errorf("not connected")
	}
	defer q.unregisterDataConns(conns)

	// SHA-256 of the whole file cannot be composed from per-range digests, so
	// the source is hashed sequentially alongside the concurrent range sends
	// and the digest is carried by COMMIT-HASH.
	var (
		wholeDigest [sha256.Size]byte
		digestErr   error
		digestDone  chan struct{}
	)
	if q.config.Checksum {
		digestDone = make(chan struct{})
		go func() {
			defer close(digestDone)
			wholeDigest, digestErr = sourceDigest(src, size, q.copyBufferSize())
		}()
	}

	transferErr := q.sendRanges(control, conns, src, token, remotePath, size, chunks, callback)
	if digestDone != nil {
		<-digestDone
		if transferErr == nil {
			transferErr = digestErr
		}
	}
	if transferErr == nil {
		if commitErr := q.commitRanges(token, size, wholeDigest); commitErr != nil {
			transferErr = commitErr
		} else {
			return nil
		}
	}
	// No partial file may appear at the destination, so abort the staged file.
	return errors.Join(transferErr, q.abortRanges(token))
}

// dialDataConns creates count independent sender sockets, each dialing the
// receiver's advertised port at the same peer address the control connection
// authenticated, with the same TLS verification and certificate pinning.
func (q *QUICTransport) dialDataConns(control *quic.Conn, count, port int) ([]*fanoutConn, error) {
	peer, ok := control.RemoteAddr().(*net.UDPAddr)
	if !ok {
		return nil, fmt.Errorf("control connection has no UDP peer address")
	}
	tlsConfig, err := quicClientTLSConfig(q.host, q.config.CertificatePEM)
	if err != nil {
		return nil, fmt.Errorf("invalid QUIC certificate: %w", err)
	}
	remote := &net.UDPAddr{IP: peer.IP, Port: port, Zone: peer.Zone}
	network := "udp4"
	if remote.IP.To4() == nil {
		network = "udp6"
	}
	conns := make([]*fanoutConn, 0, count)
	for i := 0; i < count; i++ {
		conn, err := q.dialDataConn(network, remote, tlsConfig)
		if err != nil {
			closeFanoutConns(conns)
			return nil, err
		}
		conns = append(conns, conn)
	}
	return conns, nil
}

func (q *QUICTransport) dialDataConn(network string, remote *net.UDPAddr, tlsConfig *tls.Config) (*fanoutConn, error) {
	udp, err := net.ListenUDP(network, &net.UDPAddr{Port: 0})
	if err != nil {
		return nil, fmt.Errorf("open data socket failed: %w", err)
	}
	data := &fanoutConn{udp: udp, transport: &quic.Transport{Conn: udp}}
	ctx, cancel := context.WithTimeout(context.Background(), q.timeout())
	defer cancel()
	conn, err := data.transport.Dial(ctx, remote, tlsConfig, q.quicConfig())
	if err != nil {
		_ = data.close()
		return nil, fmt.Errorf("dial data connection failed: %w", err)
	}
	data.conn = conn
	return data, nil
}

type rangeJob struct {
	chunk    ranged.Chunk
	attempts int
}

// sendRanges runs one worker per data connection over a shared queue, so a
// range that fails is retried from its original offset on whichever connection
// becomes free next. Ranges are idempotent fixed-offset writes, so retrying
// elsewhere is safe. The context derives from the control connection: if
// control dies, the transfer fails rather than continuing blindly.
func (q *QUICTransport) sendRanges(control *quic.Conn, conns []*fanoutConn, src io.ReaderAt, token, remotePath string, total int64, chunks []ranged.Chunk, callback func(int64)) error {
	ctx, cancel := context.WithCancel(control.Context())
	defer cancel()

	jobs := make(chan rangeJob, len(chunks))
	for _, chunk := range chunks {
		jobs <- rangeJob{chunk: chunk}
	}
	var (
		mu          sync.Mutex
		firstErr    error
		outstanding = len(chunks)
		closed      bool
	)
	// resolve retires one range, successfully or finally. Queued jobs plus jobs
	// in flight always equal outstanding, so no worker can block forever.
	resolve := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil && firstErr == nil {
			firstErr = err
			cancel()
		}
		if outstanding--; outstanding == 0 && !closed {
			closed = true
			close(jobs)
		}
	}
	budget := q.config.MaxRetries
	if budget < 0 {
		budget = 0
	}

	var workers sync.WaitGroup
	for _, conn := range conns {
		workers.Add(1)
		go func(conn *fanoutConn) {
			defer workers.Done()
			for {
				var job rangeJob
				select {
				case <-ctx.Done():
					return
				case queued, ok := <-jobs:
					if !ok {
						return
					}
					job = queued
				}
				err := q.sendRange(ctx, conn, src, token, remotePath, total, job.chunk, callback)
				if err == nil {
					resolve(nil)
					continue
				}
				job.attempts++
				if job.attempts > budget {
					resolve(fmt.Errorf("range %d+%d failed after %d attempts: %w", job.chunk.Offset, job.chunk.Length, job.attempts, err))
					return
				}
				select {
				case jobs <- job:
				default:
					resolve(fmt.Errorf("range %d+%d could not be requeued: %w", job.chunk.Offset, job.chunk.Length, err))
					return
				}
				if conn.dead() {
					// This socket is gone; its range is already queued for a
					// surviving connection to pick up.
					return
				}
			}
		}(conn)
	}
	workers.Wait()

	mu.Lock()
	defer mu.Unlock()
	if firstErr != nil {
		return firstErr
	}
	if outstanding > 0 {
		return fmt.Errorf("fan-out incomplete: %d range(s) unsent after all data connections stopped", outstanding)
	}
	return nil
}

// sendRange sends exactly one chunk, read concurrently at its own offset, and
// verifies the receiver's acknowledgement for that offset and length.
func (q *QUICTransport) sendRange(ctx context.Context, conn *fanoutConn, src io.ReaderAt, token, remotePath string, total int64, chunk ranged.Chunk, callback func(int64)) (err error) {
	if err = ranged.ValidateRange(chunk.Offset, chunk.Length, total); err != nil {
		return err
	}
	streamCtx, cancel := context.WithTimeout(ctx, q.timeout())
	defer cancel()
	stream, err := conn.conn.OpenStreamSync(streamCtx)
	if err != nil {
		return fmt.Errorf("open range stream failed: %w", err)
	}
	defer func() { finishQUICStream(stream, err) }()
	operation := &quicOperation{stream, q.timeout()}
	command := ranged.CmdSendRange
	if q.config.Checksum {
		command = ranged.CmdSendRangeHash
	}
	if _, err = fmt.Fprintf(operation, "%s %s %d %d %d %s\n", command, token, chunk.Offset, chunk.Length, total,
		base64.StdEncoding.EncodeToString([]byte(remotePath))); err != nil {
		return fmt.Errorf("send range command failed: %w", err)
	}
	// A SectionReader keeps each connection's reads at its own offset; the
	// caller's file is never seeked.
	section := io.NewSectionReader(src, chunk.Offset, chunk.Length)
	digest, err := copyPayload(progressWriter{operation, callback}, section, chunk.Length, q.config)
	if err != nil {
		return fmt.Errorf("range transfer failed: %w", err)
	}
	// Exactly chunk.Length bytes, no trailer. FIN preserves queued data.
	if err = operation.Close(); err != nil {
		return fmt.Errorf("finish range failed: %w", err)
	}
	line, err := readProtocolLine(bufio.NewReader(operation))
	if err != nil {
		return err
	}
	return validateRangeReply(line, chunk.Offset, chunk.Length, digest, q.config.Checksum)
}

func sourceDigest(src io.ReaderAt, size int64, bufferSize int) ([sha256.Size]byte, error) {
	hash := sha256.New()
	if _, err := io.CopyBuffer(hash, io.NewSectionReader(src, 0, size), make([]byte, bufferSize)); err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("hash source failed: %w", err)
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest, nil
}

func (q *QUICTransport) commitRanges(token string, total int64, digest [sha256.Size]byte) error {
	request := fmt.Sprintf("%s %s %d\n", ranged.CmdCommit, token, total)
	if q.config.Checksum {
		request = fmt.Sprintf("%s %s %d %x\n", ranged.CmdCommitHash, token, total, digest)
	}
	line, err := q.controlExchange(request)
	if err != nil {
		return fmt.Errorf("commit failed: %w", err)
	}
	return validateCommitReply(line, total, digest, q.config.Checksum)
}

func (q *QUICTransport) abortRanges(token string) error {
	line, err := q.controlExchange(fmt.Sprintf("%s %s\n", ranged.CmdAbort, token))
	if err != nil {
		return fmt.Errorf("abort failed: %w", err)
	}
	if line != "OK "+ranged.CmdAbort {
		return fmt.Errorf("invalid ABORT acknowledgement: %q", line)
	}
	return nil
}

// controlExchange runs a one-line request/response on the control connection.
func (q *QUICTransport) controlExchange(request string) (line string, err error) {
	stream, _, err := q.operation()
	if err != nil {
		return "", err
	}
	defer func() { finishQUICStream(stream.Stream, err) }()
	if _, err = io.WriteString(stream, request); err != nil {
		return "", fmt.Errorf("send command failed: %w", err)
	}
	if err = stream.Close(); err != nil {
		return "", fmt.Errorf("finish request failed: %w", err)
	}
	return readProtocolLine(bufio.NewReader(stream))
}

// validateRangeReply checks "OK RANGE <offset> <length>[ <hexSHA256>]".
func validateRangeReply(line string, offset, length int64, digest [sha256.Size]byte, enabled bool) error {
	fields := 4
	if enabled {
		fields = 5
	}
	parts := strings.Split(line, " ")
	if len(parts) != fields || parts[0] != "OK" || parts[1] != "RANGE" {
		return fmt.Errorf("invalid RANGE acknowledgement: %q", line)
	}
	if n, err := parseSize(parts[2]); err != nil || n != offset {
		return fmt.Errorf("invalid RANGE acknowledgement offset: %q (want %d)", parts[2], offset)
	}
	if n, err := parseSize(parts[3]); err != nil || n != length {
		return fmt.Errorf("invalid RANGE acknowledgement length: %q (want %d)", parts[3], length)
	}
	if enabled && !digestMatches(parts[4], digest) {
		return fmt.Errorf("RANGE checksum mismatch at offset %d: malformed or different digest %q", offset, parts[4])
	}
	return nil
}

// validateCommitReply checks "OK COMMIT <total>[ <hexSHA256>]". With checksums
// the echoed digest is the receiver's whole-file read-back of what it persisted.
func validateCommitReply(line string, total int64, digest [sha256.Size]byte, enabled bool) error {
	fields := 3
	if enabled {
		fields = 4
	}
	parts := strings.Split(line, " ")
	if len(parts) != fields || parts[0] != "OK" || parts[1] != ranged.CmdCommit {
		return fmt.Errorf("invalid COMMIT acknowledgement: %q", line)
	}
	if n, err := parseSize(parts[2]); err != nil || n != total {
		return fmt.Errorf("invalid COMMIT acknowledgement size: %q (want %d)", parts[2], total)
	}
	if enabled && !digestMatches(parts[3], digest) {
		return fmt.Errorf("COMMIT checksum mismatch: malformed or different digest %q", parts[3])
	}
	return nil
}
