package checksum

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"sync"
	"testing"
	"time"
)

type shaReaderFunc func([]byte) (int, error)

func (f shaReaderFunc) Read(p []byte) (int, error) { return f(p) }

type shaWriterFunc func([]byte) (int, error)

func (f shaWriterFunc) Write(p []byte) (int, error) { return f(p) }

func shaTestPayload(n int) []byte {
	p := make([]byte, n)
	for i := range p {
		p[i] = byte(i*37 + i/251 + i/copySHA256BufferSize)
	}
	return p
}

func TestCopyNWithSHA256FramingAndReuse(t *testing.T) {
	for _, size := range []int{0, 1, copySHA256BufferSize - 1, copySHA256BufferSize, copySHA256BufferSize + 1, copySHA256Buffers * copySHA256BufferSize, 8*1024*1024 + 17} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			payload := shaTestPayload(size)
			const sentinel = "protocol next frame"
			src := bytes.NewReader(append(payload[:size:size], []byte(sentinel)...))
			var dst bytes.Buffer
			// Record buffer identity without keeping payload copies alive.
			seen := make(map[*byte]bool)
			writer := shaWriterFunc(func(p []byte) (int, error) {
				seen[&p[0]] = true
				return dst.Write(p)
			})
			n, digest, err := CopyNWithSHA256(writer, src, int64(size))
			if err != nil || n != int64(size) || digest != sha256.Sum256(payload) || !bytes.Equal(dst.Bytes(), payload) {
				t.Fatalf("copy: written=%d err=%v digest=%x", n, err, digest)
			}
			if len(seen) > copySHA256Buffers {
				t.Fatalf("used %d buffers, want at most %d", len(seen), copySHA256Buffers)
			}
			if size > copySHA256Buffers*copySHA256BufferSize && len(seen) != copySHA256Buffers {
				t.Fatalf("large copy used %d buffers", len(seen))
			}
			remaining, err := io.ReadAll(src)
			if err != nil || string(remaining) != sentinel {
				t.Fatalf("trailing frame: %q, %v", remaining, err)
			}
		})
	}
}

func TestValidateBufferSize(t *testing.T) {
	for _, bufferSize := range []int{-1, 0, MinBufferSize - 1, MinBufferSize, MinBufferSize + 1, DefaultBufferSize, 256 * 1024, 1024 * 1024, MaxBufferSize - 1, MaxBufferSize, MaxBufferSize + 1, int(^uint(0) >> 1)} {
		t.Run(fmt.Sprint(bufferSize), func(t *testing.T) {
			wantValid := bufferSize >= MinBufferSize && bufferSize <= MaxBufferSize
			if err := ValidateBufferSize(bufferSize); (err == nil) != wantValid {
				t.Fatalf("ValidateBufferSize(%d) = %v, want valid=%v", bufferSize, err, wantValid)
			}
		})
	}
}

func TestCopyNWithSHA256BufferValidation(t *testing.T) {
	reader := shaReaderFunc(func([]byte) (int, error) { panic("unexpected read") })
	writer := shaWriterFunc(func([]byte) (int, error) { panic("unexpected write") })
	for _, bufferSize := range []int{-1, MinBufferSize - 1, MaxBufferSize + 1, int(^uint(0) >> 1)} {
		for _, size := range []int64{-1, 0, 1} {
			t.Run(fmt.Sprintf("buffer=%d/size=%d", bufferSize, size), func(t *testing.T) {
				n, digest, err := CopyNWithSHA256Buffer(writer, reader, size, bufferSize)
				if n != 0 || digest != sha256.Sum256(nil) || err != ValidateBufferSize(bufferSize) {
					t.Fatalf("invalid buffer: %d, %x, %v", n, digest, err)
				}
				if allocs := testing.AllocsPerRun(100, func() {
					CopyNWithSHA256Buffer(writer, reader, size, bufferSize)
				}); allocs != 0 {
					t.Fatalf("invalid buffer allocations = %v, want 0", allocs)
				}
			})
		}
	}
	for _, bufferSize := range []int{0, MinBufferSize, DefaultBufferSize, 256 * 1024, 1024 * 1024, MaxBufferSize} {
		for _, size := range []int64{-1, 0} {
			n, digest, err := CopyNWithSHA256Buffer(writer, reader, size, bufferSize)
			if n != 0 || digest != sha256.Sum256(nil) || (err != nil) != (size < 0) {
				t.Fatalf("buffer=%d size=%d: %d, %x, %v", bufferSize, size, n, digest, err)
			}
			if allocs := testing.AllocsPerRun(100, func() {
				CopyNWithSHA256Buffer(writer, reader, size, bufferSize)
			}); allocs != 0 {
				t.Fatalf("nonpositive size allocations = %v, want 0", allocs)
			}
		}
	}
}

