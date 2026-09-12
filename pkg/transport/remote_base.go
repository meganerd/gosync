package transport

import (
	"encoding/base64"
	"path/filepath"
	"strings"
)

// RemoteBaseProvider optionally exposes the server's absolute base directory
// after Connect. An empty string means discovery was unavailable or malformed.
// This metadata is for display only and must not change transfer paths.
type RemoteBaseProvider interface {
	RemoteBase() string
}

func (t *TCPTransport) RemoteBase() string { return t.remoteBase }

func (t *ServerTransport) RemoteBase() string { return t.remoteBase }

func parseRemoteBase(pong string) string {
	fields := strings.Fields(pong)
	if len(fields) != 3 || fields[0] != "OK" || fields[1] != "PONG" {
		return ""
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(fields[2])
	if err != nil {
		return ""
	}
	base := string(decoded)
	if !filepath.IsAbs(base) || strings.ContainsRune(base, '\x00') {
		return ""
	}
	return base
}
