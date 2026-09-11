//go:build linux || darwin

package rundirectory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"golang.org/x/sys/unix"
)

// PublishedRunRoot is a descriptor-pinned, read-only identity for one
// published run root. OpenRoot returns another descriptor for the same inode.
type PublishedRunRoot interface {
	ID() runid.ID
	OpenRoot() (*os.File, error)
}

// PublishedRunRoots owns a point-in-time set of descriptor-pinned run roots.
// Callers must close it after reading from the roots.
type PublishedRunRoots interface {
	Roots() []PublishedRunRoot
	Close() error
}

type pinnedPublishedRoot struct {
	id   runid.ID
	root *os.File
}

func (root *pinnedPublishedRoot) ID() runid.ID { return root.id }

func (root *pinnedPublishedRoot) OpenRoot() (*os.File, error) {
	return duplicateDirectory(root.root, runRootLabel(root.id))
}

type pinnedPublishedRoots struct {
	roots []*pinnedPublishedRoot
}

func (roots *pinnedPublishedRoots) Roots() []PublishedRunRoot {
	result := make([]PublishedRunRoot, len(roots.roots))
	for index, root := range roots.roots {
		result[index] = root
	}
	return result
}

func (roots *pinnedPublishedRoots) Close() error {
	var closeErr error
	for _, root := range roots.roots {
		closeErr = errors.Join(closeErr, root.root.Close())
	}
	roots.roots = nil
	return closeErr
}

// OpenPublishedRunRoots pins every currently published run root while holding
// only the short authority lock. Network evidence remains opaque to this package.
func (run *LiveRun) OpenPublishedRunRoots(ctx context.Context) (PublishedRunRoots, error) {
	if ctx == nil {
		return nil, errors.New("published-root snapshot context is required")
	}
	current, err := run.OpenRoot()
	if err != nil {
		return nil, err
	}
	defer current.Close()
	return run.lease.owner.openPublishedRunRoots(ctx, run.ID(), current)
}

func (owner *Owner) openPublishedRunRoots(ctx context.Context, currentID runid.ID,
	current *os.File,
) (PublishedRunRoots, error) {
	authority, err := owner.openAuthority()
	if err != nil {
		return nil, err
	}
	defer authority.Close()
	if err := lockWithContext(ctx, authority.root); err != nil {
		return nil, fmt.Errorf("lock authority for published roots: %w", err)
	}
	defer unlock(authority.root)
	if err := owner.verifyAuthorityLocation(authority); err != nil {
		return nil, fmt.Errorf("verify authority for published roots: %w", err)
	}
	return owner.pinPublishedRunRoots(authority.root, currentID, current)
}

func (owner *Owner) pinPublishedRunRoots(authority *os.File, currentID runid.ID,
	current *os.File,
) (_ PublishedRunRoots, resultErr error) {
	names, err := readDirectoryNames(authority)
	if err != nil {
		return nil, fmt.Errorf("list published run roots: %w", err)
	}
	sort.Strings(names)
	result := &pinnedPublishedRoots{}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, result.Close())
		}
	}()
	for _, name := range names {
		if isTemporaryName(name) {
			if err := owner.validateTransientRoot(authority, name); err != nil {
				return nil, err
			}
			continue
		}
		id, err := runid.Parse(name)
		if err != nil {
			return nil, fmt.Errorf("unexpected entry %q in authority root", name)
		}
		root, err := owner.pinPublishedRunRoot(authority, id)
		if err != nil {
			return nil, err
		}
		result.roots = append(result.roots, root)
	}
	if err := requireCurrentPublishedRoot(result.roots, currentID, current); err != nil {
		return nil, err
	}
	return result, nil
}

func (owner *Owner) validateTransientRoot(authority *os.File, name string) error {
	root, err := openDirectoryAt(authority, name, filepath.Join(owner.authorityRoot, name))
	if err != nil {
		return fmt.Errorf("open transient run root %q: %w", name, err)
	}
	defer root.Close()
	if err := validateOwnedDirectory(root, 0o700); err != nil {
		return fmt.Errorf("validate transient run root %q: %w", name, err)
	}
	return nil
}

func (owner *Owner) pinPublishedRunRoot(authority *os.File, id runid.ID) (*pinnedPublishedRoot, error) {
	name := id.String()
	var entry unix.Stat_t
	if err := unix.Fstatat(int(authority.Fd()), name, &entry, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, fmt.Errorf("inspect published run root %s: %w", id, err)
	}
	root, err := openDirectoryAt(authority, name, filepath.Join(owner.authorityRoot, name))
	if err != nil {
		return nil, fmt.Errorf("open published run root %s: %w", id, err)
	}
	if err := validatePinnedDirectory(root, entry); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("validate published run root %s: %w", id, err)
	}
	return &pinnedPublishedRoot{id: id, root: root}, nil
}

func validatePinnedDirectory(root *os.File, entry unix.Stat_t) error {
	if err := validateOwnedDirectory(root, 0o700); err != nil {
		return err
	}
	var pinned unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &pinned); err != nil {
		return err
	}
	if entry.Mode&unix.S_IFMT != unix.S_IFDIR || entry.Dev != pinned.Dev || entry.Ino != pinned.Ino {
		return errors.New("directory entry changed identity while being pinned")
	}
	return nil
}

func requireCurrentPublishedRoot(roots []*pinnedPublishedRoot, currentID runid.ID,
	current *os.File,
) error {
	for _, root := range roots {
		if root.id != currentID {
			continue
		}
		same, err := sameDirectory(root.root, current)
		if err != nil {
			return fmt.Errorf("compare current run root identity: %w", err)
		}
		if !same {
			return fmt.Errorf("current run %s changed identity", currentID)
		}
		return nil
	}
	return fmt.Errorf("current run %s is no longer published", currentID)
}

func duplicateDirectory(root *os.File, label string) (*os.File, error) {
	fd, err := unix.FcntlInt(root.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("duplicate published run root descriptor: %w", err)
	}
	return os.NewFile(uintptr(fd), label), nil
}
