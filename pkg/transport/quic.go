package transport

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gbjohnso/gosync/pkg/checksum"
	"github.com/quic-go/quic-go"
)

// QUICTransport multiplexes independent receiver operations over one UDP connection.
// The mutex protects connection state only; transfers never hold it during I/O.
type QUICTransport struct {
	mu               sync.Mutex
	progressCallback func(int64)
	config           Config
	conn             *quic.Conn
	host             string
	remoteBase       string
	generation       uint64
	// Fan-out state, negotiated once per control connection. fanoutMax is 0
	// when the receiver does not support fan-out or was never asked.
	fanoutMax   int
	fanoutPort  int
	dataSockets int
	dataConns   map[*fanoutConn]struct{}
}

func NewQUICTransport(config Config) *QUICTransport {
	config.CertificatePEM = append([]byte(nil), config.CertificatePEM...)
	return &QUICTransport{config: config}
}

func (q *QUICTransport) SetProgressCallback(callback func(int64)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.progressCallback = callback
}

func (q *QUICTransport) SetChecksum(enabled bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.config.Checksum = enabled
}

func (q *QUICTransport) Name() string { return "quic" }

func (q *QUICTransport) timeout() time.Duration {
	if q.config.Timeout <= 0 {
		return 30 * time.Second
	}
	return time.Duration(q.config.Timeout) * time.Second
}

// quicConfig keeps the single-connection configuration untouched. With fan-out
// the control connection is idle for the whole ranged transfer, so keep-alives
// stop it from timing out before COMMIT; data connections use the same config
// because one that finishes early must survive to accept a retried range.
func (q *QUICTransport) quicConfig() *quic.Config {
	config := &quic.Config{MaxIdleTimeout: q.timeout()}
	if q.config.Connections > 1 {
		config.KeepAlivePeriod = q.timeout() / 2
	}
	return config
}

func (q *QUICTransport) Connect(host string, port int) error {
	if q.config.BufferSize != 0 {
		if err := checksum.ValidateBufferSize(q.config.BufferSize); err != nil {
			return fmt.Errorf("invalid buffer size: %w", err)
		}
	}
	tlsConfig, err := quicClientTLSConfig(host, q.config.CertificatePEM)
	if err != nil {
		return fmt.Errorf("invalid QUIC certificate: %w", err)
	}
	q.mu.Lock()
	q.generation++
	generation := q.generation
	old := q.conn
	q.conn, q.remoteBase = nil, ""
	q.fanoutMax, q.fanoutPort = 0, 0
	q.mu.Unlock()
	if old != nil {
		_ = old.CloseWithError(0, "reconnecting")
	}

	ctx, cancel := context.WithTimeout(context.Background(), q.timeout())
	defer cancel()
	conn, err := quic.DialAddr(ctx, net.JoinHostPort(host, strconv.Itoa(port)), tlsConfig, q.quicConfig())
	if err != nil {
		return fmt.Errorf("quic dial failed: %w", err)
	}
	base, err := q.ping(ctx, conn)
	if err != nil {
		_ = conn.CloseWithError(0, "ping failed")
		return fmt.Errorf("ping failed: %w", err)
	}
	if !q.config.Checksum {
		if err := q.capabilities(ctx, conn); err != nil {
			_ = conn.CloseWithError(0, "capability negotiation failed")
			return err
		}
	}
	// Only ask when fan-out was requested, so the default single-connection
	// configuration puts no new command on the wire. A receiver without
	// fan-out answers with an error; that silently selects one connection.
	var fanoutMax, fanoutPort int
	if q.config.Connections > 1 {
		if count, port, fanoutErr := q.queryFanout(ctx, conn); fanoutErr == nil {
			fanoutMax, fanoutPort = count, port
		}
	}
	q.mu.Lock()
	if q.generation != generation {
		q.mu.Unlock()
		_ = conn.CloseWithError(0, "connect superseded")
		return fmt.Errorf("connect canceled: %w", context.Canceled)
	}
	q.conn, q.host, q.remoteBase = conn, host, base
	q.fanoutMax, q.fanoutPort = fanoutMax, fanoutPort
	q.mu.Unlock()
	return nil
}

