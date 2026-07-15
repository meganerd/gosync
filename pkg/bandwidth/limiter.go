package bandwidth

import (
	"io"
	"time"
)

type Limiter struct {
	limit     int64
	bucket    int64
	lastCheck time.Time
}

func NewLimiter(bytesPerSecond int64) *Limiter {
	return &Limiter{
		limit:     bytesPerSecond,
		bucket:    bytesPerSecond,
		lastCheck: time.Now(),
	}
}

func (l *Limiter) Allow(n int) bool {
	if l.limit == 0 {
		return true
	}

	now := time.Now()
	elapsed := now.Sub(l.lastCheck).Seconds()
	l.lastCheck = now

	l.bucket += int64(float64(l.limit) * elapsed)
	if l.bucket > l.limit {
		l.bucket = l.limit
	}

	if int64(n) <= l.bucket {
		l.bucket -= int64(n)
		return true
	}

	return false
}

func (l *Limiter) Wait(n int) {
	if l.limit == 0 {
		return
	}

	for !l.Allow(n) {
		time.Sleep(time.Millisecond)
	}
}

type LimitedReader struct {
	reader  io.Reader
	limiter *Limiter
}

func NewLimitedReader(reader io.Reader, limiter *Limiter) *LimitedReader {
	return &LimitedReader{
		reader:  reader,
		limiter: limiter,
	}
}

func (r *LimitedReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.limiter.Wait(n)
	}
	return n, err
}
