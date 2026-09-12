package transport_test

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gbjohnso/gosync/pkg/server"
	"github.com/gbjohnso/gosync/pkg/transport"
	"github.com/quic-go/quic-go"
)

func TestChecksumModesEndToEnd(t *testing.T) {
	for _, kind := range []string{"tcp", "server", "quic"} {
		for _, enabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/checksum=%t", kind, enabled), func(t *testing.T) {
				base := t.TempDir()
				receiver := server.NewServer("127.0.0.1:0", base)
				if kind == "quic" {
					if err := receiver.SetTransport("quic"); err != nil {
						t.Fatal(err)
					}
				}
				done := make(chan error, 1)
				go func() { done <- receiver.Start() }()
				t.Cleanup(func() {
					receiver.Stop()
					select {
					case err := <-done:
						if err != nil {
							t.Error(err)
						}
					case <-time.After(5 * time.Second):
						t.Error("receiver shutdown timed out")
					}
				})
				deadline := time.Now().Add(5 * time.Second)
				for receiver.Addr() == nil {
					if time.Now().After(deadline) {
						t.Fatal("receiver startup timed out")
					}
					time.Sleep(time.Millisecond)
				}
				// Exercise the zero-value off default as well as explicit legacy mode.
				config := transport.Config{Timeout: 2, Checksum: enabled, BufferSize: 64 * 1024}
				var tr transport.Transport
				port := 0
				if kind == "quic" {
					config.CertificatePEM = receiver.CertificatePEM()
					tr = transport.NewQUICTransport(config)
					port = receiver.Addr().(*net.UDPAddr).Port
				} else {
					port = receiver.Addr().(*net.TCPAddr).Port
					if kind == "tcp" {
						tr = transport.NewTCPTransport(config)
					} else {
						tr = transport.NewServerTransport(config)
					}
				}
				t.Cleanup(func() { tr.Close() })
				if err := tr.Connect("127.0.0.1", port); err != nil {
					t.Fatal(err)
				}
				var progress atomic.Int64
				tr.(transport.ProgressReporter).SetProgressCallback(func(n int64) { progress.Add(n) })
				for _, size := range []int{0, 7, 64*1024 - 1, 64 * 1024, 64*1024 + 1, 256*1024 + 17} {
					payload := make([]byte, size)
					for i := range payload {
						payload[i] = byte(i*31 + i/251)
					}
					for _, file := range []bool{false, true} {
						remote := fmt.Sprintf("nested folder/%d-%t 雪", size, file)
						local := filepath.Join(t.TempDir(), "local")
						before := progress.Load()
						var err error
						if file {
							if err := os.WriteFile(local, payload, 0600); err != nil {
								t.Fatal(err)
							}
							err = tr.SendFile(local, remote)
						} else {
							err = tr.SendStream(bytes.NewReader(payload), remote, int64(size))
						}
						if err != nil {
							t.Fatal(err)
						}
						if progress.Load()-before != int64(size) {
							t.Fatal("progress must count only payload bytes")
						}
						stored, err := os.ReadFile(filepath.Join(base, remote))
						if err != nil || !bytes.Equal(stored, payload) {
							t.Fatalf("stored payload mismatch: %v", err)
						}
						var got []byte
						if file {
							err = tr.ReceiveFile(remote, local)
							if err == nil {
								got, err = os.ReadFile(local)
							}
						} else {
							var dst bytes.Buffer
							err = tr.ReceiveStream(remote, &dst)
							got = dst.Bytes()
						}
						if err != nil || !bytes.Equal(got, payload) {
							t.Fatalf("received payload mismatch: %v", err)
						}
					}
				}
			})
		}
	}
}

