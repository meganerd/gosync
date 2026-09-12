package sync

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gbjohnso/gosync/pkg/progress"
	"github.com/gbjohnso/gosync/pkg/scanner"
	"github.com/gbjohnso/gosync/pkg/transport"
	"github.com/gbjohnso/gosync/pkg/worker"
)

type Sync struct {
	source          string
	destination     string
	transport       transport.Transport
	pool            *worker.WorkerPool
	scanner         *scanner.Scanner
	config          Config
	createTargetDir bool
	remoteBase      string
}

type Config struct {
	Workers     int
	Connections int // QUIC data connections (sender sockets) per file; 0 leaves the transport's own setting
	Bandwidth   int64
	Compression bool
	DryRun      bool
	Verbose     bool
	Quiet       bool
	Progress    bool
	Resume      bool
	Checksum    bool
	Excludes    []string
	Includes    []string
}

func NewSync(source, destination string, transport transport.Transport, config Config) *Sync {
	createTargetDir := true
	if strings.HasSuffix(source, "/") || strings.HasSuffix(source, "\\") {
		createTargetDir = false
	}

	return &Sync{
		source:          source,
		destination:     destination,
		transport:       transport,
		config:          config,
		createTargetDir: createTargetDir,
	}
}

func (s *Sync) SetRemoteBase(base string) {
	s.remoteBase = base
}

func (s *Sync) Run() error {
	start := time.Now()

	if !s.config.Quiet {
		fmt.Printf("Starting gosync: %s -> %s\n", s.source, s.destination)
	}

	s.scanner = scanner.NewScanner(
		[]string{s.source},
		s.config.Excludes,
		s.config.Includes,
	)

	files, err := s.scanner.Scan()
	if err != nil {
		return fmt.Errorf("scan failed: %w", err)
	}

	if !s.config.Quiet {
		fmt.Printf("Found %d files (%s)\n", len(files), formatSize(s.scanner.TotalSize()))
	}

	workers := s.config.Workers
	if workers == 0 {
		workers = s.scanner.AdaptiveWorkerCount(16)
		if !s.config.Quiet {
			fmt.Printf("Adaptive workers: %d\n", workers)
		}
	}

	if !s.config.DryRun {
		if configurable, ok := s.transport.(transport.ChecksumConfigurer); ok {
			configurable.SetChecksum(s.config.Checksum)
		}
		// Fan-out is negotiated during the handshake, so configure it first.
		if s.config.Connections > 0 {
			if configurable, ok := s.transport.(transport.ConnectionsConfigurer); ok {
				if err := configurable.SetConnections(s.config.Connections); err != nil {
					return fmt.Errorf("invalid connections: %w", err)
				}
			}
		}
		if err := s.transport.Connect(extractHost(s.destination), extractPort(s.destination)); err != nil {
			return fmt.Errorf("connect failed: %w", err)
		}
		defer s.transport.Close()
	}

	sourcePath, destinationPath := s.progressPaths()
	var display *transferProgress
	if s.config.Progress && !s.config.Quiet && !s.config.DryRun {
		display = &transferProgress{
			tracker:     progress.NewProgress(s.scanner.TotalSize(), len(files)),
			writer:      os.Stderr,
			source:      sourcePath,
			destination: destinationPath,
		}
		if info, err := os.Stderr.Stat(); err == nil {
			display.terminal = info.Mode()&os.ModeCharDevice != 0
		}
		if reporter, ok := s.transport.(transport.ProgressReporter); ok {
			display.streaming = true
			reporter.SetProgressCallback(display.tracker.AddBytes)
			defer reporter.SetProgressCallback(nil)
		}
	}

	s.pool = worker.NewWorkerPool(workers, s.transport)
	s.pool.Start()

	resultsDone := make(chan struct{})
	go func() {
		defer close(resultsDone)
		s.processResults(display)
	}()

	for _, file := range files {
		remotePath := s.buildRemotePath(file.Path)

		if s.config.DryRun {
			if !s.config.Quiet {
				fmt.Printf("[DRY RUN] Would transfer: %s -> %s\n", file.Path, remotePath)
			}
			continue
		}

		job := worker.TransferJob{
			LocalPath:  file.Path,
			RemotePath: remotePath,
			Size:       file.Size,
			Priority:   file.Priority,
		}
		s.pool.Submit(job)
	}

	s.pool.WaitForCompletion()
	<-resultsDone

	stats := s.pool.GetStats()
	elapsed := time.Since(start)

	if !s.config.Quiet {
		fmt.Printf("\nTransfer complete:\n")
		fmt.Printf("  Source: %s\n  Destination: %s\n", sourcePath, destinationPath)
		fmt.Printf("  Files: %d transferred, %d failed\n", stats.CompletedFiles, stats.FailedFiles)
		fmt.Printf("  Size:  %s\n", formatSize(stats.TransferredBytes))
		fmt.Printf("  Time:  %s\n", elapsed.Round(time.Millisecond))
		fmt.Printf("  Speed: %s/s\n", formatSize(int64(float64(stats.TransferredBytes)/elapsed.Seconds())))
	}

	if stats.FailedFiles > 0 {
		return fmt.Errorf("%d file transfers failed", stats.FailedFiles)
	}
	return nil
}

