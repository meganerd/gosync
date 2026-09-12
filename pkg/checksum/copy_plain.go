package checksum

import (
	"errors"
	"io"
)

// CopyNBuffer copies exactly size bytes without hashing. Zero bufferSize selects
// DefaultBufferSize; other values must pass ValidateBufferSize. Invalid sizes
// fail without I/O, and a zero copy size succeeds without allocating a buffer.
// Reads never request bytes beyond size or bufferSize. ReaderFrom and WriterTo
// are deliberately not used, so they cannot bypass the buffer limit.
//
// Premature EOF returns io.EOF; EOF accompanying the final bytes is success.
// Other read errors are preserved, including on the final read. Write errors
// take precedence, short writes return io.ErrShortWrite, and 100 consecutive
// empty reads return io.ErrNoProgress. The count is bytes accepted by dst.
func CopyNBuffer(dst io.Writer, src io.Reader, size int64, bufferSize int) (written int64, err error) {
	if bufferSize == 0 {
		bufferSize = DefaultBufferSize
	}
	if err := ValidateBufferSize(bufferSize); err != nil {
		return 0, err
	}
	if size < 0 {
		return 0, errors.New("checksum: negative copy size")
	}
	if size == 0 {
		return 0, nil
	}

	buf := make([]byte, min(int64(bufferSize), size))
	emptyReads := 0
	for written < size {
		limit := min(int64(len(buf)), size-written)
		n, readErr := src.Read(buf[:limit])
		if n < 0 || int64(n) > limit {
			return written, errors.New("checksum: invalid read count")
		}
		if n > 0 {
			emptyReads = 0
			nw, writeErr := dst.Write(buf[:n])
			if nw < 0 || nw > n {
				nw = 0
				if writeErr == nil {
					writeErr = io.ErrShortWrite
				}
			}
			written += int64(nw)
			if writeErr != nil {
				return written, writeErr
			}
			if nw != n {
				return written, io.ErrShortWrite
			}
		} else {
			emptyReads++
		}
		if readErr != nil {
			if readErr == io.EOF && written == size {
				return written, nil
			}
			return written, readErr
		}
		if emptyReads >= 100 {
			return written, io.ErrNoProgress
		}
	}
	return written, nil
}
