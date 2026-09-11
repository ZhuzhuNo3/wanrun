package rundirectory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoverySkipsActiveRun(t *testing.T) {
	owner, _ := newTestOwner(t)
	live, err := owner.Create(newTestRunID(t))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = live.Close() })

	called := false
	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("RecoverStale: %v", err)
	}
	if called {
		t.Fatal("active run was offered for recovery")
	}
}

func TestRecoverySuccessOffersExactRunAndRemovesRoot(t *testing.T) {
	owner, authority := newTestOwner(t)
	id := makeStaleRun(t, owner)
	callbacks := 0

	if err := owner.RecoverStale(context.Background(), func(stale *StaleRun) error {
		callbacks++
		if stale.ID() != id {
			t.Fatalf("stale ID = %q, want %q", stale.ID(), id)
		}
		return nil
	}); err != nil {
		t.Fatalf("RecoverStale: %v", err)
	}
	if callbacks != 1 {
		t.Fatalf("recovery callbacks = %d, want 1", callbacks)
	}
	assertPathAbsent(t, filepath.Join(authority, id.String()))
}

func TestRecoveryCallbackFailurePreservesRootForLaterRetry(t *testing.T) {
	owner, authority := newTestOwner(t)
	id := makeStaleRun(t, owner)
	wantErr := errors.New("network cleanup failed")

	err := owner.RecoverStale(context.Background(), func(stale *StaleRun) error {
		if stale.ID() != id {
			t.Fatalf("stale ID = %q, want %q", stale.ID(), id)
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RecoverStale error = %v, want %v", err, wantErr)
	}
	assertDirectoryExists(t, filepath.Join(authority, id.String()))

	callbacks := 0
	if err := owner.RecoverStale(context.Background(), func(stale *StaleRun) error {
		callbacks++
		if stale.ID() != id {
			t.Fatalf("retry stale ID = %q, want %q", stale.ID(), id)
		}
		return nil
	}); err != nil {
		t.Fatalf("retry RecoverStale: %v", err)
	}
	if callbacks != 1 {
		t.Fatalf("retry callbacks = %d, want 1", callbacks)
	}
	assertPathAbsent(t, filepath.Join(authority, id.String()))
}

func TestRecoveryJoinsCallbackAndLeaseCloseErrors(t *testing.T) {
	owner, authority := newTestOwner(t)
	id := makeStaleRun(t, owner)
	wantErr := errors.New("network cleanup failed")

	err := owner.RecoverStale(context.Background(), func(stale *StaleRun) error {
		if closeErr := stale.lease.recovery.Close(); closeErr != nil {
			t.Fatalf("close recovery descriptor: %v", closeErr)
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RecoverStale error = %v, want callback error %v", err, wantErr)
	}
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("RecoverStale error = %v, want lease-close os.ErrClosed", err)
	}
	assertDirectoryExists(t, filepath.Join(authority, id.String()))
}

func TestRecoveryUnknownContentPreservesOriginalRoot(t *testing.T) {
	owner, authority := newTestOwner(t)
	id := makeStaleRun(t, owner)
	runRoot := filepath.Join(authority, id.String())
	foreign := filepath.Join(runRoot, "foreign")
	if err := os.WriteFile(foreign, []byte("keep"), 0o600); err != nil {
		t.Fatalf("write foreign file: %v", err)
	}

	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err == nil {
		t.Fatal("RecoverStale removed a run containing unknown data")
	}
	if got, err := os.ReadFile(foreign); err != nil || string(got) != "keep" {
		t.Fatalf("foreign file changed: content=%q err=%v", got, err)
	}
	assertDirectoryExists(t, runRoot)

	if err := os.Remove(foreign); err != nil {
		t.Fatalf("remove foreign file: %v", err)
	}
	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err != nil {
		t.Fatalf("RecoverStale after removing foreign file: %v", err)
	}
	assertPathAbsent(t, runRoot)
}

var errRemovingSync = errors.New("injected removing-directory sync failure")

type failRemovingSyncDurability struct {
	authority string
	failed    bool
}

func (durability *failRemovingSyncDurability) Sync(file *os.File) error {
	if file.Name() != durability.authority {
		return nil
	}
	entries, err := os.ReadDir(durability.authority)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), removingPrefix) && !durability.failed {
			durability.failed = true
			return errRemovingSync
		}
	}
	return nil
}

func TestRecoveryPostRenameSyncFailurePreservesRemovingEvidence(t *testing.T) {
	authority := filepath.Join(realPath(t, t.TempDir()), "transferlanes")
	owner, err := newOwner(authority, &failRemovingSyncDurability{authority: authority})
	if err != nil {
		t.Fatalf("newOwner: %v", err)
	}
	id := makeStaleRun(t, owner)

	err = owner.RecoverStale(context.Background(), func(stale *StaleRun) error {
		if stale.ID() != id {
			t.Fatalf("stale ID = %q, want %q", stale.ID(), id)
		}
		return nil
	})
	if !errors.Is(err, errRemovingSync) {
		t.Fatalf("RecoverStale error = %v, want %v", err, errRemovingSync)
	}
	assertPathAbsent(t, filepath.Join(authority, id.String()))

	wantPrefix := removingPrefix + id.String() + "-"
	entries, err := os.ReadDir(authority)
	if err != nil {
		t.Fatalf("ReadDir authority: %v", err)
	}
	var evidence string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), wantPrefix) {
			if evidence != "" {
				t.Fatalf("multiple removing entries for %s: %q and %q", id, evidence, entry.Name())
			}
			evidence = entry.Name()
		}
	}
	if evidence == "" {
		t.Fatalf("no removing evidence with prefix %q", wantPrefix)
	}

	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error {
		t.Fatal("unpublished removing evidence was offered to callback")
		return nil
	}); err != nil {
		t.Fatalf("RecoverStale evidence cleanup: %v", err)
	}
	assertPathAbsent(t, filepath.Join(authority, evidence))
}
