package transport

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	_ ChecksumConfigurer = (*TCPTransport)(nil)
	_ ChecksumConfigurer = (*ServerTransport)(nil)
	_ ChecksumConfigurer = (*QUICTransport)(nil)
)

func TestStrictTCPReplies(t *testing.T) {
	hash := checksumIntegrationHex([]byte("payload"))
	for _, kind := range []string{"tcp", "server"} {
		for _, enabled := range []bool{false, true} {
			ack := "OK NONE 7\n"
			trailer := "END 7\n"
			if enabled {
				ack = "OK " + hash + " 7\n"
				trailer = "CHECKSUM " + hash + "\n"
			}
			sends := []string{ack, "", "OK\n", "OK NONE\n", "OKAY NONE 7\n", "OK NONE 8\n", "OK NONE -1\n", "OK NONE +7\n", "OK NONE nope\n", "OK NONE 9223372036854775808\n", "OK ab 7\n", "OK " + strings.Repeat("z", 64) + " 7\n", "OK " + checksumIntegrationHex(nil) + " 7\n", "OK " + hash + " 8\n", strings.TrimSuffix(ack, "\n"), strings.TrimSuffix(ack, "\n") + " extra\n", " " + ack, strings.Repeat("x", 5000) + "\n"}
			receives := []string{"OK SIZE 7\npayload" + trailer, "", "OK SIZE -1\n", "OK SIZE +7\n", "OK SIZE nope\n", "OK SIZE 9223372036854775808\n", "OK SIZES 7\n", "OK SIZE 7 extra\n", "OK SIZE 8\nshort", "OK SIZE 7\npayload", "OK SIZE 7\npayload\n", "OK SIZE 7\npayloadBOGUS\n", "OK SIZE 7\npayloadCHECKSUM ab\n", "OK SIZE 7\npayloadCHECKSUM " + checksumIntegrationHex(nil) + "\n", "OK SIZE 7\npayloadEND 8\n", "OK SIZE 7\npayloadEND -1\n", "OK SIZE 7\npayloadEND +7\n", "OK SIZE 7\npayloadEND 7 extra\n", "OK SIZE 7\npayload" + strings.TrimSuffix(trailer, "\n"), "OK SIZE 7\npayload " + trailer}
			if enabled {
				sends = append(sends, "OK NONE 7\n")
				receives = append(receives, "OK SIZE 7\npayloadEND 7\n")
			} else {
				sends = append(sends, "OK "+hash+" 7\n")
				receives = append(receives, "OK SIZE 7\npayloadCHECKSUM "+hash+"\n")
			}
			for _, file := range []bool{false, true} {
				for _, send := range []bool{false, true} {
					replies := receives
					if send {
						replies = sends
					}
					for i, reply := range replies {
						t.Run(fmt.Sprintf("%s/hash=%t/file=%t/send=%t/reply=%d", kind, enabled, file, send, i), func(t *testing.T) {
							tr, conn, _ := newChecksumIntegrationTransport(kind, reply, nil, Config{Checksum: enabled})
							var err error
							path := filepath.Join(t.TempDir(), "local")
							if send {
								if file {
									if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
										t.Fatal(err)
									}
									err = tr.SendFile(path, checksumIntegrationPath)
								} else {
									err = tr.SendStream(strings.NewReader("payload"), checksumIntegrationPath, 7)
								}
							} else {
								if file {
									err = tr.ReceiveFile(checksumIntegrationPath, path)
								} else {
									err = tr.ReceiveStream(checksumIntegrationPath, io.Discard)
								}
							}
							if (err == nil) != (i == 0) {
								t.Fatalf("reply %q: error = %v", reply, err)
							}
							verb := "RECEIVE"
							if send {
								verb = "SEND"
							}
							if !strings.HasPrefix(conn.output.String(), transferCommand(verb, enabled)+" ") {
								t.Fatalf("wrong command: %q", conn.output.String())
							}
						})
					}
				}
			}
		}
	}
}

