package deploy

import (
	"strings"
	"testing"

	"github.com/gbjohnso/gosync/pkg/checksum"
)

func TestDeploymentTransportSelection(t *testing.T) {
	for _, tc := range []struct{ selected, receiver, socketOption string }{
		{"quic", "quic", "-ulnH"}, {"tcp", "tcp", "-tlnH"}, {"server", "tcp", "-tlnH"},
	} {
		t.Run(tc.selected, func(t *testing.T) {
			d := NewDeployer("source", "host:/target", "")
			if err := d.SetTransport(tc.selected); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(d.remoteServerCommand(), "--transport "+tc.receiver) {
				t.Fatalf("wrong receiver command: %s", d.remoteServerCommand())
			}
			if !strings.Contains(d.portCheckCommand(60321), tc.socketOption) || !strings.Contains(d.portCheckCommand(60321), "60321") {
				t.Fatalf("wrong protocol port check: %s", d.portCheckCommand(60321))
			}
			if err := d.SetTransport("ssh"); err == nil || d.transportType != tc.receiver {
				t.Fatal("invalid deployment transport accepted or changed selection")
			}
		})
	}
}

func TestDeploymentPassesReceiverBufferSize(t *testing.T) {
	d := NewDeployer("source", "host:/tmp/target", "")
	if err := d.parseDestination(); err != nil {
		t.Fatal(err)
	}
	d.SetServerPort(54321)
	for _, size := range []int{checksum.DefaultBufferSize, 256 * 1024, 1024 * 1024} {
		if err := d.SetBufferSize(size); err != nil {
			t.Fatal(err)
		}
		command := d.remoteServerCommand()
		var expected string
		switch size {
		case checksum.DefaultBufferSize:
			expected = "--buffer-size 32768"
		case 256 * 1024:
			expected = "--buffer-size 262144"
		default:
			expected = "--buffer-size 1048576"
		}
		if !strings.Contains(command, expected) || !strings.Contains(command, "--listen 0.0.0.0:54321 --base '/tmp/target'") {
			t.Fatalf("remote command = %q", command)
		}
	}
	previous := d.bufferSize
	if err := d.SetBufferSize(0); err == nil || d.bufferSize != previous {
		t.Fatal("invalid size accepted or changed configuration")
	}
}
