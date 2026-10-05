package worker

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gbjohnso/gosync/pkg/transport"
)

type WorkerPool struct {
	workers          int
	transport        transport.Transport
	jobs             chan TransferJob
	results          chan TransferResult
	wg               sync.WaitGroup
	ctx              context.Context
	cancel           context.CancelFunc
	closeJobsOnce    sync.Once
	closeResultsOnce sync.Once
	stats            Stats
	statsMu          sync.RWMutex
}

type TransferJob struct {
	LocalPath  string
	RemotePath string
	Size       int64
	IsDevice   bool
	Priority   int
}

type TransferResult struct {
	Job      TransferJob
	Duration time.Duration
	Speed    float64
	Checksum string
	Error    error
}

type Stats struct {
	TotalFiles       int
	CompletedFiles   int
	FailedFiles      int
	TotalBytes       int64
	TransferredBytes int64
	StartTime        time.Time
	EndTime          time.Time
}

func NewWorkerPool(workers int, transport transport.Transport) *WorkerPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &WorkerPool{
		workers:   workers,
		transport: transport,
		jobs:      make(chan TransferJob, workers*2),
		results:   make(chan TransferResult, workers*2),
		ctx:       ctx,
		cancel:    cancel,
		stats: Stats{
			StartTime: time.Now(),
		},
	}
}

func (p *WorkerPool) Start() {
	for i := 0; i < p.workers; i++ {
		p.wg.Add(1)
		go p.worker(i)
	}
}

func (p *WorkerPool) worker(id int) {
	defer p.wg.Done()

	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}

		select {
		case <-p.ctx.Done():
			return
		case job, ok := <-p.jobs:
			if !ok {
				return
			}
			result := p.processJob(id, job)
			p.results <- result
		}
	}
}

func (p *WorkerPool) processJob(id int, job TransferJob) TransferResult {
	start := time.Now()

	var err error
	if job.IsDevice {
		if sender, ok := p.transport.(transport.SizedFileSender); ok {
			err = sender.SendSizedFile(job.LocalPath, job.RemotePath, job.Size)
		} else {
			err = fmt.Errorf("transport %s does not support block-device sources", p.transport.Name())
		}
	} else {
		err = p.transport.SendFile(job.LocalPath, job.RemotePath)
	}
	if err != nil {
		p.updateStats(false, job.Size)
		return TransferResult{
			Job:      job,
			Duration: time.Since(start),
			Error:    err,
		}
	}

	p.updateStats(true, job.Size)
	return TransferResult{
		Job:      job,
		Duration: time.Since(start),
		Speed:    float64(job.Size) / time.Since(start).Seconds(),
	}
}

func (p *WorkerPool) updateStats(success bool, bytes int64) {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()

	p.stats.TotalFiles++
	if success {
		p.stats.CompletedFiles++
		p.stats.TransferredBytes += bytes
	} else {
		p.stats.FailedFiles++
	}
	p.stats.TotalBytes += bytes
}

func (p *WorkerPool) Submit(job TransferJob) {
	p.jobs <- job
}

func (p *WorkerPool) Results() <-chan TransferResult {
	return p.results
}

func (p *WorkerPool) Close() {
	p.cancel()
	p.closeJobsOnce.Do(func() {
		close(p.jobs)
	})
	p.wg.Wait()
	p.closeResultsOnce.Do(func() {
		close(p.results)
	})
}

func (p *WorkerPool) GetStats() Stats {
	p.statsMu.RLock()
	defer p.statsMu.RUnlock()
	return p.stats
}

func (p *WorkerPool) WaitForCompletion() {
	p.closeJobsOnce.Do(func() {
		close(p.jobs)
	})
	p.wg.Wait()
	p.closeResultsOnce.Do(func() {
		close(p.results)
	})
	p.statsMu.Lock()
	p.stats.EndTime = time.Now()
	p.statsMu.Unlock()
}

func (p *WorkerPool) Progress() float64 {
	p.statsMu.RLock()
	defer p.statsMu.RUnlock()

	if p.stats.TotalFiles == 0 {
		return 0
	}
	return float64(p.stats.CompletedFiles) / float64(p.stats.TotalFiles) * 100
}

func (p *WorkerPool) Speed() float64 {
	p.statsMu.RLock()
	defer p.statsMu.RUnlock()

	elapsed := time.Since(p.stats.StartTime).Seconds()
	if elapsed == 0 {
		return 0
	}
	return float64(p.stats.TransferredBytes) / elapsed
}
