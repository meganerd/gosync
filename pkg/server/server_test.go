package server

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
)

func TestServerHandleClientPingListAndUnknown(t *testing.T) {
	baseDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(baseDir, "one.txt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	server := NewServer("127.0.0.1:0", baseDir)
	client, conn := net.Pipe()
	defer client.Close()
	go server.handleClient(conn)

	reader := bufio.NewReader(client)
	if _, err := fmt.Fprint(client, "PING\n"); err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(line) != "OK PONG" {
		t.Fatalf("PING response = %q", line)
	}

	if _, err := fmt.Fprint(client, "LIST\n"); err != nil {
		t.Fatal(err)
	}
	line, err = reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(line), "OK LIST 1") {
		t.Fatalf("LIST header = %q", line)
	}
	entry, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(entry, "one.txt") {
		t.Fatalf("LIST entry = %q", entry)
	}

	if _, err := fmt.Fprint(client, "BOGUS\n"); err != nil {
		t.Fatal(err)
	}
	line, err = reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(line), "ERROR unknown command") {
		t.Fatalf("unknown command response = %q", line)
	}

	if _, err := fmt.Fprint(client, "QUIT\n"); err != nil {
		t.Fatal(err)
	}
}

func TestServerHandleClientSendWithSpacesInPath(t *testing.T) {
	baseDir := t.TempDir()
	server := NewServer("127.0.0.1:0", baseDir)

	data := []byte("spaced payload")
	client, conn := net.Pipe()
	go server.handleClient(conn)
	reader := bufio.NewReader(client)

	spacedPath := "My Movie Folder/My Movie 2024 1080p.mkv"
	if _, err := fmt.Fprintf(client, "SEND %d %s\n", len(data), base64.StdEncoding.EncodeToString([]byte(spacedPath))); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := client.Write(data); err != nil {
		t.Fatal(err)
	}
	response, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(response), "OK ") {
		t.Fatalf("SEND response = %q", response)
	}
	stored, err := os.ReadFile(filepath.Join(baseDir, filepath.FromSlash(spacedPath)))
	if err != nil {
		t.Fatalf("spaced path not stored intact: %v", err)
	}
	if !bytes.Equal(stored, data) {
		t.Fatalf("stored data = %q, want %q", stored, data)
	}
	client.Close()

	client2, conn2 := net.Pipe()
	defer client2.Close()
	go server.handleClient(conn2)
	reader2 := bufio.NewReader(client2)

	if _, err := fmt.Fprint(client2, "RECEIVE "+base64.StdEncoding.EncodeToString([]byte(spacedPath))+"\n"); err != nil {
		t.Fatal(err)
	}
	header, err := reader2.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(header) != "OK SIZE 14" {
		t.Fatalf("RECEIVE header = %q", header)
	}
	body := make([]byte, 14)
	if _, err := io.ReadFull(reader2, body); err != nil {
		t.Fatal(err)
	}
	if string(body) != "spaced payload" {
		t.Fatalf("RECEIVE body = %q", body)
	}
	checksumLine, err := reader2.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	serverChecksum := sha256.Sum256([]byte("spaced payload"))
	if strings.TrimSpace(checksumLine) != "CHECKSUM "+hex.EncodeToString(serverChecksum[:]) {
		t.Fatalf("checksum line = %q", checksumLine)
	}
}

func TestServerHandleClientSendAndReceive(t *testing.T) {
	baseDir := t.TempDir()
	server := NewServer("127.0.0.1:0", baseDir)

	data := []byte("payload from client")
	client, conn := net.Pipe()
	go server.handleClient(conn)
	reader := bufio.NewReader(client)

	if _, err := fmt.Fprintf(client, "SEND %d %s\n", len(data), base64.StdEncoding.EncodeToString([]byte("nested/file.txt"))); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if _, err := client.Write(data); err != nil {
		t.Fatal(err)
	}
	response, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(response), "OK ") {
		t.Fatalf("SEND response = %q", response)
	}
	stored, err := os.ReadFile(filepath.Join(baseDir, "nested", "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, data) {
		t.Fatalf("stored data = %q, want %q", stored, data)
	}
	client.Close()

	receivePath := filepath.Join(baseDir, "from-server.txt")
	serverChecksum := sha256.Sum256([]byte("server payload"))
	if err := os.WriteFile(receivePath, []byte("server payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	client2, conn2 := net.Pipe()
	defer client2.Close()
	go server.handleClient(conn2)
	reader2 := bufio.NewReader(client2)

	if _, err := fmt.Fprint(client2, "RECEIVE "+base64.StdEncoding.EncodeToString([]byte("from-server.txt"))+"\n"); err != nil {
		t.Fatal(err)
	}
	header, err := reader2.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(header) != "OK SIZE 14" {
		t.Fatalf("RECEIVE header = %q", header)
	}
	body := make([]byte, 14)
	if _, err := io.ReadFull(reader2, body); err != nil {
		t.Fatal(err)
	}
	if string(body) != "server payload" {
		t.Fatalf("RECEIVE body = %q", body)
	}
	checksumLine, err := reader2.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(checksumLine) != "CHECKSUM "+hex.EncodeToString(serverChecksum[:]) {
		t.Fatalf("checksum line = %q", checksumLine)
	}
}
