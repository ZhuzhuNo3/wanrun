//go:build linux || darwin

package rundirectory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"golang.org/x/sys/unix"
)

func (owner *Owner) snapshotRecoveryEntries(
	ctx context.Context,
	authority *authorityDirectory,
) ([]string, error) {
	if err := lockWithContext(ctx, authority.root); err != nil {
		return nil, fmt.Errorf("lock authority for recovery snapshot: %w", err)
	}
	defer unlock(authority.root)
	if err := owner.verifyAuthorityLocation(authority); err != nil {
		return nil, fmt.Errorf("verify authority for recovery snapshot: %w", err)
	}

	names, err := readDirectoryNames(authority.root)
	if err != nil {
		return nil, fmt.Errorf("list authority for recovery snapshot: %w", err)
	}
	published := make([]string, 0, len(names))
	for _, name := range names {
		if !isTemporaryName(name) {
			published = append(published, name)
			continue
		}
		if err := owner.removeUnpublished(authority, name); err != nil {
			return nil, err
		}
	}
	sort.Strings(published)
	return published, nil
}

func (owner *Owner) removeUnpublished(authority *authorityDirectory, name string) error {
	if !isTemporaryName(name) {
		return fmt.Errorf("refuse to remove non-temporary entry %q", name)
	}
	path := filepath.Join(owner.authorityRoot, name)
	directory, err := openDirectoryAt(authority.root, name, path)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open temporary directory %q: %w", name, err)
	}
	defer directory.Close()
	if err := validateOwnedDirectory(directory, 0o700); err != nil {
		return fmt.Errorf("validate temporary directory %q: %w", name, err)
	}
	if err := validateFixedEntries(directory, true); err != nil {
		return fmt.Errorf("validate temporary directory %q: %w", name, err)
	}
	for _, lockName := range []string{livenessLockName, recoveryLockName} {
		if err := unix.Unlinkat(int(directory.Fd()), lockName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return fmt.Errorf("remove %s from %q: %w", lockName, name, err)
		}
	}
	if err := unix.Unlinkat(int(authority.root.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("remove temporary directory %q: %w", name, err)
	}
	if err := owner.durability.Sync(authority.root); err != nil {
		return fmt.Errorf("sync removal of temporary directory %q: %w", name, err)
	}
	return nil
}

func (owner *Owner) removeClaimedRun(ctx context.Context, authority *authorityDirectory,
	lease *runLease,
) error {
	if err := lockWithContext(ctx, authority.root); err != nil {
		return fmt.Errorf("lock authority for run removal: %w", err)
	}
	defer unlock(authority.root)
	if err := owner.verifyAuthorityLocation(authority); err != nil {
		return fmt.Errorf("verify authority for run removal: %w", err)
	}

	current, err := openDirectoryAt(authority.root, lease.id.String(), runRootLabel(lease.id))
	if err != nil {
		return fmt.Errorf("reopen claimed run %s: %w", lease.id, err)
	}
	defer current.Close()
	same, err := sameDirectory(current, lease.root)
	if err != nil {
		return fmt.Errorf("verify claimed run %s: %w", lease.id, err)
	}
	if !same {
		return fmt.Errorf("claimed run %s changed identity", lease.id)
	}
	if err := validateFixedEntries(lease.root, false); err != nil {
		return fmt.Errorf("validate completed run %s: %w", lease.id, err)
	}

	removingName, err := freshTemporaryName(removingPrefix, lease.id)
	if err != nil {
		return err
	}
	if err := unix.Renameat(
		int(authority.root.Fd()), lease.id.String(),
		int(authority.root.Fd()), removingName,
	); err != nil {
		return fmt.Errorf("isolate completed run %s: %w", lease.id, err)
	}
	if err := owner.durability.Sync(authority.root); err != nil {
		return fmt.Errorf("sync isolated run %s: %w", lease.id, err)
	}
	if err := owner.removeUnpublished(authority, removingName); err != nil {
		return fmt.Errorf("remove completed run %s: %w", lease.id, err)
	}
	return nil
}

func (owner *Owner) completeLive(ctx context.Context, lease *runLease) error {
	lease.mu.Lock()
	if lease.closed || lease.retained {
		lease.mu.Unlock()
		return errors.New("live run is closed or retained by process containment")
	}
	recovery, err := openLockFile(
		lease.root,
		recoveryLockName,
		filepath.Join(runRootLabel(lease.id), recoveryLockName),
	)
	if err != nil {
		lease.mu.Unlock()
		return errors.Join(fmt.Errorf("open recovery lock for completion: %w", err), lease.close())
	}
	if err := lockWithContext(ctx, recovery); err != nil {
		_ = recovery.Close()
		lease.mu.Unlock()
		return errors.Join(fmt.Errorf("lock recovery file for completion: %w", err), lease.close())
	}
	lease.recovery = recovery
	lease.mu.Unlock()

	authority, err := owner.openAuthority()
	if err != nil {
		return errors.Join(err, lease.close())
	}
	removeErr := owner.removeClaimedRun(ctx, authority, lease)
	closeErr := authority.Close()
	return errors.Join(removeErr, closeErr, lease.close())
}

func validateFixedEntries(directory *os.File, allowMissing bool) error {
	names, err := readDirectoryNames(directory)
	if err != nil {
		return err
	}
	found := map[string]bool{}
	for _, name := range names {
		if name != livenessLockName && name != recoveryLockName {
			return fmt.Errorf("unexpected entry %q", name)
		}
		file, err := openLockFile(directory, name, filepath.Join(directory.Name(), name))
		if err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		found[name] = true
	}
	if !allowMissing && (!found[livenessLockName] || !found[recoveryLockName]) {
		return errors.New("run root is missing a lock file")
	}
	return nil
}

func isTemporaryName(name string) bool {
	return validTemporarySuffix(strings.TrimPrefix(name, stagingPrefix), name != strings.TrimPrefix(name, stagingPrefix)) ||
		validTemporarySuffix(strings.TrimPrefix(name, removingPrefix), name != strings.TrimPrefix(name, removingPrefix))
}

func validTemporarySuffix(suffix string, matched bool) bool {
	if !matched || len(suffix) != 65 || suffix[32] != '-' {
		return false
	}
	_, firstErr := runid.Parse(suffix[:32])
	_, secondErr := runid.Parse(suffix[33:])
	return firstErr == nil && secondErr == nil
}
