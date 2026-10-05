package scanner

import (
	"fmt"
	"os"
)

var getBlockDeviceSize = platformBlockDeviceSize

func sourceSize(path string, info os.FileInfo) (size int64, isDevice bool, err error) {
	mode := info.Mode()
	if mode.IsRegular() {
		return info.Size(), false, nil
	}
	if mode&os.ModeDevice != 0 && mode&os.ModeCharDevice == 0 {
		size, err := getBlockDeviceSize(path)
		if err != nil {
			return 0, true, fmt.Errorf("get block device size for %q: %w", path, err)
		}
		if size <= 0 {
			return 0, true, fmt.Errorf("get block device size for %q: invalid size %d", path, size)
		}
		return size, true, nil
	}
	return 0, false, fmt.Errorf("unsupported source type %q (%s)", path, mode.Type())
}
