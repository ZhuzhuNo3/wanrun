//go:build linux || darwin

package rundirectory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"golang.org/x/sys/unix"
)

const (
	privateAuthorityBase   = "/tmp"
	privateAuthorityPrefix = ".transferlanes-private-"
)

type privateAuthority struct {
	mu     sync.RWMutex
	parent *os.File
	root   *os.File
	name   string
	closed bool
}

func createPrivateAuthority(durability durableFiles) (*Owner, string, error) {
	base, err := filepath.EvalSymlinks(privateAuthorityBase)
	if err != nil {
		return nil, "", fmt.Errorf("resolve private authority base: %w", err)
	}
	parent, err := openDirectory(base)
	if err != nil {
		return nil, "", fmt.Errorf("open private authority base: %w", err)
	}
	id, err := runid.New()
	if err != nil {
		_ = parent.Close()
		return nil, "", fmt.Errorf("name private authority: %w", err)
	}
	name := privateAuthorityPrefix + id.String()
	path := filepath.Join(base, name)
	if err := unix.Mkdirat(int(parent.Fd()), name, 0o700); err != nil {
		_ = parent.Close()
		return nil, "", fmt.Errorf("create private authority: %w", err)
	}
	var created unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &created, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, "", errors.Join(fmt.Errorf("identify fresh private authority: %w", err),
			discardPrivateAuthority(parent, nil, name, durability))
	}
	root, err := openDirectoryAt(parent, name, path)
	if err != nil {
		return nil, "", errors.Join(fmt.Errorf("bind private authority: %w", err),
			discardPrivateAuthority(parent, nil, name, durability))
	}
	if err := validateOwnedDirectory(root, 0o700); err != nil {
		return nil, "", errors.Join(fmt.Errorf("validate private authority: %w", err),
			discardPrivateAuthority(parent, root, name, durability))
	}
	var bound unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &bound); err != nil ||
		created.Dev != bound.Dev || created.Ino != bound.Ino {
		return nil, "", errors.Join(errors.New("fresh private authority changed identity before binding"),
			err, discardPrivateAuthority(parent, root, name, durability))
	}
	owner := &Owner{authorityRoot: path, durability: durability, retryDelay: 10 * time.Millisecond}
	owner.private = &privateAuthority{parent: parent, root: root, name: name}
	if err := owner.syncAuthorityCreation(root, parent); err != nil {
		return nil, "", errors.Join(err, discardPrivateAuthority(parent, root, name, durability))
	}
	return owner, path, nil
}

func discardPrivateAuthority(parent, root *os.File, name string, durability durableFiles) error {
	var closeRootErr error
	if root != nil {
		closeRootErr = root.Close()
	}
	removeErr := unix.Unlinkat(int(parent.Fd()), name, unix.AT_REMOVEDIR)
	syncErr := durability.Sync(parent)
	return errors.Join(closeRootErr, removeErr, syncErr, parent.Close())
}

func (private *privateAuthority) open(displayPath string) (*authorityDirectory, error) {
	private.mu.RLock()
	if private.closed {
		private.mu.RUnlock()
		return nil, errors.New("private authority is closing or closed")
	}
	parent, err := openDirectoryCopy(private.parent, filepath.Dir(displayPath))
	if err != nil {
		private.mu.RUnlock()
		return nil, err
	}
	root, err := openDirectoryCopy(private.root, displayPath)
	if err != nil {
		_ = parent.Close()
		private.mu.RUnlock()
		return nil, err
	}
	return &authorityDirectory{parent: parent, root: root, private: private}, nil
}

func (private *privateAuthority) release() {
	private.mu.RUnlock()
}

// ClosePrivateAuthority removes the fresh authority created by CreatePrivateAuthority.
// It refuses production owners, non-empty authorities, and replaced directory entries.
func (owner *Owner) ClosePrivateAuthority(ctx context.Context) error {
	if owner.private == nil {
		return errors.New("owner does not own a private authority")
	}
	return owner.private.close(ctx, owner.authorityRoot, owner.durability)
}

func (private *privateAuthority) close(ctx context.Context, displayPath string,
	durability durableFiles,
) error {
	for !private.mu.TryLock() {
		if err := waitForRetry(ctx, time.Millisecond); err != nil {
			return err
		}
	}
	defer private.mu.Unlock()
	if private.closed {
		return nil
	}
	entries, err := readDirectoryNames(private.root)
	if err != nil {
		return fmt.Errorf("inspect private authority before removal: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("private authority is not empty: %v", entries)
	}
	current, err := openDirectoryAt(private.parent, private.name, displayPath)
	if err != nil {
		return fmt.Errorf("reopen private authority for removal: %w", err)
	}
	same, compareErr := sameDirectory(current, private.root)
	closeCurrentErr := current.Close()
	if compareErr != nil {
		return errors.Join(fmt.Errorf("compare private authority for removal: %w", compareErr),
			closeCurrentErr)
	}
	if !same {
		return errors.Join(errors.New("private authority path changed identity"), closeCurrentErr)
	}
	if closeCurrentErr != nil {
		return closeCurrentErr
	}
	if err := unix.Unlinkat(int(private.parent.Fd()), private.name, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("remove private authority: %w", err)
	}
	syncErr := durability.Sync(private.parent)
	closeErr := errors.Join(private.root.Close(), private.parent.Close())
	private.closed = true
	return errors.Join(syncErr, closeErr)
}
