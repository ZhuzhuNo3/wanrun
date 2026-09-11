//go:build linux || darwin

package fileviews

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
)

func TestRecoverConvergesInterruptedEvidence(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		build func(*testing.T, *os.File, runid.ID)
	}{
		{name: "views only", build: createViewsOnlyEvidence},
		{name: "marker only", build: createMarkerOnlyEvidence},
		{name: "partial marker", build: createPartialMarkerEvidence},
		{name: "corrupt marker only", build: createCorruptMarkerEvidence},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			root, path, id := openEvidenceTestRoot(t)
			fixture.build(t, root, id)

			owner := New()
			if err := owner.recover(context.Background(), recoveryTestRunAccess{id: id, root: root}); err != nil {
				t.Fatalf("recover interrupted evidence: %v", err)
			}
			assertNoFileViewEvidence(t, path)
			if err := owner.recover(context.Background(), recoveryTestRunAccess{id: id, root: root}); err != nil {
				t.Fatalf("repeat recovery: %v", err)
			}
		})
	}
}

func createViewsOnlyEvidence(t *testing.T, root *os.File, _ runid.ID) {
	t.Helper()
	if err := createFileViewMountpoint(root); err != nil {
		t.Fatal(err)
	}
}

func createMarkerOnlyEvidence(t *testing.T, root *os.File, id runid.ID) {
	t.Helper()
	createCompleteEvidence(t, root, id)
	if err := removeFileViewMountpoint(root); err != nil {
		t.Fatal(err)
	}
}

func createPartialMarkerEvidence(t *testing.T, root *os.File, _ runid.ID) {
	t.Helper()
	if err := createFileViewMountpoint(root); err != nil {
		t.Fatal(err)
	}
	writeRawOwnership(t, root, ownershipFormat+"\nrun=")
}

func createCorruptMarkerEvidence(t *testing.T, root *os.File, _ runid.ID) {
	t.Helper()
	writeRawOwnership(t, root, "not ownership evidence\n")
}

func TestPrepareMountpointRollsBackEveryFailedStage(t *testing.T) {
	for _, stage := range []string{"create mountpoint", "inspect mountpoint", "write ownership", "sync root"} {
		t.Run(stage, func(t *testing.T) {
			root, path, id := openEvidenceTestRoot(t)
			files := &failingFileViewEvidenceFiles{stage: stage}

			if _, err := prepareMountpointWithFiles(root, id, files); !errors.Is(err, errInjectedEvidenceIO) {
				t.Fatalf("prepare error=%v, want injected failure", err)
			}
			assertNoFileViewEvidence(t, path)
		})
	}
}

