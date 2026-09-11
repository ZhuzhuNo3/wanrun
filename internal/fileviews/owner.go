package fileviews

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type runAccess interface {
	ID() runid.ID
	OpenRoot() (*os.File, error)
}

// FileViews owns creation and stale recovery for one run's read-only views.
type FileViews struct{ mounts viewMountOpener }

type viewMountOpener interface {
	open(string, *mountRoot) (mountedViewFilesystem, error)
}

type systemViewMountOpener struct{}

func (systemViewMountOpener) open(path string, root *mountRoot) (mountedViewFilesystem, error) {
	return mountReadOnlyViews(path, root)
}

func New() *FileViews { return &FileViews{mounts: systemViewMountOpener{}} }

// Preflight verifies the local FUSE prerequisite without changing host network state.
func (*FileViews) Preflight() error { return preflightMount() }

// Open mounts one filesystem containing every transfer view. A non-nil set returned with an
// error still owns a live partial mount and must be closed by the caller.
func (owner *FileViews) Open(ctx context.Context, run *rundirectory.LiveRun,
	allocation sourcefiles.TransferAllocation, source *sourcefiles.SourceRoot,
) (*FileViewSet, error) {
	if run == nil {
		return nil, errors.New("live run is required for file views")
	}
	return owner.open(ctx, run, allocation, source)
}

func (owner *FileViews) open(ctx context.Context, run runAccess,
	allocation sourcefiles.TransferAllocation, source *sourcefiles.SourceRoot,
) (*FileViewSet, error) {
	if ctx == nil || run == nil || owner == nil || owner.mounts == nil {
		return nil, errors.New("file views require context and run access")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := owner.Preflight(); err != nil {
		return nil, fmt.Errorf("preflight FUSE file views: %w", err)
	}
	tree, access, err := prepareLogicalViews(allocation, source)
	if err != nil {
		return nil, err
	}
	return openFileViewSet(run, allocation, tree, access, owner.mounts)
}

func prepareLogicalViews(allocation sourcefiles.TransferAllocation,
	source *sourcefiles.SourceRoot,
) (*viewTree, *sourcefiles.SourceAccess, error) {
	if !allocation.BelongsTo(source) {
		return nil, nil, errors.New("file views require the source directory that produced the allocation")
	}
	tree, err := newViewTree(allocation)
	if err != nil {
		return nil, nil, fmt.Errorf("build file-view tree: %w", err)
	}
	access, err := source.AcquireAccess(allocation.Snapshot())
	if err != nil {
		return nil, nil, fmt.Errorf("acquire source access for file views: %w", err)
	}
	return tree, access, nil
}

func openFileViewSet(run runAccess, allocation sourcefiles.TransferAllocation,
	tree *viewTree, access *sourcefiles.SourceAccess, mounts viewMountOpener,
) (*FileViewSet, error) {
	root, err := openRunRoot(run)
	if err != nil {
		return nil, errors.Join(err, access.Close())
	}
	ownership, err := prepareMountpoint(root, run.ID())
	if err != nil {
		return nil, errors.Join(err, root.Close(), access.Close())
	}
	mounted, err := mounts.open(mountpointPath(root), &mountRoot{tree: tree, access: access})
	if err != nil {
		return nil, errors.Join(fmt.Errorf("mount read-only file views: %w", err),
			removePreparedEvidence(root, ownership), root.Close(), access.Close())
	}
	if mounted == nil {
		return nil, errors.Join(errors.New("mount read-only file views returned no owner"),
			removePreparedEvidence(root, ownership), root.Close(), access.Close())
	}
	views, err := openMountedViews(root, allocation)
	set := newFileViewSet(root, ownership, run.ID(), allocation.BaseName(), views, mounted, access)
	if err != nil {
		return set, err
	}
	return set, nil
}

func openRunRoot(run runAccess) (*os.File, error) {
	root, err := run.OpenRoot()
	if err != nil {
		return nil, fmt.Errorf("open live run root for file views: %w", err)
	}
	if err := validatePrivateDirectory(root); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("validate live run root for file views: %w", err)
	}
	return root, nil
}

func openMountedViews(root *os.File, allocation sourcefiles.TransferAllocation) (
	map[transfernumber.Number]*os.File, error,
) {
	result := make(map[transfernumber.Number]*os.File, allocation.TransferCount())
	for transfer := range allocation.Transfers() {
		path := viewsDirectoryName + "/" + transferDirectoryName(transfer) + "/" + allocation.BaseName()
		view, err := openDirectoryAt(root, path, false)
		if err != nil {
			return result, fmt.Errorf("open transfer %d mounted view: %w", transfer.Value(), err)
		}
		result[transfer] = view
	}
	return result, nil
}

// Recover converges safe, unmounted file-view evidence left in a stale run. Complete
// ownership evidence must still match that exact run and mountpoint identity.
func (owner *FileViews) Recover(ctx context.Context, run *rundirectory.StaleRun) error {
	if run == nil {
		return errors.New("stale run is required for file-view recovery")
	}
	return owner.recover(ctx, run)
}

func (owner *FileViews) recover(ctx context.Context, run runAccess) error {
	if ctx == nil || run == nil || owner == nil {
		return errors.New("file-view recovery requires context and run access")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := openRunRoot(run)
	if err != nil {
		return err
	}
	defer root.Close()
	return recoverFileViewEvidence(root, run.ID(), descriptorFileViewEvidenceFiles{})
}

func transferDirectoryName(id transfernumber.Number) string {
	return fmt.Sprintf("transfer-%03d", id.Value())
}

func parseTransferDirectoryName(name string, transfers []transfernumber.Number) (transfernumber.Number, bool) {
	for _, transfer := range transfers {
		if transferDirectoryName(transfer) == name {
			return transfer, true
		}
	}
	return transfernumber.Number{}, false
}
