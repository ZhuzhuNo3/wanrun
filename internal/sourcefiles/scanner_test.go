//go:build linux || darwin

package sourcefiles

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestScanDefaultOmitsSymlinksAndIncludesLogicalEmptyDirectories(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source")
	mustMkdir(t, filepath.Join(root, "empty"))
	mustMkdir(t, filepath.Join(root, "only-ignored"))
	mustWrite(t, filepath.Join(root, "nested", "file"), "contents")
	mustWrite(t, filepath.Join(root, ".hidden"), "hidden")
	mustSymlink(t, "../nested/file", filepath.Join(root, "only-ignored", "link"))
	mustSymlink(t, "nested/file", filepath.Join(root, "file-link"))
	if err := unix.Mkfifo(filepath.Join(root, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}

	owned, snapshot, err := openAndScan(t, root, false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filePaths(snapshot), []string{".hidden", "nested/file"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("files=%v want=%v", got, want)
	}
	if got, want := emptyPaths(snapshot), []string{"empty", "only-ignored"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("empty directories=%v want=%v", got, want)
	}
	summary := snapshot.Summary()
	if summary.FollowedSymlinks() != 0 || summary.IgnoredSymlinks() != 2 ||
		summary.EmptyDirectories() != 2 || summary.IgnoredSpecialFiles() != 1 {
		t.Fatalf("summary=%#v", summary)
	}
	if _, err := owned.Scan(context.Background(), false); !errors.Is(err, ErrSourceRootScanned) {
		t.Fatalf("second scan error=%v want ErrSourceRootScanned", err)
	}
}

func TestScanFollowSymlinksExpandsLogicalNamesAndExternalTargets(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source")
	external := filepath.Join(t.TempDir(), "external")
	mustWrite(t, filepath.Join(root, "inside", "file"), "inside")
	mustWrite(t, filepath.Join(external, "outside"), "outside")
	mustMkdir(t, filepath.Join(external, "empty"))
	mustSymlink(t, "inside/file", filepath.Join(root, "file-alias"))
	mustSymlink(t, "file-alias", filepath.Join(root, "multi-alias"))
	mustSymlink(t, "inside", filepath.Join(root, "directory-alias"))
	mustSymlink(t, external, filepath.Join(root, "external-alias"))
	relativeOutside, err := filepath.Rel(root, filepath.Join(external, "outside"))
	if err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, relativeOutside, filepath.Join(root, "parent-alias"))

	owned, snapshot, err := openAndScan(t, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filePaths(snapshot), []string{
		"directory-alias/file", "external-alias/outside", "file-alias", "inside/file", "multi-alias",
		"parent-alias",
	}; !reflect.DeepEqual(got, want) {
		t.Fatalf("files=%v want=%v", got, want)
	}
	if got, want := emptyPaths(snapshot), []string{"external-alias/empty"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("empty directories=%v want=%v", got, want)
	}
	if snapshot.Summary().FollowedSymlinks() != 5 || snapshot.Summary().IgnoredSymlinks() != 0 {
		t.Fatalf("summary=%#v", snapshot.Summary())
	}
	access, err := owned.AcquireAccess(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	for file := range snapshot.Files() {
		opened, _, err := access.OpenRegular(file.Backing())
		if err != nil {
			t.Fatalf("open %q: %v", file.RelativePath(), err)
		}
		contents, readErr := io.ReadAll(opened)
		_ = opened.Close()
		if readErr != nil || len(contents) == 0 {
			t.Fatalf("read %q: %q %v", file.RelativePath(), contents, readErr)
		}
	}
}

func TestScanFollowSymlinkKeepsObservedRootAfterPathReplacement(t *testing.T) {
	parent := t.TempDir()
	sourcePath := filepath.Join(parent, "source")
	mustWrite(t, filepath.Join(sourcePath, "target"), "A")
	mustSymlink(t, "target", filepath.Join(sourcePath, "alias"))
	root, err := OpenSourceRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	borrow, err := root.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	defer borrow.Close()
	observed, err := statSourceAt(borrow.root, "alias")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(sourcePath, filepath.Join(parent, "moved-source")); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(sourcePath, "target"), "BBBB")
	mustSymlink(t, "target", filepath.Join(sourcePath, "alias"))
	pathResolved, err := filepath.EvalSymlinks(filepath.Join(sourcePath, "alias"))
	if err != nil {
		t.Fatal(err)
	}
	pathContents, err := os.ReadFile(pathResolved)
	if err != nil || string(pathContents) != "BBBB" {
		t.Fatalf("replacement path contents=%q err=%v", pathContents, err)
	}
	target, err := resolveSourceSymlink(borrow.root, "alias", observed)
	if err != nil {
		t.Fatal(err)
	}
	if target.anchor == nil || target.kind != sourceRegularObject || target.name != "target" || target.size != 1 {
		t.Fatalf("descriptor target=%#v want original regular file", target)
	}
	if err := target.anchor.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := root.Scan(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filePaths(snapshot), []string{"alias", "target"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("files=%v want=%v", got, want)
	}
	access, err := root.AcquireAccess(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	for file := range snapshot.Files() {
		opened, _, openErr := access.OpenRegular(file.Backing())
		if openErr != nil {
			t.Fatalf("open %q: %v", file.RelativePath(), openErr)
		}
		contents, readErr := io.ReadAll(opened)
		_ = opened.Close()
		if readErr != nil || string(contents) != "A" {
			t.Fatalf("read %q contents=%q err=%v", file.RelativePath(), contents, readErr)
		}
	}
}

func TestScanFollowSymlinksRejectsBrokenAndRecursiveTargets(t *testing.T) {
	for _, test := range []struct {
		name string
		make func(string)
		path string
	}{
		{name: "broken", path: "broken", make: func(root string) {
			mustSymlink(t, "missing", filepath.Join(root, "broken"))
		}},
		{name: "link cycle", path: "first", make: func(root string) {
			mustSymlink(t, "second", filepath.Join(root, "first"))
			mustSymlink(t, "first", filepath.Join(root, "second"))
		}},
		{name: "directory recursion", path: "child/back", make: func(root string) {
			mustMkdir(t, filepath.Join(root, "child"))
			mustSymlink(t, root, filepath.Join(root, "child", "back"))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "source")
			mustMkdir(t, root)
			test.make(root)
			_, _, err := openAndScan(t, root, true)
			if err == nil || !strings.Contains(err.Error(), test.path) {
				t.Fatalf("scan error=%v, want logical path %q", err, test.path)
			}
		})
	}
}

func TestScanFollowSymlinkIgnoresFinalSpecialFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source")
	mustWrite(t, filepath.Join(root, "ordinary"), "contents")
	pipe := filepath.Join(t.TempDir(), "pipe")
	if err := unix.Mkfifo(pipe, 0o600); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, pipe, filepath.Join(root, "pipe-alias"))
	_, snapshot, err := openAndScan(t, root, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filePaths(snapshot), []string{"ordinary"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("files=%v want=%v", got, want)
	}
	if snapshot.Summary().FollowedSymlinks() != 1 ||
		snapshot.Summary().IgnoredSpecialFiles() != 1 {
		t.Fatalf("summary=%#v", snapshot.Summary())
	}
}

func TestSourceAccessKeepsExpandedAnchorAndReopensOrdinaryReplacement(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source")
	targetParent := filepath.Join(t.TempDir(), "target")
	mustWrite(t, filepath.Join(targetParent, "payload"), "old")
	mustMkdir(t, root)
	link := filepath.Join(root, "alias")
	mustSymlink(t, filepath.Join(targetParent, "payload"), link)
	owned, snapshot, err := openAndScan(t, root, true)
	if err != nil {
		t.Fatal(err)
	}
	access, err := owned.AcquireAccess(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	file := slices.Collect(snapshot.Files())[0]
	opened, _, err := access.OpenRegular(file.Backing())
	if err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	mustWrite(t, filepath.Join(other, "payload"), "wrong")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, filepath.Join(other, "payload"), link)
	mustWrite(t, filepath.Join(targetParent, "replacement"), "new")
	if err := os.Rename(filepath.Join(targetParent, "replacement"), filepath.Join(targetParent, "payload")); err != nil {
		t.Fatal(err)
	}
	old, err := io.ReadAll(opened)
	_ = opened.Close()
	if err != nil || string(old) != "old" {
		t.Fatalf("already-open contents=%q err=%v", old, err)
	}
	reopened, _, err := access.OpenRegular(file.Backing())
	if err != nil {
		t.Fatal(err)
	}
	current, err := io.ReadAll(reopened)
	_ = reopened.Close()
	if err != nil || string(current) != "new" {
		t.Fatalf("reopened contents=%q err=%v", current, err)
	}
	if err := owned.Close(); !errors.Is(err, ErrSourceRootBorrowed) {
		t.Fatalf("close with access=%v", err)
	}
	if err := access.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSourceAccessNeverFollowsReplacementPathComponents(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source")
	mustWrite(t, filepath.Join(root, "nested", "file"), "original")
	owned, snapshot, err := openAndScan(t, root, false)
	if err != nil {
		t.Fatal(err)
	}
	access, err := owned.AcquireAccess(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer access.Close()
	external := filepath.Join(t.TempDir(), "external")
	mustWrite(t, filepath.Join(external, "file"), "outside")
	if err := os.Rename(filepath.Join(root, "nested"), filepath.Join(root, "old-nested")); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, external, filepath.Join(root, "nested"))
	if file, _, err := access.OpenRegular(slices.Collect(snapshot.Files())[0].Backing()); err == nil {
		_ = file.Close()
		t.Fatal("source access followed a replacement directory symlink")
	}
}

func TestScanRejectsNoTransferItemsAndFinalRootSymlink(t *testing.T) {
	root := filepath.Join(t.TempDir(), "empty")
	mustMkdir(t, root)
	_, snapshot, err := openAndScan(t, root, false)
	if !errors.Is(err, ErrNoTransferItems) || snapshot.FileCount() != 0 {
		t.Fatalf("empty source snapshot=%#v err=%v", snapshot, err)
	}
	real := filepath.Join(t.TempDir(), "real")
	mustMkdir(t, real)
	link := filepath.Join(t.TempDir(), "source")
	mustSymlink(t, real, link)
	if opened, err := OpenSourceRoot(link); err == nil {
		_ = opened.Close()
		t.Fatal("final source symlink was accepted")
	}
}

func openAndScan(t *testing.T, path string, follow bool) (*SourceRoot, SourceSnapshot, error) {
	t.Helper()
	root, err := OpenSourceRoot(path)
	if err != nil {
		return nil, SourceSnapshot{}, err
	}
	t.Cleanup(func() { _ = root.Close() })
	snapshot, scanErr := root.Scan(context.Background(), follow)
	return root, snapshot, scanErr
}

func filePaths(snapshot SourceSnapshot) []string {
	result := make([]string, 0, snapshot.FileCount())
	for file := range snapshot.Files() {
		result = append(result, file.RelativePath())
	}
	sort.Strings(result)
	return result
}

func emptyPaths(snapshot SourceSnapshot) []string {
	result := make([]string, 0, snapshot.EmptyDirectoryCount())
	for directory := range snapshot.EmptyDirectories() {
		result = append(result, directory.RelativePath())
	}
	sort.Strings(result)
	return result
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, target, path string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}
