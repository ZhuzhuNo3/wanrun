//go:build linux || darwin

package fileviews

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

type fileViewDirectoryFacts struct {
	identity       fileIdentity
	ownedByProcess bool
	permissions    os.FileMode
	links          uint64
}

type fileViewOwnershipFacts struct {
	regular        bool
	ownedByProcess bool
	permissions    os.FileMode
	links          uint64
	size           int64
}

func createFileViewMountpoint(root *os.File) error {
	return unix.Mkdirat(int(root.Fd()), viewsDirectoryName, 0o700)
}

func removeFileViewMountpoint(root *os.File) error {
	return unix.Unlinkat(int(root.Fd()), viewsDirectoryName, unix.AT_REMOVEDIR)
}

func removeFileViewOwnership(root *os.File) error {
	return unix.Unlinkat(int(root.Fd()), ownershipFileName, 0)
}

func fileViewEntryPresent(root *os.File, name string) (bool, error) {
	var status unix.Stat_t
	err := unix.Fstatat(int(root.Fd()), name, &status, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	return false, err
}

func createFileViewOwnership(root *os.File) (*os.File, error) {
	fd, err := unix.Openat(int(root.Fd()), ownershipFileName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	return wrapFileViewDescriptor(fd, ownershipFileName)
}

func openFileViewOwnership(root *os.File) (*os.File, error) {
	fd, err := unix.Openat(int(root.Fd()), ownershipFileName,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return wrapFileViewDescriptor(fd, ownershipFileName)
}

func inspectFileViewDirectory(directory *os.File) (fileViewDirectoryFacts, error) {
	var status unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &status); err != nil {
		return fileViewDirectoryFacts{}, err
	}
	if uint32(status.Mode)&unix.S_IFMT != unix.S_IFDIR {
		return fileViewDirectoryFacts{}, errors.New("file-view path is not a directory")
	}
	return fileViewDirectoryFacts{
		identity:       fileIdentity{device: uint64(status.Dev), inode: status.Ino},
		ownedByProcess: status.Uid == uint32(os.Geteuid()),
		permissions:    os.FileMode(status.Mode) & 0o7777,
		links:          uint64(status.Nlink),
	}, nil
}

func inspectFileViewOwnershipFile(file *os.File) (fileViewOwnershipFacts, error) {
	var status unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &status); err != nil {
		return fileViewOwnershipFacts{}, err
	}
	return fileViewOwnershipFacts{
		regular:        uint32(status.Mode)&unix.S_IFMT == unix.S_IFREG,
		ownedByProcess: status.Uid == uint32(os.Geteuid()),
		permissions:    os.FileMode(status.Mode) & 0o7777,
		links:          uint64(status.Nlink),
		size:           status.Size,
	}, nil
}

func openFileViewDirectoryAt(parent *os.File, path string) (*os.File, error) {
	current, err := duplicateFileViewDescriptor(parent, "file-view-path")
	if err != nil {
		return nil, err
	}
	for _, component := range strings.Split(path, "/") {
		fd, openErr := unix.Openat(int(current.Fd()), component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = current.Close()
		if openErr != nil {
			return nil, openErr
		}
		current, err = wrapFileViewDescriptor(fd, component)
		if err != nil {
			return nil, err
		}
	}
	return current, nil
}

func duplicateFileViewDescriptor(source *os.File, name string) (*os.File, error) {
	fd, err := unix.FcntlInt(source.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return wrapFileViewDescriptor(fd, name)
}

func wrapFileViewDescriptor(fd int, name string) (*os.File, error) {
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("wrap file-view descriptor")
	}
	return file, nil
}
