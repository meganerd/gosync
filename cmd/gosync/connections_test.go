package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gbjohnso/gosync/pkg/ranged"
	"github.com/gbjohnso/gosync/pkg/server"
)

func TestConnectionsHelper(t *testing.T) {
	if os.Getenv("GOSYNC_CONNECTIONS_HELPER") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("GOSYNC_CONNECTIONS_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"gosync"}, args...)
	main()
	if want := os.Getenv("GOSYNC_CONNECTIONS_WANT"); want != "" && flag.Lookup("connections").Value.String() != want {
		t.Fatalf("connections = %s, want %s", flag.Lookup("connections").Value.String(), want)
	}
	os.Exit(0)
}

func TestConnectionsCLI(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name      string
		args      []string
		want      string
		errorText string
	}{
		{"default is one", []string{"-transport", "quic", "-dry-run", "-quiet", source, "host:/target"}, "1", ""},
		{"quic fan-out", []string{"-transport", "quic", "-connections", "4", "-dry-run", "-quiet", source, "host:/target"}, "4", ""},
		{"quic maximum", []string{"-transport", "quic", "-connections", fmt.Sprint(ranged.MaxConnections), "-dry-run", "-quiet", source, "host:/target"}, fmt.Sprint(ranged.MaxConnections), ""},
		{"quic with checksum", []string{"-transport", "quic", "-connections", "2", "-checksum", "-dry-run", "-quiet", source, "host:/target"}, "2", ""},
		// One connection is today's behavior, so it stays valid everywhere.
		{"one with ssh", []string{"-transport", "ssh", "-connections", "1", "-dry-run", "-quiet", source, "host:/target"}, "1", ""},
		{"one with tcp", []string{"-transport", "tcp", "-connections", "1", "-dry-run", "-quiet", source, "host:/target"}, "1", ""},
		// Above one, anything but QUIC is rejected rather than silently ignored.
		{"fan-out with tcp", []string{"-transport", "tcp", "-connections", "4", source, "host:/target"}, "", "applies only to QUIC data transfer"},
		{"fan-out with server", []string{"-transport", "server", "-connections", "2", source, "host:/target"}, "", "applies only to QUIC data transfer"},
		{"fan-out with ssh", []string{"-transport", "ssh", "-connections", "8", source, "host:/target"}, "", "applies only to QUIC data transfer"},
		{"zero", []string{"-connections", "0", source, "host:/target"}, "", "-connections must be between 1 and 16"},
		{"negative", []string{"-connections", "-2", source, "host:/target"}, "", "-connections must be between 1 and 16"},
		{"above maximum", []string{"-connections", fmt.Sprint(ranged.MaxConnections + 1), source, "host:/target"}, "", "-connections must be between 1 and 16"},
		{"non-numeric", []string{"-connections", "four", source, "host:/target"}, "", "invalid value"},
		{"deploy rejects before SSH", []string{"-deploy", "-connections", "99", source, "host:/target"}, "", "-connections must be between 1 and 16"},
		{"deploy tcp rejects fan-out", []string{"-deploy", "-transport", "tcp", "-connections", "4", source, "host:/target"}, "", "applies only to QUIC data transfer"},
		{"serve zero", []string{"serve", "-connections", "0"}, "", "Error: -connections:"},
		{"serve above maximum", []string{"serve", "-connections", fmt.Sprint(ranged.MaxConnections + 1)}, "", "Error: -connections:"},
		{"serve fan-out with tcp", []string{"serve", "-connections", "4", "-transport", "tcp"}, "", "applies only to -transport quic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestConnectionsHelper$")
			cmd.Env = append(os.Environ(), "GOSYNC_CONNECTIONS_HELPER=1",
				"GOSYNC_CONNECTIONS_ARGS="+string(encoded), "GOSYNC_CONNECTIONS_WANT="+tc.want)
			out, err := cmd.CombinedOutput()
			if tc.errorText != "" {
				if err == nil || !strings.Contains(string(out), tc.errorText) {
					t.Fatalf("error %v, output %q; want %q", err, out, tc.errorText)
				}
			} else if err != nil || len(out) != 0 {
				t.Fatalf("error %v, unexpected output %q", err, out)
			}
		})
	}
}

type connectionsServer struct{ configured int }

func (s *connectionsServer) SetMaxConnections(connections int) error {
	s.configured = connections
	return nil
}

type rejectingServer struct{}

func (rejectingServer) SetMaxConnections(int) error { return fmt.Errorf("receiver refused") }

type legacyServer struct{}

// The real receiver must satisfy the interface this plumbing looks for.
// Without this guard a setter rename silently disables the flag, because the
// type switch just falls through to the no-setter branch.
func TestServerImplementsConnectionsSetter(t *testing.T) {
	var srv any = server.NewServer("127.0.0.1:0", t.TempDir())
	configurable, ok := srv.(interface{ SetMaxConnections(int) error })
	if !ok {
		t.Fatal("*server.Server no longer implements SetMaxConnections(int) error; -connections would be silently ignored")
	}
	if err := configurable.SetMaxConnections(4); err != nil {
		t.Fatalf("SetMaxConnections(4) on the real server: %v", err)
	}
}

// The receiver's setter is discovered through an interface, so this flag
// plumbing must validate on its own and work with or without that setter.
func TestConfigureServerConnections(t *testing.T) {
	server := &connectionsServer{}
	if err := configureServerConnections(server, 4, "quic"); err != nil || server.configured != 4 {
		t.Fatalf("configured = %d, err = %v", server.configured, err)
	}
	if err := configureServerConnections(rejectingServer{}, 2, "quic"); err == nil {
		t.Fatal("receiver rejection was ignored")
	}
	// A receiver without the setter still accepts the default.
	if err := configureServerConnections(legacyServer{}, 1, "quic"); err != nil {
		t.Fatalf("default rejected on a receiver without fan-out: %v", err)
	}
	if err := configureServerConnections(legacyServer{}, 4, "quic"); err == nil {
		t.Fatal("fan-out accepted on a receiver without the setter")
	}
	for _, tc := range []struct {
		connections int
		transport   string
	}{
		{0, "quic"}, {-1, "quic"}, {ranged.MaxConnections + 1, "quic"}, {4, "tcp"}, {2, ""},
	} {
		server := &connectionsServer{}
		if err := configureServerConnections(server, tc.connections, tc.transport); err == nil {
			t.Fatalf("connections=%d transport=%q accepted", tc.connections, tc.transport)
		}
		if server.configured != 0 {
			t.Fatalf("invalid configuration reached the receiver: %d", server.configured)
		}
	}
	// One connection is the existing behavior and must stay valid for TCP.
	if err := configureServerConnections(&connectionsServer{}, 1, "tcp"); err != nil {
		t.Fatalf("single connection rejected for tcp: %v", err)
	}
}
