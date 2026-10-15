//go:build !linux

package zerocopy

import (
	"io"
	"os"
)

// SendFileN reports ineligibility on non-Linux platforms.
func SendFileN(io.Writer, *os.File, int64) (written int64, used bool, err error) {
	return 0, false, nil
}
