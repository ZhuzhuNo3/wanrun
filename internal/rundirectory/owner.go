//go:build linux || darwin

package rundirectory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
)

const (
	AuthorityRoot    = "/var/tmp/transferlanes"
	livenessLockName = "liveness.lock"
	recoveryLockName = "recovery.lock"
	stagingPrefix    = ".staging-"
	removingPrefix   = ".removing-"
)

var errRecoveryCallbackRequired = errors.New("stale recovery callback is required")

// Owner is the sole authority for publishing, discovering, and finally
// removing Transfer Lanes run roots.
type Owner struct {
	authorityRoot string
	private       *privateAuthority
	durability    durableFiles
	retryDelay    time.Duration
}

// New returns the production owner rooted at the single recovery authority.
func New() *Owner {
	return &Owner{
		authorityRoot: AuthorityRoot,
		durability:    systemDurability{},
		retryDelay:    10 * time.Millisecond,
	}
}

// CreatePrivateAuthority creates and descriptor-binds a fresh non-production authority for
// isolated package composition and tests. The returned path is observational; Owner never
// reopens it. New remains the only production constructor and is fixed at AuthorityRoot.
func CreatePrivateAuthority() (*Owner, string, error) {
	return createPrivateAuthority(systemDurability{})
}

func newOwner(root string, durability durableFiles) (*Owner, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return nil, fmt.Errorf("invalid authority root %q", root)
	}
	if base := filepath.Base(root); base == "." || base == ".." || base == string(filepath.Separator) {
		return nil, fmt.Errorf("invalid authority root %q", root)
	}
	if durability == nil {
		return nil, errors.New("durability boundary is required")
	}
	return &Owner{
		authorityRoot: root,
		durability:    durability,
		retryDelay:    10 * time.Millisecond,
	}, nil
}

// Create durably publishes one run root while retaining its liveness lease.
func (owner *Owner) Create(id runid.ID) (*LiveRun, error) {
	if err := validateRunID(id); err != nil {
		return nil, err
	}
	authority, err := owner.openAuthority()
	if err != nil {
		return nil, err
	}
	defer authority.Close()

	if err := lockWithContext(context.Background(), authority.root); err != nil {
		return nil, fmt.Errorf("lock authority root: %w", err)
	}
	defer unlock(authority.root)

	return owner.publishRun(authority, id)
}

// RecoverStale offers each provably stale run under an exclusive lease. A nil
// callback result certifies that other owners have finished, allowing this
// owner to remove the fixed run root.
func (owner *Owner) RecoverStale(ctx context.Context, recoverRun func(*StaleRun) error) error {
	if recoverRun == nil {
		return errRecoveryCallbackRequired
	}
	for {
		restart, err := owner.recoverPass(ctx, recoverRun)
		if err != nil {
			return err
		}
		if !restart {
			return nil
		}
		if err := waitForRetry(ctx, owner.retryDelay); err != nil {
			return err
		}
	}
}

func (owner *Owner) recoverPass(ctx context.Context, recoverRun func(*StaleRun) error) (bool, error) {
	authority, err := owner.openAuthority()
	if err != nil {
		return false, err
	}
	defer authority.Close()

	names, err := owner.snapshotRecoveryEntries(ctx, authority)
	if err != nil {
		return false, err
	}
	for _, name := range names {
		id, err := runid.Parse(name)
		if err != nil {
			return false, fmt.Errorf("unexpected entry %q in authority root", name)
		}
		claim, err := owner.claimRun(authority.root, id)
		if err != nil {
			return false, err
		}
		switch claim.kind {
		case claimMissing, claimActive:
			continue
		case claimBusy:
			return true, nil
		case claimStale:
			if err := owner.recoverClaim(ctx, authority, claim.lease, recoverRun); err != nil {
				return false, err
			}
		default:
			return false, errors.New("invalid stale-run claim result")
		}
	}
	return false, nil
}

func (owner *Owner) recoverClaim(
	ctx context.Context,
	authority *authorityDirectory,
	lease *runLease,
	recoverRun func(*StaleRun) error,
) error {
	if err := owner.verifyAuthorityLocation(authority); err != nil {
		return errors.Join(fmt.Errorf("verify recovery authority: %w", err), lease.close())
	}
	stale := &StaleRun{lease: lease}
	if err := recoverRun(stale); err != nil {
		return errors.Join(err, lease.close())
	}
	if err := owner.removeClaimedRun(ctx, authority, lease); err != nil {
		return errors.Join(err, lease.close())
	}
	return lease.close()
}

func validateRunID(id runid.ID) error {
	parsed, err := runid.Parse(id.String())
	if err != nil || parsed != id {
		return fmt.Errorf("invalid run ID: %w", runid.ErrInvalid)
	}
	return nil
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type durableFiles interface {
	Sync(*os.File) error
}

type systemDurability struct{}

func (systemDurability) Sync(file *os.File) error {
	return file.Sync()
}
