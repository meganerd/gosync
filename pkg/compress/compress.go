package compress

import (
	"compress/gzip"
	"io"
)

type Compressor struct {
	level int
}

func NewCompressor(level int) *Compressor {
	return &Compressor{level: level}
}

func (c *Compressor) Compress(reader io.Reader) (io.Reader, error) {
	return gzip.NewReader(reader)
}

func (c *Compressor) Decompress(reader io.Reader) (io.Reader, error) {
	return gzip.NewReader(reader)
}