func TestCopyNWithSHA256BufferFramingAndReuse(t *testing.T) {
	for _, configuredSize := range []int{0, MinBufferSize, DefaultBufferSize, 256 * 1024, 1024 * 1024, MaxBufferSize} {
		bufferSize := configuredSize
		if bufferSize == 0 {
			bufferSize = DefaultBufferSize
		}
		for _, size := range []int{0, 1, bufferSize - 1, bufferSize, bufferSize + 1, 4*bufferSize - 1, 4 * bufferSize, 4*bufferSize + 1, 8*bufferSize + 17} {
			t.Run(fmt.Sprintf("buffer=%d/size=%d", configuredSize, size), func(t *testing.T) {
				payload := shaTestPayload(size)
				const sentinel = "protocol next frame"
				src := bytes.NewReader(append(payload[:size:size], []byte(sentinel)...))
				var dst bytes.Buffer
				seen := make(map[*byte]bool)
				reader := shaReaderFunc(func(p []byte) (int, error) {
					want := min(bufferSize, size-dst.Len())
					if len(p) != want || cap(p) != bufferSize {
						t.Fatalf("read buffer len=%d cap=%d, want len=%d cap=%d", len(p), cap(p), want, bufferSize)
					}
					return src.Read(p)
				})
				writer := shaWriterFunc(func(p []byte) (int, error) {
					seen[&p[0]] = true
					return dst.Write(p)
				})
				n, digest, err := CopyNWithSHA256Buffer(writer, reader, int64(size), configuredSize)
				if err != nil || n != int64(size) || digest != sha256.Sum256(payload) || !bytes.Equal(dst.Bytes(), payload) {
					t.Fatalf("copy: written=%d err=%v digest=%x", n, err, digest)
				}
				wantBuffers := min(copySHA256Buffers, (size+bufferSize-1)/bufferSize)
				if len(seen) != wantBuffers {
					t.Fatalf("used %d buffers, want %d", len(seen), wantBuffers)
				}
				remaining, err := io.ReadAll(src)
				if err != nil || string(remaining) != sentinel {
					t.Fatalf("trailing frame: %q, %v", remaining, err)
				}
			})
		}
	}
}

func TestCopyNWithSHA256BufferRestoresShortReads(t *testing.T) {
	for _, bufferSize := range []int{MinBufferSize, DefaultBufferSize, 256 * 1024, 1024 * 1024, MaxBufferSize} {
		t.Run(fmt.Sprint(bufferSize), func(t *testing.T) {
			payload := shaTestPayload(6*bufferSize + 17)
			src := bytes.NewReader(payload)
			calls := 0
			reader := shaReaderFunc(func(p []byte) (int, error) {
				if len(p) != min(bufferSize, src.Len()) {
					t.Fatalf("buffer not restored: len=%d remaining=%d", len(p), src.Len())
				}
				calls++
				if calls <= copySHA256Buffers {
					return src.Read(p[:7])
				}
				return src.Read(p)
			})
			n, digest, err := CopyNWithSHA256Buffer(io.Discard, reader, int64(len(payload)), bufferSize)
			if err != nil || n != int64(len(payload)) || digest != sha256.Sum256(payload) {
				t.Fatalf("copy: written=%d err=%v digest=%x", n, err, digest)
			}
		})
	}
}

func TestCopyNWithSHA256EmptyAndNegative(t *testing.T) {
	reader := shaReaderFunc(func([]byte) (int, error) { panic("unexpected read") })
	writer := shaWriterFunc(func([]byte) (int, error) { panic("unexpected write") })
	for _, size := range []int64{-1, 0} {
		n, digest, err := CopyNWithSHA256(writer, reader, size)
		if n != 0 || digest != sha256.Sum256(nil) || (err != nil) != (size < 0) {
			t.Fatalf("size %d: %d, %x, %v", size, n, digest, err)
		}
	}
	if allocs := testing.AllocsPerRun(100, func() {
		CopyNWithSHA256(writer, reader, 0)
	}); allocs != 0 {
		t.Fatalf("zero-size allocations = %v, want 0", allocs)
	}
}

func TestCopyNWithSHA256Errors(t *testing.T) {
	t.Run("default API", func(t *testing.T) { testCopyNWithSHA256Errors(t, CopyNWithSHA256) })
	for _, bufferSize := range []int{0, MinBufferSize, DefaultBufferSize, 256 * 1024, 1024 * 1024, MaxBufferSize} {
		t.Run(fmt.Sprint(bufferSize), func(t *testing.T) {
			testCopyNWithSHA256Errors(t, func(dst io.Writer, src io.Reader, size int64) (int64, [sha256.Size]byte, error) {
				return CopyNWithSHA256Buffer(dst, src, size, bufferSize)
			})
		})
	}
}

