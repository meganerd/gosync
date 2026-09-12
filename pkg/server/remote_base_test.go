package server

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestPingBaseDiscovery(t *testing.T) {
	for _, baseDir := range []string{filepath.Join(t.TempDir(), "base with spaces"), filepath.Join("relative base", "nested")} {
		t.Run(baseDir, func(t *testing.T) {
			absolute, err := filepath.Abs(baseDir)
			if err != nil {
				t.Fatal(err)
			}
			s := NewServer("", baseDir)
			client, conn := net.Pipe()
			done := make(chan struct{})
			go func() { defer close(done); s.handleClient(conn) }()
			defer func() { client.Close(); <-done }()
			client.SetDeadline(time.Now().Add(5 * time.Second))
			reader := bufio.NewReader(client)
			for _, tc := range []struct{ command, want string }{
				{"PING", "OK PONG\n"},
				{"PING BASE", "OK PONG " + base64.StdEncoding.EncodeToString([]byte(absolute)) + "\n"},
				{"PING", "OK PONG\n"},
				{"PING OTHER", "OK PONG\n"},
			} {
				if _, err := fmt.Fprintln(client, tc.command); err != nil {
					t.Fatal(err)
				}
				got, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				if got != tc.want {
					t.Fatalf("%s response = %q, want %q", tc.command, got, tc.want)
				}
			}
			if s.baseDir != baseDir {
				t.Fatalf("baseDir changed from %q to %q", baseDir, s.baseDir)
			}
		})
	}
}
