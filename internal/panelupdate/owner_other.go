//go:build !unix

package panelupdate

import "os"

func trustedOwner(info os.FileInfo) bool { return false }
