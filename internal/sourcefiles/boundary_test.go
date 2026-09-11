//go:build linux || darwin

package sourcefiles

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSourceRootTransfersOwnedDescriptorOnce(t *testing.T) {
	parent := t.TempDir()
	path := filepath.Join(parent, "source")
	mustWrite(t, filepath.Join(path, "original"), "original")
	root, err := OpenSourceRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	borrow, err := root.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.TransferDescriptor(); !errors.Is(err, ErrSourceRootBorrowed) {
		t.Fatalf("transfer while borrowed=%v", err)
	}
	_ = borrow.Close()
	descriptor, err := root.TransferDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := root.TransferDescriptor(); !errors.Is(err, ErrSourceRootTransferred) {
		t.Fatalf("second transfer=%v", err)
	}
	original := path + "-original"
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(path, "replacement"), "replacement")
	received, err := TakeSourceRoot(descriptor, path)
	if err != nil {
		t.Fatal(err)
	}
	defer received.Close()
	snapshot, err := received.Scan(context.Background(), false)
	if err != nil || !reflect.DeepEqual(filePaths(snapshot), []string{"original"}) {
		t.Fatalf("transferred snapshot=%v err=%v", filePaths(snapshot), err)
	}
}