func (q *QUICTransport) ping(ctx context.Context, conn *quic.Conn) (base string, err error) {
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return "", err
	}
	defer func() { finishQUICStream(stream, err) }()
	deadline, _ := ctx.Deadline()
	if err = stream.SetDeadline(deadline); err != nil {
		return "", err
	}
	if _, err = io.WriteString(stream, "PING BASE\n"); err != nil {
		return "", err
	}
	if err = stream.Close(); err != nil {
		return "", err
	}
	pong, err := readQUICLine(bufio.NewReader(stream))
	if err != nil {
		return "", err
	}
	base = parseRemoteBase(pong)
	if base == "" {
		return "", fmt.Errorf("invalid PONG: %q", pong)
	}
	return base, nil
}

// CAPS is an independent request, never appended to the PING stream.
func (q *QUICTransport) capabilities(ctx context.Context, conn *quic.Conn) (err error) {
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return noHashCompatibilityError(err)
	}
	defer func() { finishQUICStream(stream, err) }()
	deadline, _ := ctx.Deadline()
	if err = stream.SetDeadline(deadline); err != nil {
		return noHashCompatibilityError(err)
	}
	if _, err = io.WriteString(stream, "CAPS\n"); err != nil {
		return noHashCompatibilityError(err)
	}
	if err = stream.Close(); err != nil {
		return noHashCompatibilityError(err)
	}
	line, err := readProtocolLine(bufio.NewReader(stream))
	if err != nil {
		return noHashCompatibilityError(err)
	}
	if line != "OK CAPS NOHASH" {
		return noHashCompatibilityError(fmt.Errorf("unexpected capability response %q", line))
	}
	return nil
}

func (q *QUICTransport) RemoteBase() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.remoteBase
}

func (q *QUICTransport) SendFile(localPath, remotePath string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open file failed: %w", err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat file failed: %w", err)
	}
	return q.SendStream(file, remotePath, stat.Size())
}

func (q *QUICTransport) SendSizedFile(localPath, remotePath string, size int64) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open file failed: %w", err)
	}
	defer file.Close()
	return q.SendStream(file, remotePath, size)
}

func (q *QUICTransport) ReceiveFile(remotePath, localPath string) error {
	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create file failed: %w", err)
	}
	transferErr := q.ReceiveStream(remotePath, file)
	closeErr := file.Close()
	if transferErr != nil {
		return transferErr
	}
	return closeErr
}

// quicOperation refreshes deadlines for every network read/write rather than
// imposing a total transfer deadline. Other busy streams cannot keep a stalled
// operation alive indefinitely. As with the other transports, arbitrary caller
// Readers/Writers must return from their own I/O; we cannot cancel those calls.
type quicOperation struct {
	*quic.Stream
	idle time.Duration
}

func (s *quicOperation) Read(p []byte) (int, error) {
	if err := s.SetReadDeadline(time.Now().Add(s.idle)); err != nil {
		return 0, err
	}
	return s.Stream.Read(p)
}

func (s *quicOperation) Write(p []byte) (int, error) {
	if err := s.SetWriteDeadline(time.Now().Add(s.idle)); err != nil {
		return 0, err
	}
	return s.Stream.Write(p)
}

func (q *QUICTransport) operation() (*quicOperation, func(int64), error) {
	q.mu.Lock()
	conn, callback := q.conn, q.progressCallback
	q.mu.Unlock()
	if conn == nil {
		return nil, nil, fmt.Errorf("not connected")
	}
	ctx, cancel := context.WithTimeout(conn.Context(), q.timeout())
	defer cancel()
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("open stream failed: %w", err)
	}
	return &quicOperation{stream, q.timeout()}, callback, nil
}

func finishQUICStream(stream *quic.Stream, err error) {
	stream.CancelRead(0)
	if err != nil {
		// FIN alone leaves a peer waiting for an incomplete payload. RESET also
		// discards queued writes on failure; never do that after a successful op.
		stream.CancelWrite(1)
	} else {
		_ = stream.Close()
	}
}

