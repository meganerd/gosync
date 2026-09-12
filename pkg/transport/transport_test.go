package transport

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gbjohnso/gosync/pkg/server"
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
		ping, _ := reader.ReadString('\n')
		if ping != "PING BASE\n" {
			t.Errorf("ping = %q", ping)
			return
		}
		conn.Write([]byte("OK PONG\n"))

		header, _ := reader.ReadString('\n')
		if header != "SEND 14 "+base64.StdEncoding.EncodeToString([]byte("remote/file.txt"))+"\n" {
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

		sum := sha256.Sum256(payload)
		wantChecksum := hex.EncodeToString(sum[:])
		conn.Write([]byte(fmt.Sprintf("OK %s 14\n", wantChecksum)))

		quit, _ := reader.ReadString('\n')
		if quit != "QUIT\n" {
			t.Errorf("quit = %q", quit)
		}
	}()

	transport := NewTCPTransport(Config{Checksum: true, Timeout: 1})
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
		ping, _ := reader.ReadString('\n')
		if ping != "PING BASE\n" {
			t.Errorf("ping = %q", ping)
			return
		}
		conn.Write([]byte("OK PONG\n"))

		header, _ := reader.ReadString('\n')
		if header != "RECEIVE "+base64.StdEncoding.EncodeToString([]byte("src/file.txt"))+"\n" {
			t.Errorf("header = %q", header)
			return
		}
		conn.Write([]byte(fmt.Sprintf("OK SIZE %d\n", len(payload))))
		conn.Write(payload)
		sum := sha256.Sum256(payload)
		conn.Write([]byte(fmt.Sprintf("CHECKSUM %s\n", hex.EncodeToString(sum[:]))))
	}()

	transport := NewTCPTransport(Config{Checksum: true, Timeout: 1})
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

func TestTCPTransportRoundTripWithSpaces(t *testing.T) {
	baseDir := t.TempDir()
	server := server.NewServer("127.0.0.1:0", baseDir)
	go func() {
		_ = server.Start()
	}()

	// Wait for the listener to bind before connecting.
	var addr *net.TCPAddr
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if a := server.Addr(); a != nil {
			addr = a.(*net.TCPAddr)
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if addr == nil {
		t.Fatal("server never bound a listener")
	}

	transport := NewTCPTransport(Config{Checksum: true, Timeout: 5})
	if err := transport.Connect("127.0.0.1", addr.Port); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}

	remotePath := "My Movies/Allan Quatermain and the Lost City of Gold 2025 1080p.mkv"
	payload := []byte("payload with spaces in path")
	if err := transport.SendStream(bytes.NewReader(payload), remotePath, int64(len(payload))); err != nil {
		t.Fatalf("SendStream() error = %v", err)
	}

	stored, err := os.ReadFile(filepath.Join(baseDir, filepath.FromSlash(remotePath)))
	if err != nil {
		t.Fatalf("file with spaces not stored intact: %v", err)
	}
	if !bytes.Equal(stored, payload) {
		t.Fatalf("stored = %q, want %q", stored, payload)
	}

	var buf bytes.Buffer
	if err := transport.ReceiveStream(remotePath, &buf); err != nil {
		t.Fatalf("ReceiveStream() error = %v", err)
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Fatalf("received = %q, want %q", buf.Bytes(), payload)
	}

	if err := transport.Close(); err != nil {
		t.Fatal(err)
	}
	server.Stop()
}
