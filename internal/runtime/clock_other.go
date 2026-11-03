//go:build !linux

package runtime

func monotonicSeconds() (float64, error) { return 0, ErrUnsupported }