// Bound protocol lines so a malformed peer cannot grow memory without limit.
func readQUICLine(reader *bufio.Reader) (string, error) {
	return readProtocolLine(reader)
}

func (q *QUICTransport) SendStream(reader io.Reader, remotePath string, size int64) error {
	if size < 0 {
		return fmt.Errorf("invalid send size: %d", size)
	}
	// Fan-out reads chunks concurrently at offsets, so it needs a ReaderAt.
	// An arbitrary Reader keeps the single-connection path and is never seeked.
	if source, ok := reader.(io.ReaderAt); ok && q.fanoutRequested() {
		if err := q.sendFanout(source, remotePath, size); !errors.Is(err, errFanoutUnavailable) {
			return err
		}
	}
	return q.sendSingleStream(reader, remotePath, size)
}

func (q *QUICTransport) sendSingleStream(reader io.Reader, remotePath string, size int64) (err error) {
	stream, callback, err := q.operation()
	if err != nil {
		return err
	}
	defer func() { finishQUICStream(stream.Stream, err) }()
	if _, err = fmt.Fprintf(stream, "%s %d %s\n", transferCommand("SEND", q.config.Checksum), size, base64.StdEncoding.EncodeToString([]byte(remotePath))); err != nil {
		return fmt.Errorf("send command failed: %w", err)
	}
	digest, err := copyPayload(progressWriter{stream, callback}, reader, size, q.config)
	if err != nil {
		return fmt.Errorf("transfer failed: %w", err)
	}
	// Exactly size bytes, no client checksum trailer. FIN preserves queued data.
	if err = stream.Close(); err != nil {
		return fmt.Errorf("finish request failed: %w", err)
	}
	response, err := readQUICLine(bufio.NewReader(stream))
	if err != nil {
		return err
	}
	return validateSendReply(response, size, digest, q.config.Checksum)
}

func (q *QUICTransport) ReceiveStream(remotePath string, writer io.Writer) (err error) {
	stream, _, err := q.operation()
	if err != nil {
		return err
	}
	defer func() { finishQUICStream(stream.Stream, err) }()
	if _, err = fmt.Fprintf(stream, "%s %s\n", transferCommand("RECEIVE", q.config.Checksum), base64.StdEncoding.EncodeToString([]byte(remotePath))); err != nil {
		return fmt.Errorf("send command failed: %w", err)
	}
	if err = stream.Close(); err != nil {
		return fmt.Errorf("finish request failed: %w", err)
	}
	reader := bufio.NewReader(stream)
	response, err := readQUICLine(reader)
	if err != nil {
		return err
	}
	size, err := receiveSize(response)
	if err != nil {
		return err
	}
	digest, err := copyPayload(writer, reader, size, q.config)
	if err != nil {
		return fmt.Errorf("transfer failed: %w", err)
	}
	line, err := readQUICLine(reader)
	if err != nil {
		return fmt.Errorf("read checksum failed: %w", err)
	}
	return validateTrailer(line, size, digest, q.config.Checksum)
}

func (q *QUICTransport) Close() error {
	q.mu.Lock()
	conn := q.conn
	data := make([]*fanoutConn, 0, len(q.dataConns))
	for dataConn := range q.dataConns {
		data = append(data, dataConn)
	}
	q.dataConns = nil
	q.conn, q.host, q.remoteBase = nil, "", ""
	q.fanoutMax, q.fanoutPort = 0, 0
	q.generation++
	q.mu.Unlock()
	// Data sockets outlive nothing: in-flight ranges fail and their transfer
	// reports failure rather than leaving sockets or goroutines behind.
	var errs []error
	for _, dataConn := range data {
		errs = append(errs, dataConn.close())
	}
	if conn != nil {
		errs = append(errs, conn.CloseWithError(0, ""))
	}
	return errors.Join(errs...)
}

func (q *QUICTransport) IsConnected() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.conn != nil && q.conn.Context().Err() == nil
}

func (q *QUICTransport) buildRemotePath(localPath, baseDir string) string {
	rel, err := filepath.Rel(baseDir, localPath)
	if err != nil {
		return filepath.Base(localPath)
	}
	return filepath.Join(baseDir, rel)
}
