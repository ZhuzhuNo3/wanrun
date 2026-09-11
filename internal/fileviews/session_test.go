//go:build linux

package fileviews

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestFileViewSetRetainsSourceAndEvidenceUntilMountStopIsConfirmed(t *testing.T) {
	for _, failure := range []string{"unmount", "server wait"} {
		t.Run(failure, func(t *testing.T) {
			source, allocation := viewTreeFixture(t)
			access, err := source.AcquireAccess(allocation.Snapshot())
			if err != nil {
				t.Fatal(err)
			}
			path := t.TempDir()
			root, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			id, err := runid.New()
			if err != nil {
				t.Fatal(err)
			}
			ownership, err := prepareMountpoint(root, id)
			if err != nil {
				t.Fatal(err)
			}
			stopErr := errors.New(failure + " failed")
			mounted := &observedMountedView{}
			if failure == "unmount" {
				mounted.unmountErr = stopErr
			} else {
				mounted.waitErr = stopErr
			}
			set := newFileViewSet(root, ownership, id, allocation.BaseName(),
				map[transfernumber.Number]*os.File{}, mounted, access)

			if err := set.Close(context.Background()); !errors.Is(err, stopErr) {
				t.Fatalf("first Close error = %v, want %v", err, stopErr)
			}
			if set.MountStopped() {
				t.Fatal("failed mount stop was reported as confirmed")
			}
			for _, name := range []string{viewsDirectoryName, ownershipFileName} {
				if _, err := os.Lstat(filepath.Join(path, name)); err != nil {
					t.Fatalf("retained evidence %q: %v", name, err)
				}
			}
			if err := source.Close(); !errors.Is(err, sourcefiles.ErrSourceRootBorrowed) {
				t.Fatalf("source Close = %v, want borrowed", err)
			}

			mounted.unmountErr = nil
			mounted.waitErr = nil
			if err := set.Close(context.Background()); err != nil {
				t.Fatalf("retry Close: %v", err)
			}
			if !set.MountStopped() {
				t.Fatal("completed mount stop was not reported")
			}
			for _, name := range []string{viewsDirectoryName, ownershipFileName} {
				if _, err := os.Lstat(filepath.Join(path, name)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("evidence %q remains: %v", name, err)
				}
			}
			if err := source.Close(); err != nil {
				t.Fatalf("source remained borrowed after retry: %v", err)
			}
		})
	}
}

func TestPartialViewOpenReturnsTheMountedOwnerForCleanup(t *testing.T) {
	source, allocation := viewTreeFixture(t)
	tree, err := newViewTree(allocation)
	if err != nil {
		t.Fatal(err)
	}
	access, err := source.AcquireAccess(allocation.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	runPath := t.TempDir()
	if err := os.Chmod(runPath, 0o700); err != nil {
		t.Fatal(err)
	}
	runRoot, err := os.Open(runPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runRoot.Close() })
	id, err := runid.New()
	if err != nil {
		t.Fatal(err)
	}
	mounts := &partialViewMount{baseName: allocation.BaseName(), mounted: &observedMountedView{}}
	set, openErr := openFileViewSet(staticRunAccess{id: id, root: runRoot}, allocation, tree, access, mounts)
	if openErr == nil || set == nil {
		t.Fatalf("partial view open returned set=%v error=%v", set, openErr)
	}
	first, _ := transfernumber.New(1)
	lease, err := set.OpenView(first)
	if err != nil {
		t.Fatalf("completed partial view was not owned: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); !errors.Is(err, sourcefiles.ErrSourceRootBorrowed) {
		t.Fatalf("partial mount lost its source lease: %v", err)
	}
	if err := set.Close(context.Background()); err != nil {
		t.Fatalf("close partial view owner: %v", err)
	}
	if !set.MountStopped() || mounts.mounted.unmountCalls != 1 || mounts.mounted.waitCalls != 1 {
		t.Fatalf("partial mount close: stopped=%t unmount=%d wait=%d",
			set.MountStopped(), mounts.mounted.unmountCalls, mounts.mounted.waitCalls)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("partial mount retained source after close: %v", err)
	}
}

type staticRunAccess struct {
	id   runid.ID
	root *os.File
}

func (run staticRunAccess) ID() runid.ID { return run.id }

func (run staticRunAccess) OpenRoot() (*os.File, error) {
	return duplicateFileViewDescriptor(run.root, "file-view-test-run")
}

type partialViewMount struct {
	baseName string
	mounted  *observedMountedView
}

func (mount *partialViewMount) open(path string, _ *mountRoot) (mountedViewFilesystem, error) {
	view := filepath.Join(path, transferDirectoryName(mustTransferNumber(1)), mount.baseName)
	if err := os.MkdirAll(view, 0o700); err != nil {
		return nil, err
	}
	mount.mounted.onUnmount = func() error {
		return os.RemoveAll(filepath.Join(path, transferDirectoryName(mustTransferNumber(1))))
	}
	return mount.mounted, nil
}

func mustTransferNumber(value int) transfernumber.Number {
	number, _ := transfernumber.New(value)
	return number
}

type observedMountedView struct {
	unmountErr   error
	waitErr      error
	unmountCalls int
	waitCalls    int
	onUnmount    func() error
}

func (mounted *observedMountedView) unmount() error {
	mounted.unmountCalls++
	if mounted.unmountErr != nil {
		return mounted.unmountErr
	}
	if mounted.onUnmount != nil {
		return mounted.onUnmount()
	}
	return nil
}

func (mounted *observedMountedView) wait(context.Context) error {
	mounted.waitCalls++
	return mounted.waitErr
}
