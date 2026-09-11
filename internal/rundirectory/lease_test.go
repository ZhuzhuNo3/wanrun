package rundirectory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestLiveCompleteDeadlineReleasesLivenessAndPreservesRecoverableRoot(t *testing.T) {
	owner, authority := newTestOwner(t)
	id := newTestRunID(t)
	live, err := owner.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	authorityFile, err := os.Open(authority)
	if err != nil {
		t.Fatal(err)
	}
	defer authorityFile.Close()
	if err := unix.Flock(int(authorityFile.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = live.Complete(ctx)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("bounded completion error=%v duration=%s", err, time.Since(started))
	}
	assertDirectoryExists(t, filepath.Join(authority, id.String()))
	if err := unix.Flock(int(authorityFile.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	recovered := 0
	if err := owner.RecoverStale(context.Background(), func(stale *StaleRun) error {
		recovered++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if recovered != 1 {
		t.Fatalf("released live run recovery count = %d", recovered)
	}
	assertPathAbsent(t, filepath.Join(authority, id.String()))
}

func TestStaleRemovalDeadlineReleasesClaimAndPreservesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "authority")
	owner, err := newOwner(root, systemDurability{})
	if err != nil {
		t.Fatal(err)
	}
	id := newTestRunID(t)
	live, err := owner.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}

	var authorityLock *os.File
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err = owner.RecoverStale(ctx, func(*StaleRun) error {
		authorityLock, err = os.Open(root)
		if err != nil {
			return err
		}
		return unix.Flock(int(authorityLock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("RecoverStale error=%v, want deadline acquiring final removal lock", err)
	}
	if _, err := os.Stat(filepath.Join(root, id.String())); err != nil {
		t.Fatalf("stale root was removed after removal deadline: %v", err)
	}
	if err := unix.Flock(int(authorityLock.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := authorityLock.Close(); err != nil {
		t.Fatal(err)
	}
	callbacks := 0
	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error {
		callbacks++
		return nil
	}); err != nil {
		t.Fatalf("retry stale removal: %v", err)
	}
	if callbacks != 1 {
		t.Fatalf("retry callback count=%d, want 1", callbacks)
	}
}

func TestTransferredLivenessBlocksRecoveryUntilProcessContainment(t *testing.T) {
	owner, authority := newTestOwner(t)
	id := newTestRunID(t)
	live, err := owner.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	contained := make(chan struct{})
	if err := live.RetainLivenessUntil(contained); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err == nil {
		t.Fatal("former LiveRun owner released transferred liveness")
	}
	callbacks := 0
	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error {
		callbacks++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if callbacks != 0 {
		t.Fatalf("active retained run recovery callbacks=%d", callbacks)
	}
	close(contained)
	deadline := time.Now().Add(time.Second)
	for callbacks == 0 && time.Now().Before(deadline) {
		if err := owner.RecoverStale(context.Background(), func(*StaleRun) error {
			callbacks++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if callbacks == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	if callbacks != 1 {
		t.Fatalf("contained run recovery callbacks=%d", callbacks)
	}
	assertPathAbsent(t, filepath.Join(authority, id.String()))
}

func TestRunLeasesDoNotExposeStringPaths(t *testing.T) {
	t.Parallel()

	for _, leaseType := range []reflect.Type{
		reflect.TypeOf((*LiveRun)(nil)),
		reflect.TypeOf((*StaleRun)(nil)),
	} {
		if _, exposed := leaseType.MethodByName("Path"); exposed {
			t.Errorf("%s exposes a Path method", leaseType)
		}
	}
}

func TestLiveRunAccessRemainsBoundToOriginalDirectory(t *testing.T) {
	owner, authority := newTestOwner(t)
	id := newTestRunID(t)
	live, err := owner.Create(id)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	runRoot := filepath.Join(authority, id.String())
	original, err := os.Stat(runRoot)
	if err != nil {
		t.Fatalf("stat original run root: %v", err)
	}
	displaced := filepath.Join(filepath.Dir(authority), "displaced-run")
	if err := os.Rename(runRoot, displaced); err != nil {
		t.Fatalf("rename run root: %v", err)
	}
	if err := os.Mkdir(runRoot, 0o700); err != nil {
		t.Fatalf("create replacement run root: %v", err)
	}
	replacement, err := os.Stat(runRoot)
	if err != nil {
		t.Fatalf("stat replacement run root: %v", err)
	}

	root, err := live.OpenRoot()
	if err != nil {
		t.Fatalf("OpenRoot after replacement: %v", err)
	}
	if filepath.IsAbs(root.Name()) {
		_ = root.Close()
		t.Fatalf("OpenRoot exposed an absolute path through File.Name: %q", root.Name())
	}
	opened, err := root.Stat()
	if err != nil {
		_ = root.Close()
		t.Fatalf("stat opened root: %v", err)
	}
	if !os.SameFile(opened, original) || os.SameFile(opened, replacement) {
		_ = root.Close()
		t.Fatal("OpenRoot followed the replacement instead of the original inode")
	}

	if err := live.Close(); err != nil {
		_ = root.Close()
		t.Fatalf("Close LiveRun: %v", err)
	}
	if reopened, err := live.OpenRoot(); err == nil {
		_ = reopened.Close()
		_ = root.Close()
		t.Fatal("closed LiveRun opened a new root descriptor")
	}
	if _, err := root.Stat(); err != nil {
		_ = root.Close()
		t.Fatalf("caller-owned descriptor was closed with its lease: %v", err)
	}
	if err := root.Close(); err != nil {
		t.Fatalf("close caller-owned root descriptor: %v", err)
	}
}

func TestStaleRunAccessRemainsBoundAndEndsWithCallback(t *testing.T) {
	owner, authority := newTestOwner(t)
	id := makeStaleRun(t, owner)
	runRoot := filepath.Join(authority, id.String())
	displaced := filepath.Join(filepath.Dir(authority), "displaced-stale-run")
	wantErr := errors.New("keep stale root")
	var callbackLease *StaleRun
	var callbackRoot *os.File
	t.Cleanup(func() {
		if callbackRoot != nil {
			_ = callbackRoot.Close()
		}
	})

	err := owner.RecoverStale(context.Background(), func(stale *StaleRun) error {
		callbackLease = stale
		original, statErr := os.Stat(runRoot)
		if statErr != nil {
			t.Fatalf("stat original stale root: %v", statErr)
		}
		if renameErr := os.Rename(runRoot, displaced); renameErr != nil {
			t.Fatalf("rename stale root: %v", renameErr)
		}
		if mkdirErr := os.Mkdir(runRoot, 0o700); mkdirErr != nil {
			t.Fatalf("create stale replacement: %v", mkdirErr)
		}
		replacement, statErr := os.Stat(runRoot)
		if statErr != nil {
			t.Fatalf("stat stale replacement: %v", statErr)
		}

		root, openErr := stale.OpenRoot()
		if openErr != nil {
			t.Fatalf("OpenRoot after stale replacement: %v", openErr)
		}
		callbackRoot = root
		if filepath.IsAbs(root.Name()) {
			t.Fatalf("StaleRun exposed an absolute path through File.Name: %q", root.Name())
		}
		opened, statErr := root.Stat()
		if statErr != nil {
			t.Fatalf("stat opened stale root: %v", statErr)
		}
		if !os.SameFile(opened, original) || os.SameFile(opened, replacement) {
			t.Fatal("StaleRun followed the replacement instead of the claimed inode")
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RecoverStale error = %v, want %v", err, wantErr)
	}
	if callbackLease == nil {
		t.Fatal("recovery callback did not receive a lease")
	}
	if root, err := callbackLease.OpenRoot(); err == nil {
		_ = root.Close()
		t.Fatal("StaleRun opened a new descriptor after its callback ended")
	}
	if callbackRoot == nil {
		t.Fatal("recovery callback did not obtain a root descriptor")
	}
	if _, err := callbackRoot.Stat(); err != nil {
		t.Fatalf("callback-owned descriptor was closed with its lease: %v", err)
	}
	if err := callbackRoot.Close(); err != nil {
		t.Fatalf("close callback-owned root descriptor: %v", err)
	}
	callbackRoot = nil
}
