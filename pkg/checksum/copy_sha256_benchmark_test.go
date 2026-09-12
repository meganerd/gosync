package checksum

import (
	"bytes"
	"crypto/sha256"
	"io"
	"testing"
)

// The synchronous baseline uses the same chunk size, foreground reads/writes,
// and SHA-256, but hashes each accepted chunk before starting the next read.
func shaCopySync(dst io.Writer, src io.Reader, size int64) (written int64, digest [sha256.Size]byte, err error) {
	h := sha256.New()
	buf := make([]byte, copySHA256BufferSize)
	for written < size {
		n, readErr := src.Read(buf[:min(int64(len(buf)), size-written)])
		if n > 0 {
			nw, writeErr := dst.Write(buf[:n])
			written += int64(nw)
			h.Write(buf[:nw])
			if writeErr != nil {
				err = writeErr
				break
			}
			if nw != n {
				err = io.ErrShortWrite
				break
			}
		}
		if readErr != nil {
			if readErr != io.EOF || written != size {
				err = readErr
			}
			break
		}
	}
	h.Sum(digest[:0])
	return
}

func BenchmarkCopyNWithSHA256(b *testing.B) {
	payload := shaTestPayload(8 * 1024 * 1024)
	want := sha256.Sum256(payload)
	for _, bench := range []struct {
		name string
		copy func(io.Writer, io.Reader, int64) (int64, [sha256.Size]byte, error)
	}{
		{"sync", shaCopySync},
		{"async", CopyNWithSHA256},
	} {
		b.Run(bench.name, func(b *testing.B) {
			// Both paths copy every payload byte to real, preallocated storage.
			// Setup and post-run content verification are outside the timing.
			var dst bytes.Buffer
			dst.Grow(len(payload))
			src := bytes.NewReader(payload)
			b.SetBytes(int64(len(payload)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst.Reset()
				src.Reset(payload)
				n, digest, err := bench.copy(&dst, src, int64(len(payload)))
				if n != int64(len(payload)) || digest != want || err != nil {
					b.Fatalf("copy: %d, %x, %v", n, digest, err)
				}
			}
			b.StopTimer()
			if !bytes.Equal(dst.Bytes(), payload) {
				b.Fatal("copied payload differs")
			}
		})
	}
}
