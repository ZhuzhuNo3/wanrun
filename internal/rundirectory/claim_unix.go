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

type claimKind uint8

const (
	claimMissing claimKind = iota
	claimActive
	claimBusy
	claimStale
)

type runClaim struct {
	kind  claimKind
	lease *runLease
}

func (owner *Owner) claimRun(authority *os.File, id runid.ID) (runClaim, error) {
	path := filepath.Join(owner.authorityRoot, id.String())
	root, err := openDirectoryAt(authority, id.String(), path)
	if errors.Is(err, unix.ENOENT) {
		return runClaim{kind: claimMissing}, nil
	}
	if err != nil {
		return runClaim{}, fmt.Errorf("open run root %s: %w", id, err)
	}
	if err := validateOwnedDirectory(root, 0o700); err != nil {
		_ = root.Close()
		return runClaim{}, fmt.Errorf("validate run root %s: %w", id, err)
	}
	return owner.lockRunForRecovery(id, path, root)
}

func (owner *Owner) lockRunForRecovery(id runid.ID, path string, root *os.File) (runClaim, error) {
	recovery, err := openLockFile(root, recoveryLockName, filepath.Join(path, recoveryLockName))
	if err != nil {
		_ = root.Close()
		return runClaim{}, fmt.Errorf("open recovery lock for %s: %w", id, err)
	}
	locked, err := tryLock(recovery)
	if err != nil {
		_ = recovery.Close()
		_ = root.Close()
		return runClaim{}, fmt.Errorf("lock recovery file for %s: %w", id, err)
	}
	if !locked {
		_ = recovery.Close()
		_ = root.Close()
		return runClaim{kind: claimBusy}, nil
	}
	return owner.checkRunLiveness(id, path, root, recovery)
}

func (owner *Owner) checkRunLiveness(
	id runid.ID,
	path string,
	root *os.File,
	recovery *os.File,
) (runClaim, error) {
	liveness, err := openLockFile(root, livenessLockName, filepath.Join(path, livenessLockName))
	if err != nil {
		_ = unlock(recovery)
		_ = recovery.Close()
		_ = root.Close()
		return runClaim{}, fmt.Errorf("open liveness lock for %s: %w", id, err)
	}
	liveLocked, err := tryLock(liveness)
	if err != nil {
		_ = closeRunFiles(liveness, recovery, root)
		return runClaim{}, fmt.Errorf("lock liveness file for %s: %w", id, err)
	}
	if !liveLocked {
		_ = closeRunFiles(nil, recovery, root)
		_ = liveness.Close()
		return runClaim{kind: claimActive}, nil
	}

	return runClaim{
		kind: claimStale,
		lease: &runLease{
			owner:    owner,
			id:       id,
			root:     root,
			live:     liveness,
			recovery: recovery,
		},
	}, nil
}

func readDirectoryNames(directory *os.File) ([]string, error) {
	copy, err := openDirectoryCopy(directory, directory.Name())
	if err != nil {
		return nil, err
	}
	defer copy.Close()
	return copy.Readdirnames(-1)
}
