//go:build !linux

package servicehelper

import (
	"context"
	"errors"
	"io"
)

func runPlatform(context.Context, request, io.Reader, io.Writer) error {
	return errors.New("native runtime helper requires Linux and systemd")
}

func installPlatform(context.Context, io.Reader) error {
	return errors.New("native service registration requires Linux and systemd")
}
