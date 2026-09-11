package rundirectory

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
)

func TestCreatePublishesLockedRun(t *testing.T) {
	owner, authority := newTestOwner(t)
	id := newTestRunID(t)

	live, err := owner.Create(id)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = live.Close() })

	assertPublishedRun(t, live, authority, id)
}

func newTestOwner(t *testing.T) (*Owner, string) {
	t.Helper()
	root := filepath.Join(realPath(t, t.TempDir()), "transferlanes")
	owner, err := newOwner(root, noOpDurability{})
	if err != nil {
		t.Fatalf("newOwner: %v", err)
	}
	return owner, root
}

type noOpDurability struct{}

func (noOpDurability) Sync(*os.File) error { return nil }

func newTestRunID(t *testing.T) runid.ID {
	t.Helper()
	id, err := runid.New()
	if err != nil {
		t.Fatalf("runid.New: %v", err)
	}
	return id
}

func makeStaleRun(t *testing.T, owner *Owner) runid.ID {
	t.Helper()
	id := newTestRunID(t)
	live, err := owner.Create(id)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := live.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return id
}

func assertPublishedRun(t *testing.T, live *LiveRun, authority string, id runid.ID) {
	t.Helper()
	wantPath := filepath.Join(authority, id.String())
	if live.ID() != id {
		t.Fatalf("published run ID = %q, want %q", live.ID(), id)
	}
	info, err := os.Lstat(wantPath)
	if err != nil {
		t.Fatalf("Lstat run root: %v", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		t.Fatalf("run root mode = %v", info.Mode())
	}
	for _, name := range []string{livenessLockName, recoveryLockName} {
		info, err := os.Lstat(filepath.Join(wantPath, name))
		if err != nil {
			t.Fatalf("Lstat %s: %v", name, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v", name, info.Mode())
		}
	}
}

func assertDirectoryExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("directory %q: info=%v err=%v", path, info, err)
	}
}

func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path %q still exists: %v", path, err)
	}
}

func realPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks(%q): %v", path, err)
	}
	return resolved
}
