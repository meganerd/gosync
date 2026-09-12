package deploy

import (
	"fmt"
	"strings"
	"testing"

	"github.com/gbjohnso/gosync/pkg/ranged"
)

func TestDeploymentPassesReceiverConnections(t *testing.T) {
	d := NewDeployer("source", "host:/tmp/target", "")
	if err := d.parseDestination(); err != nil {
		t.Fatal(err)
	}
	d.SetServerPort(54321)
	if err := d.SetTransport("quic"); err != nil {
		t.Fatal(err)
	}
	// The default is carried explicitly, exactly as --buffer-size is.
	if !strings.Contains(d.remoteServerCommand(), "--connections 1") {
		t.Fatalf("default receiver command = %q", d.remoteServerCommand())
	}
	for _, connections := range []int{1, 2, 4, ranged.MaxConnections} {
		if err := d.SetConnections(connections); err != nil {
			t.Fatal(err)
		}
		command := d.remoteServerCommand()
		if !strings.Contains(command, fmt.Sprintf("--connections %d", connections)) {
			t.Fatalf("remote command = %q, want --connections %d", command, connections)
		}
		if !strings.Contains(command, "--listen 0.0.0.0:54321 --base '/tmp/target'") || !strings.Contains(command, "--transport quic") {
			t.Fatalf("remote command lost existing arguments: %q", command)
		}
	}
	previous := d.connections
	for _, invalid := range []int{0, -1, ranged.MaxConnections + 1} {
		if err := d.SetConnections(invalid); err == nil || d.connections != previous {
			t.Fatalf("connections=%d accepted or changed configuration (%d)", invalid, d.connections)
		}
	}
}

// Fan-out is QUIC-only, so the TCP receiver command must stay as it was.
func TestDeploymentOmitsConnectionsForTCP(t *testing.T) {
	for _, protocol := range []string{"tcp", "server"} {
		t.Run(protocol, func(t *testing.T) {
			d := NewDeployer("source", "host:/tmp/target", "")
			if err := d.parseDestination(); err != nil {
				t.Fatal(err)
			}
			d.SetServerPort(54321)
			if err := d.SetTransport(protocol); err != nil {
				t.Fatal(err)
			}
			if err := d.SetConnections(4); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(d.remoteServerCommand(), "--connections") {
				t.Fatalf("TCP receiver command changed: %q", d.remoteServerCommand())
			}
		})
	}
}

// Measurement showed receiver sockets are worth only ~9-11%, so fan-out keeps
// the existing single-port probe and must not widen the deployment contract.
func TestDeploymentProbesOneUDPPort(t *testing.T) {
	d := NewDeployer("source", "host:/tmp/target", "")
	if err := d.SetTransport("quic"); err != nil {
		t.Fatal(err)
	}
	if err := d.SetConnections(ranged.MaxConnections); err != nil {
		t.Fatal(err)
	}
	command := d.portCheckCommand(60321)
	if !strings.Contains(command, "-ulnH") || strings.Count(command, "sport") != 1 || !strings.Contains(command, "60321") {
		t.Fatalf("port probe = %q", command)
	}
	if count := strings.Count(d.remoteServerCommand(), "--listen"); count != 1 {
		t.Fatalf("remote command advertises %d listen addresses", count)
	}
}
