package transport

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"sync/atomic"
	"testing"
)

// BenchmarkTransferLoopSynthetic isolates read/copy/hash/callback CPU without
// sockets or files. It models the sender loop, not the deployed protocol. Both
// source and destination copies are real; io.Discard would omit the latter.
func BenchmarkTransferLoopSynthetic(b *testing.B) {
	const size = 8 << 20
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i*31 + i/251)
	}
	wantHash := sha256.Sum256(payload)
	for _, bufferSize := range []int{32 << 10, 256 << 10, 1 << 20} {
		for _, hashing := range []bool{false, true} {
			for _, callback := range []bool{false, true} {
				name := fmt.Sprintf("buffer=%dKiB/SHA256=%t/ProgressAtomic=%t", bufferSize>>10, hashing, callback)
				b.Run(name, func(b *testing.B) {
					var destination bytes.Buffer
					destination.Grow(size)
					var progress atomic.Int64
					var report func(int64)
					if callback {
						report = func(n int64) { progress.Add(n) }
					}
					source := bytes.NewReader(payload)
					var digest [sha256.Size]byte
					b.SetBytes(size)
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						source.Reset(payload)
						destination.Reset()
						// Allocate once per transfer, as production does. The
						// destination capacity is reused to exclude sink growth.
						buf := make([]byte, bufferSize)
						var writer io.Writer = progressWriter{&destination, report}
						var hasher hash.Hash
						if hashing {
							hasher = sha256.New()
							writer = io.MultiWriter(writer, hasher)
						}
						remaining := int64(size)
						for remaining > 0 {
							n, err := source.Read(buf)
							if err != nil {
								b.Fatal(err)
							}
							written, err := writer.Write(buf[:n])
							if err != nil || written != n {
								b.Fatalf("write: got %d, want %d, error %v", written, n, err)
							}
							remaining -= int64(n)
						}
						if hashing {
							hasher.Sum(digest[:0])
						}
					}
					b.StopTimer()
					if !bytes.Equal(destination.Bytes(), payload) {
						b.Fatal("copied payload differs")
					}
					if hashing && digest != wantHash {
						b.Fatal("SHA256 differs")
					}
					if callback && progress.Load() != int64(b.N)*size {
						b.Fatalf("progress bytes: got %d, want %d", progress.Load(), int64(b.N)*size)
					}
				})
			}
		}
	}
}
