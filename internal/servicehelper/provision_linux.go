//go:build linux

package servicehelper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/syslog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"golang.org/x/sys/unix"
)

func canAccess(mode, uid, gid uint32, a account, want uint32) bool {
	bits := mode & 7
	if uid == a.uid {
		bits = (mode >> 6) & 7
	} else if a.groups[gid] {
		bits = (mode >> 3) & 7
	}
	return bits&want == want
}

func checkTree(ctx context.Context, path string, panel, mc account) error {
	r, err := dataDir(path)
	if err != nil {
		return err
	}
	defer r.Close()
	return walkDataTree(ctx, r, func(f *os.File, st *unix.Stat_t) error {
		kind := st.Mode & unix.S_IFMT
		if kind != unix.S_IFDIR && kind != unix.S_IFREG || kind == unix.S_IFREG && st.Nlink != 1 {
			return errors.New("adopted tree contains links or special files; ownership was not changed")
		}
		want := uint32(6)
		if kind == unix.S_IFDIR {
			want = 7
		}
		acl, err := readAccessACL(f)
		if err != nil {
			return err
		}
		if !canAccessACL(st, acl, panel, want) || !canAccessACL(st, acl, mc, want) {
			return errors.New("instance permissions do not grant panel and dedicated Minecraft user read/write access; prepare permissions explicitly")
		}
		return nil
	})
}

func ensureAccount(ctx context.Context, r runner, p policy, id string) (account, account, error) {
	panel, err := lookupAccount(p.PanelUser)
	if err != nil {
		return account{}, account{}, err
	}
	name := accountName(id)
	if _, err := user.LookupGroup(name); err != nil {
		var unknown user.UnknownGroupError
		if !errors.As(err, &unknown) {
			return account{}, account{}, err
		}
		if err := checkExecutable("/usr/sbin/groupadd"); err != nil {
			return account{}, account{}, err
		}
		if _, err := r.run(ctx, "/usr/sbin/groupadd", nil, "--system", name); err != nil {
			return account{}, account{}, err
		}
	}
	if _, err := user.Lookup(name); err != nil {
		var unknown user.UnknownUserError
		if !errors.As(err, &unknown) {
			return account{}, account{}, err
		}
		if err := checkExecutable("/usr/sbin/useradd"); err != nil {
			return account{}, account{}, err
		}
		if _, err := r.run(ctx, "/usr/sbin/useradd", nil, "--system", "--gid", name, "--no-create-home", "--home-dir", "/nonexistent", "--shell", "/usr/sbin/nologin", name); err != nil {
			return account{}, account{}, err
		}
	}
	mc, err := lookupAccount(name)
	if err != nil {
		return account{}, account{}, err
	}
	u, err := user.Lookup(name)
	if err != nil || u.HomeDir != "/nonexistent" {
		return account{}, account{}, errors.New("existing instance account is not a dedicated service account")
	}
	group, err := user.LookupGroup(name)
	if err != nil || group.Gid != strconv.FormatUint(uint64(mc.gid), 10) || mc.uid == panel.uid || len(mc.groups) != 1 {
		return account{}, account{}, errors.New("dedicated instance account has unexpected identity or supplementary groups")
	}
	if !panel.groups[mc.gid] {
		if err := checkExecutable("/usr/sbin/usermod"); err != nil {
			return account{}, account{}, err
		}
		if _, err := r.run(ctx, "/usr/sbin/usermod", nil, "--append", "--groups", name, p.PanelUser); err != nil {
			return account{}, account{}, err
		}
		panel, err = lookupAccount(p.PanelUser)
		if err != nil {
			return account{}, account{}, err
		}
	}
	if !panel.groups[mc.gid] {
		return account{}, account{}, errors.New("panel group membership did not update; check the host account provider")
	}
	return panel, mc, nil
}

func checkConflicts(i model.Instance, who *account) error {
	dir, err := trustedDir(manifestDir)
	if err != nil {
		return err
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".prepared-") && model.ValidID(strings.TrimPrefix(entry.Name(), ".prepared-")) {
			continue
		}
		if entry.IsDir() {
			return errors.New("unexpected directory in instance manifest store")
		}
		b, err := readTrusted(filepath.Join(manifestDir, entry.Name()))
		if err != nil {
			return err
		}
		var existing manifest
		if err := decode(bytes.NewReader(b), &existing); err != nil {
			return err
		}
		other := existing.Instance
		if !model.ValidID(other.ID) || entry.Name() != other.ID+".json" {
			return errors.New("invalid entry in instance manifest store")
		}
		if other.ID == i.ID {
			if who == nil {
				old, _ := json.Marshal(other)
				next, _ := json.Marshal(i)
				if !bytes.Equal(old, next) {
					return errors.New("instance already registered differently; stop and review it offline")
				}
			}
			continue
		}
		if who != nil && (existing.UID == who.uid || existing.GID == who.gid) {
			return errors.New("instance account/group identity conflicts with a registered instance")
		}
		if accountName(other.ID) == accountName(i.ID) {
			return errors.New("instance account prefix collision; choose a new instance ID")
		}
		if within(i.Root, other.Root) || within(other.Root, i.Root) || i.Port == other.Port || i.Port == other.RCONPort || i.RCONPort == other.Port || i.RCONPort == other.RCONPort {
			return errors.New("instance data root or TCP ports conflict with a registered instance")
		}
	}
	return nil
}

