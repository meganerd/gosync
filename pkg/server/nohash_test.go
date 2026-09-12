package server

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
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

func TestChecksumModeWire(t *testing.T) {
	for _, protocol := range []string{"tcp", "quic"} {
		for _, buffer := range []int{0, checksum.MinBufferSize, 256 * 1024} {
			t.Run(fmt.Sprintf("%s/buffer=%d", protocol, buffer), func(t *testing.T) {
				s, _ := startTestServer(t, protocol, buffer)
				var exchange func(string, []byte, string)
				if protocol == "quic" {
					conn := dialRawQUIC(t, s)
					exchange = func(command string, payload []byte, want string) {
						t.Helper()
						got, err := rawOperation(conn, command, payload, false)
						if err != nil || string(got) != want {
							t.Fatalf("%s: reply mismatch (got %d bytes, want %d), err=%v", command, len(got), len(want), err)
						}
					}
					// CAPS is a complete operation without waiting for client FIN.
					exchange("CAPS\nPING", nil, "OK CAPS NOHASH\n")
				} else {
					conn, err := net.DialTimeout("tcp", s.Addr().String(), 5*time.Second)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { conn.Close() })
					reader := bufio.NewReader(conn)
					exchange = func(command string, payload []byte, want string) {
						t.Helper()
						conn.SetDeadline(time.Now().Add(10 * time.Second))
						// Pipeline a command directly after every payload/command. It must
						// survive bounded SEND reads and follow the RECEIVE trailer.
						written := make(chan error, 1)
						go func() {
							_, err := io.Copy(conn, io.MultiReader(strings.NewReader(command+"\n"), bytes.NewReader(payload), strings.NewReader("PING\n")))
							written <- err
						}()
						want += "OK PONG\n"
						got := make([]byte, len(want))
						_, err := io.ReadFull(reader, got)
						writeErr := <-written
						if err != nil || writeErr != nil || string(got) != want {
							t.Fatalf("%s: reply mismatch, read=%v write=%v", command, err, writeErr)
						}
					}
				}

				// No capability negotiation is required for legacy clients.
				for _, hash := range []bool{true, false, true} {
					for _, size := range []int{0, 2*s.bufferSize + 17} {
						payload := make([]byte, size)
						for i := range payload {
							payload[i] = byte(i*31 + i/251)
						}
						name := "nested folder/payload file"
						encoded := base64.StdEncoding.EncodeToString([]byte(name))
						send, receive := "SEND-NOHASH", "RECEIVE-NOHASH"
						ack := fmt.Sprintf("OK NONE %d\n", size)
						trailer := fmt.Sprintf("END %d\n", size)
						if hash {
							send, receive = "SEND", "RECEIVE"
							digest := sha256.Sum256(payload)
							ack = fmt.Sprintf("OK %x %d\n", digest, size)
							trailer = fmt.Sprintf("CHECKSUM %x\n", digest)
						}
						exchange(fmt.Sprintf("%s %d %s", send, size, encoded), payload, ack)
						stored, err := os.ReadFile(filepath.Join(s.baseDir, name))
						if err != nil || !bytes.Equal(stored, payload) {
							t.Fatalf("stored payload: %v", err)
						}
						exchange(receive+" "+encoded, nil, fmt.Sprintf("OK SIZE %d\n%s%s", size, payload, trailer))
					}
					exchange("CAPS", nil, "OK CAPS NOHASH\n")
				}
				for _, tc := range []struct{ command, message string }{
					{"CAPS extra", "CAPS takes no arguments"},
					{"SEND-NOHASH", "SEND requires size and path"},
					{"SEND-NOHASH -1 eA==", "SEND requires non-negative numeric size"},
					{"SEND-NOHASH nope eA==", "SEND requires non-negative numeric size"},
					{"SEND-NOHASH 9223372036854775808 eA==", "SEND requires non-negative numeric size"},
					{"SEND-NOHASH 1 !!!", "SEND path not base64 encoded"},
					{"SEND-NOHASH 0 eA== extra", "SEND path not base64 encoded"},
					{"RECEIVE-NOHASH", "RECEIVE requires path"},
					{"RECEIVE-NOHASH !!!", "RECEIVE path not base64 encoded"},
					{"RECEIVE-NOHASH eA== extra", "RECEIVE requires path"},
				} {
					exchange(tc.command, nil, "ERROR "+tc.message+"\n")
				}
				if _, err := os.Stat(filepath.Join(s.baseDir, "x")); !os.IsNotExist(err) {
					t.Fatalf("invalid command created file: %v", err)
				}
			})
		}
	}
}

func TestNoHashWireTruncation(t *testing.T) {
	for _, protocol := range []string{"tcp", "quic"} {
		t.Run(protocol, func(t *testing.T) {
			s, _ := startTestServer(t, protocol, 0)
			var reply []byte
			var err error
			if protocol == "quic" {
				conn := dialRawQUIC(t, s)
				reply, err = rawOperation(conn, "SEND-NOHASH 100 eA==", []byte("short"), true)
				if err != nil {
					t.Fatal(err)
				}
				// A failed transfer must not poison subsequent streams.
				caps, capsErr := rawOperation(conn, "CAPS", nil, false)
				if capsErr != nil || string(caps) != "OK CAPS NOHASH\n" {
					t.Fatalf("CAPS after failure: %q, %v", caps, capsErr)
				}
			} else {
				conn, dialErr := net.DialTimeout("tcp", s.Addr().String(), 5*time.Second)
				if dialErr != nil {
					t.Fatal(dialErr)
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err = io.WriteString(conn, "SEND-NOHASH 100 eA==\nshort"); err != nil {
					t.Fatal(err)
				}
				if err = conn.(*net.TCPConn).CloseWrite(); err != nil {
					t.Fatal(err)
				}
				reply, err = io.ReadAll(conn)
			}
			if err != nil || string(reply) != "ERROR receive failed: EOF\n" {
				t.Fatalf("truncated SEND: %q, %v", reply, err)
			}
		})
	}
}

func TestNoHashHandlerBufferBounds(t *testing.T) {
	for _, buffer := range []int{0, checksum.MinBufferSize, checksum.MaxBufferSize} {
		t.Run(fmt.Sprint(buffer), func(t *testing.T) {
			s := &Server{baseDir: t.TempDir(), bufferSize: buffer}
			effective := buffer
			if effective == 0 {
				effective = checksum.DefaultBufferSize
			}
			payload := bytes.Repeat([]byte("x"), effective+17)
			conn := &bufferSizeServerConn{input: bytes.NewReader(append(payload[:len(payload):len(payload)], []byte("NEXT\n")...))}
			s.handleSendMode(conn, conn, "payload", int64(len(payload)), false)
			if conn.maxRead != effective || conn.output.String() != fmt.Sprintf("OK NONE %d\n", len(payload)) {
				t.Fatal("SEND buffer or response mismatch")
			}
			rest, _ := io.ReadAll(conn.input)
			if string(rest) != "NEXT\n" {
				t.Fatalf("overread: %q", rest)
			}
			conn.output.Reset()
			conn.maxWrite = 0
			s.handleReceiveMode(conn, "payload", false)
			want := fmt.Sprintf("OK SIZE %d\n%sEND %d\n", len(payload), payload, len(payload))
			if conn.maxWrite != effective || conn.output.String() != want {
				t.Fatal("RECEIVE buffer or response mismatch")
			}
		})
	}
}
