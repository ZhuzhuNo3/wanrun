//go:build linux || darwin

package hostnetwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

type heldHostLock struct{ file *os.File }

func acquireHostLock(ctx context.Context, path string) (*heldHostLock, error) {
	if err := ensureLockDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open host-network lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := verifyLockFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := waitHostLock(ctx, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &heldHostLock{file: file}, nil
}

func ensureLockDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create host-network lock directory: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect host-network lock directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("unsafe host-network lock directory %q", path)
	}
	var status unix.Stat_t
	if err := unix.Lstat(path, &status); err != nil || status.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("unsafe host-network lock directory ownership %q", path)
	}
	return nil
}

func verifyLockFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect host-network lock: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o177 != 0 {
		return errors.New("unsafe host-network lock file")
	}
	var status unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &status); err != nil ||
		status.Uid != uint32(os.Geteuid()) || status.Nlink != 1 {
		return errors.New("host-network lock ownership is unsafe")
	}
	return nil
}

func waitHostLock(ctx context.Context, file *os.File) error {
	for {
		err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return fmt.Errorf("lock host network: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (lock *heldHostLock) Close() error {
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	return errors.Join(unlockErr, lock.file.Close())
}