func installPlatform(ctx context.Context, stdin io.Reader) (result error) {
	if err := independentRoot(); err != nil {
		return err
	}
	return provisionPlatform(ctx, stdin, request{op: "provision"})
}

func provisionPlatform(ctx context.Context, stdin io.Reader, req request) (result error) {
	if err := rootOnly(); err != nil {
		return err
	}
	p, err := loadPolicy()
	if err != nil {
		return err
	}
	var instance model.Instance
	if err := decode(stdin, &instance); err != nil {
		return err
	}
	if err := validateInstance(p, instance); err != nil {
		return err
	}
	if req.op == "reconfigure" && instance.ID != req.id {
		return errors.New("reconfiguration ID differs from its registered instance")
	}
	if err := checkExecutable(instance.JavaPath); err != nil {
		return err
	}
	if err := checkExecutable(controlPath); err != nil {
		return err
	}
	if err := checkExecutable(systemctl); err != nil {
		return err
	}
	for _, root := range append([]string{p.ManagedRoot}, p.AdoptRoots...) {
		f, err := trustedDir(root)
		if err != nil {
			return err
		}
		f.Close()
	}
	lock, err := lockRegistry()
	if err != nil {
		return err
	}
	defer lock.Close()
	if req.op != "reconfigure" {
		if err := checkConflicts(instance, nil); err != nil {
			return err
		}
	} else {
		if err := checkReconfiguration(ctx, processRunner{}, p, instance); err != nil {
			return err
		}
	}
	journal, err := syslog.New(syslog.LOG_AUTHPRIV|syslog.LOG_NOTICE, "new-empire-runtime")
	if err != nil {
		return errors.New("privileged audit socket unavailable; refusing registration")
	}
	defer journal.Close()
	if err := journal.Notice("requested registration " + instance.ID); err != nil {
		return err
	}
	defer func() {
		outcome := "succeeded"
		if result != nil {
			outcome = "failed " + safeDiagnostic(result)
		}
		_ = journal.Notice(outcome + " registration " + instance.ID)
	}()
	r := processRunner{}
	panel, mc, err := ensureAccount(ctx, r, p, instance.ID)
	if err != nil {
		return err
	}
	if err := checkConflicts(instance, &mc); err != nil {
		return err
	}
	var root *os.File
	if instance.Adopted {
		root, err = dataDir(instance.Root)
	} else {
		root, err = ensureManagedRoot(instance.Root, panel.uid, mc.gid)
	}
	if err != nil {
		return err
	}
	defer root.Close()
	dev, ino, err := identity(root)
	if err != nil {
		return err
	}
	m := manifest{Instance: instance, Device: dev, Inode: ino, UID: mc.uid, GID: mc.gid}
	if f, statErr := trustedFile(filepath.Join(unitDir, unitName(instance.ID))); statErr == nil {
		f.Close()
		if err := proveStopped(ctx, r, instance.ID, mc.uid, false); err != nil {
			return err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := noInstanceProcesses(mc.uid); err != nil {
		return err
	}
	m, err = prepareInstance(ctx, p, m)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if req.op == "reconfigure" {
		if err := atomicWriteTrusted(filepath.Join(unitDir, unitName(instance.ID)), renderUnit(instance), 0644); err != nil {
			return err
		}
		if err := atomicWriteTrusted(filepath.Join(manifestDir, instance.ID+".json"), data, 0600); err != nil {
			return err
		}
	} else {
		if err := writeExclusive(filepath.Join(unitDir, unitName(instance.ID)), renderUnit(instance), 0644); err != nil {
			return err
		}
		if err := writeExclusive(filepath.Join(manifestDir, instance.ID+".json"), data, 0600); err != nil {
			return err
		}
	}
	_, err = r.run(ctx, systemctl, nil, "daemon-reload")
	return err
}

func unchangedIdentity(before, after model.Instance) bool {
	return before.ID == after.ID && before.Root == after.Root && before.Port == after.Port && before.RCONPort == after.RCONPort && before.Adopted == after.Adopted && before.CreatedAt.Equal(after.CreatedAt)
}

func checkReconfiguration(ctx context.Context, r runner, p policy, instance model.Instance) error {
	m, err := loadManifest(p, instance.ID)
	if err != nil {
		return err
	}
	if !unchangedIdentity(m.Instance, instance) {
		return errors.New("reconfiguration cannot change ID, root, ports, adoption mode, or creation identity")
	}
	if instance.Adopted {
		root, err := dataDir(instance.Root)
		if err != nil {
			return err
		}
		dev, ino, err := identity(root)
		root.Close()
		if err != nil || dev != m.Device || ino != m.Inode {
			return errors.New("adopted directory identity changed; independent root review is required")
		}
	}
	state, err := r.run(ctx, systemctl, nil, "show", "--property=ActiveState", "--value", unitName(instance.ID))
	if err != nil || string(state) != "inactive\n" && string(state) != "failed\n" {
		return errors.New("reconfiguration requires a proven stopped unit")
	}
	pid, err := r.run(ctx, systemctl, nil, "show", "--property=MainPID", "--value", unitName(instance.ID))
	if err != nil || string(pid) != "0\n" {
		return errors.New("reconfiguration requires no running Minecraft process")
	}
	return nil
}
