//go:build !unix

package ctl

import "io/fs"

// statOwner returns 0, false on non-Unix platforms.
func statOwner(fs.FileInfo) (uint32, bool) { return 0, false }
