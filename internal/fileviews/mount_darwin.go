//go:build darwin

package fileviews

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

type mountedFilesystem struct{}

func preflightMount() error { return errors.New("FUSE file views require a supported Linux build") }
func mountReadOnlyViews(string, *mountRoot) (*mountedFilesystem, error) {
	return nil, preflightMount()
}
func (*mountedFilesystem) unmount() error             { return nil }
func (*mountedFilesystem) wait(context.Context) error { return nil }
func mountpointPath(*os.File) string                  { return "" }
func directorySharesMount(left, right *os.File) (bool, error) {
	var leftStatus, rightStatus unix.Statfs_t
	if err := unix.Fstatfs(int(left.Fd()), &leftStatus); err != nil {
		return false, err
	}
	if err := unix.Fstatfs(int(right.Fd()), &rightStatus); err != nil {
		return false, err
	}
	return leftStatus.Fsid == rightStatus.Fsid, nil
}
