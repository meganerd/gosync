//go:build !(linux || darwin || dragonfly || freebsd || netbsd || openbsd)

package server

import (
	"errors"
	"syscall"
)

// setReusePort reports that this platform has no SO_REUSEPORT. The caller logs
// the reason and falls back to a single socket on the same port, which
// measurement shows still delivers most of the fan-out gain.
func setReusePort(network, address string, conn syscall.RawConn) error {
	return errors.New("SO_REUSEPORT is not supported on this platform")
}
