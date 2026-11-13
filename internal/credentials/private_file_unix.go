//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

package credentials

import (
	"os"
	"syscall"
)

func ownedByProcess(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}
