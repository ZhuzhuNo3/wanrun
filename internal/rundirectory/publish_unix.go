//go:build linux || darwin

package rundirectory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"golang.org/x/sys/unix"
)

type runPublication struct {
	stagingName string
	published   bool
	directory   *os.File
	liveness    *os.File
	recovery    *os.File
}

func (owner *Owner) publishRun(authority *authorityDirectory, id runid.ID) (_ *LiveRun, resultErr error) {
	publication, err := owner.stageRun(authority, id)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, owner.discardRunPublication(authority, id, publication))
		}
	}()

	if err := owner.commitRunPublication(authority, id, publication); err != nil {
		return nil, err
	}
	return owner.liveRunFromPublication(id, publication)
}

func (owner *Owner) stageRun(
	authority *authorityDirectory,
	id runid.ID,
) (_ *runPublication, resultErr error) {
	stagingName, err := freshTemporaryName(stagingPrefix, id)
	if err != nil {
		return nil, err
	}
	if err := unix.Mkdirat(int(authority.root.Fd()), stagingName, 0o700); err != nil {
		return nil, fmt.Errorf("create staging directory: %w", err)
	}

	publication := &runPublication{stagingName: stagingName}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(
				resultErr,
				owner.discardRunPublication(authority, id, publication),
			)
		}
	}()

	stagingPath := filepath.Join(owner.authorityRoot, stagingName)
	publication.directory, err = openDirectoryAt(authority.root, stagingName, stagingPath)
	if err != nil {
		return nil, fmt.Errorf("open staging directory: %w", err)
	}
	if err := validateOwnedDirectory(publication.directory, 0o700); err != nil {
		return nil, fmt.Errorf("validate staging directory: %w", err)
	}
	publication.liveness, err = createLockFile(
		publication.directory, livenessLockName, filepath.Join(stagingPath, livenessLockName),
	)
	if err != nil {
		return nil, fmt.Errorf("create liveness lock: %w", err)
	}
	publication.recovery, err = createLockFile(
		publication.directory, recoveryLockName, filepath.Join(stagingPath, recoveryLockName),
	)
	if err != nil {
		return nil, fmt.Errorf("create recovery lock: %w", err)
	}
	if err := owner.syncPublishedFiles(
		publication.directory, publication.liveness, publication.recovery,
	); err != nil {
		return nil, err
	}
	if locked, err := tryLock(publication.liveness); err != nil {
		return nil, fmt.Errorf("lock liveness file: %w", err)
	} else if !locked {
		return nil, errors.New("new liveness file is unexpectedly locked")
	}
	return publication, nil
}

func (owner *Owner) commitRunPublication(
	authority *authorityDirectory,
	id runid.ID,
	publication *runPublication,
) error {
	if exists, err := entryExists(authority.root, id.String()); err != nil {
		return fmt.Errorf("check run identity collision: %w", err)
	} else if exists {
		return fmt.Errorf("run %s already exists", id)
	}
	if err := unix.Renameat(
		int(authority.root.Fd()), publication.stagingName,
		int(authority.root.Fd()), id.String(),
	); err != nil {
		return fmt.Errorf("publish run root: %w", err)
	}
	publication.published = true
	if err := owner.durability.Sync(authority.root); err != nil {
		return fmt.Errorf("sync published run root: %w", err)
	}
	if err := owner.verifyAuthorityLocation(authority); err != nil {
		return fmt.Errorf("verify published authority: %w", err)
	}
	return nil
}

func (owner *Owner) liveRunFromPublication(id runid.ID, publication *runPublication) (*LiveRun, error) {
	if err := publication.recovery.Close(); err != nil {
		return nil, fmt.Errorf("close recovery lock after publish: %w", err)
	}
	publication.recovery = nil
	lease := &runLease{
		owner: owner,
		id:    id,
		root:  publication.directory,
		live:  publication.liveness,
	}
	publication.directory = nil
	publication.liveness = nil
	return &LiveRun{lease: lease}, nil
}

func (owner *Owner) discardRunPublication(
	authority *authorityDirectory,
	id runid.ID,
	publication *runPublication,
) error {
	closeErr := closeRunFiles(publication.liveness, publication.recovery, publication.directory)
	publication.liveness = nil
	publication.recovery = nil
	publication.directory = nil

	if publication.published {
		if err := unix.Renameat(
			int(authority.root.Fd()), id.String(),
			int(authority.root.Fd()), publication.stagingName,
		); err != nil {
			return errors.Join(closeErr, fmt.Errorf("isolate failed publication: %w", err))
		}
		publication.published = false
	}
	return errors.Join(closeErr, owner.removeUnpublished(authority, publication.stagingName))
}

func (owner *Owner) syncPublishedFiles(directory, liveness, recovery *os.File) error {
	if err := owner.durability.Sync(liveness); err != nil {
		return fmt.Errorf("sync liveness lock: %w", err)
	}
	if err := owner.durability.Sync(recovery); err != nil {
		return fmt.Errorf("sync recovery lock: %w", err)
	}
	if err := owner.durability.Sync(directory); err != nil {
		return fmt.Errorf("sync staging directory: %w", err)
	}
	return nil
}

func freshTemporaryName(prefix string, id runid.ID) (string, error) {
	nonce, err := runid.New()
	if err != nil {
		return "", fmt.Errorf("generate temporary directory name: %w", err)
	}
	return prefix + id.String() + "-" + nonce.String(), nil
}

func closeRunFiles(liveness, recovery, directory *os.File) error {
	var closeErr error
	if liveness != nil {
		closeErr = errors.Join(closeErr, unlock(liveness), liveness.Close())
	}
	if recovery != nil {
		closeErr = errors.Join(closeErr, recovery.Close())
	}
	if directory != nil {
		closeErr = errors.Join(closeErr, directory.Close())
	}
	return closeErr
}

func entryExists(parent *os.File, name string) (bool, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	return false, err
}
