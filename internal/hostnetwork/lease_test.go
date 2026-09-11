package hostnetwork

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

func mustTransferNumber(t *testing.T, number int) transfernumber.Number {
	t.Helper()
	id, err := transfernumber.New(number)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestNamespaceLeaseRemainsBoundToOriginalInode(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "namespace")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	session := newSession(nil, nil, networkClaim{}, &hostInstallation{
		namespaces: map[transfernumber.Number]*os.File{mustTransferNumber(t, 1): base}})
	lease, err := session.OpenNamespace(mustTransferNumber(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	leasedFD, err := unix.Dup(int(lease.Descriptor()))
	if err != nil {
		t.Fatal(err)
	}
	leased := os.NewFile(uintptr(leasedFD), "leased-namespace")
	defer leased.Close()
	leasedInfo, err := leased.Stat()
	if err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(path + ".old")
	if err != nil {
		t.Fatal(err)
	}
	replacementInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(leasedInfo, originalInfo) || os.SameFile(leasedInfo, replacementInfo) {
		t.Fatal("namespace lease followed a path replacement")
	}
}

func TestSessionCloseWaitsForLeasesAndPreventsNewOnes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "namespace")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	session := newSession(nil, nil, networkClaim{}, &hostInstallation{
		namespaces: map[transfernumber.Number]*os.File{mustTransferNumber(t, 1): base}})
	lease, err := session.OpenNamespace(mustTransferNumber(t, 1))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := session.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want context deadline", err)
	}
	if _, err := session.OpenNamespace(mustTransferNumber(t, 1)); !errors.Is(err, errSessionClosing) {
		t.Fatalf("OpenNamespace after Close error = %v, want closing error", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}
