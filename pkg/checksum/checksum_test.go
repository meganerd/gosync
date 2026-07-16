package checksum

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifierCalculateAndVerify(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "file.txt")
	content := []byte("checksum me")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}

	want := sha256.Sum256(content)
	wantHex := hex.EncodeToString(want[:])

	verifier := NewVerifier("sha256")
	got, err := verifier.Calculate(path)
	if err != nil {
		t.Fatalf("Calculate() error = %v", err)
	}
	if got != wantHex {
		t.Fatalf("Calculate() = %q, want %q", got, wantHex)
	}

	ok, err := verifier.Verify(path, wantHex)
	if err != nil || !ok {
		t.Fatalf("Verify() = %v, %v; want true, nil", ok, err)
	}

	ok, err = verifier.Verify(path, strings.Repeat("0", len(wantHex)))
	if err != nil || ok {
		t.Fatalf("Verify(wrong checksum) = %v, %v; want false, nil", ok, err)
	}
}

func TestVerifierCalculateReaderAndErrors(t *testing.T) {
	verifier := NewVerifier("unknown")
	got, err := verifier.CalculateReader(strings.NewReader("reader payload"))
	if err != nil {
		t.Fatalf("CalculateReader() error = %v", err)
	}
	want := sha256.Sum256([]byte("reader payload"))
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("CalculateReader() = %q", got)
	}

	if _, err := verifier.Calculate(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Fatal("expected Calculate() on missing file to fail")
	}
}
