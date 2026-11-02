//go:build !unix

package api

import (
	"errors"
	"os"
)

func freeDisk(root *os.Root) (uint64, error) {
	return 0, errors.New("disk-space preflight is unavailable on this development platform")
}
func ensureSpace(root *os.Root, need int64) error { _, err := freeDisk(root); return err }
