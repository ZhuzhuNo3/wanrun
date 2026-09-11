//go:build linux

package fileviews

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openChildViewDescriptor(root *os.File) (*os.File, error) {
	fd, err := unix.Openat(int(root.Fd()), ".",
		unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "child-file-view-root")
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("wrap child file-view path descriptor")
	}
	return file, nil
}
