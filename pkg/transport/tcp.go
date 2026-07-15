package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

type TCPTransport struct {
	config   Config
	conn     net.Conn
	connected bool
}

func NewTCPTransport(config Config) *TCPTransport {
	return &TCPTransport{config: config}
}

func (t *TCPTransport) Name() string {
	return "tcp"
}

func (t *TCPTransport) Connect(host string, port int) error {
	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := net.DialTimeout("tcp", addr, time.Duration(t.config.Timeout)*time.Second)
	if err != nil {
		return fmt.Errorf("tcp connect failed: %w", err)
	}
	t.conn = conn
	t.connected = true
	return nil
}

func (t *TCPTransport) SendFile(localPath, remotePath string) error {
	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("open file failed: %w", err)
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat file failed: %w", err)
	}

	return t.SendStream(file, remotePath, stat.Size())
}

func (t *TCPTransport) ReceiveFile(remotePath, localPath string) error {
	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create file failed: %w", err)
	}
	defer file.Close()

	return t.ReceiveStream(remotePath, file)
}

func (t *TCPTransport) SendStream(reader io.Reader, remotePath string, size int64) error {
	if !t.connected {
		return fmt.Errorf("not connected")
	}

	header := fmt.Sprintf("SEND %s %d\n", remotePath, size)
	_, err := t.conn.Write([]byte(header))
	if err != nil {
		return fmt.Errorf("send header failed: %w", err)
	}

	hasher := sha256.New()
	writer := io.MultiWriter(t.conn, hasher)

	_, err = io.Copy(writer, reader)
	if err != nil {
		return fmt.Errorf("send data failed: %w", err)
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))
	_, err = t.conn.Write([]byte(fmt.Sprintf("\nCHECKSUM %s\n", checksum)))
	if err != nil {
		return fmt.Errorf("send checksum failed: %w", err)
	}

	return nil
}

func (t *TCPTransport) ReceiveStream(remotePath string, writer io.Writer) error {
	if !t.connected {
		return fmt.Errorf("not connected")
	}

	header := fmt.Sprintf("RECEIVE %s\n", remotePath)
	_, err := t.conn.Write([]byte(header))
	if err != nil {
		return fmt.Errorf("send receive request failed: %w", err)
	}

	hasher := sha256.New()
	multiWriter := io.MultiWriter(writer, hasher)

	_, err = io.Copy(multiWriter, t.conn)
	if err != nil && err != io.EOF {
		return fmt.Errorf("receive data failed: %w", err)
	}

	return nil
}

func (t *TCPTransport) Close() error {
	if t.conn != nil {
		t.connected = false
		return t.conn.Close()
	}
	return nil
}

func (t *TCPTransport) IsConnected() bool {
	return t.connected
}

func (t *TCPTransport) buildRemotePath(localPath, baseDir string) string {
	rel, err := filepath.Rel(baseDir, localPath)
	if err != nil {
		return filepath.Base(localPath)
	}
	return filepath.Join(baseDir, rel)
}
