//go:build !linux

package scanner

import "fmt"

func platformBlockDeviceSize(path string) (int64, error) {
	return 0, fmt.Errorf("block-device sources are supported only on Linux")
}
