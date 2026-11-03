package updates

import (
	"regexp"
	"strings"
)

var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

type version struct {
	core []string
	pre  []string
}

func parseVersion(s string) (version, error) {
	m := versionPattern.FindStringSubmatch(s)
	if m == nil {
		return version{}, problem(ErrInvalid, "%q is not a SemVer version without a v prefix", s)
	}
	v := version{core: m[1:4]}
	if m[4] != "" {
		v.pre = strings.Split(m[4], ".")
		for _, id := range v.pre {
			if numeric(id) && len(id) > 1 && id[0] == '0' {
				return version{}, problem(ErrInvalid, "numeric prerelease identifiers cannot have leading zeroes")
			}
		}
	}
	return v, nil
}

func numeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func compareNumeric(a, b string) int {
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}

func (a version) compare(b version) int {
	for i := range a.core {
		if n := compareNumeric(a.core[i], b.core[i]); n != 0 {
			return n
		}
	}
	if len(a.pre) == 0 && len(b.pre) != 0 {
		return 1
	}
	if len(b.pre) == 0 && len(a.pre) != 0 {
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		x, y := a.pre[i], b.pre[i]
		var n int
		switch {
		case numeric(x) && numeric(y):
			n = compareNumeric(x, y)
		case numeric(x):
			n = -1
		case numeric(y):
			n = 1
		default:
			n = strings.Compare(x, y)
		}
		if n != 0 {
			return n
		}
	}
	if len(a.pre) < len(b.pre) {
		return -1
	}
	if len(a.pre) > len(b.pre) {
		return 1
	}
	return 0
}
