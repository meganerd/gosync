package resume

import (
	"errors"
	"os"
	"testing"
)

func TestTrackerStateLifecycleAndPersistence(t *testing.T) {
	tracker := NewTracker("/src", "/dst")
	tracker.stateFile = tracker.stateFile + "-test"
	t.Cleanup(func() { _ = os.Remove(tracker.stateFile) })

	if err := tracker.Load(); err != nil {
		t.Fatalf("Load(missing) error = %v", err)
	}

	tracker.MarkStarted("a.txt", 10)
	tracker.MarkCompleted("a.txt", 10, "abc")
	tracker.MarkFailed("b.txt", 20, errors.New("boom"))

	if !tracker.IsCompleted("a.txt", 10) {
		t.Fatal("expected a.txt to be completed")
	}
	if tracker.IsCompleted("a.txt", 11) || tracker.IsCompleted("missing", 1) {
		t.Fatal("unexpected IsCompleted result")
	}

	completed, failed, pending := tracker.GetStats()
	if completed != 1 || failed != 1 || pending != 0 {
		t.Fatalf("GetStats() = %d,%d,%d", completed, failed, pending)
	}

	if err := tracker.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	loaded := NewTracker("/src", "/dst")
	loaded.stateFile = tracker.stateFile
	if err := loaded.Load(); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !loaded.IsCompleted("a.txt", 10) {
		t.Fatal("loaded tracker missing completed entry")
	}

	if err := loaded.Cleanup(); err != nil {
		t.Fatalf("Cleanup() error = %v", err)
	}
	if _, err := os.Stat(tracker.stateFile); !os.IsNotExist(err) {
		t.Fatalf("state file still exists, err = %v", err)
	}
}

func TestHashPathIsStableAndDistinct(t *testing.T) {
	a := hashPath("src", "dst")
	b := hashPath("src", "dst")
	c := hashPath("src2", "dst")
	if a != b {
		t.Fatalf("hashPath should be stable: %q != %q", a, b)
	}
	if a == c {
		t.Fatalf("hashPath should differ for distinct inputs: %q == %q", a, c)
	}
}
