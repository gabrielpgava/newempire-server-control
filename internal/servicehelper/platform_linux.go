//go:build linux

package servicehelper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/syslog"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func rootOnly() error {
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		return errors.New("this command requires real and effective root privileges")
	}
	return nil
}

func independentRoot() error {
	if err := rootOnly(); err != nil {
		return err
	}
	if uid := os.Getenv("SUDO_UID"); uid != "" && uid != "0" {
		return errors.New("registration/protection requires an independent root administrator, not panel sudo")
	}
	return nil
}

type account struct {
	uid, gid uint32
	groups   map[uint32]bool
}

func lookupAccount(name string) (account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return account{}, err
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil || uid == 0 {
		return account{}, errors.New("service user must not be root")
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil || gid == 0 {
		return account{}, errors.New("service group must not be root")
	}
	groupIDs, err := u.GroupIds()
	if err != nil {
		return account{}, err
	}
	a := account{uid: uint32(uid), gid: uint32(gid), groups: map[uint32]bool{uint32(gid): true}}
	for _, id := range groupIDs {
		n, err := strconv.ParseUint(id, 10, 32)
		if err != nil || n == 0 {
			return account{}, errors.New("service account has an unsafe group")
		}
		a.groups[uint32(n)] = true
	}
	return a, nil
}

func loadPolicy() (policy, error) {
	b, err := readTrusted(configPath)
	if err != nil {
		return policy{}, err
	}
	var p policy
	if err := decode(bytes.NewReader(b), &p); err != nil {
		return p, err
	}
	return p, p.validate()
}

func authorize(p policy, req request) error {
	if err := rootOnly(); err != nil {
		return err
	}
	if req.op == "protect" {
		return independentRoot()
	}
	panel, err := lookupAccount(p.PanelUser)
	if err != nil {
		return err
	}
	if uid := os.Getenv("SUDO_UID"); uid != "" && uid != "0" {
		if uid != strconv.FormatUint(uint64(panel.uid), 10) || os.Getenv("SUDO_USER") != p.PanelUser {
			return errors.New("runtime helper may be called only by the configured panel account or root")
		}
	}
	return nil
}

func loadManifest(p policy, id string) (manifest, error) {
	b, err := readTrusted(filepath.Join(manifestDir, id+".json"))
	if err != nil {
		return manifest{}, errors.New("instance is not root-registered; run install-service from an administrator shell")
	}
	var m manifest
	if err := decode(bytes.NewReader(b), &m); err != nil {
		return m, err
	}
	if m.Instance.ID != id || m.UID == 0 || m.GID == 0 || m.Inode == 0 {
		return m, errors.New("invalid root-owned instance manifest")
	}
	if err := validateInstance(p, m.Instance); err != nil {
		return m, err
	}
	a, err := lookupAccount(accountName(id))
	if err != nil || a.uid != m.UID || a.gid != m.GID || len(a.groups) != 1 {
		return m, errors.New("instance account no longer matches its registration")
	}
	u, err := user.Lookup(accountName(id))
	if err != nil || u.HomeDir != "/nonexistent" {
		return m, errors.New("instance account must be a dedicated non-login service account")
	}
	unit, err := readTrusted(filepath.Join(unitDir, unitName(id)))
	if err != nil || !bytes.Equal(unit, renderUnit(m.Instance)) {
		return m, errors.New("instance unit differs from its root registration")
	}
	return m, nil
}

func verifyData(m manifest) error {
	f, err := dataDir(m.Instance.Root)
	if err != nil {
		return err
	}
	defer f.Close()
	dev, ino, err := identity(f)
	if err != nil || dev != m.Device || ino != m.Inode {
		return errors.New("instance directory identity changed; root re-registration is required")
	}
	jar, err := launcherFile(f, m.Instance.Jar)
	if err != nil {
		return err
	}
	defer jar.Close()
	var st unix.Stat_t
	if err := unix.Fstat(int(jar.Fd()), &st); err != nil {
		return err
	}
	a := account{uid: m.UID, gid: m.GID, groups: map[uint32]bool{m.GID: true}}
	acl, err := readAccessACL(jar)
	if err != nil {
		return err
	}
	if !canAccessACL(&st, acl, a, 4) {
		return errors.New("instance user cannot read the registered launcher; use group-readable files")
	}
	return checkExecutable(m.Instance.JavaPath)
}

func verifyLoadedCommand(ctx context.Context, r runner, m manifest) error {
	i := m.Instance
	value, err := r.run(ctx, systemctl, nil, "show", "--property=ExecStart", "--value", unitName(i.ID))
	prefix := fmt.Sprintf("{ path=%s ; argv[]=%s -Xms256M -Xmx%dM -jar ./%s nogui ; ignore_errors=no ;", i.JavaPath, i.JavaPath, i.MemoryMB, i.Jar)
	if err != nil || !strings.HasPrefix(string(value), prefix) || strings.Count(string(value), "{ path=") != 1 {
		return errors.New("loaded systemd Java command differs from the root registration; reload/review while stopped")
	}
	return nil
}

func runPlatform(ctx context.Context, req request, stdin io.Reader, stdout io.Writer) (result error) {
	if err := rootOnly(); err != nil {
		return err
	}
	p, err := loadPolicy()
	if err != nil {
		return err
	}
	if err := authorize(p, req); err != nil {
		return err
	}
	if req.op == "provision" || req.op == "reconfigure" {
		return provisionPlatform(ctx, stdin, req)
	}
	if err := checkExecutable(controlPath); err != nil {
		return err
	}
	journal, err := syslog.New(syslog.LOG_AUTHPRIV|syslog.LOG_NOTICE, "new-empire-runtime")
	if err != nil {
		return errors.New("privileged audit socket unavailable; refusing runtime operation")
	}
	defer journal.Close()
	if err := journal.Notice("requested " + req.op + " " + req.id); err != nil {
		return errors.New("privileged audit write failed")
	}
	defer func() {
		outcome := "succeeded"
		if result != nil {
			outcome = "failed " + safeDiagnostic(result)
		}
		_ = journal.Notice(outcome + " " + req.op + " " + req.id)
	}()
	var m manifest
	if req.op != "panel-update" {
		m, err = loadManifest(p, req.id)
		if err != nil {
			return err
		}
	}
	if req.op == "start" || req.op == "restart" || req.op == "protect" {
		if err := checkExecutable(nft); err != nil {
			return err
		}
	}
	if req.op == "logs" {
		err = checkExecutable(journalctl)
	} else {
		err = checkExecutable(systemctl)
	}
	if err != nil {
		return err
	}
	if req.op == "protect" || req.op == "prepare" {
		m, err = prepareRegistered(ctx, p, m, req.op == "protect")
		if err != nil {
			return err
		}
		if req.op == "prepare" {
			return nil
		}
	}
	data, err := dispatch(ctx, processRunner{}, req, m)
	if err != nil {
		return err
	}
	if req.op == "stop" {
		if _, err := prepareRegistered(ctx, p, m, false); err != nil {
			return err
		}
	}
	_, err = stdout.Write(data)
	return err
}

func safeDiagnostic(err error) string {
	text := err.Error()
	if len(text) > 1024 {
		text = text[:1024]
	}
	return strconv.Quote(text)
}
