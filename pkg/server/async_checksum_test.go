package server

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gbjohnso/gosync/pkg/checksum"
)

func TestServerSetBufferSize(t *testing.T) {
	s := NewServer("", t.TempDir())
	if s.bufferSize != checksum.DefaultBufferSize {
		t.Fatalf("default buffer size = %d", s.bufferSize)
	}
	for _, size := range []int{checksum.MinBufferSize, 256 * 1024, checksum.MaxBufferSize} {
		if err := s.SetBufferSize(size); err != nil || s.bufferSize != size {
			t.Fatalf("SetBufferSize(%d): size=%d, err=%v", size, s.bufferSize, err)
		}
	}
	for _, size := range []int{-1, 0, checksum.MinBufferSize - 1, checksum.MaxBufferSize + 1} {
		previous := s.bufferSize
		if err := s.SetBufferSize(size); err == nil || !errors.Is(err, checksum.ValidateBufferSize(size)) {
			t.Fatalf("SetBufferSize(%d) = %v, want validation error", size, err)
		}
		if s.bufferSize != previous {
			t.Fatal("invalid setter changed buffer size")
		}
	}
}

func TestAsyncChecksumServerFraming(t *testing.T) {
	for _, bufferSize := range []int{0, 256 * 1024} {
		t.Run(fmt.Sprintf("buffer=%d", bufferSize), func(t *testing.T) {
			asyncChecksumServerFraming(t, bufferSize)
		})
	}
}

func asyncChecksumServerFraming(t *testing.T, bufferSize int) {
	base := t.TempDir()
	s := NewServer("", base)
	if bufferSize != 0 {
		if err := s.SetBufferSize(bufferSize); err != nil {
			t.Fatal(err)
		}
	}
	client, conn := net.Pipe()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); s.handleClient(conn) }()
	t.Cleanup(func() { client.Close(); <-done })
	payload := make([]byte, 4*s.bufferSize+17)
	for i := range payload {
		payload[i] = byte(i*31 + i/251)
	}
	digest := sha256.Sum256(payload)
	encoded := base64.StdEncoding.EncodeToString([]byte("payload"))
	writeDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(client, io.MultiReader(
			strings.NewReader(fmt.Sprintf("SEND %d %s\n", len(payload), encoded)),
			bytes.NewReader(payload), strings.NewReader("PING\n")))
		writeDone <- err
	}()
	reader := bufio.NewReader(client)
	response, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("OK %x %d\n", digest, len(payload)); response != want {
		t.Fatalf("response %q, want %q", response, want)
	}
	response, err = reader.ReadString('\n')
	if err != nil || response != "OK PONG\n" {
		t.Fatalf("next command: %q, %v", response, err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(base, "payload"))
	if err != nil || !bytes.Equal(stored, payload) {
		t.Fatalf("stored payload mismatch: %v", err)
	}

	go func() { _, err := fmt.Fprintf(client, "RECEIVE %s\n", encoded); writeDone <- err }()
	response, err = reader.ReadString('\n')
	if err != nil || response != fmt.Sprintf("OK SIZE %d\n", len(payload)) {
		t.Fatalf("receive header: %q, %v", response, err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(reader, received); err != nil {
		t.Fatal(err)
	}
	response, err = reader.ReadString('\n')
	if err != nil || response != fmt.Sprintf("CHECKSUM %x\n", digest) {
		t.Fatalf("checksum trailer: %q, %v", response, err)
	}
	if !bytes.Equal(received, payload) {
		t.Fatal("received payload mismatch")
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

// Scripted I/O exposes copy buffer lengths without socket fragmentation or startup.
type bufferSizeServerConn struct {
	net.Conn
	input    *bytes.Reader
	output   bytes.Buffer
	maxRead  int
	maxWrite int
}

func (c *bufferSizeServerConn) Read(p []byte) (int, error) {
	c.maxRead = max(c.maxRead, len(p))
	return c.input.Read(p)
}

func (c *bufferSizeServerConn) Write(p []byte) (int, error) {
	c.maxWrite = max(c.maxWrite, len(p))
	return c.output.Write(p)
}

func TestServerBufferSizeHandlerFlow(t *testing.T) {
	for _, configuredSize := range []int{0, 256 * 1024} {
		t.Run(fmt.Sprint(configuredSize), func(t *testing.T) {
			// Preserve compatibility with direct zero-valued test/benchmark structs.
			s := &Server{baseDir: t.TempDir()}
			effectiveSize := checksum.DefaultBufferSize
			if configuredSize != 0 {
				if err := s.SetBufferSize(configuredSize); err != nil {
					t.Fatal(err)
				}
				effectiveSize = configuredSize
			}
			payload := make([]byte, 4*effectiveSize+17)
			for i := range payload {
				payload[i] = byte(i*31 + i/251)
			}
			digest := sha256.Sum256(payload)
			const sentinel = "NEXT FRAME\n"
			conn := &bufferSizeServerConn{input: bytes.NewReader(append(payload[:len(payload):len(payload)], []byte(sentinel)...))}
			s.handleSend(conn, conn, "payload", int64(len(payload)))
			if conn.maxRead != effectiveSize {
				t.Fatalf("SEND read buffer = %d, want %d", conn.maxRead, effectiveSize)
			}
			if want := fmt.Sprintf("OK %x %d\n", digest, len(payload)); conn.output.String() != want {
				t.Fatalf("SEND response = %q, want %q", conn.output.String(), want)
			}
			rest, err := io.ReadAll(conn.input)
			if err != nil || string(rest) != sentinel {
				t.Fatalf("SEND consumed next frame: %q, %v", rest, err)
			}
			stored, err := os.ReadFile(filepath.Join(s.baseDir, "payload"))
			if err != nil || !bytes.Equal(stored, payload) {
				t.Fatalf("stored payload mismatch: %v", err)
			}
			conn.output.Reset()
			conn.maxWrite = 0
			s.handleReceive(conn, "payload")
			if conn.maxWrite != effectiveSize {
				t.Fatalf("RECEIVE write buffer = %d, want %d", conn.maxWrite, effectiveSize)
			}
			want := fmt.Sprintf("OK SIZE %d\n%sCHECKSUM %x\n", len(payload), payload, digest)
			if conn.output.String() != want {
				t.Fatal("RECEIVE header, payload, or checksum differs")
			}
		})
	}
}

func TestAsyncChecksumServerRejectsTruncatedPayload(t *testing.T) {
	for _, payload := range [][]byte{nil, []byte("partial")} {
		base := t.TempDir()
		s := NewServer("", base)
		client, conn := net.Pipe()
		if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer conn.Close()
			s.handleSend(bytes.NewReader(payload), conn, "payload", 100)
		}()
		response, err := io.ReadAll(client)
		client.Close()
		<-done
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(response), "ERROR receive failed:") || strings.Contains(string(response), "OK ") {
			t.Fatalf("truncation acknowledged: %q", response)
		}
	}
}
