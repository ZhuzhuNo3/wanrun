package rundirectory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOwnerRejectsUnsafeAuthorityRoots(t *testing.T) {
	t.Parallel()

	for _, root := range []string{"/", "relative", "/tmp/../tmp/transferlanes"} {
		if _, err := newOwner(root, systemDurability{}); err == nil {
			t.Errorf("newOwner(%q) succeeded", root)
		}
	}
}

func TestPrivateAuthorityKeepsRunRootsOutOfProductionAuthority(t *testing.T) {
	t.Parallel()

	productionBefore := observeProductionAuthority(t)
	owner, root, err := CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.ClosePrivateAuthority(context.Background()); err != nil {
			t.Errorf("close private authority: %v", err)
		}
	})
	id := newTestRunID(t)
	live, err := owner.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, id.String())); err != nil {
		t.Fatalf("private run root absent: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(AuthorityRoot, id.String())); !os.IsNotExist(err) {
		t.Fatalf("private RunID appeared in production authority: %v", err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err != nil {
		t.Fatal(err)
	}
	assertPathAbsent(t, filepath.Join(root, id.String()))
	if after := observeProductionAuthority(t); after != productionBefore {
		t.Fatalf("production authority changed: before=%#v after=%#v", productionBefore, after)
	}
}

func TestPrivateAuthorityMutationStaysOnCreatedDirectoryAfterPathReplacement(t *testing.T) {
	for _, replacement := range []string{"directory", "production-symlink"} {
		t.Run(replacement, func(t *testing.T) {
			productionBefore := observeProductionAuthority(t)
			owner, root, err := CreatePrivateAuthority()
			if err != nil {
				t.Fatal(err)
			}
			displaced := root + ".displaced"
			t.Cleanup(func() {
				_ = os.Remove(root)
				_ = os.Rename(displaced, root)
				_ = owner.ClosePrivateAuthority(context.Background())
			})
			if err := os.Rename(root, displaced); err != nil {
				t.Fatal(err)
			}
			switch replacement {
			case "directory":
				if err := os.Mkdir(root, 0o700); err != nil {
					t.Fatal(err)
				}
			case "production-symlink":
				if err := os.Symlink(AuthorityRoot, root); err != nil {
					t.Fatal(err)
				}
			}

			id := newTestRunID(t)
			live, err := owner.Create(id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(displaced, id.String())); err != nil {
				t.Fatalf("descriptor-bound run absent: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(AuthorityRoot, id.String())); !os.IsNotExist(err) {
				t.Fatalf("replacement redirected run into production: %v", err)
			}
			if err := live.Close(); err != nil {
				t.Fatal(err)
			}
			if err := owner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(root); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(displaced, root); err != nil {
				t.Fatal(err)
			}
			if err := owner.ClosePrivateAuthority(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertPathAbsent(t, root)
			if after := observeProductionAuthority(t); after != productionBefore {
				t.Fatalf("production authority changed: before=%#v after=%#v", productionBefore, after)
			}
		})
	}
}

func TestPrivateAuthorityCloseIsBoundedByActiveDirectoryUse(t *testing.T) {
	owner, root, err := CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	authority, err := owner.openAuthority()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := owner.ClosePrivateAuthority(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("close with active use returned %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.ClosePrivateAuthority(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertPathAbsent(t, root)
}

type productionAuthorityObservation struct {
	exists  bool
	mode    os.FileMode
	size    int64
	modTime time.Time
	link    string
}

func observeProductionAuthority(t *testing.T) productionAuthorityObservation {
	t.Helper()
	info, err := os.Lstat(AuthorityRoot)
	if os.IsNotExist(err) {
		return productionAuthorityObservation{}
	}
	if err != nil {
		t.Fatal(err)
	}
	observation := productionAuthorityObservation{exists: true, mode: info.Mode(),
		size: info.Size(), modTime: info.ModTime()}
	if info.Mode()&os.ModeSymlink != 0 {
		observation.link, err = os.Readlink(AuthorityRoot)
		if err != nil {
			t.Fatal(err)
		}
	}
	return observation
}

func TestAuthorityRootCannotBeSymlink(t *testing.T) {
	base := realPath(t, t.TempDir())
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatalf("Mkdir outside: %v", err)
	}
	authority := filepath.Join(base, "transferlanes")
	if err := os.Symlink(outside, authority); err != nil {
		t.Fatalf("Symlink authority: %v", err)
	}
	owner, err := newOwner(authority, systemDurability{})
	if err != nil {
		t.Fatalf("newOwner: %v", err)
	}
	if _, err := owner.Create(newTestRunID(t)); err == nil {
		t.Fatal("Create followed authority-root symlink")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatalf("ReadDir outside: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("outside directory changed: %v", entries)
	}
}

func TestCreateWithholdsLiveRunWhenAuthorityRootIsReplaced(t *testing.T) {
	base := realPath(t, t.TempDir())
	authority := filepath.Join(base, "transferlanes")
	syncs := newBlockingStagingSync()
	owner, err := newOwner(authority, syncs)
	if err != nil {
		t.Fatalf("newOwner: %v", err)
	}
	id := newTestRunID(t)

	type createResult struct {
		live *LiveRun
		err  error
	}
	result := make(chan createResult, 1)
	go func() {
		live, createErr := owner.Create(id)
		result <- createResult{live: live, err: createErr}
	}()

	select {
	case <-syncs.reached:
	case <-time.After(2 * time.Second):
		t.Fatal("Create did not reach staging durability gate")
	}
	displaced := filepath.Join(base, "displaced")
	if err := os.Rename(authority, displaced); err != nil {
		t.Fatalf("rename authority root: %v", err)
	}
	if err := os.Mkdir(authority, 0o700); err != nil {
		t.Fatalf("create replacement authority root: %v", err)
	}
	close(syncs.release)

	created := <-result
	if created.err == nil {
		if created.live != nil {
			_ = created.live.Close()
		}
		t.Fatal("Create returned a LiveRun after the authority root was replaced")
	}
	if created.live != nil {
		t.Fatal("Create returned a usable owner after authority replacement")
	}
	assertPathAbsent(t, filepath.Join(authority, id.String()))
	assertPathAbsent(t, filepath.Join(displaced, id.String()))
}

func TestRecoveryRejectsSymlinkRunWithoutTouchingTarget(t *testing.T) {
	owner, authority := newTestOwner(t)
	live, err := owner.Create(newTestRunID(t))
	if err != nil {
		t.Fatalf("Create authority root: %v", err)
	}
	if err := live.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err != nil {
		t.Fatalf("remove setup run: %v", err)
	}

	id := newTestRunID(t)
	target := filepath.Join(realPath(t, t.TempDir()), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir target: %v", err)
	}
	marker := filepath.Join(target, "marker")
	if err := os.WriteFile(marker, []byte("safe"), 0o600); err != nil {
		t.Fatalf("WriteFile marker: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(authority, id.String())); err != nil {
		t.Fatalf("Symlink run: %v", err)
	}

	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err == nil {
		t.Fatal("RecoverStale accepted a symlink run root")
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "safe" {
		t.Fatalf("symlink target changed: content=%q err=%v", got, err)
	}
}

func TestRecoveryDoesNotDeleteRunRootReplacement(t *testing.T) {
	owner, authority := newTestOwner(t)
	id := makeStaleRun(t, owner)
	runRoot := filepath.Join(authority, id.String())
	displaced := filepath.Join(realPath(t, t.TempDir()), "displaced-run")
	marker := filepath.Join(displaced, "marker")

	err := owner.RecoverStale(context.Background(), func(*StaleRun) error {
		if err := os.Rename(runRoot, displaced); err != nil {
			t.Fatalf("rename claimed run: %v", err)
		}
		if err := os.WriteFile(marker, []byte("original"), 0o600); err != nil {
			t.Fatalf("write marker: %v", err)
		}
		if err := os.Mkdir(runRoot, 0o700); err != nil {
			t.Fatalf("create replacement run root: %v", err)
		}
		for _, name := range []string{livenessLockName, recoveryLockName} {
			if err := os.WriteFile(filepath.Join(runRoot, name), nil, 0o600); err != nil {
				t.Fatalf("create replacement %s: %v", name, err)
			}
		}
		return nil
	})
	if err == nil {
		t.Fatal("RecoverStale accepted a replaced run root")
	}
	if got, readErr := os.ReadFile(marker); readErr != nil || string(got) != "original" {
		t.Fatalf("displaced run changed: content=%q err=%v", got, readErr)
	}
	assertDirectoryExists(t, runRoot)
}

func TestUnpublishedStagingDirectoryIsCleaned(t *testing.T) {
	owner, authority := newTestOwner(t)
	live, err := owner.Create(newTestRunID(t))
	if err != nil {
		t.Fatalf("Create authority root: %v", err)
	}
	if err := live.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err != nil {
		t.Fatalf("remove setup run: %v", err)
	}

	staging := filepath.Join(authority, newStagingName(t))
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatalf("Mkdir staging: %v", err)
	}
	if err := os.WriteFile(filepath.Join(staging, livenessLockName), nil, 0o600); err != nil {
		t.Fatalf("WriteFile liveness: %v", err)
	}
	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error {
		t.Fatal("staging directory was offered as a published run")
		return nil
	}); err != nil {
		t.Fatalf("RecoverStale: %v", err)
	}
	assertPathAbsent(t, staging)
}

func TestUnpublishedStagingSymlinkDoesNotEscapeAuthority(t *testing.T) {
	owner, authority := newTestOwner(t)
	live, err := owner.Create(newTestRunID(t))
	if err != nil {
		t.Fatalf("Create authority root: %v", err)
	}
	if err := live.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err != nil {
		t.Fatalf("remove setup run: %v", err)
	}

	target := filepath.Join(realPath(t, t.TempDir()), "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatalf("Mkdir target: %v", err)
	}
	marker := filepath.Join(target, "marker")
	if err := os.WriteFile(marker, []byte("safe"), 0o600); err != nil {
		t.Fatalf("WriteFile marker: %v", err)
	}
	staging := filepath.Join(authority, newStagingName(t))
	if err := os.Symlink(target, staging); err != nil {
		t.Fatalf("Symlink staging: %v", err)
	}

	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err == nil {
		t.Fatal("RecoverStale followed an unpublished staging symlink")
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "safe" {
		t.Fatalf("staging symlink target changed: content=%q err=%v", got, err)
	}
}

func newStagingName(t *testing.T) string {
	t.Helper()
	return stagingPrefix + newTestRunID(t).String() + "-" + newTestRunID(t).String()
}

type blockingStagingSync struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingStagingSync() *blockingStagingSync {
	return &blockingStagingSync{
		reached: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (syncs *blockingStagingSync) Sync(file *os.File) error {
	if err := file.Sync(); err != nil {
		return err
	}
	if strings.HasPrefix(filepath.Base(file.Name()), stagingPrefix) {
		syncs.once.Do(func() { close(syncs.reached) })
		<-syncs.release
	}
	return nil
}
