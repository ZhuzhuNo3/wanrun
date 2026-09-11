//go:build linux || darwin

package sourcefiles

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func openSourceDirectoryPath(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return wrapSourceDescriptor(fd, path)
}

func openSourceDirectoryAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return wrapSourceDescriptor(fd, name)
}

func openSourceRegularAt(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	return wrapSourceDescriptor(fd, name)
}

func duplicateSourceDescriptor(source *os.File, name string) (*os.File, error) {
	fd, err := unix.FcntlInt(source.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return wrapSourceDescriptor(fd, name)
}

func wrapSourceDescriptor(fd int, name string) (*os.File, error) {
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, errors.New("wrap source descriptor")
	}
	return file, nil
}

func lstatSource(path string) (sourceIdentity, error) {
	var status unix.Stat_t
	if err := unix.Lstat(path, &status); err != nil {
		return sourceIdentity{}, err
	}
	return identityFromStat(status), nil
}

func statSourceAt(directory *os.File, name string) (sourceIdentity, error) {
	var status unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &status, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return sourceIdentity{}, err
	}
	return identityFromStat(status), nil
}

func readSourceSymlinkAt(directory *os.File, name string) (string, error) {
	for size := 256; size <= 1<<20; size *= 2 {
		buffer := make([]byte, size)
		length, err := unix.Readlinkat(int(directory.Fd()), name, buffer)
		if err != nil {
			return "", err
		}
		if length < len(buffer) {
			return string(buffer[:length]), nil
		}
	}
	return "", errors.New("source symlink target is too long")
}

func inspectDescriptor(file *os.File) (sourceIdentity, error) {
	var status unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &status); err != nil {
		return sourceIdentity{}, err
	}
	return identityFromStat(status), nil
}

func inspectOpenedRegular(file *os.File) (sourceIdentity, uint64, error) {
	var status unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &status); err != nil {
		return sourceIdentity{}, 0, err
	}
	identity := identityFromStat(status)
	if identity.kind != sourceRegularObject || status.Size < 0 {
		return sourceIdentity{}, 0, errors.New("opened source object is not a regular file")
	}
	return identity, uint64(status.Size), nil
}

func metadataFromDescriptor(file *os.File, expected sourceObjectKind) (FileMetadata, error) {
	var status unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &status); err != nil {
		return FileMetadata{}, err
	}
	if identityFromStat(status).kind != expected || status.Size < 0 {
		return FileMetadata{}, errors.New("source backing object changed type")
	}
	info, err := file.Stat()
	if err != nil {
		return FileMetadata{}, err
	}
	return FileMetadata{mode: uint32(status.Mode), size: uint64(status.Size), mtime: info.ModTime()}, nil
}

func openBackingObject(anchor *os.File, components []string,
	wantDirectory bool,
) (*os.File, error) {
	current, err := duplicateSourceDescriptor(anchor, "source-backing-walk")
	if err != nil {
		return nil, err
	}
	for index, component := range components {
		last := index == len(components)-1
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if !last || wantDirectory {
			flags |= unix.O_DIRECTORY
		}
		fd, openErr := unix.Openat(int(current.Fd()), component, flags, 0)
		_ = current.Close()
		if openErr != nil {
			return nil, openErr
		}
		current, err = wrapSourceDescriptor(fd, component)
		if err != nil {
			return nil, err
		}
	}
	return current, nil
}

func identityFromStat(status unix.Stat_t) sourceIdentity {
	return sourceIdentity{device: uint64(status.Dev), inode: status.Ino,
		kind: sourceKind(uint32(status.Mode) & unix.S_IFMT)}
}

func sourceKind(mode uint32) sourceObjectKind {
	switch mode {
	case unix.S_IFDIR:
		return sourceDirectoryObject
	case unix.S_IFREG:
		return sourceRegularObject
	case unix.S_IFLNK:
		return sourceSymlinkObject
	default:
		return sourceSpecialObject
	}
}
