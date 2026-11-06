package fcdisk

import (
	"os"

	"golang.org/x/sys/unix"
)

// cloneFile makes dst share src's blocks through FICLONE, a reflink that a
// write to either file then unshares. It fails where the filesystem has no
// reflinks, as ext4 does, and on two filesystems.
var cloneFile = func(dst, src *os.File) error {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
}
