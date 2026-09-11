package rundirectory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestCreateSyncsEveryPublicationLevelBeforeReturning(t *testing.T) {
	base := realPath(t, t.TempDir())
	root := filepath.Join(base, "transferlanes")
	syncs := &recordingDurability{}
	owner, err := newOwner(root, syncs)
	if err != nil {
		t.Fatalf("newOwner: %v", err)
	}
	live, err := owner.Create(newTestRunID(t))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	paths := append([]string(nil), syncs.paths...)
	t.Cleanup(func() { _ = live.Close() })

	firstRoot := indexPath(paths, root, 0)
	parent := indexPath(paths, base, 0)
	liveness := indexBase(paths, livenessLockName)
	recovery := indexBase(paths, recoveryLockName)
	staging := indexBasePrefix(paths, stagingPrefix)
	finalRoot := indexPath(paths, root, firstRoot+1)
	for name, index := range map[string]int{
		"initial authority directory":   firstRoot,
		"authority parent directory":    parent,
		"liveness lock file":            liveness,
		"recovery lock file":            recovery,
		"staging directory":             staging,
		"published authority directory": finalRoot,
	} {
		if index < 0 {
			t.Errorf("Create did not sync %s; synced paths: %v", name, paths)
		}
	}
	if t.Failed() {
		return
	}
	if firstRoot > liveness || parent > liveness {
		t.Fatalf("authority creation was not durable before run files: %v", paths)
	}
	if liveness > staging || recovery > staging || staging > finalRoot {
		t.Fatalf("run publication durability order is unsafe: %v", paths)
	}
}

func TestEveryCreateSyncFailureWithholdsLiveRun(t *testing.T) {
	callCount := observeCreateSyncCount(t)
	if callCount == 0 {
		t.Fatal("Create performed no durable sync")
	}

	for failAt := 1; failAt <= callCount; failAt++ {
		t.Run(syncFailureName(failAt), func(t *testing.T) {
			root := filepath.Join(realPath(t, t.TempDir()), "transferlanes")
			syncs := &failingDurability{failAt: failAt}
			owner, err := newOwner(root, syncs)
			if err != nil {
				t.Fatalf("newOwner: %v", err)
			}
			live, err := owner.Create(newTestRunID(t))
			if !errors.Is(err, errInjectedSync) {
				t.Fatalf("Create error = %v, want injected sync failure %d", err, failAt)
			}
			if live != nil {
				t.Fatal("Create returned a LiveRun after a sync failure")
			}

			goodOwner, err := newOwner(root, systemDurability{})
			if err != nil {
				t.Fatalf("newOwner for cleanup: %v", err)
			}
			if err := goodOwner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err != nil {
				t.Fatalf("recover after sync failure: %v", err)
			}
		})
	}
}

func observeCreateSyncCount(t *testing.T) int {
	t.Helper()
	root := filepath.Join(realPath(t, t.TempDir()), "transferlanes")
	syncs := &failingDurability{}
	owner, err := newOwner(root, syncs)
	if err != nil {
		t.Fatalf("newOwner: %v", err)
	}
	live, err := owner.Create(newTestRunID(t))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	count := syncs.calls
	if err := live.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err != nil {
		t.Fatalf("RecoverStale cleanup: %v", err)
	}
	return count
}

var errInjectedSync = errors.New("injected sync failure")

type failingDurability struct {
	calls  int
	failAt int
}

type recordingDurability struct {
	paths []string
}

func (durability *recordingDurability) Sync(file *os.File) error {
	durability.paths = append(durability.paths, file.Name())
	return file.Sync()
}

func (d *failingDurability) Sync(file *os.File) error {
	d.calls++
	if d.calls == d.failAt {
		return errInjectedSync
	}
	return file.Sync()
}

func syncFailureName(call int) string {
	return "sync-" + strconv.Itoa(call)
}

func indexPath(paths []string, want string, start int) int {
	for index := start; index < len(paths); index++ {
		if paths[index] == want {
			return index
		}
	}
	return -1
}

func indexBase(paths []string, want string) int {
	for index, path := range paths {
		if filepath.Base(path) == want {
			return index
		}
	}
	return -1
}

func indexBasePrefix(paths []string, prefix string) int {
	for index, path := range paths {
		if strings.HasPrefix(filepath.Base(path), prefix) {
			return index
		}
	}
	return -1
}
