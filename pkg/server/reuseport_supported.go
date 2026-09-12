//go:build linux || darwin || dragonfly || freebsd || netbsd || openbsd

package server

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// setReusePort lets several sockets share one UDP port, which is how the
// receiver presents a socket group on a single advertised port.
//
// The constant comes from golang.org/x/sys/unix, already in this module's
// dependency graph, because the standard library's syscall package does not
// export SO_REUSEPORT on Linux and its value is not the same on every Linux
// architecture.
func setReusePort(network, address string, conn syscall.RawConn) error {
	var setErr error
	if err := conn.Control(func(fd uintptr) {
		setErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); err != nil {
		return err
	}
	if setErr != nil {
		return fmt.Errorf("set SO_REUSEPORT: %w", setErr)
	}
	return nil
}
