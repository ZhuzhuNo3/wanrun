//go:build linux && rootintegration

package root_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/fileviews"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

func TestFileViewOpenRefusesExistingFinalEvidenceAndRemainsRecoverable(t *testing.T) {
	before := authorityEntries(t)
	id, _ := runid.New()
	live, err := rundirectory.New().Create(id)
	if err != nil {
		t.Fatal(err)
	}
	runRoot := filepath.Join(rundirectory.AuthorityRoot, id.String())
	foreign := filepath.Join(runRoot, "views")
	if err := os.WriteFile(foreign, []byte("foreign\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, allocation := oneTransferAllocation(t)
	if session, err := fileviews.New().Open(context.Background(), live, allocation, source); err == nil {
		_ = session.Close(context.Background())
		t.Fatal("FileViews.Open accepted existing final evidence")
	}
	if contents, err := os.ReadFile(foreign); err != nil || string(contents) != "foreign\n" {
		t.Fatalf("FileViews.Open changed foreign evidence: %q, %v", contents, err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	callbacks, err := recoverExactTransferRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if callbacks != 1 {
		t.Fatalf("file-view open stale recovery callback count=%d", callbacks)
	}
	if _, err := os.Stat(runRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file-view open run %s remains: %v", id, err)
	}
	if after := authorityEntries(t); !slices.Equal(after, before) {
		t.Fatalf("file-view open failure left authority entries: before=%v after=%v", before, after)
	}
}

func TestRunDirectoryFinalLockDeadlineReleasesLivenessAndRemainsRecoverable(t *testing.T) {
	before := authorityEntries(t)
	id, _ := runid.New()
	live, err := rundirectory.New().Create(id)
	if err != nil {
		t.Fatal(err)
	}
	runRoot := filepath.Join(rundirectory.AuthorityRoot, id.String())
	authority, err := os.Open(rundirectory.AuthorityRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(authority.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	started := time.Now()
	err = live.Complete(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("bounded run completion = %v after %s", err, time.Since(started))
	}
	if _, err := os.Stat(runRoot); err != nil {
		t.Fatalf("run root lost after final-lock deadline: %v", err)
	}
	if err := unix.Flock(int(authority.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	callbacks, err := recoverExactTransferRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if callbacks != 1 {
		t.Fatalf("run-directory deadline stale recovery callback count=%d", callbacks)
	}
	if _, err := os.Stat(runRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("run-directory deadline run %s remains: %v", id, err)
	}
	if after := authorityEntries(t); !slices.Equal(after, before) {
		t.Fatalf("run-directory deadline left authority entries: before=%v after=%v", before, after)
	}
}

func oneTransferAllocation(t *testing.T) (*sourcefiles.SourceRoot, sourcefiles.TransferAllocation) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "captured-source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := sourcefiles.OpenSourceRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.Close() })
	snapshot, err := directory.Scan(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := transfernumber.New(1)
	weight, _ := sourcefiles.NewTransferWeight(id, 1)
	weights, _ := sourcefiles.NewTransferWeights([]sourcefiles.TransferWeight{weight})
	allocation, err := sourcefiles.AllocateTransferItems(snapshot, weights)
	if err != nil {
		t.Fatal(err)
	}
	return directory, allocation
}
