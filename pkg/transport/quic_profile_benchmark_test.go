package transport_test

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gbjohnso/gosync/pkg/ranged"
	"github.com/gbjohnso/gosync/pkg/server"
	"github.com/gbjohnso/gosync/pkg/transport"
)

// BenchmarkQUICSend profiles the production QUIC sender and receiver together
// over loopback. It deliberately keeps TLS, AEAD, packetization, UDP syscalls,
// staging-file writes, fsync, rename, and (when enabled) range plus whole-file
// checksums in the timed path. Connection=1 is the existing single-connection
// protocol; larger values exercise multi-socket fan-out.
//
// Put TMPDIR on the filesystem being investigated. A tmpfs TMPDIR largely
// removes physical-storage latency while retaining file-system syscalls. See
// docs/quic-profiling.md for reproducible CPU and allocation profile commands.
func BenchmarkQUICSend(b *testing.B) {
	const size = int64(ranged.MinFanoutSize) + 4097
	for _, checksum := range []bool{false, true} {
		for _, connections := range []int{1, 4} {
			name := fmt.Sprintf("connections=%d/checksum=%t", connections, checksum)
			b.Run(name, func(b *testing.B) {
				base := b.TempDir()
				srv := server.NewServer("127.0.0.1:0", base)
				if err := srv.SetTransport("quic"); err != nil {
					b.Fatal(err)
				}
				if err := srv.SetMaxConnections(connections); err != nil {
					b.Fatalf("receiver rejected %d connections: %v", connections, err)
				}
				done := make(chan error, 1)
				go func() { done <- srv.Start() }()
				b.Cleanup(func() {
					srv.Stop()
					select {
					case err := <-done:
						if err != nil {
							b.Error(err)
						}
					case <-time.After(10 * time.Second):
						b.Error("server did not stop")
					}
				})
				deadline := time.Now().Add(10 * time.Second)
				for srv.Addr() == nil {
					if time.Now().After(deadline) {
						b.Fatal("server startup timed out")
					}
					time.Sleep(time.Millisecond)
				}

				addr := srv.Addr().(*net.UDPAddr)
				client := transport.NewQUICTransport(transport.Config{
					Checksum:       checksum,
					Connections:    connections,
					MaxRetries:     3,
					Timeout:        30,
					CertificatePEM: srv.CertificatePEM(),
				})
				b.Cleanup(func() { client.Close() })
				if err := client.Connect(addr.IP.String(), addr.Port); err != nil {
					b.Fatal(err)
				}
				sourcePath := filepath.Join(b.TempDir(), "source.bin")
				source, err := os.OpenFile(sourcePath, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
				if err != nil {
					b.Fatal(err)
				}
				block := make([]byte, 1<<20)
				for written := int64(0); written < size; {
					n := min(int64(len(block)), size-written)
					for i := int64(0); i < n; i++ {
						position := written + i
						block[i] = byte(position*31 + position/251)
					}
					if _, err := source.Write(block[:n]); err != nil {
						source.Close()
						b.Fatal(err)
					}
					written += n
				}
				if err := source.Close(); err != nil {
					b.Fatal(err)
				}
				remote := filepath.Join("profile", "payload.bin")

				b.SetBytes(size)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if err := client.SendFile(sourcePath, remote); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
			})
		}
	}
}
