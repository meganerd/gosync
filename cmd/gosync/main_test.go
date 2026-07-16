package main

import (
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
	case "version":
		os.Args = []string{"gosync", "-version"}
	case "dryrun":
		source := os.Getenv("GOSYNC_TEST_SOURCE")
		os.Args = []string{"gosync", "-dry-run", "-quiet", "-workers", "3", source, "host:/backup"}
	default:
		os.Exit(2)
	}

	main()
	os.Exit(0)
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
