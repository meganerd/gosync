package checksum

// The pools cover the default configuration, which is both the common case and
// the only size for which a global bound can stay simple and predictable.
// Custom buffer sizes retain their existing allocate-per-transfer behavior.
const (
	plainPoolBuffers = 128
	shaPoolSlabs     = 32
	copyPoolMaxBytes = plainPoolBuffers*DefaultBufferSize + shaPoolSlabs*copySHA256Buffers*DefaultBufferSize
)

var (
	plainCopyPool = newCopyBufferPool(DefaultBufferSize, plainPoolBuffers)
	shaCopyPool   = newCopyBufferPool(copySHA256Buffers*DefaultBufferSize, shaPoolSlabs)
)

type copyBufferPool struct {
	size int
	free chan []byte
}

func newCopyBufferPool(size, capacity int) copyBufferPool {
	return copyBufferPool{size: size, free: make(chan []byte, capacity)}
}

func (p copyBufferPool) get() []byte {
	select {
	case buf := <-p.free:
		return buf[:p.size]
	default:
		return make([]byte, p.size)
	}
}

func (p copyBufferPool) put(buf []byte) {
	if cap(buf) != p.size {
		return
	}
	buf = buf[:p.size]
	clear(buf) // A later transfer must not observe data from an earlier one.
	select {
	case p.free <- buf:
	default: // Retention is bounded; excess buffers remain garbage collectible.
	}
}
