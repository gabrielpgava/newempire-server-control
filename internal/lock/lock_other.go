//go:build !unix

package lock

import "errors"

func Acquire(path string) (func(), error) {
	return nil, errors.New("exclusive application locking is not implemented on this development platform")
}
