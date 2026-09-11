//go:build linux && rootintegration

package runlogs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

func TestRunLogsRejectSourceBindAndNestedMountAliases(t *testing.T) {
	requireRootMountNamespace(t, func() {
		root := t.TempDir()
		sourcePath := filepath.Join(root, "source")
		sourceSubtree := filepath.Join(sourcePath, "subtree")
		nestedMount := filepath.Join(sourcePath, "nested-mount")
		mkdirAll(t, sourcePath, sourceSubtree, nestedMount)
		mountTmpfs(t, nestedMount)
		nestedSubtree := filepath.Join(nestedMount, "subtree")
		mkdirAll(t, nestedSubtree)

		aliases := filepath.Join(root, "aliases")
		rootAlias := filepath.Join(aliases, "root")
		subtreeAlias := filepath.Join(aliases, "subtree")
		nestedAlias := filepath.Join(aliases, "nested")
		nestedSubtreeAlias := filepath.Join(aliases, "nested-subtree")
		external := filepath.Join(aliases, "external")
		mkdirAll(t, aliases, rootAlias, subtreeAlias, nestedAlias, nestedSubtreeAlias, external)
		bindMount(t, sourcePath, rootAlias)
		bindMount(t, sourceSubtree, subtreeAlias)
		bindMount(t, nestedMount, nestedAlias)
		bindMount(t, nestedSubtree, nestedSubtreeAlias)
		mountTmpfs(t, external)

		source := openRootTestSource(t, sourcePath)
		recovery, closeRecovery := rootTestRecovery(t)
		defer closeRecovery()
		id, _ := transfernumber.New(1)
		for _, path := range []string{rootAlias, subtreeAlias, nestedAlias, nestedSubtreeAlias} {
			if logs, err := Open(context.Background(), source, recovery, path, Interactive,
				[]transfernumber.Number{id}); err == nil {
				_ = logs.Close()
				t.Errorf("source mount alias %s was accepted", path)
			}
		}
		for _, path := range []string{sourcePath, sourceSubtree, nestedMount, nestedSubtree} {
			if _, err := os.Stat(filepath.Join(path, "transfer-01.ptylog")); !os.IsNotExist(err) {
				t.Errorf("source location %s gained a log: %v", path, err)
			}
		}
		logs, err := Open(context.Background(), source, recovery, filepath.Join(external, "logs"),
			Interactive, []transfernumber.Number{id})
		if err != nil {
			t.Fatalf("unrelated mounted filesystem was rejected: %v", err)
		}
		if err := logs.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestRunLogsRejectRecoveryBindAliasesAndKeepStaleEvidence(t *testing.T) {
	requireRootMountNamespace(t, func() {
		owner, id, authorityBefore, runBefore := staleProductionRun(t)
		recovery, err := owner.OpenRecoveryRoot()
		if err != nil {
			t.Fatal(err)
		}
		defer recovery.Close()
		source := unrelatedRootTestSource(t)
		candidate := t.TempDir()
		rootAlias := filepath.Join(candidate, "authority-alias")
		runAlias := filepath.Join(candidate, "run-alias")
		mkdirAll(t, rootAlias, runAlias)
		bindMount(t, rundirectory.AuthorityRoot, rootAlias)
		bindMount(t, filepath.Join(rundirectory.AuthorityRoot, id.String()), runAlias)

		transfer, _ := transfernumber.New(1)
		for _, path := range []string{filepath.Join(rootAlias, "logs"), runAlias} {
			if logs, err := Open(context.Background(), source, recovery, path, Interactive,
				[]transfernumber.Number{transfer}); err == nil {
				_ = logs.Close()
				t.Fatalf("recovery bind alias %s was accepted", path)
			}
		}
		assertPathsAbsent(t, filepath.Join(rundirectory.AuthorityRoot, "logs"),
			filepath.Join(rundirectory.AuthorityRoot, id.String(), "transfer-01.ptylog"))
		assertStaleEvidence(t, id, authorityBefore, runBefore)
	})
}

func TestRunLogsRejectNestedRecoveryBackingAliases(t *testing.T) {
	requireRootMountNamespace(t, func() {
		owner, id, authorityBefore, runBefore := staleProductionRun(t)
		runRoot := filepath.Join(rundirectory.AuthorityRoot, id.String())
		nested := filepath.Join(runRoot, "nested mount")
		mkdirAll(t, nested)
		t.Cleanup(func() {
			if err := os.RemoveAll(nested); err != nil {
				t.Errorf("remove nested recovery mount fixture: %v", err)
			}
		})
		mountTmpfs(t, nested)
		subtree := filepath.Join(nested, "private subtree")
		deeper := filepath.Join(nested, "deeper mount")
		mkdirAll(t, subtree, deeper)
		mountTmpfs(t, deeper)

		candidate := t.TempDir()
		rootAlias := filepath.Join(candidate, "nested root alias")
		subtreeAlias := filepath.Join(candidate, "nested subtree alias")
		deeperAlias := filepath.Join(candidate, "deeper alias")
		external := filepath.Join(candidate, "ordinary external filesystem")
		mkdirAll(t, rootAlias, subtreeAlias, deeperAlias, external)
		bindMount(t, nested, rootAlias)
		bindMount(t, subtree, subtreeAlias)
		bindMount(t, deeper, deeperAlias)
		mountTmpfs(t, external)

		recovery, err := owner.OpenRecoveryRoot()
		if err != nil {
			t.Fatal(err)
		}
		defer recovery.Close()
		source := unrelatedRootTestSource(t)
		transfer, _ := transfernumber.New(1)
		for _, path := range []string{
			filepath.Join(rootAlias, "logs"),
			filepath.Join(subtreeAlias, "logs"),
			filepath.Join(deeperAlias, "logs"),
		} {
			if logs, err := Open(context.Background(), source, recovery, path, Interactive,
				[]transfernumber.Number{transfer}); err == nil {
				_ = logs.Close()
				t.Errorf("nested recovery alias %s was accepted", path)
			}
		}
		assertPathsAbsent(t, filepath.Join(nested, "logs"), filepath.Join(subtree, "logs"),
			filepath.Join(deeper, "logs"))

		externalLogs := filepath.Join(external, "logs")
		logs, err := Open(context.Background(), source, recovery, externalLogs, Interactive,
			[]transfernumber.Number{transfer})
		if err != nil {
			t.Fatalf("ordinary external filesystem was rejected: %v", err)
		}
		want := []byte("external-log\r\n")
		if err := logs.Write(transfer, PTY, want); err != nil {
			t.Fatal(err)
		}
		if err := logs.Close(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(filepath.Join(externalLogs, "transfer-01.ptylog"))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("external log=%q error=%v", got, err)
		}
		assertStaleEvidence(t, id, authorityBefore, runBefore)
	})
}

func requireRootMountNamespace(t *testing.T, test func()) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatalf("mount identity test requires root, got euid %d", os.Geteuid())
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	originalMount, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		t.Fatal(err)
	}
	defer originalMount.Close()
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("mount namespace unavailable: %v", err)
	}
	defer func() {
		if err := unix.Setns(int(originalMount.Fd()), unix.CLONE_NEWNS); err != nil {
			t.Errorf("restore test mount namespace: %v", err)
		}
	}()
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	test()
}

