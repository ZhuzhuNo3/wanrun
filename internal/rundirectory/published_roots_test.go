//go:build linux || darwin

package rundirectory

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"golang.org/x/sys/unix"
)

func TestLiveRunSnapshotsEveryPublishedRootInIdentityOrder(t *testing.T) {
	owner, authority := newTestOwner(t)
	first := createLiveRun(t, owner)
	second := createLiveRun(t, owner)
	t.Cleanup(func() {
		_ = first.Close()
		_ = second.Close()
	})

	roots, err := second.OpenPublishedRunRoots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()

	want := []string{first.ID().String(), second.ID().String()}
	slices.Sort(want)
	var got []string
	for _, root := range roots.Roots() {
		got = append(got, root.ID().String())
		opened, err := root.OpenRoot()
		if err != nil {
			t.Fatalf("open pinned root %s: %v", root.ID(), err)
		}
		if err := validateOwnedDirectory(opened, 0o700); err != nil {
			_ = opened.Close()
			t.Fatalf("validate pinned root %s: %v", root.ID(), err)
		}
		_ = opened.Close()
	}
	if !slices.Equal(got, want) {
		t.Fatalf("published roots = %v, want %v", got, want)
	}
	created := createLiveRun(t, owner)
	if err := created.Close(); err != nil {
		t.Fatalf("close run created after snapshot: %v", err)
	}
	assertDirectoryExists(t, authority)
}

func TestPublishedRootSnapshotPinsIdentityAcrossRemoval(t *testing.T) {
	owner, authority := newTestOwner(t)
	observer := createLiveRun(t, owner)
	removed := createLiveRun(t, owner)
	t.Cleanup(func() { _ = observer.Close() })
	marker := []byte("pinned")
	if err := os.WriteFile(filepath.Join(authority, removed.ID().String(), "marker"), marker, 0o600); err != nil {
		t.Fatal(err)
	}

	roots, err := observer.OpenPublishedRunRoots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer roots.Close()
	pinned := publishedRootByID(t, roots, removed.ID())
	if err := removed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.RecoverStale(context.Background(), func(stale *StaleRun) error {
		root, openErr := stale.OpenRoot()
		if openErr != nil {
			return openErr
		}
		defer root.Close()
		return unix.Unlinkat(int(root.Fd()), "marker", 0)
	}); err != nil {
		t.Fatal(err)
	}

	root, err := pinned.OpenRoot()
	if err != nil {
		t.Fatalf("duplicate removed pinned root: %v", err)
	}
	defer root.Close()
	if _, err := root.Stat(); err != nil {
		t.Fatalf("stat removed pinned root: %v", err)
	}
}

func TestPublishedRootSnapshotIgnoresValidTransientNamesAndRejectsUnknownEntries(t *testing.T) {
	owner, authority := newTestOwner(t)
	live := createLiveRun(t, owner)
	t.Cleanup(func() { _ = live.Close() })
	for _, prefix := range []string{stagingPrefix, removingPrefix} {
		name, err := freshTemporaryName(prefix, newTestRunID(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(authority, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if roots, err := live.OpenPublishedRunRoots(context.Background()); err != nil {
		t.Fatalf("valid transient names: %v", err)
	} else {
		_ = roots.Close()
	}

	unknown := filepath.Join(authority, ".staging-not-a-run")
	if err := os.Mkdir(unknown, 0o700); err != nil {
		t.Fatal(err)
	}
	if roots, err := live.OpenPublishedRunRoots(context.Background()); err == nil {
		_ = roots.Close()
		t.Fatal("unknown authority entry was accepted")
	}
}

func TestPublishedRootSnapshotDoesNotAcquireRunLivenessOrRecoveryLocks(t *testing.T) {
	owner, authority := newTestOwner(t)
	observer := createLiveRun(t, owner)
	active := createLiveRun(t, owner)
	recoveryLocked := createLiveRun(t, owner)
	t.Cleanup(func() {
		_ = observer.Close()
		_ = active.Close()
	})
	if err := recoveryLocked.Close(); err != nil {
		t.Fatal(err)
	}
	recoveryPath := filepath.Join(authority, recoveryLocked.ID().String(), recoveryLockName)
	recovery, err := os.OpenFile(recoveryPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(recovery.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = recovery.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = unix.Flock(int(recovery.Fd()), unix.LOCK_UN)
		_ = recovery.Close()
	})

	roots, err := observer.OpenPublishedRunRoots(context.Background())
	if err != nil {
		t.Fatalf("snapshot with active and recovery-locked roots: %v", err)
	}
	defer roots.Close()
	got := make(map[runid.ID]bool)
	for _, root := range roots.Roots() {
		got[root.ID()] = true
	}
	for _, id := range []runid.ID{observer.ID(), active.ID(), recoveryLocked.ID()} {
		if !got[id] {
			t.Fatalf("published root %s was omitted because of its run lock", id)
		}
	}
}

func createLiveRun(t *testing.T, owner *Owner) *LiveRun {
	t.Helper()
	live, err := owner.Create(newTestRunID(t))
	if err != nil {
		t.Fatal(err)
	}
	return live
}

func publishedRootByID(t *testing.T, roots PublishedRunRoots, id runid.ID) PublishedRunRoot {
	t.Helper()
	for _, root := range roots.Roots() {
		if root.ID() == id {
			return root
		}
	}
	t.Fatalf("published root %s is absent", id)
	return nil
}
