package transport

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
	"sync/atomic"
	"testing"
	"time"
)

var (
	_ ProgressReporter = (*TCPTransport)(nil)
	_ ProgressReporter = (*ServerTransport)(nil)
	_ ProgressReporter = (*SSHTransport)(nil)
	_ ProgressReporter = (*QUICTransport)(nil)
)

type progressWriteFunc func([]byte) (int, error)

func (f progressWriteFunc) Write(p []byte) (int, error) { return f(p) }

func TestProgressWriter(t *testing.T) {
	writeErr := errors.New("write failed")
	for _, tc := range []struct {
		name        string
		n           int
		err         error
		nilCallback bool
	}{
		{"full", 6, nil, false},
		{"partial", 3, nil, false},
		{"partial error", 3, writeErr, false},
		{"full with error", 6, writeErr, false},
		{"zero error", 0, writeErr, false},
		{"zero", 0, nil, false},
		{"nil callback", 6, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var total int64
			calls := 0
			callback := func(n int64) { total += n; calls++ }
			if tc.nilCallback {
				callback = nil
			}
			writer := progressWriter{progressWriteFunc(func(p []byte) (int, error) {
				if string(p) != "abcdef" {
					t.Fatalf("payload = %q", p)
				}
				if calls != 0 {
					t.Fatal("callback ran before write")
				}
				return tc.n, tc.err
			}), callback}
			n, err := writer.Write([]byte("abcdef"))
			if n != tc.n || err != tc.err {
				t.Fatalf("Write = (%d, %v), want (%d, %v)", n, err, tc.n, tc.err)
			}
			wantCalls, wantTotal := 0, int64(0)
			if tc.n > 0 && !tc.nilCallback {
				wantCalls, wantTotal = 1, int64(tc.n)
			}
			if calls != wantCalls || total != wantTotal {
				t.Fatalf("progress = (%d calls, %d bytes), want (%d, %d)", calls, total, wantCalls, wantTotal)
			}
		})
	}
}

func TestProgressWriterCopyShortWrite(t *testing.T) {
	var total int64
	writer := progressWriter{progressWriteFunc(func([]byte) (int, error) { return 2, nil }), func(n int64) { total += n }}
	n, err := io.Copy(writer, bytes.NewBufferString("abcdef"))
	if n != 2 || !errors.Is(err, io.ErrShortWrite) || total != 2 {
		t.Fatalf("copy = (%d, %v), progress = %d", n, err, total)
	}
}

func TestProgressTCPSends(t *testing.T) {
	for _, backend := range []string{"tcp", "server"} {
		for _, file := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/file=%t", backend, file), func(t *testing.T) {
				client, peer := net.Pipe()
				defer client.Close()
				defer peer.Close()
				deadline := time.Now().Add(5 * time.Second)
				client.SetDeadline(deadline)
				peer.SetDeadline(deadline)
				var sender Transport
				if backend == "tcp" {
					sender = &TCPTransport{config: Config{Checksum: true}, conn: client, reader: bufio.NewReader(client)}
				} else {
					sender = &ServerTransport{config: Config{Checksum: true}, conn: client, reader: bufio.NewReader(client)}
				}
				reporter := sender.(ProgressReporter)
				payload := bytes.Repeat([]byte("payload"), 20000)
				path := filepath.Join(t.TempDir(), "payload")
				if err := os.WriteFile(path, payload, 0600); err != nil {
					t.Fatal(err)
				}
				var total atomic.Int64
				var calls atomic.Int64
				reporter.SetProgressCallback(func(n int64) {
					if n <= 0 {
						t.Errorf("nonpositive delta %d", n)
					}
					total.Add(n)
					calls.Add(1)
				})
				for _, enabled := range []bool{true, false} {
					if !enabled {
						reporter.SetProgressCallback(nil)
					}
					before := calls.Load()
					done := make(chan error, 1)
					go func() {
						r := bufio.NewReader(peer)
						header, err := r.ReadString('\n')
						if err != nil {
							done <- err
							return
						}
						want := fmt.Sprintf("SEND %d %s\n", len(payload), base64.StdEncoding.EncodeToString([]byte("remote file")))
						if header != want {
							done <- fmt.Errorf("header = %q, want %q", header, want)
							return
						}
						body := make([]byte, len(payload))
						if _, err := io.ReadFull(r, body); err != nil {
							done <- err
							return
						}
						if !bytes.Equal(body, payload) {
							done <- errors.New("payload mismatch")
							return
						}
						// Earlier chunks must be reported before the final acknowledgement.
						if enabled && total.Load() == 0 {
							done <- errors.New("no live progress before acknowledgement")
							return
						}
						_, err = fmt.Fprintf(peer, "OK %x %d\n", sha256.Sum256(body), len(body))
						done <- err
					}()
					var err error
					if file {
						err = sender.SendFile(path, "remote file")
					} else {
						err = sender.SendStream(bytes.NewReader(payload), "remote file", int64(len(payload)))
					}
					if err != nil {
						t.Fatal(err)
					}
					if err := <-done; err != nil {
						t.Fatal(err)
					}
					if total.Load() != int64(len(payload)) {
						t.Fatalf("progress = %d, want %d", total.Load(), len(payload))
					}
					if enabled && calls.Load() < 2 {
						t.Fatal("expected incremental progress")
					}
					if !enabled && calls.Load() != before {
						t.Fatal("nil callback did not remove reporting")
					}
				}
			})
		}
	}
}
