package transport

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	_ Transport = (*TCPTransport)(nil)
	_ Transport = (*QUICTransport)(nil)
	_ Transport = (*SSHTransport)(nil)
	_ Transport = (*ServerTransport)(nil)
)

func TestTCPTransportSendStreamAndClose(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	payload := []byte("hello over tcp")
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		reader := bufio.NewReader(conn)
		header, _ := reader.ReadString('\n')
		if header != "SEND remote/file.txt 14\n" {
			t.Errorf("header = %q", header)
			return
		}

		body := make([]byte, len(payload))
		if _, err := io.ReadFull(reader, body); err != nil {
			t.Errorf("ReadFull body: %v", err)
			return
		}
		if !bytes.Equal(body, payload) {
			t.Errorf("body = %q, want %q", body, payload)
		}

		checksumLine, _ := reader.ReadString('\n')
		sum := sha256.Sum256(payload)
		wantChecksum := hex.EncodeToString(sum[:])
		if strings.TrimSpace(checksumLine) != "CHECKSUM "+wantChecksum {
			t.Errorf("checksum line = %q, want %q", strings.TrimSpace(checksumLine), "CHECKSUM "+wantChecksum)
		}
	}()

	transport := NewTCPTransport(Config{Timeout: 1})
	addr := ln.Addr().(*net.TCPAddr)
	if err := transport.Connect("127.0.0.1", addr.Port); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if !transport.IsConnected() || transport.Name() != "tcp" {
		t.Fatal("transport should report connected tcp")
	}

	if err := transport.SendStream(bytes.NewReader(payload), "remote/file.txt", int64(len(payload))); err != nil {
		t.Fatalf("SendStream() error = %v", err)
	}
	if err := transport.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if transport.IsConnected() {
		t.Fatal("transport should report disconnected after Close")
	}
	<-serverDone
}

func TestTCPTransportReceiveStreamAndFileOperations(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	payload := []byte("remote payload")
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		header, _ := reader.ReadString('\n')
		if header != "RECEIVE src/file.txt\n" {
			t.Errorf("header = %q", header)
			return
		}
		conn.Write(payload)
	}()

	transport := NewTCPTransport(Config{Timeout: 1})
	addr := ln.Addr().(*net.TCPAddr)
	if err := transport.Connect("127.0.0.1", addr.Port); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	var buf bytes.Buffer
	if err := transport.ReceiveStream("src/file.txt", &buf); err != nil {
		t.Fatalf("ReceiveStream() error = %v", err)
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Fatalf("received payload = %q, want %q", buf.Bytes(), payload)
	}

	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}

	tmp := t.TempDir()
	file := filepath.Join(tmp, "local.txt")
	if err := os.WriteFile(file, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := transport.SendFile(file, "ignored"); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("SendFile() without connection error = %v", err)
	}
	if err := transport.ReceiveFile("remote", filepath.Join(tmp, "out.txt")); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("ReceiveFile() without connection error = %v", err)
	}
}

func TestTCPTransportBuildRemotePathFallback(t *testing.T) {
	transport := NewTCPTransport(Config{})
	if got := transport.buildRemotePath(filepath.Join("base", "sub", "file.txt"), "base"); got != filepath.Join("base", "sub", "file.txt") {
		t.Fatalf("buildRemotePath() = %q", got)
	}

	if err := transport.Connect("127.0.0.1", 1); err == nil {
		t.Fatal("expected connect to closed port to fail")
	}
	if transport.IsConnected() {
		t.Fatal("failed connect should not set connected")
	}

	time.Sleep(1 * time.Millisecond)
}
