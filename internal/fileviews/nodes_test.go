package fileviews

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/hanwen/go-fuse/v2/fuse"
)

func TestReadOnlyFileGetattrPreservesOpenedFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "special-mode")
	if err := os.WriteFile(path, []byte("contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o754|os.ModeSetuid|os.ModeSetgid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()

	var out fuse.AttrOut
	if errno := (&readOnlyFile{file: opened}).Getattr(context.Background(), &out); errno != 0 {
		t.Fatalf("getattr errno=%v", errno)
	}
	if got, want := out.Mode&(syscall.S_IFMT|0o7777), uint32(syscall.S_IFREG|0o7754); got != want {
		t.Fatalf("mode=%#o want=%#o", got, want)
	}
}
