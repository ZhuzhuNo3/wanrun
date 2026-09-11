package fileviews

import (
	"errors"
	"fmt"
	"os"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
)

type fileViewEvidenceFiles interface {
	presence(*os.File) (fileViewEvidencePresence, error)
	createMountpoint(*os.File) error
	inspectMountpoint(*os.File) (fileViewMountpointEvidence, error)
	writeOwnership(*os.File, fileViewOwnership) error
	inspectOwnership(*os.File) (fileViewOwnershipEvidence, error)
	removeMountpoint(*os.File) error
	removeOwnership(*os.File) error
	syncRoot(*os.File) error
}

type fileViewEvidencePresence struct {
	mountpoint bool
	ownership  bool
}

func (presence fileViewEvidencePresence) any() bool {
	return presence.mountpoint || presence.ownership
}

type fileViewMountpointEvidence struct {
	identity  fileIdentity
	sameMount bool
}

type observedFileViewEvidence struct {
	presence   fileViewEvidencePresence
	mountpoint fileViewMountpointEvidence
	ownership  fileViewOwnershipEvidence
}

type descriptorFileViewEvidenceFiles struct{}

func (descriptorFileViewEvidenceFiles) presence(root *os.File) (fileViewEvidencePresence, error) {
	mountpoint, mountpointErr := fileViewEntryPresent(root, viewsDirectoryName)
	ownership, ownershipErr := fileViewEntryPresent(root, ownershipFileName)
	return fileViewEvidencePresence{mountpoint: mountpoint, ownership: ownership},
		errors.Join(mountpointErr, ownershipErr)
}

func (descriptorFileViewEvidenceFiles) createMountpoint(root *os.File) error {
	return createFileViewMountpoint(root)
}

func (descriptorFileViewEvidenceFiles) inspectMountpoint(root *os.File) (
	fileViewMountpointEvidence, error,
) {
	directory, err := openDirectoryAt(root, viewsDirectoryName, true)
	if err != nil {
		return fileViewMountpointEvidence{}, err
	}
	identity, identityErr := directoryIdentity(directory)
	sameMount, mountErr := directorySharesMount(root, directory)
	closeErr := directory.Close()
	return fileViewMountpointEvidence{identity: identity, sameMount: sameMount},
		errors.Join(identityErr, mountErr, closeErr)
}

func (descriptorFileViewEvidenceFiles) writeOwnership(root *os.File,
	ownership fileViewOwnership,
) error {
	return writeFileViewOwnership(root, ownership)
}

func (descriptorFileViewEvidenceFiles) inspectOwnership(root *os.File) (
	fileViewOwnershipEvidence, error,
) {
	return inspectFileViewOwnership(root)
}

func (descriptorFileViewEvidenceFiles) removeMountpoint(root *os.File) error {
	return removeFileViewMountpoint(root)
}

func (descriptorFileViewEvidenceFiles) removeOwnership(root *os.File) error {
	return removeFileViewOwnership(root)
}

func (descriptorFileViewEvidenceFiles) syncRoot(root *os.File) error { return root.Sync() }

func prepareMountpoint(root *os.File, id runid.ID) (fileViewOwnership, error) {
	return prepareMountpointWithFiles(root, id, descriptorFileViewEvidenceFiles{})
}

func prepareMountpointWithFiles(root *os.File, id runid.ID,
	files fileViewEvidenceFiles,
) (fileViewOwnership, error) {
	presence, err := files.presence(root)
	if err != nil || presence.any() {
		return fileViewOwnership{}, errors.Join(errors.New("file-view evidence already exists"), err)
	}
	if err := files.createMountpoint(root); err != nil {
		return fileViewOwnership{}, fmt.Errorf("create file-view mountpoint: %w", err)
	}
	mountpoint, err := files.inspectMountpoint(root)
	if err != nil || !mountpoint.sameMount {
		if err == nil {
			err = errors.New("new file-view mountpoint changed mounts")
		}
		return fileViewOwnership{}, errors.Join(err, recoverFileViewEvidence(root, id, files))
	}
	ownership := newFileViewOwnership(id, mountpoint.identity)
	if err := files.writeOwnership(root, ownership); err != nil {
		return fileViewOwnership{}, errors.Join(err, recoverFileViewEvidence(root, id, files))
	}
	if err := files.syncRoot(root); err != nil {
		return fileViewOwnership{}, errors.Join(fmt.Errorf("sync file-view evidence: %w", err),
			recoverFileViewEvidence(root, id, files))
	}
	return ownership, nil
}

