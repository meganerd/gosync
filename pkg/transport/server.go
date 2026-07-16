package transport

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type ServerTransport struct {
	config Config
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex
}

func NewServerTransport(config Config) *ServerTransport {
	return &ServerTransport{
		config: config,
	}
}

func (t *ServerTransport) Connect(host string, port int) error {
	addr := fmt.Sprintf("%s:%d", host, port)
	conn, err := net.DialTimeout("tcp", addr, time.Duration(t.config.Timeout)*time.Second)
	if err != nil {
		return fmt.Errorf("connect to server failed: %w", err)
	}

	t.conn = conn
	t.reader = bufio.NewReader(conn)

	if err := t.sendCommand("PING"); err != nil {
		conn.Close()
		return fmt.Errorf("ping failed: %w", err)
	}

	pong, err := t.readResponse()
	if err != nil || !strings.HasPrefix(pong, "OK PONG") {
		conn.Close()
		return fmt.Errorf("ping failed: no PONG from server")
	}

	return nil
}

func (t *ServerTransport) SendFile(localPath, remotePath string) error {
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

	size := stat.Size()

	if err := t.sendCommand(fmt.Sprintf("SEND %s %d", remotePath, size)); err != nil {
		return fmt.Errorf("send command failed: %w", err)
	}

	hasher := sha256.New()
	writer := io.MultiWriter(t.conn, hasher)

	remaining := size
	buf := make([]byte, 32*1024)
	for remaining > 0 {
		n, err := file.Read(buf)
		if n > 0 {
			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("write failed: %w", writeErr)
			}
			remaining -= int64(n)
		}
		if err != nil {
			if err != io.EOF {
				return fmt.Errorf("read failed: %w", err)
			}
			break
		}
	}

	response, err := t.readResponse()
	if err != nil {
		return fmt.Errorf("read response failed: %w", err)
	}

	if !strings.HasPrefix(response, "OK") {
		return fmt.Errorf("server error: %s", response)
	}

	parts := strings.Fields(response)
	if len(parts) >= 3 {
		expectedChecksum := parts[1]
		actualChecksum := hex.EncodeToString(hasher.Sum(nil))
		if expectedChecksum != actualChecksum {
			return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum)
		}
	}

	return nil
}

func (t *ServerTransport) ReceiveFile(remotePath, localPath string) error {
	if err := t.sendCommand(fmt.Sprintf("RECEIVE %s", remotePath)); err != nil {
		return fmt.Errorf("send command failed: %w", err)
	}

	response, err := t.readResponse()
	if err != nil {
		return fmt.Errorf("read response failed: %w", err)
	}

	if !strings.HasPrefix(response, "OK SIZE") {
		return fmt.Errorf("server error: %s", response)
	}

	sizeStr := strings.TrimPrefix(response, "OK SIZE ")
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return fmt.Errorf("parse size failed: %w", err)
	}

	file, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("create file failed: %w", err)
	}
	defer file.Close()

	hasher := sha256.New()
	writer := io.MultiWriter(file, hasher)

	remaining := size
	buf := make([]byte, 32*1024)
	for remaining > 0 {
		n, err := t.conn.Read(buf)
		if n > 0 {
			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("write failed: %w", writeErr)
			}
			remaining -= int64(n)
		}
		if err != nil {
			if err != io.EOF {
				return fmt.Errorf("read failed: %w", err)
			}
			break
		}
	}

	checksumLine, err := t.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read checksum failed: %w", err)
	}

	checksumLine = strings.TrimSpace(checksumLine)
	if strings.HasPrefix(checksumLine, "CHECKSUM ") {
		expectedChecksum := strings.TrimPrefix(checksumLine, "CHECKSUM ")
		actualChecksum := hex.EncodeToString(hasher.Sum(nil))
		if expectedChecksum != actualChecksum {
			return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum)
		}
	}

	return nil
}

func (t *ServerTransport) SendStream(reader io.Reader, remotePath string, size int64) error {
	if err := t.sendCommand(fmt.Sprintf("SEND %s %d", remotePath, size)); err != nil {
		return fmt.Errorf("send command failed: %w", err)
	}

	hasher := sha256.New()
	writer := io.MultiWriter(t.conn, hasher)

	remaining := size
	buf := make([]byte, 32*1024)
	for remaining > 0 {
		n, err := reader.Read(buf)
		if n > 0 {
			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("write failed: %w", writeErr)
			}
			remaining -= int64(n)
		}
		if err != nil {
			if err != io.EOF {
				return fmt.Errorf("read failed: %w", err)
			}
			break
		}
	}

	response, err := t.readResponse()
	if err != nil {
		return fmt.Errorf("read response failed: %w", err)
	}

	if !strings.HasPrefix(response, "OK") {
		return fmt.Errorf("server error: %s", response)
	}

	parts := strings.Fields(response)
	if len(parts) >= 3 {
		expectedChecksum := parts[1]
		actualChecksum := hex.EncodeToString(hasher.Sum(nil))
		if expectedChecksum != actualChecksum {
			return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum)
		}
	}

	return nil
}

func (t *ServerTransport) ReceiveStream(remotePath string, writer io.Writer) error {
	if err := t.sendCommand(fmt.Sprintf("RECEIVE %s", remotePath)); err != nil {
		return fmt.Errorf("send command failed: %w", err)
	}

	response, err := t.readResponse()
	if err != nil {
		return fmt.Errorf("read response failed: %w", err)
	}

	if !strings.HasPrefix(response, "OK SIZE") {
		return fmt.Errorf("server error: %s", response)
	}

	sizeStr := strings.TrimPrefix(response, "OK SIZE ")
	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return fmt.Errorf("parse size failed: %w", err)
	}

	hasher := sha256.New()
	copyWriter := io.MultiWriter(writer, hasher)

	remaining := size
	buf := make([]byte, 32*1024)
	for remaining > 0 {
		n, err := t.conn.Read(buf)
		if n > 0 {
			if _, writeErr := copyWriter.Write(buf[:n]); writeErr != nil {
				return fmt.Errorf("write failed: %w", writeErr)
			}
			remaining -= int64(n)
		}
		if err != nil {
			if err != io.EOF {
				return fmt.Errorf("read failed: %w", err)
			}
			break
		}
	}

	checksumLine, err := t.reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read checksum failed: %w", err)
	}

	checksumLine = strings.TrimSpace(checksumLine)
	if strings.HasPrefix(checksumLine, "CHECKSUM ") {
		expectedChecksum := strings.TrimPrefix(checksumLine, "CHECKSUM ")
		actualChecksum := hex.EncodeToString(hasher.Sum(nil))
		if expectedChecksum != actualChecksum {
			return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedChecksum, actualChecksum)
		}
	}

	return nil
}

func (t *ServerTransport) Close() error {
	if t.conn != nil {
		t.sendCommand("QUIT")
		return t.conn.Close()
	}
	return nil
}

func (t *ServerTransport) IsConnected() bool {
	return t.conn != nil
}

func (t *ServerTransport) Name() string {
	return "server"
}

func (t *ServerTransport) sendCommand(cmd string) error {
	_, err := fmt.Fprintf(t.conn, "%s\n", cmd)
	return err
}

func (t *ServerTransport) readResponse() (string, error) {
	line, err := t.reader.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
