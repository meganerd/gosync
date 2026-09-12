package sync

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
)

type fakeTransport struct {
	mu          sync.Mutex
	connected   bool
	connectHost string
	connectPort int
	sent        []struct{ local, remote string }
	connectErr  error
	sendErr     map[string]error
}

func (f *fakeTransport) Name() string { return "fake" }
func (f *fakeTransport) Connect(host string, port int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connectHost = host
	f.connectPort = port
	f.connected = true
	return f.connectErr
}
func (f *fakeTransport) SendFile(localPath, remotePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, struct{ local, remote string }{localPath, remotePath})
	return f.sendErr[localPath]
}
func (f *fakeTransport) ReceiveFile(string, string) error          { return nil }
func (f *fakeTransport) SendStream(io.Reader, string, int64) error { return nil }
func (f *fakeTransport) ReceiveStream(string, io.Writer) error     { return nil }
func (f *fakeTransport) Close() error {
	f.mu.Lock()
	f.connected = false
	f.mu.Unlock()
	return nil
}
func (f *fakeTransport) IsConnected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func TestBuildRemotePathAndHelpers(t *testing.T) {
	dir := t.TempDir()
	nestedFile := filepath.Join(dir, "nested", "child.txt")
	if err := os.MkdirAll(filepath.Dir(nestedFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nestedFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &Sync{source: dir, destination: "host:2222:/remote/base"}
	if got := s.buildRemotePath(nestedFile); got != filepath.Join("nested", "child.txt") {
		t.Fatalf("buildRemotePath(dir) = %q", got)
	}

	sourceFile := filepath.Join(dir, "single.bin")
	if err := os.WriteFile(sourceFile, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	s = &Sync{source: sourceFile, destination: "host:/remote/placed.bin"}
	if got := s.buildRemotePath(sourceFile); got != "placed.bin" {
		t.Fatalf("buildRemotePath(file) = %q, want placed.bin", got)
	}

	if got := extractHost("server:9000:/dst"); got != "server" {
		t.Fatalf("extractHost() = %q", got)
	}
	if got := extractPort("server:9000:/dst"); got != 9000 {
		t.Fatalf("extractPort() = %d", got)
	}
	if got := extractPort("server:/dst"); got != 22 {
		t.Fatalf("extractPort(default) = %d", got)
	}
}

func TestDeployedSingleFilePreservesFilename(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "Win10_22H2_English_x64v1.iso")
	if err := os.WriteFile(source, []byte("test ISO payload"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"~", "test123", "/remote/backup", "/remote/backup with spaces/", "/"} {
		t.Run(base, func(t *testing.T) {
			tr := &fakeTransport{}
			s := NewSync(source, "alice@host:54321", tr, Config{Workers: 1, Quiet: true})
			s.SetRemoteBase(base)
			if err := s.Run(); err != nil {
				t.Fatal(err)
			}
			if len(tr.sent) != 1 || tr.sent[0].remote != filepath.Base(source) {
				t.Fatalf("base %q: transfers = %+v, want filename %q", base, tr.sent, filepath.Base(source))
			}
		})
	}
}

func TestRunDryRunSkipsConnectAndTransfer(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "skip.log"), []byte("no"), 0o644); err != nil {
		t.Fatal(err)
	}

	transport := &fakeTransport{sendErr: map[string]error{}}
	s := NewSync(root, "host:1000:/dest", transport, Config{
		DryRun:   true,
		Quiet:    true,
		Includes: []string{"*.txt"},
		Excludes: []string{"*.log"},
	})

	if err := s.Run(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if transport.connectHost != "" || len(transport.sent) != 0 {
		t.Fatalf("dry-run should not connect or send, got host=%q sent=%d", transport.connectHost, len(transport.sent))
	}
}

func TestRunTransfersFilesAndBuildsRelativeRemotePaths(t *testing.T) {
	root := t.TempDir()
	files := []string{
		filepath.Join(root, "a.txt"),
		filepath.Join(root, "nested", "b.txt"),
	}
	for _, path := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(filepath.Base(path)), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	transport := &fakeTransport{sendErr: map[string]error{}}
	s := NewSync(root, "remotehost:4444:/backup", transport, Config{Workers: 2, Quiet: true})
	if err := s.Run(); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if transport.connectHost != "remotehost" || transport.connectPort != 4444 {
		t.Fatalf("Connect() called with %s:%d", transport.connectHost, transport.connectPort)
	}

	var gotRemotes []string
	for _, sent := range transport.sent {
		gotRemotes = append(gotRemotes, sent.remote)
	}
	sort.Strings(gotRemotes)
	want := []string{
		filepath.Join(filepath.Base(root), "a.txt"),
		filepath.Join(filepath.Base(root), "nested", "b.txt"),
	}
	sort.Strings(want)
	if !reflect.DeepEqual(gotRemotes, want) {
		t.Fatalf("remote paths = %v, want %v", gotRemotes, want)
	}
	if transport.IsConnected() {
		t.Fatal("transport should be closed after Run")
	}
}

func TestRunReturnsWrappedErrors(t *testing.T) {
	transport := &fakeTransport{sendErr: map[string]error{}, connectErr: errors.New("dial failed")}
	s := NewSync(filepath.Join(t.TempDir(), "missing"), "host:/dest", transport, Config{Quiet: true})
	if err := s.Run(); err == nil || err.Error() == "" {
		t.Fatal("expected scan failure for missing source")
	}

	root := t.TempDir()
	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s = NewSync(root, "host:/dest", transport, Config{Quiet: true})
	if err := s.Run(); err == nil || err.Error() != "connect failed: dial failed" {
		t.Fatalf("expected wrapped connect error, got %v", err)
	}
}