func TestQUICNoHashCapabilities(t *testing.T) {
	for _, reply := range []string{"", "OK\n", "ERROR unknown command\n", "OK CAPS\n", "OK CAPS NOHASH extra\n", "OK CAPS NOHASH \n", " OK CAPS NOHASH\n", "OK CAPS NOHASH", "OK CAPS NOHASH\r\n"} {
		t.Run(fmt.Sprintf("%q", reply), func(t *testing.T) {
			var requests atomic.Int64
			client, err := quicTestPeer(t, transport.Config{Timeout: 1}, "OK PONG "+encodedPath("/base")+"\n", func(s *quic.Stream) error {
				requests.Add(1)
				wire, err := io.ReadAll(s)
				if err != nil || string(wire) != "CAPS\n" || s.StreamID() != 4 {
					return fmt.Errorf("expected separate CAPS stream: id=%d wire=%q err=%v", s.StreamID(), wire, err)
				}
				_, err = io.WriteString(s, reply)
				return err
			})
			if err == nil || !strings.Contains(err.Error(), "upgrade receiver") || !strings.Contains(err.Error(), "-checksum") || client.IsConnected() || client.RemoteBase() != "" {
				t.Fatalf("Connect = %v, connected=%t", err, client.IsConnected())
			}
			if err := client.SendStream(strings.NewReader("payload"), "file", 7); err == nil {
				t.Fatal("send after failed CAPS succeeded")
			}
			if requests.Load() != 1 {
				t.Fatalf("requests=%d, want CAPS only", requests.Load())
			}
		})
	}
}

func TestQUICNoHashCapabilityTimeout(t *testing.T) {
	start := time.Now()
	client, err := quicTestPeer(t, transport.Config{Timeout: 1}, "OK PONG "+encodedPath("/base")+"\n", func(s *quic.Stream) error {
		wire, err := io.ReadAll(s)
		if err != nil || string(wire) != "CAPS\n" {
			return fmt.Errorf("caps %q: %v", wire, err)
		}
		<-s.Context().Done()
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "-checksum") || client.IsConnected() || time.Since(start) > 3*time.Second {
		t.Fatalf("Connect = %v, duration=%v", err, time.Since(start))
	}
}

func TestQUICNoHashStrictReplies(t *testing.T) {
	for _, send := range []bool{false, true} {
		good := "OK SIZE 7\npayloadEND 7\n"
		replies := []string{good, "", "OK SIZE -1\n", "OK SIZE +7\n", "OK SIZE 7 extra\n", "OK SIZE 8\nshort", "OK SIZE 7\npayload", "OK SIZE 7\npayloadEND\n", "OK SIZE 7\npayloadEND 8\n", "OK SIZE 7\npayloadEND -1\n", "OK SIZE 7\npayloadEND +7\n", "OK SIZE 7\npayloadEND 7 extra\n", "OK SIZE 7\npayloadCHECKSUM " + digestHex([]byte("payload")) + "\n", "OK SIZE 7\npayloadEND 7"}
		if send {
			good = "OK NONE 7\n"
			replies = []string{good, "", "OK\n", "OKAY NONE 7\n", "OK NONE 8\n", "OK NONE -1\n", "OK NONE +7\n", "OK NONE nope\n", "OK NONE 7 extra\n", "OK " + digestHex([]byte("payload")) + " 7\n", "OK NONE 7"}
		}
		for i, reply := range replies {
			t.Run(fmt.Sprintf("send=%t/reply=%d", send, i), func(t *testing.T) {
				var mu sync.Mutex
				ids := map[quic.StreamID]bool{}
				client := connectedQUICPeer(t, transport.Config{Timeout: 2}, func(s *quic.Stream) error {
					mu.Lock()
					duplicate := ids[s.StreamID()]
					ids[s.StreamID()] = true
					mu.Unlock()
					if duplicate {
						return fmt.Errorf("reused stream %d", s.StreamID())
					}
					wire, err := io.ReadAll(s)
					if err != nil {
						return err
					}
					if string(wire) == "CAPS\n" {
						if s.StreamID() != 4 {
							return fmt.Errorf("CAPS not on separate stream")
						}
						_, err = io.WriteString(s, "OK CAPS NOHASH\n")
						return err
					}
					want := "RECEIVE-NOHASH " + encodedPath(quicTestPath) + "\n"
					if send {
						want = "SEND-NOHASH 7 " + encodedPath(quicTestPath) + "\npayload"
					}
					if string(wire) != want || s.StreamID() < 8 {
						return fmt.Errorf("wrong transfer stream: id=%d wire=%q", s.StreamID(), wire)
					}
					_, err = io.WriteString(s, reply)
					return err
				})
				// Repeat successful requests to check fresh streams after CAPS and transfers.
				count := 1
				if i == 0 {
					count = 2
				}
				for j := 0; j < count; j++ {
					var err error
					if send {
						err = client.SendStream(strings.NewReader("payload"), quicTestPath, 7)
					} else {
						err = client.ReceiveStream(quicTestPath, io.Discard)
					}
					if (err == nil) != (reply == good) {
						t.Fatalf("reply %q: error=%v", reply, err)
					}
				}
			})
		}
	}
}
