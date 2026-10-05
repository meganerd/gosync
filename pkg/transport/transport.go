// Package transport defines the interface for file transfer protocols.
package transport

import (
	"io"
	"os"
)

// Transport is the interface that all transport backends must implement.
type Transport interface {
	// Name returns the transport protocol name (e.g., "quic", "tcp", "ssh").
	Name() string

	// Connect establishes a connection to the remote host.
	Connect(host string, port int) error

	// SendFile transfers a single file from local to remote.
	SendFile(localPath, remotePath string) error

	// ReceiveFile transfers a single file from remote to local.
	ReceiveFile(remotePath, localPath string) error

	// SendStream sends a stream of data (for large files).
	SendStream(reader io.Reader, remotePath string, size int64) error

	// ReceiveStream receives a stream of data (for large files).
	ReceiveStream(remotePath string, writer io.Writer) error

	// Close terminates the connection.
	Close() error

	// IsConnected returns true if the transport is connected.
	IsConnected() bool
}

// SizedFileSender transfers a path using a size discovered independently of
// os.FileInfo.Size. Linux block devices require this because stat reports zero.
type SizedFileSender interface {
	SendSizedFile(localPath, remotePath string, size int64) error
}

// FileInfo represents metadata about a file for transfer.
type FileInfo struct {
	Path    string
	Size    int64
	Mode    os.FileMode
	ModTime int64
}

// TransferResult represents the result of a file transfer.
type TransferResult struct {
	Path     string
	Size     int64
	Duration float64
	Speed    float64 // bytes per second
	Checksum string
	Error    error
}

// Config holds transport configuration.
type Config struct {
	Host        string
	Port        int
	Username    string
	KeyFile     string
	Timeout     int
	MaxRetries  int
	Compression bool
	Checksum    bool  // verify SHA-256 with legacy commands; false negotiates NOHASH
	Bandwidth   int64 // bytes per second, 0 = unlimited
	BufferSize  int   // bytes per copy buffer for TCP/server/QUIC, 0 = default (32 KiB)
	// Connections is the number of QUIC data connections (sender sockets) used
	// per file, 1..ranged.MaxConnections; 0 and 1 both mean a single connection
	// and reproduce the single-connection wire behavior exactly. Values above 1
	// apply only to QUIC and only to files at or above ranged.MinFanoutSize.
	Connections int
	// CertificatePEM pins the first certificate's exact leaf DER for QUIC.
	// Optional trailing chain certificates are accepted; private keys are not.
	// Empty means system PKI and hostname validation. Pinning replaces hostname
	// validation but still requires current validity and server-auth usage.
	CertificatePEM []byte
}
