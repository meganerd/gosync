package transport_test

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gbjohnso/gosync/pkg/ranged"
	"github.com/gbjohnso/gosync/pkg/server"
	"github.com/gbjohnso/gosync/pkg/transport"
)

// configureReceiverConnections sizes the receiver's socket group. It fails
// rather than skipping: the receiver implements this setter, so a rename must
// surface here instead of letting the test pass vacuously through the
// single-connection fallback.
func configureReceiverConnections(t *testing.T, srv *server.Server, connections int) {
	t.Helper()
	if err := srv.SetMaxConnections(connections); err != nil {
		t.Fatalf("receiver rejected %d connections: %v", connections, err)
	}
}

// TestQUICFanoutProductionReceiverIntegration drives the real pkg/server
// receiver over loopback, exercising the full ranged protocol: FANOUT
// negotiation, concurrent SEND-RANGE writes from independent sender sockets,
// and COMMIT/COMMIT-HASH with byte-exact verification.
func TestQUICFanoutProductionReceiverIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("fan-out integration moves 64 MiB per case")
	}
	for _, checksum := range []bool{false, true} {
		for _, connections := range []int{2, 4, 8} {
			t.Run(fmt.Sprintf("checksum=%v/connections=%d", checksum, connections), func(t *testing.T) {
				base := t.TempDir()
				srv := server.NewServer("127.0.0.1:0", base)
				if err := srv.SetTransport("quic"); err != nil {
					t.Fatal(err)
				}
				configureReceiverConnections(t, srv, connections)
				done := make(chan error, 1)
				go func() { done <- srv.Start() }()
				t.Cleanup(func() {
					srv.Stop()
					select {
					case err := <-done:
						if err != nil {
							t.Error(err)
						}
					case <-time.After(10 * time.Second):
						t.Error("server did not stop")
					}
				})
				deadline := time.Now().Add(10 * time.Second)
				for srv.Addr() == nil {
					select {
					case err := <-done:
						t.Fatalf("server startup: %v", err)
					default:
					}
					if time.Now().After(deadline) {
						t.Fatal("server startup timed out")
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
				t.Cleanup(func() { client.Close() })
				if err := client.Connect(addr.IP.String(), addr.Port); err != nil {
					t.Fatal(err)
				}

				const size = int64(ranged.MinFanoutSize) + 4097 // not a chunk multiple
				source, payload := fanoutSource(t, size)
				var progress atomic.Int64
				client.SetProgressCallback(func(n int64) { progress.Add(n) })
				if err := client.SendFile(source.Name(), quicTestPath); err != nil {
					t.Fatalf("fan-out transfer failed: %v", err)
				}
				stored, err := os.ReadFile(filepath.Join(base, quicTestPath))
				if err != nil {
					t.Fatalf("destination file: %v", err)
				}
				if !bytes.Equal(stored, payload) {
					t.Fatal("destination file is not byte-exact")
				}
				if progress.Load() < size {
					t.Fatalf("progress = %d, want at least %d", progress.Load(), size)
				}
				staged, err := filepath.Glob(filepath.Join(base, "**", "*.part"))
				if err != nil {
					t.Fatal(err)
				}
				if len(staged) != 0 {
					t.Fatalf("staged files left behind: %v", staged)
				}
			})
		}
	}
}
