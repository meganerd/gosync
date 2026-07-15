package progress

import (
	"fmt"
	"sync"
	"time"
)

type Progress struct {
	totalBytes    int64
 transferredBytes int64
	totalFiles    int
	completedFiles int
	failedFiles   int
	startTime     time.Time
	mu            sync.RWMutex
	callback      func(ProgressInfo)
}

type ProgressInfo struct {
	TotalBytes      int64
	TransferredBytes int64
	TotalFiles      int
	CompletedFiles  int
	FailedFiles     int
	Speed           float64
	Elapsed         time.Duration
	Percent         float64
	ETA             time.Duration
}

func NewProgress(totalBytes int64, totalFiles int) *Progress {
	return &Progress{
		totalBytes: totalBytes,
		totalFiles: totalFiles,
		startTime:  time.Now(),
	}
}

func (p *Progress) SetCallback(fn func(ProgressInfo)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.callback = fn
}

func (p *Progress) Update(bytes int64, success bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.transferredBytes += bytes
	p.totalFiles++

	if success {
		p.completedFiles++
	} else {
		p.failedFiles++
	}

	if p.callback != nil {
		p.callback(p.getInfo())
	}
}

func (p *Progress) getInfo() ProgressInfo {
	elapsed := time.Since(p.startTime)
	speed := float64(p.transferredBytes) / elapsed.Seconds()
	percent := float64(p.transferredBytes) / float64(p.totalBytes) * 100

	var eta time.Duration
	if speed > 0 {
		remaining := float64(p.totalBytes-p.transferredBytes) / speed
		eta = time.Duration(remaining) * time.Second
	}

	return ProgressInfo{
		TotalBytes:      p.totalBytes,
		TransferredBytes: p.transferredBytes,
		TotalFiles:      p.totalFiles,
		CompletedFiles:  p.completedFiles,
		FailedFiles:     p.failedFiles,
		Speed:           speed,
		Elapsed:         elapsed,
		Percent:         percent,
		ETA:             eta,
	}
}

func (p *Progress) GetInfo() ProgressInfo {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.getInfo()
}

func (p *Progress) String() string {
	info := p.GetInfo()
	return fmt.Sprintf("%.1f%% %s/%s %s/s ETA %s",
		info.Percent,
		formatSize(info.TransferredBytes),
		formatSize(info.TotalBytes),
		formatSize(int64(info.Speed)),
		info.ETA.Round(time.Second))
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
