package compress

import (
	"bytes"
	"compress/gzip"
	"io"
	"testing"
)

func TestCompressorRoundTrip(t *testing.T) {
	original := bytes.Repeat([]byte("compressible-data-"), 64)
	compressor := NewCompressor(gzip.BestCompression)

	compressedReader, err := compressor.Compress(bytes.NewReader(original))
	if err != nil {
		t.Fatalf("Compress() error = %v", err)
	}
	compressed, err := io.ReadAll(compressedReader)
	if err != nil {
		t.Fatalf("ReadAll(compressed) error = %v", err)
	}
	if bytes.Contains(compressed, original[:16]) {
		t.Fatal("compressed stream still contains obvious plain-text payload")
	}

	decompressedReader, err := compressor.Decompress(bytes.NewReader(compressed))
	if err != nil {
		t.Fatalf("Decompress() error = %v", err)
	}
	decompressed, err := io.ReadAll(decompressedReader)
	if err != nil {
		t.Fatalf("ReadAll(decompressed) error = %v", err)
	}
	if !bytes.Equal(decompressed, original) {
		t.Fatal("round-trip mismatch")
	}
}

func TestCompressorErrors(t *testing.T) {
	if _, err := NewCompressor(999).Compress(bytes.NewReader([]byte("x"))); err == nil {
		t.Fatal("expected invalid compression level to fail")
	}
	if _, err := NewCompressor(gzip.DefaultCompression).Decompress(bytes.NewReader([]byte("not gzip"))); err == nil {
		t.Fatal("expected invalid gzip stream to fail")
	}
}
