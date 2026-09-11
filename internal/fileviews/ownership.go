package fileviews

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
)

const (
	viewsDirectoryName   = "views"
	ownershipFileName    = ".transferlanes-file-views"
	ownershipFormat      = "transferlanes-file-views-v3"
	maximumOwnershipSize = 256
)

type fileIdentity struct {
	device uint64
	inode  uint64
}

type fileViewOwnership struct {
	runID    runid.ID
	identity fileIdentity
}

type fileViewOwnershipEvidence struct {
	ownership fileViewOwnership
	complete  bool
}

func newFileViewOwnership(id runid.ID, identity fileIdentity) fileViewOwnership {
	return fileViewOwnership{runID: id, identity: identity}
}

func writeFileViewOwnership(root *os.File, ownership fileViewOwnership) error {
	file, err := createFileViewOwnership(root)
	if err != nil {
		return fmt.Errorf("create file-view ownership: %w", err)
	}
	contents := encodeFileViewOwnership(ownership)
	if _, err := file.Write(contents); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func inspectFileViewOwnership(root *os.File) (fileViewOwnershipEvidence, error) {
	file, err := openFileViewOwnership(root)
	if err != nil {
		return fileViewOwnershipEvidence{}, err
	}
	defer file.Close()
	facts, err := inspectFileViewOwnershipFile(file)
	if err != nil || !facts.regular || !facts.ownedByProcess || facts.permissions != 0o600 ||
		facts.links != 1 || facts.size > maximumOwnershipSize {
		return fileViewOwnershipEvidence{}, errors.Join(
			errors.New("file-view ownership has unsafe shape"), err)
	}
	contents, err := io.ReadAll(io.LimitReader(file, maximumOwnershipSize+1))
	if err != nil || len(contents) > maximumOwnershipSize {
		return fileViewOwnershipEvidence{}, errors.Join(errors.New("read file-view ownership"), err)
	}
	ownership, decodeErr := decodeFileViewOwnership(string(contents))
	return fileViewOwnershipEvidence{ownership: ownership, complete: decodeErr == nil}, nil
}

func encodeFileViewOwnership(value fileViewOwnership) []byte {
	return []byte(fmt.Sprintf("%s\nrun=%s\ndevice=%d\ninode=%d\n",
		ownershipFormat, value.runID, value.identity.device, value.identity.inode))
}

func decodeFileViewOwnership(contents string) (fileViewOwnership, error) {
	lines := strings.Split(contents, "\n")
	if len(lines) != 5 || lines[0] != ownershipFormat || lines[4] != "" {
		return fileViewOwnership{}, errors.New("file-view ownership format is invalid")
	}
	id, err := parseRunField(lines[1])
	if err != nil {
		return fileViewOwnership{}, err
	}
	device, deviceErr := parseUintField(lines[2], "device=")
	inode, inodeErr := parseUintField(lines[3], "inode=")
	if err := errors.Join(deviceErr, inodeErr); err != nil || inode == 0 {
		return fileViewOwnership{}, errors.Join(errors.New("file-view ownership identity is invalid"), err)
	}
	return fileViewOwnership{runID: id, identity: fileIdentity{device: device, inode: inode}}, nil
}

func parseRunField(line string) (runid.ID, error) {
	if !strings.HasPrefix(line, "run=") {
		return runid.ID{}, errors.New("file-view ownership run is invalid")
	}
	id, err := runid.Parse(strings.TrimPrefix(line, "run="))
	if err != nil {
		return runid.ID{}, errors.New("file-view ownership run is invalid")
	}
	return id, nil
}

func parseUintField(line, prefix string) (uint64, error) {
	if !strings.HasPrefix(line, prefix) {
		return 0, errors.New("file-view ownership field is invalid")
	}
	return strconv.ParseUint(strings.TrimPrefix(line, prefix), 10, 64)
}

func directoryIdentity(directory *os.File) (fileIdentity, error) {
	facts, err := inspectFileViewDirectory(directory)
	return facts.identity, err
}

func validatePrivateDirectory(directory *os.File) error {
	facts, err := inspectFileViewDirectory(directory)
	if err != nil {
		return err
	}
	if !facts.ownedByProcess || facts.permissions != 0o700 || facts.links < 1 {
		return errors.New("directory is not an effective-user-owned private directory")
	}
	return nil
}

func openDirectoryAt(parent *os.File, path string, private bool) (*os.File, error) {
	current, err := openFileViewDirectoryAt(parent, path)
	if err != nil {
		return nil, err
	}
	if private {
		if err := validatePrivateDirectory(current); err != nil {
			_ = current.Close()
			return nil, err
		}
	}
	return current, nil
}
