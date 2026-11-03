//go:build unix

package api

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func freeDisk(root *os.Root) (uint64, error) {
	f, err := root.Open(".")
	if err != nil {
		return 0, err
	}
	defer f.Close()
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &stat); err != nil {
		return 0, err
	}
	return stat.Bavail * uint64(stat.Bsize), nil
}
func ensureSpace(root *os.Root, need int64) error {
	free, err := freeDisk(root)
	if err != nil {
		return err
	}
	if need < 0 || uint64(need) > free || free-uint64(need) < 64<<20 {
		return errors.New("insufficient free disk space for staging plus the safety reserve")
	}
	return nil
}
