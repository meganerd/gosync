package transport

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/quic-go/quic-go"
)

type QUICTransport struct {
	config    Config
	conn      *quic.Conn
	stream    quic.Stream
	connected bool
}

func NewQUICTransport(config Config) *QUICTransport {
	return &QUICTransport{config: config}
}

func (q *QUICTransport) Name() string {
	return "quic"
}

func (q *QUICTransport) Connect(host string, port int) error {
	addr := fmt.Sprintf("%s:%d", host, port)
	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"gosync"},
	}

	quicConfig := &quic.Config{
		MaxIdleTimeout: time.Duration(q.config.Timeout) * time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(q.config.Timeout)*time.Second)
	defer cancel()

	conn, err := quic.DialAddr(ctx, addr, tlsConfig, quicConfig)
	if err != nil {
		return fmt.Errorf("quic dial failed: %w", err)
	}

	q.conn = conn
	q.connected = true
	return nil
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

func (q *QUICTransport) ReceiveFile(remotePath, localPath string) error {
	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create file failed: %w", err)
	}
	defer file.Close()

	return q.ReceiveStream(remotePath, file)
}

func (q *QUICTransport) SendStream(reader io.Reader, remotePath string, size int64) error {
	if !q.connected {
		return fmt.Errorf("not connected")
	}

	stream, err := q.conn.OpenStream()
	if err != nil {
		return fmt.Errorf("open stream failed: %w", err)
	}
	defer stream.Close()

	header := fmt.Sprintf("SEND %s %d\n", remotePath, size)
	_, err = stream.Write([]byte(header))
	if err != nil {
		return fmt.Errorf("send header failed: %w", err)
	}

	hasher := sha256.New()
	writer := io.MultiWriter(stream, hasher)

	_, err = io.Copy(writer, reader)
	if err != nil {
		return fmt.Errorf("send data failed: %w", err)
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))
	_, err = stream.Write([]byte(fmt.Sprintf("\nCHECKSUM %s\n", checksum)))
	if err != nil {
		return fmt.Errorf("send checksum failed: %w", err)
	}

	return nil
}

func (q *QUICTransport) ReceiveStream(remotePath string, writer io.Writer) error {
	if !q.connected {
		return fmt.Errorf("not connected")
	}

	stream, err := q.conn.AcceptStream(nil)
	if err != nil {
		return fmt.Errorf("accept stream failed: %w", err)
	}
	defer stream.Close()

	hasher := sha256.New()
	multiWriter := io.MultiWriter(writer, hasher)

	_, err = io.Copy(multiWriter, stream)
	if err != nil && err != io.EOF {
		return fmt.Errorf("receive data failed: %w", err)
	}

	return nil
}

func (q *QUICTransport) Close() error {
	if q.conn != nil {
		q.connected = false
		return q.conn.CloseWithError(0, "")
	}
	return nil
}

func (q *QUICTransport) IsConnected() bool {
	return q.connected
}

func (q *QUICTransport) buildRemotePath(localPath, baseDir string) string {
	rel, err := filepath.Rel(baseDir, localPath)
	if err != nil {
		return filepath.Base(localPath)
	}
	return filepath.Join(baseDir, rel)
}