func testCopyNWithSHA256Errors(t *testing.T, copyN func(io.Writer, io.Reader, int64) (int64, [sha256.Size]byte, error)) {
	t.Helper()
	readFailure := errors.New("read failure")
	writeFailure := errors.New("write failure")
	for _, tc := range []struct {
		name     string
		data     string
		size     int64
		readErr  error
		writeN   int // -1 means accept all
		writeErr error
		want     string
		wantErr  error
	}{
		{"empty EOF", "", 5, io.EOF, -1, nil, "", io.EOF},
		{"partial EOF", "abc", 5, io.EOF, -1, nil, "abc", io.EOF},
		{"final EOF", "abc", 3, io.EOF, -1, nil, "abc", nil},
		{"empty read error", "", 5, readFailure, -1, nil, "", readFailure},
		{"partial read error", "abc", 5, readFailure, -1, nil, "abc", readFailure},
		{"final read error", "abc", 3, readFailure, -1, nil, "abc", readFailure},
		{"short write", "abc", 3, nil, 2, nil, "ab", io.ErrShortWrite},
		{"zero write", "abc", 3, nil, 0, nil, "", io.ErrShortWrite},
		{"partial write error", "abc", 3, nil, 2, writeFailure, "ab", writeFailure},
		{"full write error", "abc", 3, nil, -1, writeFailure, "abc", writeFailure},
		{"zero write error", "abc", 3, nil, 0, writeFailure, "", writeFailure},
		{"write precedence", "abc", 5, readFailure, 1, writeFailure, "a", writeFailure},
		{"short write precedence", "abc", 5, readFailure, 1, nil, "a", io.ErrShortWrite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			src := shaReaderFunc(func(p []byte) (int, error) {
				calls++
				if calls > 1 {
					t.Fatal("unexpected subsequent read")
				}
				return copy(p, tc.data), tc.readErr
			})
			var accepted bytes.Buffer
			dst := shaWriterFunc(func(p []byte) (int, error) {
				n := len(p)
				if tc.writeN >= 0 {
					n = tc.writeN
				}
				accepted.Write(p[:n])
				return n, tc.writeErr
			})
			n, digest, err := copyN(dst, src, tc.size)
			if n != int64(len(tc.want)) || digest != sha256.Sum256([]byte(tc.want)) || !errors.Is(err, tc.wantErr) || accepted.String() != tc.want {
				t.Fatalf("got %d, %x, %v, payload %q; want %q, %v", n, digest, err, accepted.String(), tc.want, tc.wantErr)
			}
		})
	}
}

func TestCopyNWithSHA256NoProgress(t *testing.T) {
	calls := 0
	src := shaReaderFunc(func(p []byte) (int, error) {
		calls++
		if calls == copySHA256MaxEmpty {
			p[0] = 'x'
			return 1, nil // Progress resets the consecutive-empty count.
		}
		return 0, nil
	})
	var dst bytes.Buffer
	n, digest, err := CopyNWithSHA256(&dst, src, 2)
	if n != 1 || digest != sha256.Sum256([]byte("x")) || err != io.ErrNoProgress || calls != 2*copySHA256MaxEmpty {
		t.Fatalf("got %d, %x, %v after %d reads", n, digest, err, calls)
	}
}

type shaGatedHash struct {
	hash.Hash
	started chan struct{}
	release chan struct{}
	summed  chan struct{}
	once    sync.Once
}

func (h *shaGatedHash) Write(p []byte) (int, error) {
	h.once.Do(func() {
		close(h.started)
		<-h.release
	})
	return h.Hash.Write(p)
}

func (h *shaGatedHash) Sum(p []byte) []byte {
	result := h.Hash.Sum(p)
	close(h.summed)
	return result
}

type shaCopyResult struct {
	n      int64
	digest [sha256.Size]byte
	err    error
}

func shaAwait[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for copy event")
		var zero T
		return zero
	}
}

func TestCopyNWithSHA256OverlapAndBackpressure(t *testing.T) {
	for _, bufferSize := range []int{MinBufferSize, DefaultBufferSize, 256 * 1024, 1024 * 1024, MaxBufferSize} {
		t.Run(fmt.Sprint(bufferSize), func(t *testing.T) {
			testCopyNWithSHA256OverlapAndBackpressure(t, bufferSize)
		})
	}
}

