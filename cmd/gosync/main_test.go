package main

import (
	"bytes"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMainHelperProcess(t *testing.T) {
	mode := os.Getenv("GO_WANT_GOSYNC_HELPER")
	if mode == "" {
		return
	}

	switch mode {
	case "help":
		os.Args = []string{"gosync", "-help"}
	case "version":
		os.Args = []string{"gosync", "-version"}
	case "dryrun":
		source := os.Getenv("GOSYNC_TEST_SOURCE")
		os.Args = []string{"gosync", "-dry-run", "-quiet", "-workers", "3", source, "host:/backup"}
	case "progress":
		source := os.Getenv("GOSYNC_TEST_SOURCE")
		os.Args = append([]string{"gosync", "-dry-run"}, strings.Fields(os.Getenv("GOSYNC_TEST_FLAGS"))...)
		os.Args = append(os.Args, source, "host:/backup")
	default:
		os.Exit(2)
	}

	main()
	if got := flag.Lookup("transport").Value.String(); got != "quic" {
		t.Fatalf("default transport = %s, want quic", got)
	}
	if mode == "progress" {
		if got := flag.Lookup("progress").Value.String(); got != os.Getenv("GOSYNC_TEST_PROGRESS") {
			t.Fatalf("progress = %s, want %s", got, os.Getenv("GOSYNC_TEST_PROGRESS"))
		}
		if flag.Lookup("P").Value != flag.Lookup("progress").Value {
			t.Fatal("-P and -progress must share the same boolean")
		}
		if got := flag.Lookup("resume").Value.String(); got != "false" {
			t.Fatalf("progress must not enable resume, got %s", got)
		}
	}
	os.Exit(0)
}

func TestMainTransportHelp(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestMainHelperProcess")
	cmd.Env = append(os.Environ(), "GO_WANT_GOSYNC_HELPER=help")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("help: %v\n%s", err, out)
	}
	for _, want := range []string{`(default "quic")`, "actual file transfer, not SSH deployment", "honors -transport for file data", "serve supports -transport quic (UDP) or tcp"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
}

func TestMainVersionOutput(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestMainHelperProcess")
	cmd.Env = append(os.Environ(), "GO_WANT_GOSYNC_HELPER=version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper command error = %v, output = %s", err, out)
	}
	text := string(out)
	if !strings.Contains(text, "gosync ") || !strings.Contains(text, "commit:") || !strings.Contains(text, "built:") {
		t.Fatalf("version output = %q", text)
	}
}

func TestMainDryRunParsesFlagsAndExitsCleanly(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestMainHelperProcess")
	cmd.Env = append(os.Environ(),
		"GO_WANT_GOSYNC_HELPER=dryrun",
		"GOSYNC_TEST_SOURCE="+root,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("dry-run helper command error = %v, output = %s", err, out)
	}
	if len(out) != 0 {
		t.Fatalf("expected quiet dry-run to produce no output, got %q", string(out))
	}
}

func TestMainProgressFlags(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file.txt"), []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name     string
		flags    string
		progress string
		quiet    bool
	}{
		{name: "default", progress: "true"},
		{name: "quiet default", flags: "-quiet", progress: "true", quiet: true},
		{name: "long", flags: "--progress", progress: "true"},
		{name: "single dash", flags: "-progress", progress: "true"},
		{name: "short", flags: "-P", progress: "true"},
		{name: "short disables long", flags: "--progress -P=false", progress: "false"},
		{name: "long disables short", flags: "-P --progress=false", progress: "false"},
		{name: "quiet", flags: "-P --quiet", progress: "true", quiet: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=TestMainHelperProcess")
			cmd.Env = append(os.Environ(),
				"GO_WANT_GOSYNC_HELPER=progress",
				"GOSYNC_TEST_SOURCE="+root,
				"GOSYNC_TEST_FLAGS="+tc.flags,
				"GOSYNC_TEST_PROGRESS="+tc.progress,
			)
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("progress helper error = %v, stdout = %s, stderr = %s", err, &stdout, &stderr)
			}
			if stderr.Len() != 0 {
				t.Fatalf("dry-run must not emit progress on stderr, got %q", stderr.String())
			}
			if tc.quiet && stdout.Len() != 0 {
				t.Fatalf("quiet dry-run must not emit stdout, got %q", stdout.String())
			}
		})
	}
}

func TestExtractDestinationHelpers(t *testing.T) {
	if got := extractHost("alice@example.com:2222:/dst"); got != "example.com" {
		t.Fatalf("extractHost() = %q", got)
	}
	if got := extractPort("alice@example.com:2222:/dst"); got != 2222 {
		t.Fatalf("extractPort() = %d", got)
	}
	if got := extractPort("example.com:/dst"); got != 22 {
		t.Fatalf("extractPort(default) = %d", got)
	}
	if got := extractUser("alice@example.com:/dst"); got != "alice" {
		t.Fatalf("extractUser() = %q", got)
	}
	if got := extractUser("example.com:/dst"); got != "" {
		t.Fatalf("extractUser(no user) = %q", got)
	}
}