func TestTCPNoHashCapabilities(t *testing.T) {
	for _, kind := range []string{"tcp", "server"} {
		for _, reply := range []string{"OK CAPS NOHASH\n", "", "OK\n", "ERROR unknown command\n", "OK CAPS\n", "OK CAPS NOHASH extra\n", "OK CAPS NOHASH \n", " OK CAPS NOHASH\n", "OK CAPS NOHASH", "OK CAPS NOHASH\r\n"} {
			t.Run(kind+"/"+fmt.Sprintf("%q", reply), func(t *testing.T) {
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer ln.Close()
				done := make(chan error, 1)
				go func() {
					conn, err := ln.Accept()
					if err != nil {
						done <- err
						return
					}
					defer conn.Close()
					conn.SetDeadline(time.Now().Add(3 * time.Second))
					reader := bufio.NewReader(conn)
					ping, err := reader.ReadString('\n')
					if err != nil || ping != "PING BASE\n" {
						done <- fmt.Errorf("ping %q: %v", ping, err)
						return
					}
					fmt.Fprint(conn, "OK PONG\n")
					caps, err := reader.ReadString('\n')
					if err != nil || caps != "CAPS\n" {
						done <- fmt.Errorf("caps %q: %v", caps, err)
						return
					}
					fmt.Fprint(conn, reply)
					// EOF must terminate missing or incomplete responses, without closing reads.
					conn.(*net.TCPConn).CloseWrite()
					rest, err := io.ReadAll(reader)
					if err == nil && len(rest) != 0 {
						err = fmt.Errorf("file bytes/commands after failed CAPS: %q", rest)
					}
					done <- err
				}()
				config := Config{Checksum: true, Timeout: 1}
				var tr Transport = NewTCPTransport(config)
				if kind == "server" {
					tr = NewServerTransport(config)
				}
				tr.(ChecksumConfigurer).SetChecksum(false)
				err = tr.Connect("127.0.0.1", ln.Addr().(*net.TCPAddr).Port)
				if reply == "OK CAPS NOHASH\n" {
					if err != nil {
						t.Fatal(err)
					}
					// Close the socket without QUIT so the peer can assert no file bytes.
					if tcp, ok := tr.(*TCPTransport); ok {
						tcp.conn.Close()
					} else {
						tr.(*ServerTransport).conn.Close()
					}
				} else {
					if err == nil || !strings.Contains(err.Error(), "upgrade receiver") || !strings.Contains(err.Error(), "-checksum") || tr.IsConnected() {
						t.Fatalf("Connect = %v, connected=%t", err, tr.IsConnected())
					}
					if err := tr.SendStream(bytes.NewBufferString("payload"), "file", 7); err == nil {
						t.Fatal("send after failed CAPS succeeded")
					}
					tr.Close()
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestTCPNoHashCapabilityTimeout(t *testing.T) {
	for _, kind := range []string{"tcp", "server"} {
		t.Run(kind, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(4 * time.Second))
				r := bufio.NewReader(conn)
				if line, err := r.ReadString('\n'); err != nil || line != "PING BASE\n" {
					done <- fmt.Errorf("ping %q: %v", line, err)
					return
				}
				fmt.Fprint(conn, "OK PONG\n")
				if line, err := r.ReadString('\n'); err != nil || line != "CAPS\n" {
					done <- fmt.Errorf("caps %q: %v", line, err)
					return
				}
				// A receiver that stays open but never answers must not hang Connect.
				rest, err := io.ReadAll(r)
				if len(rest) != 0 {
					err = fmt.Errorf("unexpected bytes after CAPS: %q", rest)
				}
				done <- err
			}()
			var tr Transport = NewTCPTransport(Config{Timeout: 1})
			if kind == "server" {
				tr = NewServerTransport(Config{Timeout: 1})
			}
			start := time.Now()
			err = tr.Connect("127.0.0.1", ln.Addr().(*net.TCPAddr).Port)
			if err == nil || !strings.Contains(err.Error(), "-checksum") || tr.IsConnected() || time.Since(start) > 3*time.Second {
				t.Fatalf("Connect = %v, duration=%v", err, time.Since(start))
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestChecksumConfigurer(t *testing.T) {
	tcp := NewTCPTransport(Config{})
	srv := NewServerTransport(Config{})
	quic := NewQUICTransport(Config{})
	for _, enabled := range []bool{true, false} {
		for _, tr := range []ChecksumConfigurer{tcp, srv, quic} {
			tr.SetChecksum(enabled)
		}
		if tcp.config.Checksum != enabled || srv.config.Checksum != enabled || quic.config.Checksum != enabled {
			t.Fatal("checksum setting did not propagate")
		}
	}
}
