package checksum

import "testing"

func TestCopyBufferPoolBoundAndClearing(t *testing.T) {
	const (
		size     = 64
		capacity = 3
	)
	pool := newCopyBufferPool(size, capacity)
	allocated := make([][]byte, 10)
	for i := range allocated {
		allocated[i] = pool.get()
		for j := range allocated[i] {
			allocated[i][j] = byte(i + 1)
		}
	}
	for _, buf := range allocated {
		pool.put(buf)
	}
	if got := len(pool.free); got != capacity {
		t.Fatalf("retained buffers = %d, want %d", got, capacity)
	}
	for range capacity {
		for i, value := range pool.get() {
			if value != 0 {
				t.Fatalf("reused byte %d = %d, want zero", i, value)
			}
		}
	}
}

func TestCopyBufferPoolRejectsWrongCapacity(t *testing.T) {
	pool := newCopyBufferPool(64, 1)
	pool.put(make([]byte, 32))
	pool.put(make([]byte, 64, 128))
	if got := len(pool.free); got != 0 {
		t.Fatalf("retained mismatched buffers = %d, want 0", got)
	}
}

func TestCopyPoolMemoryCap(t *testing.T) {
	const want = 8 << 20
	if copyPoolMaxBytes != want {
		t.Fatalf("maximum retained bytes = %d, want %d", copyPoolMaxBytes, want)
	}
}
