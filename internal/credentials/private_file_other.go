//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly

package credentials

import "os"

// Do not guess private-file ACL semantics on an unqualified platform.
func ownedByProcess(os.FileInfo) bool { return false }
