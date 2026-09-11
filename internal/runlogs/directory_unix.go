//go:build linux || darwin

package runlogs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"golang.org/x/sys/unix"
)

type fileIdentity struct {
	device uint64
	inode  uint64
}

func openSafeLogDirectory(source *sourcefiles.SourceRoot, recovery *rundirectory.RecoveryRoot,
	path string,
) (*os.File, error) {
	parts, err := absoluteDirectoryParts(path)
	if err != nil {
		return nil, err
	}
	sourceBorrow, err := source.Borrow()
	if err != nil {
		return nil, fmt.Errorf("borrow source root for raw logs: %w", err)
	}
	defer sourceBorrow.Close()
	recoveryBorrow, err := recovery.Borrow()
	if err != nil {
		return nil, fmt.Errorf("borrow recovery root for raw logs: %w", err)
	}
	defer recoveryBorrow.Close()

	nearest, missing, err := openNearestLogDirectory(parts)
	if err != nil {
		return nil, err
	}
	if err := rejectProtectedLogLocation(int(sourceBorrow.Descriptor()), recoveryBorrow,
		int(nearest.Fd()), missing); err != nil {
		_ = nearest.Close()
		return nil, err
	}
	directory, err := createMissingLogDirectories(nearest, missing)
	if err != nil {
		return nil, err
	}
	if err := validateLogDirectory(directory); err != nil {
		_ = directory.Close()
		return nil, err
	}
	if err := rejectProtectedLogLocation(int(sourceBorrow.Descriptor()), recoveryBorrow,
		int(directory.Fd()), nil); err != nil {
		_ = directory.Close()
		return nil, err
	}
	return directory, nil
}

func absoluteDirectoryParts(path string) ([]string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) ||
		strings.IndexByte(path, 0) >= 0 {
		return nil, errors.New("raw log directory path is invalid")
	}
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)),
		string(filepath.Separator))
	for _, part := range parts {
		if !validDirectoryPart(part) {
			return nil, errors.New("raw log directory component is invalid")
		}
	}
	return parts, nil
}

func openNearestLogDirectory(parts []string) (*os.File, []string, error) {
	fd, err := unix.Open(string(filepath.Separator),
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open raw log path root: %w", err)
	}
	current := os.NewFile(uintptr(fd), "raw-log-path-root")
	for index, part := range parts {
		next, openErr := openLogDirectoryAt(current, part)
		if errors.Is(openErr, unix.ENOENT) {
			return current, append([]string(nil), parts[index:]...), nil
		}
		if openErr != nil {
			_ = current.Close()
			return nil, nil, fmt.Errorf("open raw log directory component %q: %w", part, openErr)
		}
		_ = current.Close()
		current = next
	}
	return current, nil, nil
}

func createMissingLogDirectories(current *os.File, missing []string) (*os.File, error) {
	for _, part := range missing {
		if err := unix.Mkdirat(int(current.Fd()), part, 0o700); err != nil {
			_ = current.Close()
			return nil, fmt.Errorf("create raw log directory component %q: %w", part, err)
		}
		next, err := openLogDirectoryAt(current, part)
		if err != nil {
			_ = current.Close()
			return nil, fmt.Errorf("open created raw log directory component %q: %w", part, err)
		}
		if err := validateLogDirectory(next); err != nil {
			_ = next.Close()
			_ = current.Close()
			return nil, err
		}
		if err := current.Sync(); err != nil {
			_ = next.Close()
			_ = current.Close()
			return nil, fmt.Errorf("sync raw log directory parent: %w", err)
		}
		_ = current.Close()
		current = next
	}
	return current, nil
}

