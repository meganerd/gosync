package sync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gbjohnso/gosync/pkg/transport"
)

type connectionsTransport struct {
	fakeTransport
	connections int
	events      []string
	setErr      error
}

var _ transport.ConnectionsConfigurer = (*connectionsTransport)(nil)

func (f *connectionsTransport) SetChecksum(enabled bool) {
	f.events = append(f.events, fmt.Sprintf("checksum=%t", enabled))
}

func (f *connectionsTransport) SetConnections(connections int) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.events = append(f.events, fmt.Sprintf("connections=%d", connections))
	f.connections = connections
	return nil
}

func (f *connectionsTransport) Connect(host string, port int) error {
	f.events = append(f.events, fmt.Sprintf("connect/connections=%d", f.connections))
	return f.fakeTransport.Connect(host, port)
}

// Fan-out support is negotiated during the handshake, so the connection count
// must reach the transport before Connect, like the checksum mode.
func TestRunConnectionsConfiguredBeforeConnect(t *testing.T) {
	source := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(source, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, connections := range []int{1, 4} {
		t.Run(fmt.Sprint(connections), func(t *testing.T) {
			tr := &connectionsTransport{}
			err := NewSync(source, "host:1234:/target", tr, Config{Workers: 1, Quiet: true, Connections: connections}).Run()
			if err != nil || len(tr.sent) != 1 {
				t.Fatalf("Run() = %v, sent = %v", err, tr.sent)
			}
			want := []string{
				"checksum=false",
				fmt.Sprintf("connections=%d", connections),
				fmt.Sprintf("connect/connections=%d", connections),
			}
			if !reflect.DeepEqual(tr.events, want) {
				t.Fatalf("events = %v, want %v", tr.events, want)
			}
		})
	}
}

// An unset count leaves the transport's own configuration alone, so existing
// callers that never set it are not silently downgraded.
func TestRunLeavesConnectionsUnsetWhenZero(t *testing.T) {
	source := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(source, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	tr := &connectionsTransport{connections: 8}
	if err := NewSync(source, "host:1234:/target", tr, Config{Workers: 1, Quiet: true}).Run(); err != nil {
		t.Fatal(err)
	}
	want := []string{"checksum=false", "connect/connections=8"}
	if !reflect.DeepEqual(tr.events, want) {
		t.Fatalf("events = %v, want %v", tr.events, want)
	}
}

func TestRunRejectsInvalidConnections(t *testing.T) {
	source := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(source, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	tr := &connectionsTransport{setErr: errors.New("connections must be between 1 and 16")}
	err := NewSync(source, "host:1234:/target", tr, Config{Workers: 1, Quiet: true, Connections: 99}).Run()
	if !errors.Is(err, tr.setErr) {
		t.Fatalf("Run() = %v, want the setter error", err)
	}
	if tr.IsConnected() || len(tr.sent) != 0 {
		t.Fatalf("connected=%v sent=%v after an invalid connection count", tr.IsConnected(), tr.sent)
	}
}

// A dry run touches no transport configuration at all.
func TestDryRunSkipsConnectionsConfiguration(t *testing.T) {
	source := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(source, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	tr := &connectionsTransport{}
	if err := NewSync(source, "host:1234:/target", tr, Config{Workers: 1, Quiet: true, DryRun: true, Connections: 4}).Run(); err != nil {
		t.Fatal(err)
	}
	if len(tr.events) != 0 {
		t.Fatalf("dry run configured the transport: %v", tr.events)
	}
}
