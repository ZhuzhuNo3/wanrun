package transfer

import (
	"context"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/fileviews"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

// fileViewMounts is the run-level FUSE boundary used by the transfer flow.
type fileViewMounts interface {
	preflight() error
	recover(context.Context, *rundirectory.StaleRun) error
	open(context.Context, *rundirectory.LiveRun, sourcefiles.TransferAllocation,
		*sourcefiles.SourceRoot) (activeFileViews, error)
}

// activeFileViews is the owned view set retained until every command is contained.
type activeFileViews interface {
	openView(transfernumber.Number) (fileViewLease, error)
	close(context.Context) (bool, error)
}

type fileViewLease interface {
	access() childprocesses.ViewAccess
	close() error
}

type fuseFileViewMounts struct{ owner *fileviews.FileViews }

func (views fuseFileViewMounts) preflight() error { return views.owner.Preflight() }

func (views fuseFileViewMounts) recover(ctx context.Context, run *rundirectory.StaleRun) error {
	return views.owner.Recover(ctx, run)
}

func (views fuseFileViewMounts) open(ctx context.Context, run *rundirectory.LiveRun,
	allocation sourcefiles.TransferAllocation, source *sourcefiles.SourceRoot,
) (activeFileViews, error) {
	set, err := views.owner.Open(ctx, run, allocation, source)
	if set == nil {
		return nil, err
	}
	return mountedFileViews{set: set}, err
}

type mountedFileViews struct{ set *fileviews.FileViewSet }

func (views mountedFileViews) openView(number transfernumber.Number) (fileViewLease, error) {
	lease, err := views.set.OpenView(number)
	if err != nil {
		return nil, err
	}
	return mountedFileViewLease{lease: lease}, nil
}

func (views mountedFileViews) close(ctx context.Context) (bool, error) {
	err := views.set.Close(ctx)
	return views.set.MountStopped(), err
}

type mountedFileViewLease struct{ lease *fileviews.ViewLease }

func (lease mountedFileViewLease) access() childprocesses.ViewAccess { return lease.lease.Access() }
func (lease mountedFileViewLease) close() error                      { return lease.lease.Close() }
