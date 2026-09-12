package checksum

import (
	"crypto/sha256"
	"errors"
	"hash"
	"io"
)

const (
	// DefaultBufferSize is the default size of each of the four copy buffers.
	DefaultBufferSize = 32 * 1024
	// MinBufferSize is the smallest supported copy buffer size in bytes.
	MinBufferSize = 4 * 1024
	// MaxBufferSize is the largest supported copy buffer size in bytes.
	MaxBufferSize = 4 * 1024 * 1024

	copySHA256BufferSize = DefaultBufferSize
	copySHA256Buffers    = 4
	copySHA256MaxEmpty   = 100
)

var (
	errCopySHA256NegativeSize = errors.New("checksum: negative copy size")
	errCopySHA256BufferSize   = errors.New("checksum: buffer size must be between 4096 and 4194304 bytes")
)

// ValidateBufferSize checks that bufferSize is in [MinBufferSize, MaxBufferSize].
// Zero is not a valid explicit buffer size.
func ValidateBufferSize(bufferSize int) error {
	if bufferSize < MinBufferSize || bufferSize > MaxBufferSize {
		return errCopySHA256BufferSize
	}
	return nil
}

// CopyNWithSHA256 copies exactly size bytes from src to dst and hashes the bytes
// accepted by dst. Reads and writes run synchronously in the caller; SHA-256 runs
// in one ordered worker using a fixed 128 KiB buffer ring with backpressure.
// No read requests bytes beyond size, so trailing protocol data is left unread.
//
// On every return, digest covers written bytes and the worker has drained and
// exited. Premature EOF returns io.EOF (as in io.CopyN); EOF accompanying the last
// requested bytes is success. Other read errors are preserved, including on the
// final read. Write errors take precedence over read errors; a short write with
// no error returns io.ErrShortWrite. One hundred consecutive empty, nil-error
// reads return io.ErrNoProgress. Negative sizes fail without I/O; zero sizes
// succeed without I/O or a worker. Both return the canonical empty digest.
func CopyNWithSHA256(dst io.Writer, src io.Reader, size int64) (written int64, digest [sha256.Size]byte, err error) {
	return CopyNWithSHA256Buffer(dst, src, size, DefaultBufferSize)
}

// CopyNWithSHA256Buffer is CopyNWithSHA256 with a configurable size for each of
// its four reusable buffers. Zero selects DefaultBufferSize, allowing zero-valued
// library configurations. Other sizes must pass ValidateBufferSize.
// Invalid buffer sizes return the empty digest without I/O, buffer allocation,
// or a worker, even when size is nonpositive.
func CopyNWithSHA256Buffer(dst io.Writer, src io.Reader, size int64, bufferSize int) (written int64, digest [sha256.Size]byte, err error) {
	if bufferSize == 0 {
		bufferSize = DefaultBufferSize
	}
	if err = ValidateBufferSize(bufferSize); err != nil {
		return 0, sha256.Sum256(nil), err
	}
	if size <= 0 {
		digest = sha256.Sum256(nil)
		if size < 0 {
			err = errCopySHA256NegativeSize
		}
		return
	}
	return copyNWithSHA256Buffer(dst, src, size, bufferSize, sha256.New)
}

// The factory is internal so tests can gate hash work without scheduler timing.
// The public wrapper handles validation and nonpositive sizes before this path.
func copyNWithSHA256(dst io.Writer, src io.Reader, size int64, newHash func() hash.Hash) (written int64, digest [sha256.Size]byte, err error) {
	return copyNWithSHA256Buffer(dst, src, size, DefaultBufferSize, newHash)
}

func copyNWithSHA256Buffer(dst io.Writer, src io.Reader, size int64, bufferSize int, newHash func() hash.Hash) (written int64, digest [sha256.Size]byte, err error) {
	buffers := make([]byte, copySHA256Buffers*bufferSize)
	free := make(chan []byte, copySHA256Buffers)
	pending := make(chan []byte, copySHA256Buffers)
	done := make(chan struct{})
	for i := 0; i < copySHA256Buffers; i++ {
		start, end := i*bufferSize, (i+1)*bufferSize
		free <- buffers[start:end:end]
	}
	go func() {
		defer close(done)
		h := newHash()
		for payload := range pending {
			// SHA-256 Write always consumes all bytes and cannot fail.
			_, _ = h.Write(payload)
			free <- payload[:bufferSize]
		}
		h.Sum(digest[:0])
	}()
	defer func() {
		close(pending)
		<-done
	}()

	emptyReads := 0
	for written < size {
		buf := <-free
		limit := min(int64(len(buf)), size-written)
		n, readErr := src.Read(buf[:limit])
		if n > 0 {
			emptyReads = 0
			nw, writeErr := dst.Write(buf[:n])
			if nw < 0 || nw > n {
				// Invalid Writer counts must not escape as an out-of-bounds slice.
				nw = 0
				if writeErr == nil {
					writeErr = io.ErrShortWrite
				}
			}
			written += int64(nw)
			if nw > 0 {
				pending <- buf[:nw]
			} else {
				free <- buf
			}
			if writeErr != nil {
				return written, digest, writeErr
			}
			if nw != n {
				return written, digest, io.ErrShortWrite
			}
		} else {
			free <- buf
			emptyReads++
		}
		if readErr != nil {
			if readErr == io.EOF && written == size {
				return written, digest, nil
			}
			return written, digest, readErr
		}
		if emptyReads >= copySHA256MaxEmpty {
			return written, digest, io.ErrNoProgress
		}
	}
	return written, digest, nil
}
