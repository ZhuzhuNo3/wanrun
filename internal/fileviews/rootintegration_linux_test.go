//go:build linux && rootintegration

package fileviews

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

func TestRootIntegrationSingleMountReadOnlyCacheAndLiveClose(t *testing.T) {
	requireRootIntegration(t)
	sourcePath := createRootIntegrationSource(t)
	source, err := sourcefiles.OpenSourceRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := source.Scan(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	allocation := allocateRootIntegration(t, snapshot)
	runs, authority, err := rundirectory.CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	defer runs.ClosePrivateAuthority(context.Background())
	id, _ := runid.New()
	live, err := runs.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	set, err := New().Open(context.Background(), live, allocation, source)
	if err != nil {
		t.Fatal(err)
	}

	views := openRootIntegrationViews(t, set, allocation)
	assertSingleMount(t, authority, id, len(views))
	assertLogicalViews(t, views, allocation)
	assertReadOnly(t, views, allocation)
	assertOpenedFileMode(t, sourcePath, views, allocation)
	assertCacheSemantics(t, sourcePath, views, allocation)
	assertExpandedAnchor(t, sourcePath, views, allocation)

	for _, lease := range views {
		if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := set.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	if mountedUnder(filepath.Join(authority, id.String(), viewsDirectoryName)) != 0 {
		t.Fatal("FUSE mount remained after live close")
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRootIntegrationRecoveryRefusesMountedEvidence(t *testing.T) {
	requireRootIntegration(t)
	root, path, id := openEvidenceTestRoot(t)
	createCompleteEvidence(t, root, id)
	backing := t.TempDir()
	mountpoint := filepath.Join(path, viewsDirectoryName)
	if err := unix.Mount(backing, mountpoint, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	mounted := true
	t.Cleanup(func() {
		if mounted {
			_ = unix.Unmount(mountpoint, unix.MNT_DETACH)
		}
	})

	owner := New()
	if err := owner.recover(context.Background(), recoveryTestRunAccess{id: id, root: root}); err == nil {
		t.Fatal("recovered evidence while its mountpoint was still mounted")
	}
	assertFileViewEvidencePresent(t, path)
	if err := unix.Unmount(mountpoint, 0); err != nil {
		t.Fatal(err)
	}
	mounted = false
	if err := owner.recover(context.Background(), recoveryTestRunAccess{id: id, root: root}); err != nil {
		t.Fatalf("recover after unmount: %v", err)
	}
	assertNoFileViewEvidence(t, path)
}

func requireRootIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("TRANSFERLANES_FILEVIEWS_ROOTINTEGRATION") != "1" {
		t.Fatal("TRANSFERLANES_FILEVIEWS_ROOTINTEGRATION=1 is required")
	}
	if os.Geteuid() != 0 {
		t.Fatal("root integration requires effective root")
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Fatalf("root integration requires /dev/fuse: %v", err)
	}
}

func createRootIntegrationSource(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "source")
	external := filepath.Join(t.TempDir(), "external")
	for path, contents := range map[string]string{
		filepath.Join(root, "mutable.txt"):          "old",
		filepath.Join(root, "nested", "inside.txt"): "inside",
		filepath.Join(root, "special-mode.txt"):     "mode",
		filepath.Join(external, "outside.txt"):      "outside",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "special-mode.txt"),
		0o754|os.ModeSetuid|os.ModeSetgid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested/inside.txt", filepath.Join(root, "inside-alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(root, "outside-alias")); err != nil {
		t.Fatal(err)
	}
	return root
}

func assertOpenedFileMode(t *testing.T, source string,
	views map[transfernumber.Number]*ViewLease, allocation sourcefiles.TransferAllocation,
) {
	t.Helper()
	transfer := transferForPath(t, allocation, "special-mode.txt")
	viewPath := filepath.Join(descriptorPath(views[transfer]), "special-mode.txt")
	opened, err := os.Open(viewPath)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	assertPathAndDescriptorMode(t, viewPath, opened, syscall.S_IFREG|0o7754)
	if err := os.Chmod(filepath.Join(source, "special-mode.txt"),
		0o741|os.ModeSetuid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	assertPathAndDescriptorMode(t, viewPath, opened, syscall.S_IFREG|0o5741)
}

func assertPathAndDescriptorMode(t *testing.T, path string, opened *os.File, want uint32) {
	t.Helper()
	pathInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	descriptorInfo, err := opened.Stat()
	if err != nil {
		t.Fatal(err)
	}
	pathMode := uint32(pathInfo.Sys().(*syscall.Stat_t).Mode) & (syscall.S_IFMT | 0o7777)
	descriptorMode := uint32(descriptorInfo.Sys().(*syscall.Stat_t).Mode) & (syscall.S_IFMT | 0o7777)
	if pathMode != want || descriptorMode != want || descriptorMode != pathMode {
		t.Fatalf("path mode=%#o descriptor mode=%#o want=%#o", pathMode, descriptorMode, want)
	}
}

func allocateRootIntegration(t *testing.T,
	snapshot sourcefiles.SourceSnapshot,
) sourcefiles.TransferAllocation {
	t.Helper()
	var values []sourcefiles.TransferWeight
	for index := 1; index <= 2; index++ {
		id, _ := transfernumber.New(index)
		weight, _ := sourcefiles.NewTransferWeight(id, 1)
		values = append(values, weight)
	}
	weights, _ := sourcefiles.NewTransferWeights(values)
	allocation, err := sourcefiles.AllocateTransferItems(snapshot, weights)
	if err != nil {
		t.Fatal(err)
	}
	return allocation
}

func openRootIntegrationViews(t *testing.T, set *FileViewSet,
	allocation sourcefiles.TransferAllocation,
) map[transfernumber.Number]*ViewLease {
	t.Helper()
	result := make(map[transfernumber.Number]*ViewLease)
	for transfer := range allocation.Transfers() {
		lease, err := set.OpenView(transfer)
		if err != nil {
			t.Fatal(err)
		}
		result[transfer] = lease
	}
	return result
}

func assertSingleMount(t *testing.T, authority string, id runid.ID, viewCount int) {
	t.Helper()
	mountpoint := filepath.Join(authority, id.String(), viewsDirectoryName)
	if count := mountedUnder(mountpoint); count != 1 {
		t.Fatalf("mount count=%d want=1", count)
	}
	seen := make(map[uint64]struct{})
	for transfer := 1; transfer <= viewCount; transfer++ {
		number, _ := transfernumber.New(transfer)
		path := filepath.Join(mountpoint, transferDirectoryName(number), "source")
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		mount, err := mountID(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		seen[mount] = struct{}{}
	}
	if len(seen) != 1 {
		t.Fatalf("transfer views span %d mounts", len(seen))
	}
}

func mountedUnder(path string) int {
	contents, _ := os.ReadFile("/proc/self/mountinfo")
	count := 0
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 4 && fields[4] == path {
			count++
		}
	}
	return count
}

func assertLogicalViews(t *testing.T, views map[transfernumber.Number]*ViewLease,
	allocation sourcefiles.TransferAllocation,
) {
	t.Helper()
	seen := make(map[string]struct{})
	for transfer := range allocation.Transfers() {
		root := descriptorPath(views[transfer])
		walkRoot := root + string(filepath.Separator) + "."
		actual := make(map[string]bool)
		if err := filepath.WalkDir(walkRoot, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == walkRoot {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				t.Fatalf("view contains symlink %q", path)
			}
			relative, err := filepath.Rel(walkRoot, path)
			if err != nil {
				return err
			}
			if !entry.IsDir() && !entry.Type().IsRegular() {
				t.Fatalf("view contains unsupported entry %q", relative)
			}
			actual[relative] = entry.IsDir()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if want := expectedAllocationEntries(allocation, transfer); !reflect.DeepEqual(actual, want) {
			t.Fatalf("transfer %d entries=%v want=%v", transfer.Value(), actual, want)
		}
		for file := range allocation.Files(transfer) {
			path := filepath.Join(root, file.RelativePath())
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() {
				t.Fatalf("logical file %q info=%v err=%v", file.RelativePath(), info, err)
			}
			if _, duplicate := seen[file.RelativePath()]; duplicate {
				t.Fatalf("duplicate logical file %q", file.RelativePath())
			}
			seen[file.RelativePath()] = struct{}{}
		}
		for directory := range allocation.EmptyDirectories(transfer) {
			info, err := os.Stat(filepath.Join(root, directory.RelativePath()))
			if err != nil || !info.IsDir() {
				t.Fatalf("logical empty directory %q info=%v err=%v", directory.RelativePath(), info, err)
			}
		}
	}
}

func expectedAllocationEntries(allocation sourcefiles.TransferAllocation,
	transfer transfernumber.Number,
) map[string]bool {
	result := make(map[string]bool)
	addParents := func(path string) {
		for parent := filepath.Dir(path); parent != "."; parent = filepath.Dir(parent) {
			result[parent] = true
		}
	}
	for file := range allocation.Files(transfer) {
		result[file.RelativePath()] = false
		addParents(file.RelativePath())
	}
	for directory := range allocation.EmptyDirectories(transfer) {
		result[directory.RelativePath()] = true
		addParents(directory.RelativePath())
	}
	return result
}

func assertReadOnly(t *testing.T, views map[transfernumber.Number]*ViewLease,
	allocation sourcefiles.TransferAllocation,
) {
	t.Helper()
	transfer := transferForPath(t, allocation, "mutable.txt")
	root := descriptorPath(views[transfer])
	file := filepath.Join(root, "mutable.txt")
	for name, mutate := range map[string]func() error{
		"create":  func() error { return os.WriteFile(filepath.Join(root, "new"), []byte("x"), 0o600) },
		"write":   func() error { _, err := os.OpenFile(file, os.O_WRONLY, 0); return err },
		"rename":  func() error { return os.Rename(file, filepath.Join(root, "renamed")) },
		"link":    func() error { return os.Link(file, filepath.Join(root, "hard-link")) },
		"symlink": func() error { return os.Symlink("mutable.txt", filepath.Join(root, "soft-link")) },
		"remove":  func() error { return os.Remove(file) },
		"truncate": func() error {
			return os.Truncate(file, 0)
		},
		"chmod": func() error { return os.Chmod(root, 0o700) },
		"chtimes": func() error {
			now := time.Now()
			return os.Chtimes(file, now, now)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := mutate(); err == nil || !errors.Is(err, syscall.EROFS) {
				t.Fatalf("mutation error=%v want EROFS", err)
			}
		})
	}
}

func assertCacheSemantics(t *testing.T, source string,
	views map[transfernumber.Number]*ViewLease, allocation sourcefiles.TransferAllocation,
) {
	t.Helper()
	transfer := transferForPath(t, allocation, "mutable.txt")
	viewPath := filepath.Join(descriptorPath(views[transfer]), "mutable.txt")
	opened, err := os.Open(viewPath)
	if err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(source, "replacement")
	if err := os.WriteFile(replacement, []byte("new contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantTime := time.Unix(1_700_000_000, 123_000_000)
	if err := os.Chtimes(replacement, wantTime, wantTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, filepath.Join(source, "mutable.txt")); err != nil {
		t.Fatal(err)
	}
	old, err := io.ReadAll(opened)
	_ = opened.Close()
	if err != nil || string(old) != "old" {
		t.Fatalf("already-open read=%q err=%v", old, err)
	}
	current, err := os.ReadFile(viewPath)
	if err != nil || string(current) != "new contents" {
		t.Fatalf("reopened read=%q err=%v", current, err)
	}
	info, err := os.Stat(viewPath)
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != int64(len(current)) ||
		info.ModTime().UnixNano() != wantTime.UnixNano() {
		t.Fatalf("refreshed metadata=%v err=%v", info, err)
	}
	if err := os.WriteFile(filepath.Join(source, "added-after-scan"), []byte("later"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(descriptorPath(views[transfer]), "added-after-scan")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new member became visible: %v", err)
	}
}

func assertExpandedAnchor(t *testing.T, source string,
	views map[transfernumber.Number]*ViewLease, allocation sourcefiles.TransferAllocation,
) {
	t.Helper()
	transfer := transferForPath(t, allocation, "outside-alias/outside.txt")
	path := filepath.Join(descriptorPath(views[transfer]), "outside-alias", "outside.txt")
	replacement := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(replacement, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, "outside-alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, filepath.Join(source, "outside-alias")); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "outside" {
		t.Fatalf("expanded external read=%q err=%v", contents, err)
	}
}

func transferForPath(t *testing.T, allocation sourcefiles.TransferAllocation,
	path string,
) transfernumber.Number {
	t.Helper()
	if transfer, exists := allocation.OwnerOf(path); exists {
		return transfer
	}
	t.Fatalf("path %q was not allocated", path)
	return transfernumber.Number{}
}

func descriptorPath(lease *ViewLease) string {
	return filepath.Join("/proc/self/fd", fmt.Sprintf("%d", lease.Descriptor()))
}
