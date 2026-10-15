package checksum

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

// BenchmarkCopyNConcurrent measures short, independent transfers rather than a
// single long stream. That makes per-transfer allocation and GC costs visible
// under the concurrency levels expected from a busy server.
func BenchmarkCopyNConcurrent(b *testing.B) {
	const transferSize = 1 << 20
	payload := make([]byte, transferSize)
	for i := range payload {
		payload[i] = byte(i*31 + 17)
	}

	for _, hashing := range []bool{false, true} {
		for _, concurrency := range []int{1, 8, 32, 128} {
			b.Run(fmt.Sprintf("SHA256=%t/concurrency=%d", hashing, concurrency), func(b *testing.B) {
				b.SetBytes(transferSize)
				b.ReportAllocs()
				benchmarkConcurrentCopies(b, payload, hashing, concurrency)
			})
		}
	}
}

func benchmarkConcurrentCopies(b *testing.B, payload []byte, hashing bool, concurrency int) {
	var next atomic.Uint64
	var workers sync.WaitGroup
	workers.Add(concurrency)
	b.ResetTimer()
	for range concurrency {
		go func() {
			defer workers.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= b.N {
					return
				}
				src := bytes.NewReader(payload)
				var n int64
				var err error
				if hashing {
					n, _, err = CopyNWithSHA256(io.Discard, src, int64(len(payload)))
				} else {
					n, err = CopyNBuffer(io.Discard, src, int64(len(payload)), DefaultBufferSize)
				}
				if err != nil || n != int64(len(payload)) {
					b.Errorf("copy %d: copied %d bytes: %v", i, n, err)
					return
				}
			}
		}()
	}
	workers.Wait()
	b.StopTimer()
}
