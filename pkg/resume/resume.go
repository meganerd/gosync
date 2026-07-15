package resume

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Tracker struct {
	stateFile string
	transfers map[string]TransferState
	mu        sync.RWMutex
}

type TransferState struct {
	Path      string
	Size      int64
	Checksum  string
	StartTime time.Time
	EndTime   time.Time
	Status    string
}

type ResumeState struct {
	Source      string
	Destination string
	Transfers   map[string]TransferState
	LastUpdated time.Time
}

func NewTracker(source, destination string) *Tracker {
	stateFile := filepath.Join(os.TempDir(), "gosync-"+hashPath(source, destination)+".json")
	return &Tracker{
		stateFile: stateFile,
		transfers: make(map[string]TransferState),
	}
}

func (t *Tracker) Load() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	data, err := os.ReadFile(t.stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	var state ResumeState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}

	t.transfers = state.Transfers
	return nil
}

func (t *Tracker) Save() error {
	t.mu.RLock()
	defer t.mu.RUnlock()

	state := ResumeState{
		Transfers:   t.transfers,
		LastUpdated: time.Now(),
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(t.stateFile, data, 0644)
}

func (t *Tracker) IsCompleted(path string, size int64) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()

	state, exists := t.transfers[path]
	if !exists {
		return false
	}

	return state.Status == "completed" && state.Size == size
}

func (t *Tracker) MarkStarted(path string, size int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.transfers[path] = TransferState{
		Path:      path,
		Size:      size,
		StartTime: time.Now(),
		Status:    "in-progress",
	}
}

func (t *Tracker) MarkCompleted(path string, size int64, checksum string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.transfers[path] = TransferState{
		Path:      path,
		Size:      size,
		Checksum:  checksum,
		EndTime:   time.Now(),
		Status:    "completed",
	}
}

func (t *Tracker) MarkFailed(path string, size int64, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.transfers[path] = TransferState{
		Path:      path,
		Size:      size,
		EndTime:   time.Now(),
		Status:    "failed",
	}
}

func (t *Tracker) GetStats() (completed, failed, pending int) {
	t.mu.RLock()
	defer t.mu.RUnlock()

	for _, state := range t.transfers {
		switch state.Status {
		case "completed":
			completed++
		case "failed":
			failed++
		default:
			pending++
		}
	}
	return
}

func (t *Tracker) Cleanup() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	return os.Remove(t.stateFile)
}

func hashPath(source, destination string) string {
	h := 0
	for _, c := range source + destination {
		h = h*31 + int(c)
	}
	return fmt.Sprintf("%x", h)
}
