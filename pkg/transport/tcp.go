package transport

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gbjohnso/gosync/pkg/checksum"
)

// TCPTransport is a protocol-correct gosync client for the TCP transport.
// The wire protocol matches the gosync server (pkg/server):
//   - checksum mode uses SEND/RECEIVE; otherwise CAPS negotiates the -NOHASH variants
//   - file data follows the command verbatim (no client trailing sentinel)
//   - acknowledgements and mandatory receive trailers are validated in both modes
//
// Paths are base64-encoded on the wire because they may contain spaces,
// which would otherwise be split apart by the server's space-delimited
// command framing.
//
// It deliberately mirrors ServerTransport (the deploy-flow client), which is
// the reference implementation: writes are serialized with a mutex so parallel
// workers cannot interleave frames, and every operation reads the server
// response so failures and checksum mismatches are surfaced to the caller.
type TCPTransport struct {
	progressCallback func(int64)
	config           Config
	conn             net.Conn
	reader           *bufio.Reader
	remoteBase       string
	mu               sync.Mutex
}

func NewTCPTransport(config Config) *TCPTransport {
	return &TCPTransport{config: config}
}

// SetProgressCallback implements ProgressReporter.
func (t *TCPTransport) SetProgressCallback(callback func(int64)) {
	t.progressCallback = callback
}

func (t *TCPTransport) SetChecksum(enabled bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.config.Checksum = enabled
}

func (t *TCPTransport) Name() string {
	return "tcp"
}

func (t *TCPTransport) Connect(host string, port int) error {
	if t.config.BufferSize != 0 {
		if err := checksum.ValidateBufferSize(t.config.BufferSize); err != nil {
			return fmt.Errorf("invalid buffer size: %w", err)
		}
	}
	t.remoteBase = ""
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, time.Duration(t.config.Timeout)*time.Second)
	if err != nil {
		return fmt.Errorf("connect to server failed: %w", err)
	}

	t.conn = conn
	t.reader = bufio.NewReader(conn)
	connected := false
	defer func() {
		if !connected {
			_ = conn.Close()
			t.conn, t.reader, t.remoteBase = nil, nil, ""
		}
	}()
	timeout := time.Duration(t.config.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	defer conn.SetDeadline(time.Time{})

	if err := t.sendCommand("PING BASE"); err != nil {
		conn.Close()
		return fmt.Errorf("ping failed: %w", err)
	}

	pong, err := t.readResponse()
	if err != nil || !strings.HasPrefix(pong, "OK PONG") {
		conn.Close()
		return fmt.Errorf("ping failed: no PONG from server")
	}

	if !t.config.Checksum {
		if err := requireNoHashCaps(conn, t.reader); err != nil {
			return err
		}
	}
	t.remoteBase = parseRemoteBase(pong)
	connected = true
	return nil
}

func (t *TCPTransport) SendFile(localPath, remotePath string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open file failed: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat file failed: %w", err)
	}

	return t.sendStreamLocked(file, remotePath, stat.Size())
}

func (t *TCPTransport) SendSizedFile(localPath, remotePath string, size int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open file failed: %w", err)
	}
	defer file.Close()
	return t.sendStreamLocked(file, remotePath, size)
}

func (t *TCPTransport) SendStream(reader io.Reader, remotePath string, size int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sendStreamLocked(reader, remotePath, size)
}

// sendStreamLocked must be called with t.mu held so that the command line,
// the payload, and the response read are one atomic unit on the wire.
func (t *TCPTransport) sendStreamLocked(reader io.Reader, remotePath string, size int64) error {
	if t.conn == nil {
		return fmt.Errorf("not connected")
	}

	if size < 0 {
		return fmt.Errorf("invalid send size: %d", size)
	}
	if err := t.sendCommand(fmt.Sprintf("%s %d %s", transferCommand("SEND", t.config.Checksum), size, base64.StdEncoding.EncodeToString([]byte(remotePath)))); err != nil {
		return fmt.Errorf("send command failed: %w", err)
	}

	digest, err := copyPayload(progressWriter{t.conn, t.progressCallback}, reader, size, t.config)
	if err != nil {
		return fmt.Errorf("transfer failed: %w", err)
	}

	response, err := t.readResponse()
	if err != nil {
		return fmt.Errorf("read response failed: %w", err)
	}

	return validateSendReply(response, size, digest, t.config.Checksum)
}

func (t *TCPTransport) ReceiveFile(remotePath, localPath string) error {
	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create file failed: %w", err)
	}
	defer file.Close()

	return t.ReceiveStream(remotePath, file)
}

func (t *TCPTransport) ReceiveStream(remotePath string, writer io.Writer) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.conn == nil {
		return fmt.Errorf("not connected")
	}

	if err := t.sendCommand(fmt.Sprintf("%s %s", transferCommand("RECEIVE", t.config.Checksum), base64.StdEncoding.EncodeToString([]byte(remotePath)))); err != nil {
		return fmt.Errorf("send command failed: %w", err)
	}

	response, err := t.readResponse()
	if err != nil {
		return fmt.Errorf("read response failed: %w", err)
	}

	size, err := receiveSize(response)
	if err != nil {
		return fmt.Errorf("parse size failed: %w", err)
	}

	digest, err := copyPayload(writer, t.reader, size, t.config)
	if err != nil {
		return fmt.Errorf("transfer failed: %w", err)
	}

	checksumLine, err := t.readResponse()
	if err != nil {
		return fmt.Errorf("read checksum failed: %w", err)
	}

	return validateTrailer(checksumLine, size, digest, t.config.Checksum)
}

func (t *TCPTransport) Close() error {
	if t.conn != nil {
		t.sendCommand("QUIT")
		err := t.conn.Close()
		t.conn = nil
		return err
	}
	return nil
}

func (t *TCPTransport) IsConnected() bool {
	return t.conn != nil
}

func (t *TCPTransport) sendCommand(cmd string) error {
	_, err := fmt.Fprintf(t.conn, "%s\n", cmd)
	return err
}

func (t *TCPTransport) readResponse() (string, error) {
	return readProtocolLine(t.reader)
}

func (t *TCPTransport) buildRemotePath(localPath, baseDir string) string {
	rel, err := filepath.Rel(baseDir, localPath)
	if err != nil {
		return filepath.Base(localPath)
	}
	return filepath.Join(baseDir, rel)
}
