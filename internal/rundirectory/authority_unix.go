//go:build linux || darwin

package rundirectory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type authorityDirectory struct {
	parent  *os.File
	root    *os.File
	private *privateAuthority
}

func (owner *Owner) verifyAuthorityLocation(authority *authorityDirectory) error {
	if authority.private != nil {
		same, err := sameDirectory(authority.private.root, authority.root)
		if err != nil {
			return fmt.Errorf("compare private authority identity: %w", err)
		}
		if !same {
			return errors.New("private authority changed identity")
		}
		return nil
	}
	current, err := openDirectoryAt(
		authority.parent,
		filepath.Base(owner.authorityRoot),
		owner.authorityRoot,
	)
	if err != nil {
		return fmt.Errorf("reopen authority root: %w", err)
	}
	defer current.Close()

	same, err := sameDirectory(current, authority.root)
	if err != nil {
		return fmt.Errorf("compare authority root identity: %w", err)
	}
	if !same {
		return errors.New("authority root changed identity")
	}
	return nil
}

func (owner *Owner) openAuthority() (*authorityDirectory, error) {
	if owner.private != nil {
		return owner.private.open(owner.authorityRoot)
	}
	parentPath := filepath.Dir(owner.authorityRoot)
	parent, err := openDirectory(parentPath)
	if err != nil {
		return nil, fmt.Errorf("open authority parent %q: %w", parentPath, err)
	}

	name := filepath.Base(owner.authorityRoot)
	if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		_ = parent.Close()
		return nil, fmt.Errorf("create authority root: %w", err)
	}
	root, err := openDirectoryAt(parent, name, owner.authorityRoot)
	if err != nil {
		_ = parent.Close()
		return nil, fmt.Errorf("open authority root: %w", err)
	}
	if err := validateOwnedDirectory(root, 0o700); err != nil {
		_ = root.Close()
		_ = parent.Close()
		return nil, fmt.Errorf("validate authority root: %w", err)
	}
	if err := owner.syncAuthorityCreation(root, parent); err != nil {
		_ = root.Close()
		_ = parent.Close()
		return nil, err
	}
	return &authorityDirectory{parent: parent, root: root}, nil
}

func (owner *Owner) syncAuthorityCreation(root, parent *os.File) error {
	if err := owner.durability.Sync(root); err != nil {
		return fmt.Errorf("sync authority root: %w", err)
	}
	if err := owner.durability.Sync(parent); err != nil {
		return fmt.Errorf("sync authority parent: %w", err)
	}
	return nil
}

func (directory *authorityDirectory) Close() error {
	err := errors.Join(directory.root.Close(), directory.parent.Close())
	if directory.private != nil {
		directory.private.release()
	}
	return err
}

func openDirectory(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openDirectoryAt(parent *os.File, name, displayPath string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), displayPath), nil
}

func createLockFile(parent *os.File, name, displayPath string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name,
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), displayPath)
	if err := validateOwnedRegular(file, 0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func openLockFile(parent *os.File, name, displayPath string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name,
		unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), displayPath)
	if err := validateOwnedRegular(file, 0o600); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func validateOwnedDirectory(file *os.File, mode os.FileMode) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("not a directory")
	}
	return validateOwnershipAndMode(stat, mode)
}

func validateOwnedRegular(file *os.File, mode os.FileMode) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return errors.New("not a singly linked regular file")
	}
	return validateOwnershipAndMode(stat, mode)
}

func validateOwnershipAndMode(stat unix.Stat_t, mode os.FileMode) error {
	if int(stat.Uid) != unix.Geteuid() {
		return fmt.Errorf("owner uid %d does not match effective uid", stat.Uid)
	}
	if os.FileMode(stat.Mode)&os.ModePerm != mode {
		return fmt.Errorf("mode %o, want %o", os.FileMode(stat.Mode)&os.ModePerm, mode)
	}
	return nil
}

func lockWithContext(ctx context.Context, file *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !lockBusy(err) {
			return err
		}
		if err := waitForRetry(ctx, 10*time.Millisecond); err != nil {
			return err
		}
	}
}

func tryLock(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if lockBusy(err) {
		return false, nil
	}
	return false, err
}

func unlock(file *os.File) error {
	if file == nil {
		return nil
	}
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}

func lockBusy(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN)
}

func sameDirectory(left, right *os.File) (bool, error) {
	var leftStat, rightStat unix.Stat_t
	if err := unix.Fstat(int(left.Fd()), &leftStat); err != nil {
		return false, err
	}
	if err := unix.Fstat(int(right.Fd()), &rightStat); err != nil {
		return false, err
	}
	return leftStat.Dev == rightStat.Dev && leftStat.Ino == rightStat.Ino, nil
}

func openDirectoryCopy(directory *os.File, displayPath string) (*os.File, error) {
	return openDirectoryAt(directory, ".", displayPath)
}