func recoverFileViewEvidence(root *os.File, id runid.ID,
	files fileViewEvidenceFiles,
) error {
	evidence, err := observeFileViewEvidence(root, files)
	if err != nil {
		return err
	}
	if err := validateRecoverableFileViewEvidence(evidence, id); err != nil {
		return err
	}
	return deleteFileViewEvidence(root, evidence.presence, files)
}

func removePreparedEvidence(root *os.File, ownership fileViewOwnership) error {
	return removePreparedEvidenceWithFiles(root, ownership, descriptorFileViewEvidenceFiles{})
}

func removePreparedEvidenceWithFiles(root *os.File, ownership fileViewOwnership,
	files fileViewEvidenceFiles,
) error {
	evidence, err := observeFileViewEvidence(root, files)
	if err != nil {
		return err
	}
	if err := validateOwnedFileViewEvidence(evidence, ownership); err != nil {
		return err
	}
	return deleteFileViewEvidence(root, evidence.presence, files)
}

func observeFileViewEvidence(root *os.File,
	files fileViewEvidenceFiles,
) (observedFileViewEvidence, error) {
	presence, err := files.presence(root)
	if err != nil {
		return observedFileViewEvidence{}, fmt.Errorf("inspect file-view evidence: %w", err)
	}
	evidence := observedFileViewEvidence{presence: presence}
	if presence.mountpoint {
		evidence.mountpoint, err = files.inspectMountpoint(root)
		if err != nil {
			return observedFileViewEvidence{}, fmt.Errorf("inspect file-view mountpoint: %w", err)
		}
	}
	if presence.ownership {
		evidence.ownership, err = files.inspectOwnership(root)
		if err != nil {
			return observedFileViewEvidence{}, fmt.Errorf("inspect file-view ownership: %w", err)
		}
	}
	return evidence, nil
}

func validateRecoverableFileViewEvidence(evidence observedFileViewEvidence, id runid.ID) error {
	if evidence.presence.mountpoint && !evidence.mountpoint.sameMount {
		return errors.New("file-view mountpoint is still mounted")
	}
	if !evidence.presence.ownership || !evidence.ownership.complete {
		return nil
	}
	if evidence.ownership.ownership.runID != id {
		return errors.New("stale file-view ownership does not match its run")
	}
	if evidence.presence.mountpoint &&
		evidence.ownership.ownership.identity != evidence.mountpoint.identity {
		return errors.New("file-view mountpoint changed identity")
	}
	return nil
}

func validateOwnedFileViewEvidence(evidence observedFileViewEvidence,
	ownership fileViewOwnership,
) error {
	if evidence.presence.mountpoint {
		if !evidence.mountpoint.sameMount || evidence.mountpoint.identity != ownership.identity {
			return errors.New("file-view mountpoint is mounted or changed identity")
		}
	}
	if evidence.presence.ownership &&
		(!evidence.ownership.complete || evidence.ownership.ownership != ownership) {
		return errors.New("file-view ownership changed identity")
	}
	return nil
}

func deleteFileViewEvidence(root *os.File, presence fileViewEvidencePresence,
	files fileViewEvidenceFiles,
) error {
	changed := false
	if presence.mountpoint {
		if err := files.removeMountpoint(root); err != nil {
			return fmt.Errorf("remove file-view mountpoint: %w", err)
		}
		changed = true
	}
	if presence.ownership {
		if err := files.removeOwnership(root); err != nil {
			return errors.Join(fmt.Errorf("remove file-view ownership: %w", err),
				syncChangedFileViewEvidence(root, changed, files))
		}
		changed = true
	}
	return files.syncRoot(root)
}

func syncChangedFileViewEvidence(root *os.File, changed bool,
	files fileViewEvidenceFiles,
) error {
	if !changed {
		return nil
	}
	return files.syncRoot(root)
}
