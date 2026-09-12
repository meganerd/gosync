package transport

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"
)

type remoteBaseTransport interface {
	RemoteBaseProvider
	Connect(string, int) error
	Close() error
}

func TestRemoteBaseDiscovery(t *testing.T) {
	base := filepath.Join(t.TempDir(), "server base with spaces")
	encoded := base64.StdEncoding.EncodeToString([]byte(base))
	factories := map[string]func() remoteBaseTransport{
		"TCP":    func() remoteBaseTransport { return NewTCPTransport(Config{Checksum: true, Timeout: 1}) },
		"Server": func() remoteBaseTransport { return NewServerTransport(Config{Checksum: true, Timeout: 1}) },
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			tr := factory()
			if got := tr.RemoteBase(); got != "" {
				t.Fatalf("initial RemoteBase = %q", got)
			}
			for _, tc := range []struct {
				name, response, want string
			}{
				{"spaces", "OK PONG " + encoded, base},
				{"legacy resets cache", "OK PONG", ""},
				{"rediscovery", "OK PONG " + encoded, base},
				{"malformed resets cache", "OK PONG !!!", ""},
				{"missing metadata", "OK PONG ", ""},
				{"extra fields", "OK PONG " + encoded + " extra", ""},
				{"relative path", "OK PONG " + base64.StdEncoding.EncodeToString([]byte("relative/base")), ""},
				{"NUL path", "OK PONG " + base64.StdEncoding.EncodeToString([]byte(base+"\x00")), ""},
				{"invalid pong token", "OK PONGextra " + encoded, ""},
				{"restore before failed dial", "OK PONG " + encoded, base},
			} {
				t.Run(tc.name, func(t *testing.T) {
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
						conn.SetDeadline(time.Now().Add(5 * time.Second))
						ping, err := bufio.NewReader(conn).ReadString('\n')
						if err != nil {
							done <- err
							return
						}
						if ping != "PING BASE\n" {
							done <- fmt.Errorf("handshake = %q", ping)
							return
						}
						_, err = fmt.Fprintln(conn, tc.response)
						if err == nil {
							_, err = io.Copy(io.Discard, conn)
						}
						done <- err
					}()
					if err := tr.Connect("127.0.0.1", ln.Addr().(*net.TCPAddr).Port); err != nil {
						t.Fatal(err)
					}
					got := tr.RemoteBase()
					if err := tr.Close(); err != nil {
						t.Fatal(err)
					}
					if err := <-done; err != nil {
						t.Fatal(err)
					}
					if got != tc.want {
						t.Fatalf("RemoteBase = %q, want %q", got, tc.want)
					}
				})
			}
			if err := tr.Connect("127.0.0.1", -1); err == nil {
				t.Fatal("invalid port should fail")
			}
			if got := tr.RemoteBase(); got != "" {
				t.Fatalf("RemoteBase after failed connect = %q", got)
			}
		})
	}
}
