//go:build unix

package ctl

import (
	"io/fs"
	"syscall"
)

// statOwner extracts the owning UID from FileInfo on Unix platforms.
func statOwner(fi fs.FileInfo) (uid uint32, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}
