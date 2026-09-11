package fileviews

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

var ErrFileViewSetClosing = errors.New("file-view set is closing")

// FileViewSet solely owns a live FUSE server, mount, descriptors, and source lease.
type FileViewSet struct {
	mu              sync.Mutex
	closeMu         sync.Mutex
	root            *os.File
	ownership       fileViewOwnership
	runID           runid.ID
	baseName        string
	views           map[transfernumber.Number]*os.File
	mounted         mountedViewFilesystem
	access          *sourcefiles.SourceAccess
	leases          int
	drained         chan struct{}
	closing         bool
	closed          bool
	unmounted       bool
	stopped         bool
	evidenceRemoved bool
	descriptorErr   error
}

type mountedViewFilesystem interface {
	unmount() error
	wait(context.Context) error
}

type ViewLease struct {
	once        sync.Once
	descriptors *viewDescriptors
	runID       runid.ID
	transfer    transfernumber.Number
	baseName    string
	set         *FileViewSet
	err         error
}

type ViewAccess struct {
	descriptors *viewDescriptors
	runID       runid.ID
	transfer    transfernumber.Number
	baseName    string
}

type viewDescriptors struct {
	mu     sync.Mutex
	root   *os.File
	closed bool
}

func newFileViewSet(root *os.File, ownership fileViewOwnership, runID runid.ID,
	baseName string, views map[transfernumber.Number]*os.File, mounted mountedViewFilesystem,
	access *sourcefiles.SourceAccess,
) *FileViewSet {
	return &FileViewSet{root: root, ownership: ownership, runID: runID, baseName: baseName,
		views: views, mounted: mounted, access: access, drained: make(chan struct{})}
}

func (set *FileViewSet) OpenView(id transfernumber.Number) (*ViewLease, error) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closing || set.closed {
		return nil, ErrFileViewSetClosing
	}
	base, exists := set.views[id]
	if !exists {
		return nil, fmt.Errorf("transfer %d is not part of this file-view set", id.Value())
	}
	root, err := duplicateFileViewDescriptor(base, "transferlanes-file-view-root")
	if err != nil {
		return nil, err
	}
	set.leases++
	return &ViewLease{descriptors: &viewDescriptors{root: root}, runID: set.runID,
		transfer: id, baseName: set.baseName, set: set}, nil
}

func (lease *ViewLease) Descriptor() uintptr {
	lease.descriptors.mu.Lock()
	defer lease.descriptors.mu.Unlock()
	if lease.descriptors.closed {
		return ^uintptr(0)
	}
	return lease.descriptors.root.Fd()
}

func (lease *ViewLease) Access() ViewAccess {
	return ViewAccess{descriptors: lease.descriptors, runID: lease.runID,
		transfer: lease.transfer, baseName: lease.baseName}
}

func (access ViewAccess) RunID() runid.ID                       { return access.runID }
func (access ViewAccess) TransferNumber() transfernumber.Number { return access.transfer }
func (access ViewAccess) BaseName() string                      { return access.baseName }

func (access ViewAccess) OpenChildDescriptor() (*os.File, error) {
	if access.descriptors == nil {
		return nil, errors.New("file-view access is absent")
	}
	access.descriptors.mu.Lock()
	defer access.descriptors.mu.Unlock()
	if access.descriptors.closed {
		return nil, errors.New("file-view lease is closed")
	}
	return openChildViewDescriptor(access.descriptors.root)
}

func (lease *ViewLease) Close() error {
	lease.once.Do(func() {
		lease.descriptors.mu.Lock()
		lease.descriptors.closed = true
		lease.err = lease.descriptors.root.Close()
		lease.descriptors.mu.Unlock()
		lease.set.releaseLease()
	})
	return lease.err
}

func (set *FileViewSet) releaseLease() {
	set.mu.Lock()
	defer set.mu.Unlock()
	set.leases--
	if set.closing && set.leases == 0 {
		select {
		case <-set.drained:
		default:
			close(set.drained)
		}
	}
}

// Close drains view leases, stops FUSE, returns source access, and removes evidence.
// A failed mount stop retains every dependent capability so a later Close can retry safely.
func (set *FileViewSet) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("file-view close context is required")
	}
	if err := set.beginClose(ctx); err != nil {
		return err
	}
	set.closeMu.Lock()
	defer set.closeMu.Unlock()
	if set.closed {
		return nil
	}
	set.closeViewDescriptors()
	if err := set.stopMountedFilesystem(ctx); err != nil {
		return errors.Join(set.descriptorErr, err)
	}
	accessErr := set.returnSourceAccess()
	if err := set.removeOwnershipEvidence(); err != nil {
		return errors.Join(set.descriptorErr, accessErr, err)
	}
	rootErr := set.root.Close()
	set.root = nil
	set.closed = true
	return errors.Join(set.descriptorErr, accessErr, rootErr)
}

// MountStopped reports whether Close has confirmed that the FUSE server no longer serves the
// mount. Callers use this distinction to retain source and run evidence after an uncertain stop.
func (set *FileViewSet) MountStopped() bool {
	if set == nil {
		return true
	}
	set.closeMu.Lock()
	defer set.closeMu.Unlock()
	return set.stopped
}

func (set *FileViewSet) closeViewDescriptors() {
	if set.views == nil {
		return
	}
	set.descriptorErr = closeViewFiles(set.views)
	set.views = nil
}

func (set *FileViewSet) stopMountedFilesystem(ctx context.Context) error {
	if !set.unmounted {
		if err := set.mounted.unmount(); err != nil {
			return fmt.Errorf("unmount file views: %w", err)
		}
		set.unmounted = true
	}
	if !set.stopped {
		if err := set.mounted.wait(ctx); err != nil {
			return err
		}
		set.stopped = true
	}
	return nil
}

func (set *FileViewSet) returnSourceAccess() error {
	if set.access != nil {
		err := set.access.Close()
		set.access = nil
		if err != nil {
			return fmt.Errorf("return source access: %w", err)
		}
	}
	return nil
}

func (set *FileViewSet) removeOwnershipEvidence() error {
	if !set.evidenceRemoved {
		if err := removePreparedEvidence(set.root, set.ownership); err != nil {
			return err
		}
		set.evidenceRemoved = true
	}
	return nil
}

func (set *FileViewSet) beginClose(ctx context.Context) error {
	set.mu.Lock()
	if !set.closing {
		set.closing = true
		if set.leases == 0 {
			close(set.drained)
		}
	}
	drained := set.drained
	set.mu.Unlock()
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func closeViewFiles(files map[transfernumber.Number]*os.File) error {
	failures := make([]error, 0, len(files))
	for _, file := range files {
		if file != nil {
			failures = append(failures, file.Close())
		}
	}
	return errors.Join(failures...)
}
