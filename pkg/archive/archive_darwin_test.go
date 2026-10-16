//go:build darwin

package archive

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinNativeCaseSemanticsAreExplicit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CaseProbe"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := os.Stat(filepath.Join(dir, "caseprobe"))
	if err == nil && collisionKey("CaseProbe", "darwin") != collisionKey("caseprobe", "darwin") {
		t.Fatal("case-insensitive filesystem was not matched by archive collision rules")
	}
	if err != nil {
		t.Log("case-sensitive macOS filesystem; archive collision rules remain conservative")
	}
}
