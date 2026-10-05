package worker

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

type fakeTransport struct {
	delay         time.Duration
	failPaths     map[string]error
	started       chan struct{}
	mu            sync.Mutex
	current       int
	maxConcurrent int
	remotePaths   []string
}

type sizedFakeTransport struct {
	*fakeTransport
	size int64
}

func (f *sizedFakeTransport) SendSizedFile(localPath, remotePath string, size int64) error {
	f.size = size
	return f.SendFile(localPath, remotePath)
}

func (f *fakeTransport) Name() string                              { return "fake" }
func (f *fakeTransport) Connect(string, int) error                 { return nil }
func (f *fakeTransport) ReceiveFile(string, string) error          { return nil }
func (f *fakeTransport) SendStream(io.Reader, string, int64) error { return nil }
func (f *fakeTransport) ReceiveStream(string, io.Writer) error     { return nil }
func (f *fakeTransport) Close() error                              { return nil }
func (f *fakeTransport) IsConnected() bool                         { return true }

func (f *fakeTransport) SendFile(localPath, remotePath string) error {
	if f.started != nil {
		select {
		case <-f.started:
		default:
			close(f.started)
		}
	}
	f.mu.Lock()
	f.current++
	if f.current > f.maxConcurrent {
		f.maxConcurrent = f.current
	}
	f.remotePaths = append(f.remotePaths, remotePath)
	f.mu.Unlock()

	time.Sleep(f.delay)

	f.mu.Lock()
	f.current--
	err := f.failPaths[localPath]
	f.mu.Unlock()
	return err
}

func TestWorkerPoolProcessesJobsConcurrentlyAndTracksStats(t *testing.T) {
	transport := &fakeTransport{
		delay:     25 * time.Millisecond,
		failPaths: map[string]error{"file-3": errors.New("boom")},
	}
	pool := NewWorkerPool(3, transport)
	pool.Start()

	jobs := []TransferJob{
		{LocalPath: "file-1", RemotePath: "r1", Size: 100},
		{LocalPath: "file-2", RemotePath: "r2", Size: 200},
		{LocalPath: "file-3", RemotePath: "r3", Size: 300},
		{LocalPath: "file-4", RemotePath: "r4", Size: 400},
		{LocalPath: "file-5", RemotePath: "r5", Size: 500},
	}
	for _, job := range jobs {
		pool.Submit(job)
	}

	var results []TransferResult
	for i := 0; i < len(jobs); i++ {
		select {
		case result := <-pool.Results():
			results = append(results, result)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for result")
		}
	}

	pool.WaitForCompletion()

	stats := pool.GetStats()
	if stats.TotalFiles != len(jobs) {
		t.Fatalf("TotalFiles = %d, want %d", stats.TotalFiles, len(jobs))
	}
	if stats.CompletedFiles != 4 {
		t.Fatalf("CompletedFiles = %d, want 4", stats.CompletedFiles)
	}
	if stats.FailedFiles != 1 {
		t.Fatalf("FailedFiles = %d, want 1", stats.FailedFiles)
	}
	if stats.TotalBytes != 1500 {
		t.Fatalf("TotalBytes = %d, want 1500", stats.TotalBytes)
	}
	if stats.TransferredBytes != 1200 {
		t.Fatalf("TransferredBytes = %d, want 1200", stats.TransferredBytes)
	}
	if stats.EndTime.IsZero() {
		t.Fatal("EndTime was not set")
	}

	if transport.maxConcurrent < 2 {
		t.Fatalf("max concurrent workers = %d, want at least 2", transport.maxConcurrent)
	}

	var sawError bool
	for _, result := range results {
		if result.Job.LocalPath == "file-3" {
			sawError = result.Error != nil
		}
		if result.Job.LocalPath != "file-3" && result.Error != nil {
			t.Fatalf("unexpected error for %s: %v", result.Job.LocalPath, result.Error)
		}
	}
	if !sawError {
		t.Fatal("expected failed job result")
	}

	if got := pool.Progress(); got != 80 {
		t.Fatalf("Progress() = %.2f, want 80", got)
	}
	if pool.Speed() <= 0 {
		t.Fatalf("Speed() = %f, want > 0", pool.Speed())
	}
}

func TestWorkerPoolCloseCancelsOutstandingWork(t *testing.T) {
	started := make(chan struct{})
	transport := &fakeTransport{delay: 200 * time.Millisecond, failPaths: map[string]error{}, started: started}
	pool := NewWorkerPool(1, transport)
	pool.Start()

	pool.Submit(TransferJob{LocalPath: "first", RemotePath: "remote-first", Size: 10})
	pool.Submit(TransferJob{LocalPath: "second", RemotePath: "remote-second", Size: 10})

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never started processing a job")
	}
	pool.Close()

	stats := pool.GetStats()
	if stats.TotalFiles == 0 {
		t.Fatal("expected at least one processed file before cancellation")
	}
	if stats.TotalFiles >= 2 {
		t.Fatalf("expected cancellation to stop outstanding work, got %d processed files", stats.TotalFiles)
	}

	for range pool.Results() {
	}

	_ = fmt.Sprintf("%v", stats)
}

func TestWorkerPoolUsesDiscoveredSizeForBlockDevice(t *testing.T) {
	transport := &sizedFakeTransport{fakeTransport: &fakeTransport{failPaths: map[string]error{}}}
	pool := NewWorkerPool(1, transport)
	pool.Start()
	pool.Submit(TransferJob{LocalPath: "/dev/test", RemotePath: "disk.img", Size: 4096, IsDevice: true})
	pool.WaitForCompletion()

	if transport.size != 4096 {
		t.Fatalf("SendSizedFile size = %d, want 4096", transport.size)
	}
	if stats := pool.GetStats(); stats.CompletedFiles != 1 || stats.TransferredBytes != 4096 {
		t.Fatalf("stats = %#v", stats)
	}
}