func testCopyNWithSHA256OverlapAndBackpressure(t *testing.T, bufferSize int) {
	t.Helper()
	payload := shaTestPayload(12*bufferSize + 7)
	h := &shaGatedHash{Hash: sha256.New(), started: make(chan struct{}), release: make(chan struct{}), summed: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(h.release) })
	writes := make(chan *byte, 16)
	var dst bytes.Buffer
	writer := shaWriterFunc(func(p []byte) (int, error) {
		n, err := dst.Write(p)
		writes <- &p[0]
		return n, err
	})
	result := make(chan shaCopyResult, 1)
	go func() {
		n, digest, err := copyNWithSHA256Buffer(writer, bytes.NewReader(payload), int64(len(payload)), bufferSize, func() hash.Hash { return h })
		result <- shaCopyResult{n, digest, err}
	}()
	shaAwait(t, h.started)
	seen := make(map[*byte]bool)
	for i := 0; i < copySHA256Buffers; i++ {
		address := shaAwait(t, writes)
		if seen[address] {
			t.Fatal("reused a buffer while hash worker was blocked")
		}
		seen[address] = true
	}
	// All four writes can finish while the first hash is blocked (overlap),
	// but no fifth buffer is available until hashing returns one (backpressure).
	select {
	case <-writes:
		t.Fatal("write exceeded the bounded ring while hash was blocked")
	case <-result:
		t.Fatal("returned before hash worker drained")
	case <-time.After(30 * time.Millisecond):
	}
	release.Do(func() { close(h.release) })
	got := shaAwait(t, result)
	if got.n != int64(len(payload)) || got.err != nil || got.digest != sha256.Sum256(payload) || !bytes.Equal(dst.Bytes(), payload) {
		t.Fatalf("copy result: %+v", got)
	}
	select {
	case <-h.summed:
	default:
		t.Fatal("returned before final hash")
	}
}

func TestCopyNWithSHA256DrainsOnErrors(t *testing.T) {
	for _, bufferSize := range []int{MinBufferSize, DefaultBufferSize, 256 * 1024, 1024 * 1024, MaxBufferSize} {
		t.Run(fmt.Sprint(bufferSize), func(t *testing.T) {
			testCopyNWithSHA256DrainsOnErrors(t, bufferSize)
		})
	}
}

func testCopyNWithSHA256DrainsOnErrors(t *testing.T, bufferSize int) {
	t.Helper()
	failure := errors.New("I/O failed")
	for _, mode := range []string{"read", "EOF", "write", "short", "no progress"} {
		t.Run(mode, func(t *testing.T) {
			payload := shaTestPayload(bufferSize)
			h := &shaGatedHash{Hash: sha256.New(), started: make(chan struct{}), release: make(chan struct{}), summed: make(chan struct{})}
			var release sync.Once
			defer release.Do(func() { close(h.release) })
			terminalIO := make(chan struct{})
			reads := 0
			src := shaReaderFunc(func(p []byte) (int, error) {
				reads++
				if reads == 1 {
					return copy(p, payload), nil
				}
				if mode == "no progress" {
					if reads == copySHA256MaxEmpty+1 {
						close(terminalIO)
					}
					return 0, nil
				}
				if mode == "read" || mode == "EOF" {
					close(terminalIO)
					if mode == "EOF" {
						return 0, io.EOF
					}
					return 0, failure
				}
				return copy(p, payload), nil
			})
			var dst bytes.Buffer
			writes := 0
			writer := shaWriterFunc(func(p []byte) (int, error) {
				writes++
				if writes == 2 {
					close(terminalIO)
					dst.Write(p[:7])
					if mode == "short" {
						return 7, nil
					}
					return 7, failure
				}
				return dst.Write(p)
			})
			result := make(chan shaCopyResult, 1)
			go func() {
				n, digest, err := copyNWithSHA256Buffer(writer, src, int64(3*len(payload)), bufferSize, func() hash.Hash { return h })
				result <- shaCopyResult{n, digest, err}
			}()
			shaAwait(t, h.started)
			shaAwait(t, terminalIO)
			select {
			case <-result:
				t.Fatal("error returned before hash drained")
			case <-time.After(30 * time.Millisecond):
			}
			release.Do(func() { close(h.release) })
			got := shaAwait(t, result)
			wantErr := failure
			switch mode {
			case "EOF":
				wantErr = io.EOF
			case "short":
				wantErr = io.ErrShortWrite
			case "no progress":
				wantErr = io.ErrNoProgress
			}
			if got.err != wantErr || got.n != int64(dst.Len()) || got.digest != sha256.Sum256(dst.Bytes()) {
				t.Fatalf("copy result: %+v, accepted %d", got, dst.Len())
			}
			select {
			case <-h.summed:
			default:
				t.Fatal("worker did not finalize digest")
			}
		})
	}
}
