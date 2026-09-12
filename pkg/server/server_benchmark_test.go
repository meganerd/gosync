package server

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gbjohnso/gosync/pkg/transport"
)

// BenchmarkServerTransportTCP exercises the deployed command handler and client,
// not a simulated peer. Only listener lifecycle is supplied by the benchmark:
// Start cannot be cleanly stopped and polling Addr races with listener startup.
func BenchmarkServerTransportTCP(b *testing.B) {
	const size = 8 << 20
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i*31 + i/251)
	}
	for _, mode := range []string{"SendFile", "SendFileProgressAtomic", "ReceiveFile"} {
		b.Run(mode, func(b *testing.B) {
			base := b.TempDir()
			local := b.TempDir()
			source := filepath.Join(local, "source")
			remote := filepath.Join(base, "payload")
			destination := filepath.Join(local, "received")
			for _, path := range []string{source, remote} {
				if err := os.WriteFile(path, payload, 0600); err != nil {
					b.Fatal(err)
				}
			}
			s := NewServer("", base)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				b.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				if err := conn.SetDeadline(time.Now().Add(time.Minute)); err != nil {
					conn.Close()
					done <- err
					return
				}
				s.handleClient(conn)
				done <- nil
			}()
			t := transport.NewServerTransport(transport.Config{Timeout: 5})
			b.Cleanup(func() {
				t.Close()
				listener.Close()
				select {
				case err := <-done:
					if err != nil {
						b.Error(err)
					}
				case <-time.After(5 * time.Second):
					b.Error("server handler did not stop")
				}
			})
			if err := t.Connect("127.0.0.1", listener.Addr().(*net.TCPAddr).Port); err != nil {
				b.Fatal(err)
			}
			var progress atomic.Int64
			if mode == "SendFileProgressAtomic" {
				t.SetProgressCallback(func(n int64) { progress.Add(n) })
			}
			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var err error
				if mode == "ReceiveFile" {
					err = t.ReceiveFile("payload", destination)
				} else {
					err = t.SendFile(source, "payload")
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if mode == "SendFileProgressAtomic" && progress.Load() != int64(b.N)*size {
				b.Fatalf("progress bytes: got %d, want %d", progress.Load(), int64(b.N)*size)
			}
			result := remote
			if mode == "ReceiveFile" {
				result = destination
			}
			got, err := os.ReadFile(result)
			if err != nil {
				b.Fatal(err)
			}
			if !bytes.Equal(got, payload) {
				b.Fatal("transferred payload differs")
			}
		})
	}
}
