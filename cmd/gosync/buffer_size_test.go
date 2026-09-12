package main

import (
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBufferSizeHelper(t *testing.T) {
	if os.Getenv("GOSYNC_BUFFER_HELPER") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("GOSYNC_BUFFER_ARGS")), &args); err != nil {
		t.Fatal(err)
	}
	os.Args = append([]string{"gosync"}, args...)
	main()
	if want := os.Getenv("GOSYNC_BUFFER_WANT"); want != "" && flag.Lookup("buffer-size").Value.String() != want {
		t.Fatalf("buffer size = %s, want %s", flag.Lookup("buffer-size").Value.String(), want)
	}
	os.Exit(0)
}

func TestBufferSizeCLI(t *testing.T) {
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
		{"default", []string{"-transport", "tcp", "-dry-run", "-quiet", source, "host:/target"}, "32768", ""},
		{"256KiB", []string{"-transport", "server", "-buffer-size", "262144", "-dry-run", "-quiet", source, "host:/target"}, "262144", ""},
		{"1MiB", []string{"-transport", "tcp", "-buffer-size", "1048576", "-dry-run", "-quiet", source, "host:/target"}, "1048576", ""},
		{"zero", []string{"-buffer-size", "0", source, "host:/target"}, "", "Error: -buffer-size:"},
		{"oversized", []string{"-buffer-size", "4194305", source, "host:/target"}, "", "Error: -buffer-size:"},
		{"QUIC buffer", []string{"-transport", "quic", "-buffer-size", "262144", "-dry-run", "-quiet", source, "host:/target"}, "262144", ""},
		{"unsupported SSH", []string{"-transport", "ssh", "-buffer-size", "262144", source, "host:/target"}, "", "applies only to QUIC/TCP/server"},
		{"deploy rejects before SSH", []string{"-deploy", "-buffer-size", "-1", source, "host:/target"}, "", "Error: -buffer-size:"},
		{"deploy SSH rejected", []string{"-deploy", "-transport", "ssh", source, "host:/target"}, "", "deployment supports quic, tcp, or server"},
		{"serve invalid transport", []string{"serve", "-transport", "ssh"}, "", "Error: -transport:"},
		{"serve rejects before listen", []string{"serve", "-buffer-size", "1"}, "", "Error: -buffer-size:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoded, err := json.Marshal(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestBufferSizeHelper$")
			cmd.Env = append(os.Environ(), "GOSYNC_BUFFER_HELPER=1", "GOSYNC_BUFFER_ARGS="+string(encoded), "GOSYNC_BUFFER_WANT="+tc.want)
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
