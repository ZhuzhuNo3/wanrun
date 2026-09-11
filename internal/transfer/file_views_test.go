package transfer

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

type observedFileViews struct {
	mu             sync.Mutex
	sets           map[runid.ID]*observedActiveFileViews
	preflightErr   error
	preflightCalls int
	closeErr       error
	closeCalls     int
	recoverErr     error
	recoverCalls   int
}

func newObservedFileViews() *observedFileViews {
	return &observedFileViews{sets: make(map[runid.ID]*observedActiveFileViews)}
}

func (views *observedFileViews) preflight() error {
	views.mu.Lock()
	defer views.mu.Unlock()
	views.preflightCalls++
	return views.preflightErr
}

func (views *observedFileViews) open(_ context.Context, run *rundirectory.LiveRun,
	allocation sourcefiles.TransferAllocation, source *sourcefiles.SourceRoot,
) (activeFileViews, error) {
	if source == nil || !allocation.BelongsTo(source) {
		return nil, errors.New("observed file views require matching source allocation")
	}
	root, err := run.OpenRoot()
	if err != nil {
		return nil, err
	}
	if err := unix.Mkdirat(int(root.Fd()), "views", 0o700); err != nil {
		_ = root.Close()
		return nil, err
	}
	set := &observedActiveFileViews{owner: views, root: root, runID: run.ID(),
		baseName: allocation.BaseName(), transfers: make(map[transfernumber.Number]struct{})}
	for transfer := range allocation.Transfers() {
		set.transfers[transfer] = struct{}{}
	}
	views.mu.Lock()
	views.sets[run.ID()] = set
	views.mu.Unlock()
	return set, nil
}

func (views *observedFileViews) recover(_ context.Context, run *rundirectory.StaleRun) error {
	views.mu.Lock()
	views.recoverCalls++
	recoverErr := views.recoverErr
	set := views.sets[run.ID()]
	views.mu.Unlock()
	if recoverErr != nil {
		return recoverErr
	}
	if set != nil {
		_, err := set.close(context.Background())
		return err
	}
	root, err := run.OpenRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	err = unix.Unlinkat(int(root.Fd()), "views", unix.AT_REMOVEDIR)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	return err
}

type observedActiveFileViews struct {
	mu        sync.Mutex
	owner     *observedFileViews
	root      *os.File
	runID     runid.ID
	baseName  string
	transfers map[transfernumber.Number]struct{}
	leases    int
	closed    bool
}

func (views *observedActiveFileViews) openView(number transfernumber.Number) (fileViewLease, error) {
	views.mu.Lock()
	defer views.mu.Unlock()
	if views.closed {
		return nil, errors.New("observed file views are closed")
	}
	if _, ok := views.transfers[number]; !ok {
		return nil, errors.New("observed file view transfer is absent")
	}
	views.leases++
	return &observedFileViewLease{owner: views, accessValue: observedViewAccess{
		runID: views.runID, transfer: number, baseName: views.baseName}}, nil
}

func (views *observedActiveFileViews) close(context.Context) (bool, error) {
	views.mu.Lock()
	defer views.mu.Unlock()
	views.owner.mu.Lock()
	views.owner.closeCalls++
	closeErr := views.owner.closeErr
	views.owner.mu.Unlock()
	if closeErr != nil {
		return true, closeErr
	}
	if views.closed {
		return true, nil
	}
	if views.leases != 0 {
		return false, errors.New("observed file views still have leases")
	}
	views.closed = true
	removeErr := unix.Unlinkat(int(views.root.Fd()), "views", unix.AT_REMOVEDIR)
	rootCloseErr := views.root.Close()
	views.owner.mu.Lock()
	delete(views.owner.sets, views.runID)
	views.owner.mu.Unlock()
	return true, errors.Join(removeErr, rootCloseErr)
}

func (views *observedFileViews) hasSet(id runid.ID) bool {
	views.mu.Lock()
	defer views.mu.Unlock()
	return views.sets[id] != nil
}

type observedFileViewLease struct {
	once        sync.Once
	owner       *observedActiveFileViews
	accessValue observedViewAccess
}

func (lease *observedFileViewLease) access() childprocesses.ViewAccess { return lease.accessValue }

func (lease *observedFileViewLease) close() error {
	lease.once.Do(func() {
		lease.owner.mu.Lock()
		lease.owner.leases--
		lease.owner.mu.Unlock()
	})
	return nil
}

type observedViewAccess struct {
	runID    runid.ID
	transfer transfernumber.Number
	baseName string
}

func (access observedViewAccess) RunID() runid.ID                       { return access.runID }
func (access observedViewAccess) TransferNumber() transfernumber.Number { return access.transfer }
func (access observedViewAccess) BaseName() string                      { return access.baseName }
func (observedViewAccess) OpenChildDescriptor() (*os.File, error) {
	return nil, errors.New("observed file view has no process descriptor")
}
