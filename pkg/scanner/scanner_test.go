package scanner

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestScannerScanAppliesIncludeExcludeAndSortOrder(t *testing.T) {
	root := t.TempDir()
	writeSizedFile(t, filepath.Join(root, "keep-large.bin"), 2*1024*1024)
	writeSizedFile(t, filepath.Join(root, "keep-small.txt"), 128)
	writeSizedFile(t, filepath.Join(root, "skip.log"), 256)
	writeSizedFile(t, filepath.Join(root, "nested", "keep-medium.txt"), 2*1024)

	s := NewScanner([]string{root}, []string{"*.log"}, []string{"*.txt", "*.bin"})
	files, err := s.Scan()
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}

	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(files))
	}

	gotBases := []string{
		filepath.Base(files[0].Path),
		filepath.Base(files[1].Path),
		filepath.Base(files[2].Path),
	}
	wantBases := []string{"keep-large.bin", "keep-medium.txt", "keep-small.txt"}
	if !reflect.DeepEqual(gotBases, wantBases) {
		t.Fatalf("sorted files = %v, want %v", gotBases, wantBases)
	}

	if s.TotalSize() != 2*1024*1024+2*1024+128 {
		t.Fatalf("TotalSize() = %d", s.TotalSize())
	}
	if s.FileCount() != 3 {
		t.Fatalf("FileCount() = %d, want 3", s.FileCount())
	}

	copyFiles := s.GetFiles()
	copyFiles[0].Path = "mutated"
	if s.GetFiles()[0].Path == "mutated" {
		t.Fatal("GetFiles() did not return a copy")
	}

	chunks := s.SplitBySize(2 * 1024 * 1024)
	if len(chunks) != 2 {
		t.Fatalf("SplitBySize() chunks = %d, want 2", len(chunks))
	}
}

func TestScannerSingleFileRootHonorsPatterns(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "sample.txt")
	writeSizedFile(t, file, 64)

	includeScanner := NewScanner([]string{file}, nil, []string{"*.txt"})
	files, err := includeScanner.Scan()
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(files) != 1 || files[0].Path != file {
		t.Fatalf("single file scan = %#v", files)
	}

	excludeScanner := NewScanner([]string{file}, []string{"*.txt"}, nil)
	files, err = excludeScanner.Scan()
	if err != nil {
		t.Fatalf("Scan() with exclude error = %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("excluded single file scan len = %d, want 0", len(files))
	}
}

type modeFileInfo struct {
	os.FileInfo
	mode os.FileMode
}

func (i modeFileInfo) Mode() os.FileMode { return i.mode }

func TestSourceSizeUsesLinuxBlockDeviceCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device")
	writeSizedFile(t, path, 1)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	previous := getBlockDeviceSize
	getBlockDeviceSize = func(gotPath string) (int64, error) {
		if gotPath != path {
			t.Fatalf("block device path = %q, want %q", gotPath, path)
		}
		return 8 * 1024 * 1024 * 1024, nil
	}
	t.Cleanup(func() { getBlockDeviceSize = previous })

	size, device, err := sourceSize(path, modeFileInfo{FileInfo: info, mode: os.ModeDevice})
	if err != nil {
		t.Fatal(err)
	}
	if !device || size != 8*1024*1024*1024 {
		t.Fatalf("sourceSize() = (%d, %t), want (8 GiB, true)", size, device)
	}
}

func TestSourceSizeRejectsStreamingSpecialFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "special")
	writeSizedFile(t, path, 1)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sourceSize(path, modeFileInfo{FileInfo: info, mode: os.ModeNamedPipe}); err == nil {
		t.Fatal("named pipe source was accepted without a finite size")
	}
}

func TestScannerPriorityBoundariesAndAdaptiveWorkers(t *testing.T) {
	s := NewScanner(nil, nil, nil)

	priorityCases := []struct {
		size int64
		want int
	}{
		{0, 1},
		{1024*1024 - 1, 1},
		{1024 * 1024, 2},
		{10*1024*1024 - 1, 2},
		{10 * 1024 * 1024, 3},
		{100*1024*1024 - 1, 3},
		{100 * 1024 * 1024, 4},
		{1024*1024*1024 - 1, 4},
		{1024 * 1024 * 1024, 5},
	}
	for _, tc := range priorityCases {
		if got := s.calculatePriority(tc.size); got != tc.want {
			t.Fatalf("calculatePriority(%d) = %d, want %d", tc.size, got, tc.want)
		}
	}

	s.files = make([]FileInfo, 10001)
	if got := s.AdaptiveWorkerCount(32); got != 16 {
		t.Fatalf("AdaptiveWorkerCount high file count = %d, want 16", got)
	}

	s.files = make([]FileInfo, 1001)
	if got := s.AdaptiveWorkerCount(32); got != 8 {
		t.Fatalf("AdaptiveWorkerCount medium file count = %d, want 8", got)
	}

	s.files = []FileInfo{{Size: 11 * 1024 * 1024 * 1024}}
	if got := s.AdaptiveWorkerCount(32); got != 8 {
		t.Fatalf("AdaptiveWorkerCount total size > 10GB = %d, want 8", got)
	}

	s.files = []FileInfo{{Size: 2 * 1024 * 1024 * 1024}}
	if got := s.AdaptiveWorkerCount(32); got != 4 {
		t.Fatalf("AdaptiveWorkerCount total size > 1GB = %d, want 4", got)
	}

	s.files = []FileInfo{{Size: 1}}
	if got := s.AdaptiveWorkerCount(1); got != 1 {
		t.Fatalf("AdaptiveWorkerCount respects maxWorkers = %d, want 1", got)
	}
}

func writeSizedFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}
