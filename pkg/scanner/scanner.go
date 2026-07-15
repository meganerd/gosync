package scanner

import (
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type Scanner struct {
	roots       []string
	excludes    []string
	includes    []string
	files       []FileInfo
	mu          sync.RWMutex
}

type FileInfo struct {
	Path     string
	Size     int64
	ModTime  int64
	IsDir    bool
	Priority int
}

func NewScanner(roots, excludes, includes []string) *Scanner {
	return &Scanner{
		roots:    roots,
		excludes: excludes,
		includes: includes,
	}
}

func (s *Scanner) Scan() ([]FileInfo, error) {
	s.files = nil

	for _, root := range s.roots {
		info, err := os.Stat(root)
		if err != nil {
			return nil, err
		}

		if !info.IsDir() {
			if s.shouldExclude(root) {
				continue
			}
			if !s.shouldInclude(root) {
				continue
			}
			s.files = append(s.files, FileInfo{
				Path:     root,
				Size:     info.Size(),
				ModTime:  info.ModTime().Unix(),
				IsDir:    false,
				Priority: s.calculatePriority(info.Size()),
			})
			continue
		}

		err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}

			if info.IsDir() {
				return nil
			}

			if s.shouldExclude(path) {
				return nil
			}

			if !s.shouldInclude(path) {
				return nil
			}

			s.files = append(s.files, FileInfo{
				Path:     path,
				Size:     info.Size(),
				ModTime:  info.ModTime().Unix(),
				IsDir:    false,
				Priority: s.calculatePriority(info.Size()),
			})

			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	s.sortFiles()
	return s.files, nil
}

func (s *Scanner) shouldExclude(path string) bool {
	for _, pattern := range s.excludes {
		matched, _ := filepath.Match(pattern, filepath.Base(path))
		if matched {
			return true
		}
	}
	return false
}

func (s *Scanner) shouldInclude(path string) bool {
	if len(s.includes) == 0 {
		return true
	}

	for _, pattern := range s.includes {
		matched, _ := filepath.Match(pattern, filepath.Base(path))
		if matched {
			return true
		}
	}
	return false
}

func (s *Scanner) calculatePriority(size int64) int {
	switch {
	case size < 1024*1024: // < 1MB
		return 1
	case size < 10*1024*1024: // < 10MB
		return 2
	case size < 100*1024*1024: // < 100MB
		return 3
	case size < 1024*1024*1024: // < 1GB
		return 4
	default: // >= 1GB
		return 5
	}
}

func (s *Scanner) sortFiles() {
	sort.Slice(s.files, func(i, j int) bool {
		if s.files[i].Priority != s.files[j].Priority {
			return s.files[i].Priority > s.files[j].Priority
		}
		return s.files[i].Size > s.files[j].Size
	})
}

func (s *Scanner) TotalSize() int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var total int64
	for _, f := range s.files {
		total += f.Size
	}
	return total
}

func (s *Scanner) FileCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.files)
}

func (s *Scanner) GetFiles() []FileInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	files := make([]FileInfo, len(s.files))
	copy(files, s.files)
	return files
}

func (s *Scanner) SplitBySize(chunkSize int64) [][]FileInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var chunks [][]FileInfo
	var currentChunk []FileInfo
	var currentSize int64

	for _, f := range s.files {
		if currentSize+f.Size > chunkSize && len(currentChunk) > 0 {
			chunks = append(chunks, currentChunk)
			currentChunk = nil
			currentSize = 0
		}
		currentChunk = append(currentChunk, f)
		currentSize += f.Size
	}

	if len(currentChunk) > 0 {
		chunks = append(chunks, currentChunk)
	}

	return chunks
}

func (s *Scanner) AdaptiveWorkerCount(maxWorkers int) int {
	totalSize := s.TotalSize()
	fileCount := s.FileCount()

	switch {
	case fileCount > 10000:
		return min(maxWorkers, 16)
	case fileCount > 1000:
		return min(maxWorkers, 8)
	case totalSize > 10*1024*1024*1024: // > 10GB
		return min(maxWorkers, 8)
	case totalSize > 1024*1024*1024: // > 1GB
		return min(maxWorkers, 4)
	default:
		return min(maxWorkers, 2)
	}
}
