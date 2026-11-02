//go:build linux

package servicehelper

import (
	"encoding/binary"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func readAccessACL(f *os.File) ([]byte, error) {
	data := make([]byte, 4096)
	n, err := unix.Fgetxattr(int(f.Fd()), "system.posix_acl_access", data)
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EOPNOTSUPP) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("cannot validate instance access ACL within its 4 KiB limit")
	}
	data = data[:n]
	if len(data) < 4 || len(data)%8 != 4 || binary.LittleEndian.Uint32(data) != 2 {
		return nil, errors.New("unsupported instance access ACL")
	}
	return data, nil
}

func canAccessACL(st *unix.Stat_t, data []byte, a account, want uint32) bool {
	if len(data) == 0 || a.uid == st.Uid {
		return canAccess(st.Mode, st.Uid, st.Gid, a, want)
	}
	mask, other := uint32(7), uint32(st.Mode&7)
	namedPerm, groupPerm := uint32(0), uint32(0)
	named, group := false, false
	for offset := 4; offset+8 <= len(data); offset += 8 {
		entry := data[offset:]
		tag := binary.LittleEndian.Uint16(entry)
		perm := uint32(binary.LittleEndian.Uint16(entry[2:]))
		id := binary.LittleEndian.Uint32(entry[4:])
		switch tag {
		case 2:
			if id == a.uid {
				named = true
				namedPerm = perm
			}
		case 4:
			if a.groups[st.Gid] {
				group = true
				groupPerm |= perm
			}
		case 8:
			if a.groups[id] {
				group = true
				groupPerm |= perm
			}
		case 16:
			mask = perm
		case 32:
			other = perm
		}
	}
	if named {
		return namedPerm&mask&want == want
	}
	if group {
		return groupPerm&mask&want == want
	}
	return other&want == want
}
