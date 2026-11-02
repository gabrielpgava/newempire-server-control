package servicehelper

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"time"
)

type runner interface {
	run(context.Context, string, []byte, ...string) ([]byte, error)
}

type processRunner struct{}

type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > maxOutput-b.Len() {
		return 0, errors.New("command output exceeds 4 MiB")
	}
	return b.Buffer.Write(p)
}

func (processRunner) run(ctx context.Context, path string, input []byte, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "SYSTEMD_COLORS=0", "SYSTEMD_PAGER=cat"}
	cmd.Stdin = bytes.NewReader(input)
	cmd.Stderr = io.Discard
	var output boundedBuffer
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return nil, errors.New(filepath.Base(path) + " failed; inspect the privileged service journal")
	}
	return output.Bytes(), nil
}
