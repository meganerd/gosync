//go:build linux

package archive

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestLinuxRejectsUnsupportedFIFOInEveryPolicy(t *testing.T) {
	for _, policy := range []Policy{PolicyStrict, PolicyPortable, PolicyBestEffort} {
		t.Run(string(policy), func(t *testing.T) {
			src := t.TempDir()
			if err := syscall.Mkfifo(filepath.Join(src, "pipe"), 0600); err != nil {
				t.Skip(err)
			}
			_, err := Send(context.Background(), &bytes.Buffer{}, src, Options{Policy: policy})
			if err == nil || !strings.Contains(err.Error(), `unsafe or unsupported archive entry "pipe"`) {
				t.Fatalf("error=%v", err)
			}
			if _, statErr := os.Lstat(filepath.Join(src, "pipe")); statErr != nil {
				t.Fatal(statErr)
			}
		})
	}
}

func TestLinuxPreservesPermissionBits(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	path := filepath.Join(src, "private")
	if err := os.WriteFile(path, []byte("secret"), 0610); err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if _, err := Send(context.Background(), &wire, src, Options{Policy: PolicyStrict}); err != nil {
		t.Fatal(err)
	}
	if _, err := Receive(context.Background(), &wire, dst, Options{Policy: PolicyStrict}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dst, "private"))
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0610 {
		t.Fatalf("mode=%#o, want 0610", got)
	}
}
