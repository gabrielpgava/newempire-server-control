//go:build linux

package servicehelper

import (
	"context"
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func proveStopped(ctx context.Context, r runner, id string, uid uint32, guard bool) error {
	pid, err := r.run(ctx, systemctl, nil, "show", "--property=MainPID", "--value", unitName(id))
	if err != nil || string(pid) != "0\n" {
		return errors.New("permission preparation requires no running Minecraft process")
	}
	if !guard {
		state, err := r.run(ctx, systemctl, nil, "show", "--property=ActiveState", "--value", unitName(id))
		if err != nil || string(state) != "inactive\n" && string(state) != "failed\n" {
			return errors.New("operation requires an inactive or failed instance")
		}
	}
	return noInstanceProcesses(uid)
}

func noInstanceProcesses(uid uint32) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return errors.New("cannot establish whether the instance user has remaining processes")
	}
	for _, entry := range entries {
		if _, err := strconv.ParseUint(entry.Name(), 10, 32); err != nil {
			continue
		}
		var st unix.Stat_t
		if err := unix.Stat("/proc/"+entry.Name(), &st); err != nil {
			if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ESRCH) {
				continue
			}
			return errors.New("cannot inspect remaining instance processes")
		}
		if st.Uid == uid {
			return errors.New("instance user still owns running processes; permissions were not changed")
		}
	}
	return nil
}

func prepareRegistered(ctx context.Context, p policy, m manifest, guard bool) (manifest, error) {
	lock, err := lockRegistry()
	if err != nil {
		return m, err
	}
	defer lock.Close()
	m, err = loadManifest(p, m.Instance.ID)
	if err != nil {
		return m, err
	}
	if err := proveStopped(ctx, processRunner{}, m.Instance.ID, m.UID, guard); err != nil {
		return m, err
	}
	m, err = prepareInstance(ctx, p, m)
	if err != nil {
		return m, err
	}
	if !m.Instance.Adopted {
		if err := savePreparedIdentity(m); err != nil {
			return m, err
		}
	}
	if guard {
		if err := verifyLoadedCommand(ctx, processRunner{}, m); err != nil {
			return m, err
		}
		if err := verifyData(m); err != nil {
			return m, err
		}
		if err := verifyProperties(m); err != nil {
			return m, err
		}
	}
	return m, nil
}
