//go:build linux

package servicehelper

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gabrielpgava/newempire-server-control/internal/model"
	"golang.org/x/sys/unix"
)

func validManagedEntry(st *unix.Stat_t, panelUID, mcUID uint32) bool {
	if st.Uid == 0 || st.Uid != panelUID && st.Uid != mcUID {
		return false
	}
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFDIR:
		return true
	case unix.S_IFREG:
		return st.Nlink == 1
	default:
		return false
	}
}

// Only descriptors below this root are changed; openat2 refuses links and mount crossings.
func walkDataTree(ctx context.Context, root *os.File, visit func(*os.File, *unix.Stat_t) error) error {
	count := 0
	var walk func(*os.File, int) error
	walk = func(f *os.File, depth int) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		count++
		if count > 1_000_000 || depth > 128 {
			return errors.New("instance tree validation exceeds depth/file-count limits")
		}
		var st unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &st); err != nil {
			return err
		}
		if err := visit(f, &st); err != nil {
			return err
		}
		isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
		if !isDir {
			return nil
		}
		for {
			entries, readErr := f.ReadDir(256)
			if readErr != nil && readErr != io.EOF {
				return readErr
			}
			for _, entry := range entries {
				flags := uint64(unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK)
				fd, err := unix.Openat2(int(f.Fd()), entry.Name(), &unix.OpenHow{Flags: flags, Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_XDEV})
				if err != nil {
					return errors.New("instance tree contains an unsafe link/mount or openat2 is unavailable")
				}
				child := os.NewFile(uintptr(fd), entry.Name())
				err = walk(child, depth+1)
				child.Close()
				if err != nil {
					return err
				}
			}
			if readErr == io.EOF {
				return nil
			}
		}
	}
	return walk(root, 0)
}

func prepareManaged(ctx context.Context, root *os.File, panelUID, mcUID, gid uint32, apply bool) error {
	return walkDataTree(ctx, root, func(f *os.File, st *unix.Stat_t) error {
		if !validManagedEntry(st, panelUID, mcUID) {
			return errors.New("managed tree contains a link, special file, or unexpected/root owner; no external ownership is repaired")
		}
		if !apply {
			return nil
		}
		if err := f.Chown(int(panelUID), int(gid)); err != nil {
			return err
		}
		isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
		mode := uint32(0660)
		if isDir {
			mode = 02770
		}
		if err := unix.Fchmod(int(f.Fd()), mode); err != nil {
			return err
		}
		// Named ACLs avoid restarting the panel whenever an instance group is created.
		acl := managedACL(panelUID, isDir)
		if err := unix.Fsetxattr(int(f.Fd()), "system.posix_acl_access", acl, 0); err != nil {
			return fmt.Errorf("managed instance filesystem must support POSIX ACLs: %w", err)
		}
		if isDir {
			if err := unix.Fsetxattr(int(f.Fd()), "system.posix_acl_default", acl, 0); err != nil {
				return fmt.Errorf("managed instance filesystem must support default POSIX ACLs: %w", err)
			}
		}
		return nil
	})
}

func managedACL(panelUID uint32, dir bool) []byte {
	perm := uint16(6)
	if dir {
		perm = 7
	}
	data := make([]byte, 4+5*8)
	binary.LittleEndian.PutUint32(data, 2)
	for i, tag := range []uint16{1, 2, 4, 16, 32} {
		entry := data[4+i*8:]
		binary.LittleEndian.PutUint16(entry, tag)
		p := perm
		if tag == 32 {
			p = 0
		}
		binary.LittleEndian.PutUint16(entry[2:], p)
		id := uint32(0xffffffff)
		if tag == 2 {
			id = panelUID
		}
		binary.LittleEndian.PutUint32(entry[4:], id)
	}
	return data
}

func prepareInstance(ctx context.Context, p policy, m manifest) (manifest, error) {
	panel, err := lookupAccount(p.PanelUser)
	if err != nil {
		return m, err
	}
	f, err := dataDir(m.Instance.Root)
	if err != nil {
		return m, err
	}
	defer f.Close()
	dev, ino, err := identity(f)
	if err != nil {
		return m, err
	}
	if m.Instance.Adopted {
		if dev != m.Device || ino != m.Inode {
			return m, errors.New("adopted directory identity changed; independent root review is required")
		}
		mc := account{uid: m.UID, gid: m.GID, groups: map[uint32]bool{m.GID: true}}
		if err := checkTree(ctx, m.Instance.Root, panel, mc); err != nil {
			return m, err
		}
	} else {
		var st unix.Stat_t
		if err := unix.Fstat(int(f.Fd()), &st); err != nil {
			return m, err
		}
		if st.Uid != panel.uid {
			return m, errors.New("managed directory must remain owned by the panel account")
		}
		// A validation pass avoids ordinary partial permission repairs; every apply lookup is revalidated.
		if err := prepareManaged(ctx, f, panel.uid, m.UID, m.GID, false); err != nil {
			return m, err
		}
		if _, err := f.Seek(0, 0); err != nil {
			return m, err
		}
		if err := prepareManaged(ctx, f, panel.uid, m.UID, m.GID, true); err != nil {
			return m, err
		}
		m.Device, m.Inode = dev, ino
	}
	return m, nil
}

func savePreparedIdentity(m manifest) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWriteTrusted(filepath.Join(manifestDir, m.Instance.ID+".json"), data, 0600)
}

func atomicWriteTrusted(path string, data []byte, mode uint32) error {
	previous, err := trustedFile(path)
	if err != nil {
		return err
	}
	previous.Close()
	dir, err := trustedDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	name := ".prepared-" + model.NewID()
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	defer unix.Unlinkat(int(dir.Fd()), name, 0)
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = unix.Fchmod(fd, mode); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = unix.Renameat(int(dir.Fd()), name, int(dir.Fd()), filepath.Base(path)); err != nil {
		return err
	}
	return dir.Sync()
}