func TestRecoverContinuesAfterInterruptedDeletion(t *testing.T) {
	root, path, id := openEvidenceTestRoot(t)
	ownership := createCompleteEvidence(t, root, id)
	files := &failingFileViewEvidenceFiles{stage: "remove ownership"}

	if err := removePreparedEvidenceWithFiles(root, ownership, files); !errors.Is(err, errInjectedEvidenceIO) {
		t.Fatalf("remove error=%v, want injected failure", err)
	}
	if _, err := os.Lstat(filepath.Join(path, viewsDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first deletion step did not persist: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(path, ownershipFileName)); err != nil {
		t.Fatalf("ownership needed for retry is missing: %v", err)
	}

	if err := recoverFileViewEvidence(root, id, descriptorFileViewEvidenceFiles{}); err != nil {
		t.Fatalf("continue recovery: %v", err)
	}
	assertNoFileViewEvidence(t, path)
	if err := recoverFileViewEvidence(root, id, descriptorFileViewEvidenceFiles{}); err != nil {
		t.Fatalf("repeat completed recovery: %v", err)
	}
}

func TestRecoverRetriesOtherDeletionStages(t *testing.T) {
	for _, stage := range []string{"remove mountpoint", "sync root"} {
		t.Run(stage, func(t *testing.T) {
			root, path, id := openEvidenceTestRoot(t)
			ownership := createCompleteEvidence(t, root, id)
			files := &failingFileViewEvidenceFiles{stage: stage}

			if err := removePreparedEvidenceWithFiles(root, ownership, files); !errors.Is(err, errInjectedEvidenceIO) {
				t.Fatalf("remove error=%v, want injected failure", err)
			}
			if err := recoverFileViewEvidence(root, id, files); err != nil {
				t.Fatalf("retry recovery: %v", err)
			}
			assertNoFileViewEvidence(t, path)
		})
	}
}

func TestRecoverRejectsMismatchedEvidence(t *testing.T) {
	t.Run("run", func(t *testing.T) {
		root, path, id := openEvidenceTestRoot(t)
		other, err := runid.New()
		if err != nil {
			t.Fatal(err)
		}
		createCompleteEvidence(t, root, other)

		if err := recoverFileViewEvidence(root, id, descriptorFileViewEvidenceFiles{}); err == nil {
			t.Fatal("recovered ownership belonging to another run")
		}
		assertFileViewEvidencePresent(t, path)
	})

	t.Run("mountpoint identity", func(t *testing.T) {
		root, path, id := openEvidenceTestRoot(t)
		createCompleteEvidence(t, root, id)
		original := filepath.Join(path, viewsDirectoryName+"-original")
		if err := os.Rename(filepath.Join(path, viewsDirectoryName), original); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(path, viewsDirectoryName), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := recoverFileViewEvidence(root, id, descriptorFileViewEvidenceFiles{}); err == nil {
			t.Fatal("recovered a replacement mountpoint")
		}
		assertFileViewEvidencePresent(t, path)
	})

	t.Run("unsafe marker", func(t *testing.T) {
		root, path, id := openEvidenceTestRoot(t)
		createCompleteEvidence(t, root, id)
		if err := os.Chmod(filepath.Join(path, ownershipFileName), 0o644); err != nil {
			t.Fatal(err)
		}

		if err := recoverFileViewEvidence(root, id, descriptorFileViewEvidenceFiles{}); err == nil {
			t.Fatal("recovered an unsafe ownership marker")
		}
		assertFileViewEvidencePresent(t, path)
	})
}

func createCompleteEvidence(t *testing.T, root *os.File, id runid.ID) fileViewOwnership {
	t.Helper()
	if err := createFileViewMountpoint(root); err != nil {
		t.Fatal(err)
	}
	directory, err := openDirectoryAt(root, viewsDirectoryName, true)
	if err != nil {
		t.Fatal(err)
	}
	identity, identityErr := directoryIdentity(directory)
	closeErr := directory.Close()
	if err := errors.Join(identityErr, closeErr); err != nil {
		t.Fatal(err)
	}
	ownership := newFileViewOwnership(id, identity)
	if err := writeFileViewOwnership(root, ownership); err != nil {
		t.Fatal(err)
	}
	return ownership
}

type recoveryTestRunAccess struct {
	id   runid.ID
	root *os.File
}

func (run recoveryTestRunAccess) ID() runid.ID { return run.id }

func (run recoveryTestRunAccess) OpenRoot() (*os.File, error) {
	return duplicateFileViewDescriptor(run.root, "file-view-recovery-test-run")
}

func writeRawOwnership(t *testing.T, root *os.File, contents string) {
	t.Helper()
	file, err := createFileViewOwnership(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(contents); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func openEvidenceTestRoot(t *testing.T) (*os.File, string, runid.ID) {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	id, err := runid.New()
	if err != nil {
		t.Fatal(err)
	}
	return root, path, id
}

func assertNoFileViewEvidence(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{viewsDirectoryName, ownershipFileName} {
		if _, err := os.Lstat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("evidence %q remains: %v", name, err)
		}
	}
}

func assertFileViewEvidencePresent(t *testing.T, root string) {
	t.Helper()
	for _, name := range []string{viewsDirectoryName, ownershipFileName} {
		if _, err := os.Lstat(filepath.Join(root, name)); err != nil {
			t.Fatalf("expected evidence %q: %v", name, err)
		}
	}
}

var errInjectedEvidenceIO = errors.New("injected file-view evidence failure")

type failingFileViewEvidenceFiles struct {
	descriptorFileViewEvidenceFiles
	stage string
	fired bool
}

func (files *failingFileViewEvidenceFiles) fail(stage string) bool {
	if files.stage != stage || files.fired {
		return false
	}
	files.fired = true
	return true
}

func (files *failingFileViewEvidenceFiles) createMountpoint(root *os.File) error {
	if files.fail("create mountpoint") {
		return errInjectedEvidenceIO
	}
	return files.descriptorFileViewEvidenceFiles.createMountpoint(root)
}

func (files *failingFileViewEvidenceFiles) inspectMountpoint(root *os.File) (
	fileViewMountpointEvidence, error,
) {
	if files.fail("inspect mountpoint") {
		return fileViewMountpointEvidence{}, errInjectedEvidenceIO
	}
	return files.descriptorFileViewEvidenceFiles.inspectMountpoint(root)
}

func (files *failingFileViewEvidenceFiles) writeOwnership(root *os.File,
	ownership fileViewOwnership,
) error {
	if files.fail("write ownership") {
		file, err := createFileViewOwnership(root)
		if err != nil {
			return errors.Join(errInjectedEvidenceIO, err)
		}
		_, writeErr := file.Write([]byte(ownershipFormat + "\nrun="))
		return errors.Join(errInjectedEvidenceIO, writeErr, file.Close())
	}
	return files.descriptorFileViewEvidenceFiles.writeOwnership(root, ownership)
}

func (files *failingFileViewEvidenceFiles) removeOwnership(root *os.File) error {
	if files.fail("remove ownership") {
		return errInjectedEvidenceIO
	}
	return files.descriptorFileViewEvidenceFiles.removeOwnership(root)
}

func (files *failingFileViewEvidenceFiles) removeMountpoint(root *os.File) error {
	if files.fail("remove mountpoint") {
		return errInjectedEvidenceIO
	}
	return files.descriptorFileViewEvidenceFiles.removeMountpoint(root)
}

func (files *failingFileViewEvidenceFiles) syncRoot(root *os.File) error {
	if files.fail("sync root") {
		return errInjectedEvidenceIO
	}
	return files.descriptorFileViewEvidenceFiles.syncRoot(root)
}
