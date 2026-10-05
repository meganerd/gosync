//go:build linux

package scanner

import (
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

func platformBlockDeviceSize(path string) (int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	var size uint64
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, file.Fd(), uintptr(unix.BLKGETSIZE64), uintptr(unsafe.Pointer(&size)))
	if errno != 0 {
		return 0, errno
	}
	if size > uint64(^uint64(0)>>1) {
		return 0, unix.EOVERFLOW
	}
	return int64(size), nil
}