func openLogDirectoryAt(parent *os.File, name string) (*os.File, error) {
	if !validDirectoryPart(name) {
		return nil, errors.New("raw log directory component is invalid")
	}
	fd, err := unix.Openat(int(parent.Fd()), name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func validDirectoryPart(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsRune(name, filepath.Separator)
}

func validateLogDirectory(directory *os.File) error {
	var info unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &info); err != nil {
		return err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFDIR || int(info.Uid) != os.Geteuid() || info.Mode&0o022 != 0 {
		return errors.New("raw log directory identity, owner, or mode is unsafe")
	}
	return nil
}

func createRawLog(directory *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create raw log %s without replacement: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFREG ||
		int(info.Uid) != os.Geteuid() || info.Mode&0o777 != 0o600 || uint64(info.Nlink) != 1 {
		_ = file.Close()
		return nil, errors.Join(fmt.Errorf("validate new raw log %s", name), err)
	}
	want := fileIdentity{device: uint64(info.Dev), inode: info.Ino}
	if err := verifyLogName(directory, name, want); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("recheck new raw log %s: %w", name, err)
	}
	return file, nil
}

func verifyLogName(directory *os.File, name string, want fileIdentity) error {
	var info unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), name, &info, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG ||
		(fileIdentity{device: uint64(info.Dev), inode: info.Ino}) != want {
		return errors.New("raw log name no longer identifies the opened file")
	}
	return nil
}

func rejectProtectedLogLocation(sourceFD int, recovery *rundirectory.RecoveryRootBorrow,
	candidateFD int, missing []string,
) error {
	inside, err := directoryAtOrBelow(sourceFD, candidateFD)
	if err != nil {
		return fmt.Errorf("compare source and raw log directories: %w", err)
	}
	if inside {
		return errors.New("raw log directory resolves inside source directory")
	}
	if len(recovery.MissingPath()) == 0 {
		inside, err = directoryAtOrBelow(int(recovery.Descriptor()), candidateFD)
		if err != nil {
			return fmt.Errorf("compare recovery and raw log directories: %w", err)
		}
		if inside {
			return errors.New("raw log directory resolves inside recovery authority")
		}
	}
	return rejectProtectedMountLocation(sourceFD, recovery, candidateFD, missing)
}

func directoryAtOrBelow(rootFD, candidateFD int) (bool, error) {
	root, err := directoryIdentity(rootFD)
	if err != nil {
		return false, err
	}
	currentFD, err := unix.Dup(candidateFD)
	if err != nil {
		return false, err
	}
	current := os.NewFile(uintptr(currentFD), "raw-log-ancestry")
	if current == nil {
		_ = unix.Close(currentFD)
		return false, errors.New("wrap raw log ancestry descriptor")
	}
	seen := make(map[fileIdentity]struct{})
	for {
		identity, err := directoryIdentity(int(current.Fd()))
		if err != nil {
			return false, errors.Join(err, current.Close())
		}
		if identity == root {
			return true, current.Close()
		}
		if _, duplicate := seen[identity]; duplicate {
			return false, errors.Join(errors.New("directory ancestry contains a cycle"), current.Close())
		}
		seen[identity] = struct{}{}
		parentFD, err := unix.Openat(int(current.Fd()), "..",
			unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if err != nil {
			return false, errors.Join(err, current.Close())
		}
		parent := os.NewFile(uintptr(parentFD), "raw-log-ancestry-parent")
		if parent == nil {
			_ = unix.Close(parentFD)
			return false, errors.Join(errors.New("wrap raw log ancestry parent"), current.Close())
		}
		parentIdentity, err := directoryIdentity(int(parent.Fd()))
		if err != nil {
			return false, errors.Join(err, parent.Close(), current.Close())
		}
		if parentIdentity == identity {
			return false, errors.Join(parent.Close(), current.Close())
		}
		if err := current.Close(); err != nil {
			return false, errors.Join(err, parent.Close())
		}
		current = parent
	}
}

func directoryIdentity(fd int) (fileIdentity, error) {
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return fileIdentity{}, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fileIdentity{}, errors.New("descriptor is not a directory")
	}
	return fileIdentity{device: uint64(info.Dev), inode: info.Ino}, nil
}
