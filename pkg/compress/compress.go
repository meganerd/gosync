package compress

import (
	"bytes"
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
	var buf bytes.Buffer

	writer, err := gzip.NewWriterLevel(&buf, c.level)
	if err != nil {
		return nil, err
	}

	if _, err := io.Copy(writer, reader); err != nil {
		writer.Close()
		return nil, err
	}

	if err := writer.Close(); err != nil {
		return nil, err
	}

	return bytes.NewReader(buf.Bytes()), nil
}

func (c *Compressor) Decompress(reader io.Reader) (io.Reader, error) {
	gz, err := gzip.NewReader(reader)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	data, err := io.ReadAll(gz)
	if err != nil {
		return nil, err
	}

	return bytes.NewReader(data), nil
}
