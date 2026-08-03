//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// ficlone is _IOW(0x94, 9, int): ask the filesystem to share extents between
// two files rather than copy them. The number is written out because importing
// golang.org/x/sys for it would be this project's first module dependency.
const ficlone = 0x40049409

// probeReflink reports whether dir's filesystem can clone extents.
//
// FICLONE is the honest probe -- unlike copy_file_range it either shares
// extents or fails, so its result distinguishes a free clone from an ordinary
// copy. Both files are created in dir, so both are on the filesystem being
// asked about.
func probeReflink(dir string) (bool, error) {
	src, err := os.CreateTemp(dir, ".undo-doctor-clone-src-")
	if err != nil {
		return false, err
	}
	defer os.Remove(src.Name())
	defer src.Close()
	if _, err := src.Write([]byte("undo doctor reflink probe\n")); err != nil {
		return false, err
	}

	dst, err := os.CreateTemp(dir, ".undo-doctor-clone-dst-")
	if err != nil {
		return false, err
	}
	defer os.Remove(dst.Name())
	defer dst.Close()

	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), ficlone, src.Fd())
	switch errno {
	case 0:
		return true, nil
	case syscall.EOPNOTSUPP, syscall.ENOTTY, syscall.EINVAL:
		// the filesystem cannot do it: a real answer, not a broken probe
		return false, nil
	case syscall.EXDEV:
		// both files were created in one directory, so this cannot happen
		// unless the probe itself is wrong. Do not report it as unsupported.
		return false, fmt.Errorf("reflink probe crossed a filesystem boundary inside %s", dir)
	default:
		return false, errno
	}
}
