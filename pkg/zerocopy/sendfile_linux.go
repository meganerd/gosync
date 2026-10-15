//go:build linux

// Package zerocopy contains narrowly scoped, optional kernel-assisted copies.
package zerocopy

import (
	"io"
	"net"
	"os"
)

// SendFileN copies exactly size bytes from a regular file to a TCP socket.
// The Go network poller uses sendfile(2) for this shape. used is false when the
// operands are ineligible, allowing callers to retain a portable fallback.
func SendFileN(dst io.Writer, src *os.File, size int64) (written int64, used bool, err error) {
	if size < 0 {
		return 0, false, nil
	}
	if _, ok := dst.(*net.TCPConn); !ok {
		return 0, false, nil
	}
	info, err := src.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return 0, false, nil
	}

	written, err = io.CopyN(dst, src, size)
	return written, true, err
}
