package checksum

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

type plainReadFunc func([]byte) (int, error)

func (f plainReadFunc) Read(p []byte) (int, error) { return f(p) }

type plainWriteFunc func([]byte) (int, error)

func (f plainWriteFunc) Write(p []byte) (int, error) { return f(p) }

// Embedded fast paths panic if the copy delegates instead of respecting buffers.
type plainBoundedReader struct {
	*bytes.Reader
	maxRead int
}

func (r *plainBoundedReader) Read(p []byte) (int, error) {
	r.maxRead = max(r.maxRead, len(p))
	return r.Reader.Read(p)
}
func (r *plainBoundedReader) WriteTo(io.Writer) (int64, error) { panic("WriterTo bypass") }

type plainBoundedWriter struct {
	bytes.Buffer
	maxWrite int
}

func (w *plainBoundedWriter) Write(p []byte) (int, error) {
	w.maxWrite = max(w.maxWrite, len(p))
	return w.Buffer.Write(p)
}
func (w *plainBoundedWriter) ReadFrom(io.Reader) (int64, error) { panic("ReaderFrom bypass") }

func TestCopyNBufferBoundsAndFraming(t *testing.T) {
	for _, size := range []int{0, MinBufferSize, 256 * 1024, MaxBufferSize} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			effective := size
			if effective == 0 {
				effective = DefaultBufferSize
			}
			payload := bytes.Repeat([]byte{0, '\n', 255, 42}, effective/2+7)
			r := &plainBoundedReader{Reader: bytes.NewReader(append(append([]byte(nil), payload...), []byte("NEXT\n")...))}
			w := new(plainBoundedWriter)
			n, err := CopyNBuffer(w, r, int64(len(payload)), size)
			if err != nil || n != int64(len(payload)) || !bytes.Equal(w.Bytes(), payload) {
				t.Fatalf("copy: n=%d err=%v", n, err)
			}
			if r.maxRead != effective || w.maxWrite != effective {
				t.Fatalf("buffer bounds: read=%d write=%d want=%d", r.maxRead, w.maxWrite, effective)
			}
			rest, _ := io.ReadAll(r.Reader)
			if string(rest) != "NEXT\n" {
				t.Fatalf("overread: %q", rest)
			}
		})
	}
}

func TestCopyNBufferValidation(t *testing.T) {
	for _, size := range []int64{-1, 0, 1} {
		for _, buffer := range []int{-1, 1, MinBufferSize - 1, MaxBufferSize + 1} {
			n, err := CopyNBuffer(nil, nil, size, buffer)
			if n != 0 || !errors.Is(err, ValidateBufferSize(buffer)) {
				t.Fatalf("size=%d buffer=%d: %d, %v", size, buffer, n, err)
			}
		}
	}
	if n, err := CopyNBuffer(nil, nil, -1, 0); n != 0 || err == nil {
		t.Fatalf("negative: %d, %v", n, err)
	}
	if n, err := CopyNBuffer(nil, nil, 0, 0); n != 0 || err != nil {
		t.Fatalf("empty: %d, %v", n, err)
	}
}

func TestCopyNBufferErrors(t *testing.T) {
	readError, writeError := errors.New("read failed"), errors.New("write failed")
	for _, tc := range []struct {
		name     string
		size     int64
		readN    int
		readErr  error
		writeN   int
		writeErr error
		wantN    int64
		wantErr  error
	}{
		{"final EOF", 3, 3, io.EOF, 3, nil, 3, nil},
		{"early EOF", 4, 3, io.EOF, 3, nil, 3, io.EOF},
		{"empty EOF", 3, 0, io.EOF, 0, nil, 0, io.EOF},
		{"final read error", 3, 3, readError, 3, nil, 3, readError},
		{"short write", 3, 3, nil, 2, nil, 2, io.ErrShortWrite},
		{"zero write", 3, 3, nil, 0, nil, 0, io.ErrShortWrite},
		{"write error wins", 3, 3, readError, 2, writeError, 2, writeError},
		{"full write error", 3, 3, nil, 3, writeError, 3, writeError},
		{"negative write", 3, 3, nil, -1, nil, 0, io.ErrShortWrite},
		{"oversized write", 3, 3, nil, 4, nil, 0, io.ErrShortWrite},
		{"invalid write error", 3, 3, nil, 4, writeError, 0, writeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := plainReadFunc(func(p []byte) (int, error) { copy(p, "abc"); return tc.readN, tc.readErr })
			w := plainWriteFunc(func([]byte) (int, error) { return tc.writeN, tc.writeErr })
			n, err := CopyNBuffer(w, r, tc.size, 0)
			if n != tc.wantN || !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %d, %v; want %d, %v", n, err, tc.wantN, tc.wantErr)
			}
		})
	}
	for _, count := range []int{-1, 4} {
		n, err := CopyNBuffer(io.Discard, plainReadFunc(func([]byte) (int, error) { return count, nil }), 3, 0)
		if n != 0 || err == nil {
			t.Fatalf("invalid read count: %d, %v", n, err)
		}
	}
}

func TestCopyNBufferNoProgress(t *testing.T) {
	calls := 0
	r := plainReadFunc(func([]byte) (int, error) { calls++; return 0, nil })
	if n, err := CopyNBuffer(io.Discard, r, 1, 0); n != 0 || err != io.ErrNoProgress || calls != 100 {
		t.Fatalf("no progress: n=%d err=%v calls=%d", n, err, calls)
	}
	calls = 0
	r = plainReadFunc(func(p []byte) (int, error) {
		calls++
		if calls%100 == 0 {
			p[0] = 'x'
			return 1, nil
		}
		return 0, nil
	})
	if n, err := CopyNBuffer(io.Discard, r, 2, 0); n != 2 || err != nil || calls != 200 {
		t.Fatalf("reset progress: n=%d err=%v calls=%d", n, err, calls)
	}
}
