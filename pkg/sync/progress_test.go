package sync

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gbjohnso/gosync/pkg/progress"
)

func TestProgressRendering(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		var output bytes.Buffer
		p := &transferProgress{tracker: progress.NewProgress(100, 2), writer: &output, terminal: terminal,
			source: "/local/source", destination: "user@host:1234:/remote/destination"}
		p.tracker.AddBytes(50)
		p.render(false)
		if !strings.Contains(output.String(), "50.0% 50 B/100 B") || !strings.Contains(output.String(), "Files: 0/2") {
			t.Fatalf("live progress: %q", output.String())
		}
		p.clear()
		p.tracker.Update(0, true)
		p.tracker.Update(0, false)
		p.render(true)
		if !strings.Contains(output.String(), "Files: 1/2, 1 failed") || !strings.HasSuffix(output.String(), "\n") {
			t.Fatalf("final progress: %q", output.String())
		}
		if strings.Count(output.String(), "Source: /local/source\n") != 2 ||
			strings.Count(output.String(), "Destination: user@host:1234:/remote/destination\n") != 2 {
			t.Fatalf("paths not repeated: %q", output.String())
		}
		if !terminal && strings.Contains(output.String(), "\r") {
			t.Fatal("redirected output contains carriage returns")
		}
	}
}

type remoteBaseTransport struct {
	fakeTransport
	base string
}

func (f *remoteBaseTransport) RemoteBase() string { return f.base }

func TestProgressPaths(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Dir(root))
	for _, tc := range []struct {
		name, source, destination, base, deployBase, want string
	}{
		{"directory", root, "alice@host:1234:/requested", "/actual/base", "", "alice@host:1234:/actual/base/" + filepath.Base(root)},
		{"contents", root + "/", "host:/requested", "/actual/base", "", "host:22:/actual/base"},
		{"relative source", filepath.Base(root), "host:/requested", "/actual/base", "", "host:22:/actual/base/" + filepath.Base(root)},
		{"single file", file, "host:/requested/renamed.txt", "/actual/base", "", "host:22:/actual/base/renamed.txt"},
		{"deployed single home", file, "alice@host:54321", "/home/alice", "~", "alice@host:54321:/home/alice/file.txt"},
		{"deployed single directory", file, "alice@host:54321", "/home/alice/test123", "test123", "alice@host:54321:/home/alice/test123/file.txt"},
		{"deployed", root + "/", "alice@host:54321", "/home/alice/backup", "~/backup", "alice@host:54321:/home/alice/backup"},
		{"legacy deployed", root + "/", "host:54321", "", "/backup", "host:54321:/backup"},
		{"legacy relative", root + "/", "host:54321", "", "~/backup", "host:54321:~/backup (remote-relative; absolute base unavailable)"},
		{"unknown base", root + "/", "host:/requested", "", "", "host:22:. (relative to remote base; absolute base unavailable)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := &remoteBaseTransport{base: tc.base}
			s := NewSync(tc.source, tc.destination, tr, Config{})
			s.SetRemoteBase(tc.deployBase)
			source, destination := s.progressPaths()
			wantSource, err := filepath.Abs(tc.source)
			if err != nil {
				t.Fatal(err)
			}
			if source != wantSource || destination != tc.want {
				t.Fatalf("paths = %q -> %q; want %q -> %q", source, destination, wantSource, tc.want)
			}
		})
	}
}

func TestEmptyProgressRendering(t *testing.T) {
	var output bytes.Buffer
	p := &transferProgress{tracker: progress.NewProgress(0, 0), writer: &output}
	p.render(true)
	if !strings.Contains(output.String(), "100.0% 0 B/0 B") || !strings.Contains(output.String(), "ETA 0s") {
		t.Fatalf("empty progress: %q", output.String())
	}
}

func TestRunProgressModes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config Config
		fail   bool
		want   string
	}{
		{"enabled", Config{Progress: true}, false, "100.0% 4 B/4 B"},
		{"failed", Config{Progress: true}, true, "Files: 0/1, 1 failed"},
		{"disabled", Config{}, false, ""},
		{"quiet", Config{Progress: true, Quiet: true, Verbose: true}, false, ""},
		{"dry-run", Config{Progress: true, DryRun: true}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "file")
			if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
				t.Fatal(err)
			}
			tr := &fakeTransport{sendErr: map[string]error{}}
			if tc.fail {
				tr.sendErr[path] = errors.New("send failed")
			}
			output, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			defer output.Close()
			original := os.Stderr
			os.Stderr = output
			defer func() { os.Stderr = original }()
			s := NewSync(root, "host:1234:/dest", tr, tc.config)
			if err := s.Run(); (err != nil) != tc.fail {
				t.Fatalf("Run() error = %v, want failure %v", err, tc.fail)
			}
			if _, err := output.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			text, err := io.ReadAll(output)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" && len(text) != 0 {
				t.Fatalf("unexpected progress: %q", text)
			}
			if tc.want != "" && !strings.Contains(string(text), tc.want) {
				t.Fatalf("progress %q missing %q", text, tc.want)
			}
		})
	}
}

type liveProgressTransport struct {
	fakeTransport
	callback func(int64)
	written  chan struct{}
	release  chan struct{}
}

func (f *liveProgressTransport) SetProgressCallback(fn func(int64)) { f.callback = fn }
func (f *liveProgressTransport) SendFile(local, remote string) error {
	f.callback(2)
	close(f.written)
	<-f.release
	f.callback(2)
	return f.fakeTransport.SendFile(local, remote)
}

func TestLiveProgressBeforeFileCompletion(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	original := os.Stderr
	os.Stderr = output
	defer func() { os.Stderr = original }()
	tr := &liveProgressTransport{written: make(chan struct{}), release: make(chan struct{})}
	s := NewSync(root, "host:1234:/dest", tr, Config{Workers: 1, Progress: true})
	done := make(chan error, 1)
	go func() { done <- s.Run() }()
	defer func() {
		close(tr.release)
		if err := <-done; err != nil {
			t.Error(err)
		}
		if tr.callback != nil {
			t.Error("progress callback not removed")
		}
		text, err := os.ReadFile(output.Name())
		if err != nil {
			t.Error(err)
		}
		if !strings.Contains(string(text), "100.0% 4 B/4 B") {
			t.Errorf("final progress double-counted or missing: %q", text)
		}
	}()
	select {
	case <-tr.written:
	case <-time.After(5 * time.Second):
		t.Fatal("transfer never started")
	}
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("no incremental progress before file completion")
		case <-ticker.C:
			text, err := os.ReadFile(output.Name())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(text), "50.0% 2 B/4 B") && strings.Contains(string(text), "Files: 0/1") {
				return
			}
		}
	}
}
