package transport

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/gbjohnso/gosync/pkg/checksum"
)

// ChecksumConfigurer configures transfer verification before Connect is called.
// Changing the mode requires reconnecting to negotiate receiver capabilities.
type ChecksumConfigurer interface{ SetChecksum(enabled bool) }

// ConnectionsConfigurer configures QUIC data connections (sender sockets) per
// file before Connect is called, because fan-out support is negotiated during
// the handshake. Transports without fan-out do not implement it.
type ConnectionsConfigurer interface {
	SetConnections(connections int) error
}

func transferCommand(command string, enabled bool) string {
	if !enabled {
		return command + "-NOHASH"
	}
	return command
}

func copyPayload(dst io.Writer, src io.Reader, size int64, config Config) ([sha256.Size]byte, error) {
	if config.Checksum {
		_, digest, err := checksum.CopyNWithSHA256Buffer(dst, src, size, config.BufferSize)
		return digest, err
	}
	_, err := checksum.CopyNBuffer(dst, src, size, config.BufferSize)
	return [sha256.Size]byte{}, err
}

func digestMatches(text string, digest [sha256.Size]byte) bool {
	decoded, err := hex.DecodeString(text)
	return err == nil && len(decoded) == sha256.Size && string(decoded) == string(digest[:])
}

func validateSendReply(line string, size int64, digest [sha256.Size]byte, enabled bool) error {
	parts := strings.Split(line, " ")
	if len(parts) != 3 || parts[0] != "OK" {
		return fmt.Errorf("invalid SEND acknowledgement: %q", line)
	}
	n, err := parseSize(parts[2])
	if err != nil || n != size {
		return fmt.Errorf("invalid SEND acknowledgement size: %q (want %d)", parts[2], size)
	}
	if enabled {
		if !digestMatches(parts[1], digest) {
			return fmt.Errorf("SEND checksum mismatch: malformed or different digest %q", parts[1])
		}
	} else if parts[1] != "NONE" {
		return fmt.Errorf("invalid SEND-NOHASH acknowledgement: %q", line)
	}
	return nil
}

func parseSize(text string) (int64, error) {
	if text == "" || strings.IndexFunc(text, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, fmt.Errorf("invalid size: %q", text)
	}
	return strconv.ParseInt(text, 10, 64)
}

func receiveSize(line string) (int64, error) {
	parts := strings.Split(line, " ")
	if len(parts) != 3 || parts[0] != "OK" || parts[1] != "SIZE" {
		return 0, fmt.Errorf("invalid RECEIVE response: %q", line)
	}
	return parseSize(parts[2])
}

func validateTrailer(line string, size int64, digest [sha256.Size]byte, enabled bool) error {
	parts := strings.Split(line, " ")
	if enabled {
		if len(parts) != 2 || parts[0] != "CHECKSUM" {
			return fmt.Errorf("invalid checksum reply: %q", line)
		}
		if !digestMatches(parts[1], digest) {
			return fmt.Errorf("RECEIVE checksum mismatch: malformed or different digest %q", parts[1])
		}
	} else {
		if len(parts) != 2 || parts[0] != "END" {
			return fmt.Errorf("invalid END reply: %q", line)
		}
		n, err := parseSize(parts[1])
		if err != nil || n != size {
			return fmt.Errorf("invalid END size: %q (want %d)", parts[1], size)
		}
	}
	return nil
}

// Bound framing reads and remove only the line terminator, not malformed whitespace.
func readProtocolLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return "", fmt.Errorf("read reply failed: %w", err)
	}
	return strings.TrimSuffix(string(line), "\n"), nil
}

func requireNoHashCaps(writer io.Writer, reader *bufio.Reader) error {
	if _, err := io.WriteString(writer, "CAPS\n"); err != nil {
		return noHashCompatibilityError(err)
	}
	line, err := readProtocolLine(reader)
	if err != nil {
		return noHashCompatibilityError(err)
	}
	if line != "OK CAPS NOHASH" {
		return noHashCompatibilityError(fmt.Errorf("unexpected capability response %q", line))
	}
	return nil
}

func noHashCompatibilityError(err error) error {
	return fmt.Errorf("receiver does not support checksum-off transfers: upgrade receiver or use -checksum only with size-first/base64 SHA-256 peers (older path-first peers require upgrading both ends): %w", err)
}