func staleProductionRun(t *testing.T) (*rundirectory.Owner, runid.ID, os.FileInfo, os.FileInfo) {
	t.Helper()
	if entries, err := os.ReadDir(rundirectory.AuthorityRoot); err == nil && len(entries) != 0 {
		t.Fatalf("rootintegration requires an empty production authority, found %d entries", len(entries))
	}
	owner := rundirectory.New()
	id, err := runid.New()
	if err != nil {
		t.Fatal(err)
	}
	live, err := owner.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := owner.RecoverStale(context.Background(), func(*rundirectory.StaleRun) error {
			return nil
		}); err != nil {
			t.Errorf("remove stale recovery fixture: %v", err)
		}
	})
	authorityBefore, err := os.Stat(rundirectory.AuthorityRoot)
	if err != nil {
		t.Fatal(err)
	}
	runBefore, err := os.Stat(filepath.Join(rundirectory.AuthorityRoot, id.String()))
	if err != nil {
		t.Fatal(err)
	}
	return owner, id, authorityBefore, runBefore
}

func assertStaleEvidence(t *testing.T, id runid.ID, authorityBefore, runBefore os.FileInfo) {
	t.Helper()
	authorityAfter, authorityErr := os.Stat(rundirectory.AuthorityRoot)
	runAfter, runErr := os.Stat(filepath.Join(rundirectory.AuthorityRoot, id.String()))
	if authorityErr != nil || runErr != nil || !os.SameFile(authorityBefore, authorityAfter) ||
		!os.SameFile(runBefore, runAfter) {
		t.Fatalf("stale evidence changed: authority=%v run=%v", authorityErr, runErr)
	}
	authorityEntries, err := os.ReadDir(rundirectory.AuthorityRoot)
	if err != nil || len(authorityEntries) != 1 || authorityEntries[0].Name() != id.String() {
		t.Fatalf("recovery authority entries changed: entries=%v error=%v", authorityEntries, err)
	}
	entries, err := os.ReadDir(filepath.Join(rundirectory.AuthorityRoot, id.String()))
	if err != nil {
		t.Fatalf("stale run evidence entries=%v error=%v", entries, err)
	}
	for _, entry := range entries {
		if entry.Name() == "logs" || entry.Name() == "transfer-01.ptylog" {
			t.Fatalf("raw logs changed stale recovery evidence: %v", entries)
		}
	}
}

func assertPathsAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("protected path %s was created: %v", path, err)
		}
	}
}

func rootTestRecovery(t *testing.T) (*rundirectory.RecoveryRoot, func()) {
	t.Helper()
	owner, _, err := rundirectory.CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := owner.OpenRecoveryRoot()
	if err != nil {
		t.Fatal(err)
	}
	return recovery, func() {
		_ = recovery.Close()
		_ = owner.ClosePrivateAuthority(context.Background())
	}
}

func unrelatedRootTestSource(t *testing.T) *sourcefiles.SourceRoot {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source")
	mkdirAll(t, path)
	return openRootTestSource(t, path)
}

func openRootTestSource(t *testing.T, path string) *sourcefiles.SourceRoot {
	t.Helper()
	source, err := sourcefiles.OpenSourceRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	return source
}

func mkdirAll(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func bindMount(t *testing.T, source, target string) {
	t.Helper()
	if err := unix.Mount(source, target, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Unmount(target, unix.MNT_DETACH) })
}

func mountTmpfs(t *testing.T, target string) {
	t.Helper()
	if err := unix.Mount("tmpfs", target, "tmpfs", 0, "mode=0700,size=1m"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Unmount(target, unix.MNT_DETACH) })
}
