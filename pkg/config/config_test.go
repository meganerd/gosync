package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadSaveAndDefaultConfig(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "nested", "gosync.json")
	config := &Config{
		Workers:     4,
		Transport:   "tcp",
		Bandwidth:   1024,
		Compression: true,
		Checksum:    true,
		Resume:      true,
		Verbose:     true,
		Quiet:       true,
		Excludes:    []string{"*.tmp"},
		Includes:    []string{"*.txt"},
	}

	if err := SaveConfig(path, config); err != nil {
		t.Fatalf("SaveConfig() error = %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if !reflect.DeepEqual(loaded, config) {
		t.Fatalf("loaded config = %#v, want %#v", loaded, config)
	}

	missing, err := LoadConfig(filepath.Join(tmp, "missing.json"))
	if err != nil {
		t.Fatalf("LoadConfig(missing) error = %v", err)
	}
	if !reflect.DeepEqual(missing, &Config{}) {
		t.Fatalf("missing config = %#v, want empty config", missing)
	}

	if path := DefaultConfigPath(); !strings.HasSuffix(path, ".gosync.json") {
		t.Fatalf("DefaultConfigPath() = %q", path)
	}
}

func TestMergeConfigOverridesOnlyTruthyOrNonZeroValues(t *testing.T) {
	base := &Config{
		Workers:     2,
		Transport:   "tcp",
		Bandwidth:   100,
		Compression: true,
		Checksum:    true,
		Resume:      true,
		Verbose:     true,
		Quiet:       true,
		Excludes:    []string{"*.tmp"},
		Includes:    []string{"*.txt"},
	}
	override := &Config{
		Workers:   8,
		Transport: "ssh",
		Bandwidth: 200,
		Excludes:  []string{"*.log"},
	}

	merged := MergeConfig(base, override)
	if merged.Workers != 8 || merged.Transport != "ssh" || merged.Bandwidth != 200 {
		t.Fatalf("numeric/string overrides failed: %#v", merged)
	}
	if !merged.Compression || !merged.Checksum || !merged.Resume || !merged.Verbose || !merged.Quiet {
		t.Fatalf("truthy base bools should remain true: %#v", merged)
	}
	if !reflect.DeepEqual(merged.Excludes, []string{"*.log"}) || !reflect.DeepEqual(merged.Includes, []string{"*.txt"}) {
		t.Fatalf("slice merge failed: %#v", merged)
	}

	data, err := os.ReadFile(filepath.Join(t.TempDir(), "unused"))
	if err == nil || data != nil {
		// keep the compiler from complaining about imported os in pure table tests
	}
}
