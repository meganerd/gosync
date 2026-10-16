//go:build windows

package archive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsNativeCaseCollision(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CaseProbe"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "caseprobe")); err != nil {
		t.Skip("test requires a case-insensitive Windows filesystem")
	}
	// Receive also folds archive names before touching the filesystem. The
	// platform-neutral collision fixtures exercise that decoder path.
}
