package rundirectory

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRecoveryRootReadsPrivateAuthorityWithoutRecoveryRights(t *testing.T) {
	owner, path, err := CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	root, err := owner.OpenRecoveryRoot()
	if err != nil {
		t.Fatal(err)
	}
	borrow, err := root.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	if borrow.LogicalPath() != path || len(borrow.MissingPath()) != 0 {
		t.Fatalf("recovery location=%q missing=%v", borrow.LogicalPath(), borrow.MissingPath())
	}
	var info unix.Stat_t
	if err := unix.Fstat(int(borrow.Descriptor()), &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFDIR {
		t.Fatalf("recovery descriptor is not a directory: %v", err)
	}
	if err := borrow.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if err := owner.ClosePrivateAuthority(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryRootDoesNotCreateAMissingAuthority(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "missing", "authority")
	owner, err := newOwner(path, systemDurability{})
	if err != nil {
		t.Fatal(err)
	}
	root, err := owner.OpenRecoveryRoot()
	if err != nil {
		t.Fatal(err)
	}
	borrow, err := root.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := borrow.MissingPath(), []string{"missing", "authority"}; !equalPathParts(got, want) {
		t.Fatalf("missing path=%v, want %v", got, want)
	}
	if _, err := os.Lstat(filepath.Join(base, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read-only recovery boundary created a directory: %v", err)
	}
	if err := borrow.Close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
}

func equalPathParts(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