func (s *Sync) processResults(display *transferProgress) {
	var ticks <-chan time.Time
	if display != nil {
		interval := time.Second
		if display.terminal {
			interval = 200 * time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		ticks = ticker.C
		display.render(false)
	}
	for {
		select {
		case <-ticks:
			display.render(false)
		case result, ok := <-s.pool.Results():
			if !ok {
				if display != nil {
					display.render(true)
				}
				return
			}
			if display != nil {
				var bytes int64
				// Custom transports without live reporting fall back to completed files.
				if !display.streaming && result.Error == nil {
					bytes = result.Job.Size
				}
				display.tracker.Update(bytes, result.Error == nil)
				if !s.config.Quiet && (result.Error != nil || s.config.Verbose) {
					display.clear()
				}
			}
			if result.Error != nil {
				if !s.config.Quiet {
					fmt.Printf("ERROR: %s: %v\n", result.Job.LocalPath, result.Error)
				}
			} else if s.config.Verbose && !s.config.Quiet {
				fmt.Printf("OK: %s (%s in %s)\n",
					result.Job.LocalPath,
					formatSize(result.Job.Size),
					result.Duration.Round(time.Millisecond))
			}
		}
	}
}

func (s *Sync) buildRemotePath(localPath string) string {
	remoteBase := s.remoteBase
	if remoteBase == "" {
		remoteBase = s.destination
		if idx := strings.LastIndex(remoteBase, ":"); idx != -1 {
			remoteBase = remoteBase[idx+1:]
		}
	}

	remoteBase = strings.TrimRight(remoteBase, "/\\")

	info, statErr := os.Stat(s.source)
	if statErr == nil && !info.IsDir() {
		// Deployment configures a receiver directory, not a destination filename.
		if s.remoteBase != "" {
			return filepath.Base(localPath)
		}
		return filepath.Base(remoteBase)
	}

	rel, err := filepath.Rel(s.source, localPath)
	if err != nil {
		rel = filepath.Base(localPath)
	}

	if s.createTargetDir {
		return filepath.Join(filepath.Base(s.source), rel)
	}

	return rel
}

func extractHost(dest string) string {
	if idx := strings.Index(dest, "@"); idx != -1 {
		dest = dest[idx+1:]
	}
	if idx := strings.Index(dest, ":"); idx != -1 {
		return dest[:idx]
	}
	return dest
}

func extractPort(dest string) int {
	if idx := strings.Index(dest, "@"); idx != -1 {
		dest = dest[idx+1:]
	}
	if idx := strings.Index(dest, ":"); idx != -1 {
		var port int
		fmt.Sscanf(dest[idx+1:], "%d", &port)
		if port > 0 {
			return port
		}
		return 22
	}
	return 22
}

func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)

	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
