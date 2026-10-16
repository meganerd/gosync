package archive

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRoundTripNestedTree(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	mustWrite(t, filepath.Join(src, "a", "large.bin"), bytes.Repeat([]byte("abcdef"), 300000))
	mustWrite(t, filepath.Join(src, "zero"), nil)
	if err := os.MkdirAll(filepath.Join(src, "empty", "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	want, err := Send(context.Background(), &wire, src, Options{Policy: PolicyPortable, BufferSize: 4096})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Receive(context.Background(), &wire, dst, Options{Policy: PolicyPortable})
	if err != nil {
		t.Fatal(err)
	}
	if got.Entries != want.Entries || got.Bytes != want.Bytes {
		t.Fatalf("result mismatch: got %+v want %+v", got, want)
	}
	b, err := os.ReadFile(filepath.Join(dst, "a", "large.bin"))
	if err != nil || len(b) != 1800000 {
		t.Fatalf("large file: len=%d err=%v", len(b), err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "empty", "nested")); err != nil || !fi.IsDir() {
		t.Fatalf("empty directory missing: %v", err)
	}
}

func TestSymlinkPoliciesAndEscape(t *testing.T) {
	if err := ValidateLink("dir/link", "../../escape"); err == nil {
		t.Fatal("escaping link accepted")
	}
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "target"), []byte("ok"))
	if err := os.Symlink("target", filepath.Join(src, "link")); err != nil {
		t.Skip(err)
	}
	var portable bytes.Buffer
	res, err := Send(context.Background(), &portable, src, Options{Policy: PolicyPortable})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("warnings=%v", res.Warnings)
	}
	var best bytes.Buffer
	if _, err = Send(context.Background(), &best, src, Options{Policy: PolicyBestEffort}); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if _, err = Receive(context.Background(), &best, dst, Options{Policy: PolicyBestEffort}); err != nil {
		t.Fatal(err)
	}
	if target, err := os.Readlink(filepath.Join(dst, "link")); err != nil || target != "target" {
		t.Fatalf("link=%q err=%v", target, err)
	}
}

func TestRejectsUnsafePaths(t *testing.T) {
	for _, p := range []string{"../x", "/x", "a//b", "a\\b", "a/./b", string([]byte{'a', 0, 'b'})} {
		if ValidatePath(p) == nil {
			t.Errorf("accepted %q", p)
		}
	}
}

func TestCorruptionAndInterruptedStreamCleanup(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "file"), bytes.Repeat([]byte("x"), 100000))
	var wire bytes.Buffer
	if _, err := Send(context.Background(), &wire, src, Options{Policy: PolicyStrict, BufferSize: 4096}); err != nil {
		t.Fatal(err)
	}
	b := append([]byte(nil), wire.Bytes()...)
	b[len(b)/2] ^= 1
	if _, err := Receive(context.Background(), bytes.NewReader(b), t.TempDir(), Options{Policy: PolicyStrict}); err == nil {
		t.Fatal("corruption accepted")
	}
	b = wire.Bytes()[:wire.Len()-20]
	dst := t.TempDir()
	if _, err := Receive(context.Background(), bytes.NewReader(b), dst, Options{Policy: PolicyStrict}); err == nil {
		t.Fatal("truncation accepted")
	}
	matches, _ := filepath.Glob(filepath.Join(dst, "*.gosync-part-*"))
	if len(matches) != 0 {
		t.Fatalf("staging files left: %v", matches)
	}
}

func TestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Send(ctx, io.Discard, t.TempDir(), Options{}); err == nil {
		t.Fatal("cancel ignored")
	}
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0640); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkArchiveStream(b *testing.B) {
	src := b.TempDir()
	if err := os.WriteFile(filepath.Join(src, "data"), bytes.Repeat([]byte("z"), 8<<20), 0644); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(8 << 20)
	for i := 0; i < b.N; i++ {
		if _, err := Send(context.Background(), io.Discard, src, Options{Policy: PolicyPortable, BufferSize: 64 << 10}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPerFileCopyBaseline(b *testing.B) {
	src := b.TempDir()
	path := filepath.Join(src, "data")
	if err := os.WriteFile(path, bytes.Repeat([]byte("z"), 8<<20), 0644); err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, 64<<10)
	b.ResetTimer()
	b.ReportAllocs()
	b.SetBytes(8 << 20)
	for i := 0; i < b.N; i++ {
		f, err := os.Open(path)
		if err != nil {
			b.Fatal(err)
		}
		if _, err = io.CopyBuffer(io.Discard, f, buf); err != nil {
			f.Close()
			b.Fatal(err)
		}
		if err = f.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
