//go:build linux

package fileviews

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	goFuseFS "github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	"golang.org/x/sys/unix"
)

type mountedFilesystem struct {
	server  *fuse.Server
	stopped chan struct{}
}

func preflightMount() error {
	if os.Geteuid() != 0 {
		return errors.New("FUSE file views require effective root")
	}
	device, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open /dev/fuse: %w", err)
	}
	return device.Close()
}

func mountReadOnlyViews(path string, root *mountRoot) (*mountedFilesystem, error) {
	zero := time.Duration(0)
	entry := time.Hour
	server, err := goFuseFS.Mount(path, root, &goFuseFS.Options{
		AttrTimeout:     &zero,
		EntryTimeout:    &entry,
		NegativeTimeout: &entry,
		NullPermissions: true,
		MountOptions: fuse.MountOptions{
			DirectMountStrict: true,
			DirectMountFlags:  syscall.MS_RDONLY | syscall.MS_NOSUID | syscall.MS_NODEV,
			FsName:            "transferlanes",
			Name:              "transferlanes",
			Options:           []string{"ro"},
			DisableXAttrs:     true,
		},
	})
	if err != nil {
		return nil, err
	}
	mounted := &mountedFilesystem{server: server, stopped: make(chan struct{})}
	go func() {
		server.Wait()
		close(mounted.stopped)
	}()
	return mounted, nil
}

func (mounted *mountedFilesystem) unmount() error { return mounted.server.Unmount() }

func (mounted *mountedFilesystem) wait(ctx context.Context) error {
	select {
	case <-mounted.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func mountpointPath(root *os.File) string {
	return filepath.Join("/proc/self/fd", fmt.Sprintf("%d", root.Fd()), viewsDirectoryName)
}

func directorySharesMount(left, right *os.File) (bool, error) {
	leftMount, err := mountID(left)
	if err != nil {
		return false, err
	}
	rightMount, err := mountID(right)
	return leftMount == rightMount, err
}

func mountID(directory *os.File) (uint64, error) {
	var status unix.Statx_t
	if err := unix.Statx(int(directory.Fd()), "", unix.AT_EMPTY_PATH|unix.AT_NO_AUTOMOUNT,
		unix.STATX_MNT_ID, &status); err != nil {
		return 0, err
	}
	if status.Mask&unix.STATX_MNT_ID == 0 {
		return 0, errors.New("mount identity is unavailable")
	}
	return status.Mnt_id, nil
}
