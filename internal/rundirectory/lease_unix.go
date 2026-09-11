//go:build linux || darwin

package rundirectory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"golang.org/x/sys/unix"
)

// LiveRun retains the liveness lease for a durably published run root.
type LiveRun struct {
	lease *runLease
}

func (run *LiveRun) ID() runid.ID { return run.lease.id }

// OpenRoot duplicates a descriptor bound to this exact run-root inode. The
// caller must close it; replacing the directory entry cannot redirect it.
func (run *LiveRun) OpenRoot() (*os.File, error) { return run.lease.openRoot() }

// Close releases liveness without deleting the root, making it recoverable.
func (run *LiveRun) Close() error { return run.lease.close() }

// Complete removes the fixed run root after all other live owners are closed. If the context
// expires while acquiring deletion authority, liveness is released and the root remains stale.
func (run *LiveRun) Complete(ctx context.Context) error {
	if ctx == nil {
		return errors.New("run completion context is required")
	}
	return run.lease.owner.completeLive(ctx, run.lease)
}

// RetainLivenessUntil transfers release of the liveness lease to one process-containment
// completion signal. After this call, the LiveRun cannot be opened, completed, or closed by its
// former caller; the exact liveness descriptor remains held until done closes or the supervisor
// process exits.
func (run *LiveRun) RetainLivenessUntil(done <-chan struct{}) error {
	if done == nil {
		return errors.New("process-containment completion signal is required")
	}
	return run.lease.retainUntil(done)
}

// StaleRun is an exclusive, callback-scoped lease over one stale run root.
// Its release and final deletion remain owned by RecoverStale.
type StaleRun struct {
	lease *runLease
}

func (run *StaleRun) ID() runid.ID { return run.lease.id }

// OpenRoot duplicates a descriptor bound to this exact stale run-root inode.
// The caller must close it; replacing the directory entry cannot redirect it.
func (run *StaleRun) OpenRoot() (*os.File, error) { return run.lease.openRoot() }

type runLease struct {
	mu       sync.Mutex
	owner    *Owner
	id       runid.ID
	root     *os.File
	live     *os.File
	recovery *os.File
	closed   bool
	retained bool
}

func (lease *runLease) openRoot() (*os.File, error) {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed || lease.retained {
		return nil, errors.New("run root lease is closed")
	}
	fd, err := unix.FcntlInt(lease.root.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("duplicate run root descriptor: %w", err)
	}
	return os.NewFile(uintptr(fd), runRootLabel(lease.id)), nil
}

func runRootLabel(id runid.ID) string { return "transferlanes-run-root-" + id.String() }

func (lease *runLease) close() error {
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.closed {
		return nil
	}
	if lease.retained {
		return errors.New("run liveness release belongs to process containment")
	}
	return lease.closeLocked()
}

func (lease *runLease) closeLocked() error {
	lease.closed = true
	return closeRunFiles(lease.live, lease.recovery, lease.root)
}

func (lease *runLease) retainUntil(done <-chan struct{}) error {
	lease.mu.Lock()
	if lease.closed || lease.retained {
		lease.mu.Unlock()
		return errors.New("run liveness lease cannot be transferred")
	}
	lease.retained = true
	lease.mu.Unlock()
	go func() {
		<-done
		lease.mu.Lock()
		lease.retained = false
		_ = lease.closeLocked()
		lease.mu.Unlock()
	}()
	return nil
}
