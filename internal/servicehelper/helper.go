package servicehelper

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
)

// RefuseRootHTTP must run before the HTTP listener is opened.
func RefuseRootHTTP() error {
	if os.Geteuid() == 0 || os.Getuid() == 0 {
		return errors.New("refusing to run the HTTP panel as root; use the unprivileged panel service")
	}
	return nil
}

func dispatch(ctx context.Context, r runner, req request, m manifest) ([]byte, error) {
	if req.op == "panel-update" {
		return r.run(ctx, systemctl, nil, "start", "--no-block", "new-empire-update.service")
	}
	unit := unitName(req.id)
	switch req.op {
	case "protect", "start", "restart":
		if err := protectRCON(ctx, r, m.Instance); err != nil {
			return nil, err
		}
		if req.op == "protect" {
			return nil, nil
		}
		return r.run(ctx, systemctl, nil, req.op, unit)
	case "stop":
		return r.run(ctx, systemctl, nil, "stop", unit)
	case "status":
		loaded, err := r.run(ctx, systemctl, nil, "show", "--property=LoadState", "--value", unit)
		if err != nil || string(loaded) != "loaded\n" {
			return nil, errors.New("registered systemd unit is not loaded")
		}
		return r.run(ctx, systemctl, nil, "show", "--property="+statusFields, unit)
	case "logs":
		return r.run(ctx, journalctl, nil, "--unit="+unit, "--lines="+strconv.Itoa(req.lines), "--output=json", "--no-pager", "--quiet")
	default:
		return nil, errors.New("unsupported runtime operation")
	}
}

// Run implements the validated, restricted runtime-helper CLI.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	req, err := parseRequest(args)
	if err != nil {
		return err
	}
	return runPlatform(ctx, req, stdin, stdout)
}

// InstallService consumes one bounded model.Instance JSON document from a root operator.
func InstallService(ctx context.Context, stdin io.Reader) error {
	return installPlatform(ctx, stdin)
}
