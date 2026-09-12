package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/gbjohnso/gosync/pkg/transport"
)

func TestSelectedTransferImplementation(t *testing.T) {
	for _, protocol := range []string{"quic", "tcp", "server", "ssh"} {
		tr, err := newTransferTransport(protocol, transport.Config{BufferSize: 262144})
		if err != nil {
			t.Fatal(err)
		}
		if tr.Name() != protocol {
			t.Fatalf("selected %s, constructed %s", protocol, tr.Name())
		}
	}
	if _, err := newTransferTransport("invalid", transport.Config{}); err == nil {
		t.Fatal("unsupported transport accepted")
	}
}

func TestReportFileTransferTransport(t *testing.T) {
	for _, tc := range []struct {
		selected string
		deploy   bool
		want     string
	}{
		{"quic", false, "File-transfer transport: quic (UDP)"},
		{"tcp", false, "File-transfer transport: tcp (TCP)"},
		{"server", false, "File-transfer transport: server (TCP)"},
		{"ssh", false, "File-transfer transport: ssh (SSH over TCP)"},
		{"quic", true, "File-transfer transport: quic (UDP); deployment: SSH"},
		{"tcp", true, "File-transfer transport: tcp (TCP); deployment: SSH"},

		{"server", true, "File-transfer transport: server (TCP); deployment: SSH"},
	} {
		name := tc.selected
		if tc.deploy {
			name += " deployment"
		}
		t.Run(name, func(t *testing.T) {
			for _, dryRun := range []bool{false, true} {
				for _, quiet := range []bool{false, true} {
					var out bytes.Buffer
					reportFileTransferTransport(&out, tc.selected, tc.deploy, dryRun, quiet)
					want := tc.want + "; selected; not yet connected\n"
					if dryRun {
						want = tc.want + "; planned; dry-run\n"
					}
					if quiet {
						want = ""
					}
					if out.String() != want {
						t.Errorf("dryRun=%v quiet=%v: got %q, want %q", dryRun, quiet, out.String(), want)
					}
				}
			}
		})
	}
}

func TestTransportOutputHelperProcess(t *testing.T) {
	if os.Getenv("GOSYNC_TRANSPORT_OUTPUT_HELPER") != "1" {
		return
	}
	os.Args = []string{"gosync", "-dry-run", "-transport", os.Getenv("GOSYNC_OUTPUT_TRANSPORT")}
	if os.Getenv("GOSYNC_OUTPUT_QUIET") == "true" {
		os.Args = append(os.Args, "-quiet")
	}
	os.Args = append(os.Args, os.Getenv("GOSYNC_OUTPUT_SOURCE"), "host:/backup")
	main()
	os.Exit(0)
}

func TestMainDryRunTransportOutput(t *testing.T) {
	for _, tc := range []struct {
		transport string
		protocol  string
	}{
		{"quic", "UDP"},
		{"tcp", "TCP"},
		{"server", "TCP"},
		{"ssh", "SSH over TCP"},
	} {
		t.Run(tc.transport, func(t *testing.T) {
			for _, quiet := range []string{"false", "true"} {
				cmd := exec.Command(os.Args[0], "-test.run=^TestTransportOutputHelperProcess$")
				cmd.Env = append(os.Environ(),
					"GOSYNC_TRANSPORT_OUTPUT_HELPER=1",
					"GOSYNC_OUTPUT_TRANSPORT="+tc.transport,
					"GOSYNC_OUTPUT_QUIET="+quiet,
					"GOSYNC_OUTPUT_SOURCE="+t.TempDir(),
				)
				var stdout, stderr bytes.Buffer
				cmd.Stdout = &stdout
				cmd.Stderr = &stderr
				if err := cmd.Run(); err != nil {
					t.Fatalf("CLI dry-run: %v\nstdout: %s\nstderr: %s", err, &stdout, &stderr)
				}
				if stderr.Len() != 0 {
					t.Fatalf("unexpected stderr: %q", stderr.String())
				}
				if quiet == "true" {
					if stdout.Len() != 0 {
						t.Errorf("quiet output: %q", stdout.String())
					}
					continue
				}
				want := "File-transfer transport: " + tc.transport + " (" + tc.protocol + "); planned; dry-run\n"
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("missing %q in %q", want, stdout.String())
				}
			}
		})
	}
}
