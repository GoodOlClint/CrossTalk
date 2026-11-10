//go:build darwin || linux

package codex

import (
	"os"
	"syscall"
)

func ownedBySelf(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
