package checksum

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"os"
)

type Verifier struct {
	algorithm string
}

func NewVerifier(algorithm string) *Verifier {
	return &Verifier{algorithm: algorithm}
}

func (v *Verifier) Calculate(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	var hasher hash.Hash
	switch v.algorithm {
	case "sha256":
		hasher = sha256.New()
	default:
		hasher = sha256.New()
	}

	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func (v *Verifier) Verify(filePath, expectedChecksum string) (bool, error) {
	actual, err := v.Calculate(filePath)
	if err != nil {
		return false, err
	}
	return actual == expectedChecksum, nil
}

func (v *Verifier) CalculateReader(reader io.Reader) (string, error) {
	var hasher hash.Hash
	switch v.algorithm {
	case "sha256":
		hasher = sha256.New()
	default:
		hasher = sha256.New()
	}

	if _, err := io.Copy(hasher, reader); err != nil {
		return "", err
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}
