//go:build linux || darwin

package rundirectory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var ErrRecoveryRootBorrowed = errors.New("recovery root is borrowed")

// RecoveryRoot owns a read-only descriptor anchor for the existing or prospective run authority.
// It exposes no discovery, recovery, creation, or deletion capability.
type RecoveryRoot struct {
	mu        sync.Mutex
	anchor    *os.File
	authority *authorityDirectory
	logical   string
	missing   []string
	borrows   int
	closed    bool
}

// RecoveryRootBorrow is a short-lived read-only duplicate of the authority location anchor.
type RecoveryRootBorrow struct {
	once    sync.Once
	owner   *RecoveryRoot
	anchor  *os.File
	logical string
	missing []string
	err     error
}

// OpenRecoveryRoot observes the authority location without creating it or examining stale runs.
func (owner *Owner) OpenRecoveryRoot() (*RecoveryRoot, error) {
	if owner == nil {
		return nil, errors.New("run directory owner is absent")
	}
	if owner.private != nil {
		authority, err := owner.private.open(owner.authorityRoot)
		if err != nil {
			return nil, err
		}
		return &RecoveryRoot{anchor: authority.root, authority: authority,
			logical: owner.authorityRoot}, nil
	}
	anchor, missing, err := openNearestRecoveryDirectory(owner.authorityRoot)
	if err != nil {
		return nil, err
	}
	return &RecoveryRoot{anchor: anchor, logical: owner.authorityRoot, missing: missing}, nil
}

func openNearestRecoveryDirectory(path string) (*os.File, []string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return nil, nil, errors.New("recovery authority path is invalid")
	}
	parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)),
		string(filepath.Separator))
	anchor, err := openDirectory(string(filepath.Separator))
	if err != nil {
		return nil, nil, err
	}
	for index, name := range parts {
		next, openErr := openDirectoryAt(anchor, name, filepath.Join(string(filepath.Separator),
			filepath.Join(parts[:index+1]...)))
		if errors.Is(openErr, unix.ENOENT) {
			return anchor, append([]string(nil), parts[index:]...), nil
		}
		if openErr != nil {
			_ = anchor.Close()
			return nil, nil, openErr
		}
		_ = anchor.Close()
		anchor = next
	}
	return anchor, nil, nil
}

func (root *RecoveryRoot) Borrow() (*RecoveryRootBorrow, error) {
	if root == nil {
		return nil, errors.New("recovery root is absent")
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.closed || root.anchor == nil {
		return nil, errors.New("recovery root is closed")
	}
	fd, err := unix.FcntlInt(root.anchor.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	anchor := os.NewFile(uintptr(fd), "transferlanes-recovery-root-borrow")
	if anchor == nil {
		_ = unix.Close(fd)
		return nil, errors.New("wrap recovery root descriptor")
	}
	root.borrows++
	return &RecoveryRootBorrow{owner: root, anchor: anchor, logical: root.logical,
		missing: append([]string(nil), root.missing...)}, nil
}

func (borrow *RecoveryRootBorrow) Descriptor() uintptr {
	if borrow == nil || borrow.anchor == nil {
		return ^uintptr(0)
	}
	return borrow.anchor.Fd()
}

func (borrow *RecoveryRootBorrow) LogicalPath() string {
	if borrow == nil {
		return ""
	}
	return borrow.logical
}

func (borrow *RecoveryRootBorrow) MissingPath() []string {
	if borrow == nil {
		return nil
	}
	return append([]string(nil), borrow.missing...)
}

func (borrow *RecoveryRootBorrow) Close() error {
	if borrow == nil {
		return nil
	}
	borrow.once.Do(func() {
		borrow.err = borrow.anchor.Close()
		borrow.owner.mu.Lock()
		borrow.owner.borrows--
		borrow.owner.mu.Unlock()
		borrow.anchor = nil
	})
	return borrow.err
}

func (root *RecoveryRoot) Close() error {
	if root == nil {
		return nil
	}
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.closed {
		return nil
	}
	if root.borrows != 0 {
		return ErrRecoveryRootBorrowed
	}
	root.closed = true
	if root.authority != nil {
		root.anchor = nil
		return root.authority.Close()
	}
	anchor := root.anchor
	root.anchor = nil
	return anchor.Close()
}
