//go:build linux && rootintegration

package rundirectory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"golang.org/x/sys/unix"
)

func TestPrivateAuthorityIgnoresBindMountedProductionAliases(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatalf("rootintegration requires euid 0, got %d", os.Geteuid())
	}
	productionExisted := true
	if _, err := os.Stat(AuthorityRoot); os.IsNotExist(err) {
		productionExisted = false
	} else if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(AuthorityRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if !productionExisted {
		t.Cleanup(func() { _ = os.Remove(AuthorityRoot) })
	}
	subtree := filepath.Join(AuthorityRoot, "private-bind-source")
	if err := os.Mkdir(subtree, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(subtree) })

	for name, source := range map[string]string{
		"production root":    AuthorityRoot,
		"production subtree": subtree,
	} {
		t.Run(name, func(t *testing.T) {
			owner, path, err := CreatePrivateAuthority()
			if err != nil {
				t.Fatal(err)
			}
			before := directoryEntries(t, source)
			if err := unix.Mount(source, path, "", unix.MS_BIND, ""); err != nil {
				t.Fatal(err)
			}
			mounted := true
			t.Cleanup(func() {
				if mounted {
					_ = unix.Unmount(path, unix.MNT_DETACH)
				}
				_ = owner.ClosePrivateAuthority(context.Background())
			})

			id := newTestRunID(t)
			live, err := owner.Create(id)
			if err != nil {
				t.Fatal(err)
			}
			root, err := live.OpenRoot()
			if err != nil {
				t.Fatal(err)
			}
			if got := directoryEntriesFromFile(t, root); !reflect.DeepEqual(got,
				[]string{livenessLockName, recoveryLockName}) {
				t.Fatalf("run entries=%v", got)
			}
			_ = root.Close()
			if after := directoryEntries(t, source); !reflect.DeepEqual(after, before) {
				t.Fatalf("production bind source changed: before=%v after=%v", before, after)
			}
			if err := live.Close(); err != nil {
				t.Fatal(err)
			}
			if err := owner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := unix.Unmount(path, 0); err != nil {
				t.Fatal(err)
			}
			mounted = false
			if entries := directoryEntries(t, path); len(entries) != 0 {
				t.Fatalf("private authority residue=%v", entries)
			}
			if err := owner.ClosePrivateAuthority(context.Background()); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("private authority remains: %v", err)
			}
		})
	}
}

func TestPrivateAuthorityRemainsBoundWhenProductionAppearsLater(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatalf("rootintegration requires euid 0, got %d", os.Geteuid())
	}
	existed := false
	if entries, err := os.ReadDir(AuthorityRoot); err == nil {
		if len(entries) != 0 {
			t.Fatalf("rootintegration requires an empty production authority, found %d entries", len(entries))
		}
		existed = true
		if err := os.Remove(AuthorityRoot); err != nil {
			t.Fatal(err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Remove(AuthorityRoot)
		if existed {
			_ = os.Mkdir(AuthorityRoot, 0o700)
		}
	})

	owner, path, err := CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.ClosePrivateAuthority(context.Background()) })
	if err := os.Mkdir(AuthorityRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	id := newTestRunID(t)
	live, err := owner.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	if entries := directoryEntries(t, AuthorityRoot); len(entries) != 0 {
		t.Fatalf("new production authority changed: %v", entries)
	}
	if _, err := os.Stat(filepath.Join(path, id.String())); err != nil {
		t.Fatalf("private run absent: %v", err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.RecoverStale(context.Background(), func(*StaleRun) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := owner.ClosePrivateAuthority(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func directoryEntries(t *testing.T, path string) []string {
	t.Helper()
	directory, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	return directoryEntriesFromFile(t, directory)
}

func directoryEntriesFromFile(t *testing.T, directory *os.File) []string {
	t.Helper()
	copy, err := openDirectoryCopy(directory, directory.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	entries, err := copy.Readdirnames(-1)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(entries)
	return entries
}
