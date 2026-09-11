//go:build linux || darwin

package hostnetwork

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"golang.org/x/sys/unix"
)

const (
	networkDirectoryName        = "network"
	claimFileName               = "claim"
	claimTemporaryName          = ".claim.tmp"
	claimDiscardPrefix          = ".claim.discard."
	activationFileName          = "traffic-activated"
	activationTemporaryName     = ".traffic-activated.tmp"
	activationDiscardPrefix     = ".traffic-activated.discard."
	evidenceDiscardSuffixLength = 32
)

type networkEvidence struct {
	claim     networkClaim
	activated bool
}

func publishClaim(root *os.File, claim networkClaim, syncer fileSync) error {
	encoded, err := encodeClaim(claim)
	if err != nil {
		return err
	}
	directory, _, err := openNetworkDirectory(root, true)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := syncer.Sync(root); err != nil {
		return fmt.Errorf("sync run root before network claim publication: %w", err)
	}
	state, err := inspectEvidenceDirectory(directory)
	if err != nil {
		return err
	}
	if state.claim || state.activation {
		return fmt.Errorf("network directory contains published evidence")
	}
	if err := removeEvidenceStaging(directory, state.staging, syncer); err != nil {
		return err
	}
	return writeEvidence(directory, claimTemporaryName, claimFileName, encoded, syncer)
}

func publishTrafficActivation(root *os.File, claim networkClaim, syncer fileSync) error {
	encoded, err := encodeTrafficActivation(claim)
	if err != nil {
		return err
	}
	directory, _, err := openNetworkDirectory(root, false)
	if err != nil {
		return fmt.Errorf("open network evidence for activation: %w", err)
	}
	defer directory.Close()
	state, err := inspectEvidenceDirectory(directory)
	if err != nil {
		return err
	}
	if !state.claim || state.activation {
		return fmt.Errorf("network claim is absent or already activated")
	}
	if err := removeEvidenceStaging(directory, state.staging, syncer); err != nil {
		return err
	}
	actual, err := readClaimFile(directory, claim.runID)
	if err != nil {
		return err
	}
	if !claimsEqual(actual, claim) {
		return errors.New("network claim changed before activation")
	}
	return writeEvidence(directory, activationTemporaryName, activationFileName, encoded, syncer)
}

func loadNetworkEvidence(root *os.File, id runid.ID, syncer fileSync) (networkEvidence, bool, error) {
	directory, _, err := openNetworkDirectory(root, false)
	if errors.Is(err, os.ErrNotExist) {
		return networkEvidence{}, false, nil
	}
	if err != nil {
		return networkEvidence{}, false, err
	}
	defer directory.Close()
	state, err := inspectEvidenceDirectory(directory)
	if err != nil {
		return networkEvidence{}, false, err
	}
	if err := removeEvidenceStaging(directory, state.staging, syncer); err != nil {
		return networkEvidence{}, false, err
	}
	if !state.claim {
		if err := removeEmptyNetworkDirectory(root, directory, syncer); err != nil {
			return networkEvidence{}, false, err
		}
		return networkEvidence{}, false, nil
	}
	claim, err := readClaimFile(directory, id)
	if err != nil {
		return networkEvidence{}, false, err
	}
	if state.activation {
		if err := readActivationFile(directory, claim); err != nil {
			return networkEvidence{}, false, err
		}
	}
	return networkEvidence{claim: claim, activated: state.activation}, true, nil
}

func removeNetworkEvidence(root *os.File, expected networkEvidence, syncer fileSync) error {
	actual, exists, err := loadNetworkEvidence(root, expected.claim.runID, syncer)
	if err != nil {
		return err
	}
	if !exists || actual.activated != expected.activated || !claimsEqual(actual.claim, expected.claim) {
		return errors.New("network evidence changed before removal")
	}
	directory, _, err := openNetworkDirectory(root, false)
	if err != nil {
		return fmt.Errorf("open network evidence for removal: %w", err)
	}
	if expected.activated {
		if err := unlinkEvidence(directory, activationFileName, syncer); err != nil {
			_ = directory.Close()
			return err
		}
	}
	if err := unlinkEvidence(directory, claimFileName, syncer); err != nil {
		_ = directory.Close()
		return err
	}
	if err := removeEmptyNetworkDirectory(root, directory, syncer); err != nil {
		_ = directory.Close()
		return err
	}
	return directory.Close()
}

type evidenceDirectoryState struct {
	claim      bool
	activation bool
	staging    []string
}

func inspectEvidenceDirectory(directory *os.File) (evidenceDirectoryState, error) {
	names, err := directoryNames(directory)
	if err != nil {
		return evidenceDirectoryState{}, err
	}
	state := evidenceDirectoryState{}
	for _, name := range names {
		switch {
		case name == claimFileName:
			state.claim = true
		case name == activationFileName:
			state.activation = true
		case name == claimTemporaryName || isEvidenceDiscard(name, claimDiscardPrefix):
			state.staging = append(state.staging, name)
		case name == activationTemporaryName || isEvidenceDiscard(name, activationDiscardPrefix):
			state.staging = append(state.staging, name)
		default:
			return evidenceDirectoryState{}, fmt.Errorf("network directory contains unknown evidence %q", name)
		}
	}
	if state.activation && !state.claim {
		return evidenceDirectoryState{}, errors.New("network directory has activation without its claim")
	}
	if len(state.staging) > 1 ||
		len(state.staging) == 1 && strings.HasPrefix(state.staging[0], activationDiscardPrefix) && !state.claim ||
		len(state.staging) == 1 && state.staging[0] == activationTemporaryName && !state.claim ||
		state.activation && len(state.staging) != 0 {
		return evidenceDirectoryState{}, fmt.Errorf("network directory contains contradictory evidence: %v", names)
	}
	sort.Strings(state.staging)
	return state, nil
}

func isEvidenceDiscard(name, prefix string) bool {
	suffix, found := strings.CutPrefix(name, prefix)
	if !found || len(suffix) != evidenceDiscardSuffixLength {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func writeEvidence(directory *os.File, temporaryName, finalName string, encoded []byte, syncer fileSync) error {
	fd, err := unix.Openat(int(directory.Fd()), temporaryName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary network evidence %q: %w", temporaryName, err)
	}
	temporary := os.NewFile(uintptr(fd), temporaryName)
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary network evidence %q: %w", temporaryName, err)
	}
	if err := syncer.Sync(temporary); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync temporary network evidence %q: %w", temporaryName, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary network evidence %q: %w", temporaryName, err)
	}
	if err := renameNoReplaceAt(int(directory.Fd()), temporaryName, finalName); err != nil {
		return fmt.Errorf("publish network evidence %q: %w", finalName, err)
	}
	if err := syncer.Sync(directory); err != nil {
		return fmt.Errorf("sync published network evidence %q: %w", finalName, err)
	}
	return nil
}

func readClaimFile(directory *os.File, id runid.ID) (networkClaim, error) {
	file, err := openEvidenceFile(directory, claimFileName)
	if err != nil {
		return networkClaim{}, err
	}
	defer file.Close()
	return decodeClaim(file, id)
}

func readActivationFile(directory *os.File, claim networkClaim) error {
	file, err := openEvidenceFile(directory, activationFileName)
	if err != nil {
		return err
	}
	defer file.Close()
	return decodeTrafficActivation(file, claim)
}

func openEvidenceFile(directory *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), name,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open network evidence %q: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o177 != 0 {
		_ = file.Close()
		return nil, fmt.Errorf("network evidence %q has unsafe identity or permissions", name)
	}
	var status unix.Stat_t
	if err := unix.Fstat(fd, &status); err != nil || status.Uid != uint32(os.Geteuid()) || status.Nlink != 1 {
		_ = file.Close()
		return nil, fmt.Errorf("network evidence %q has unsafe ownership", name)
	}
	return file, nil
}

func removeEvidenceStaging(directory *os.File, names []string, syncer fileSync) error {
	for _, name := range names {
		if err := isolateAndRemoveStaging(directory, name, syncer); err != nil {
			return err
		}
	}
	return nil
}

func isolateAndRemoveStaging(directory *os.File, name string, syncer fileSync) error {
	original, err := openEvidenceFile(directory, name)
	if err != nil {
		return err
	}
	defer original.Close()
	originalInfo, err := original.Stat()
	if err != nil {
		return fmt.Errorf("inspect network evidence staging %q: %w", name, err)
	}
	isolation, err := evidenceIsolationName(name)
	if err != nil {
		return err
	}
	if err := renameNoReplaceAt(int(directory.Fd()), name, isolation); err != nil {
		return fmt.Errorf("isolate network evidence staging %q: %w", name, err)
	}
	isolated, err := openEvidenceFile(directory, isolation)
	if err != nil {
		return preserveEvidenceStaging(directory, isolation, name, err)
	}
	isolatedInfo, statErr := isolated.Stat()
	_ = isolated.Close()
	if statErr != nil || !os.SameFile(originalInfo, isolatedInfo) {
		if statErr == nil {
			statErr = errors.New("network evidence staging identity changed")
		}
		return preserveEvidenceStaging(directory, isolation, name, statErr)
	}
	if err := unix.Unlinkat(int(directory.Fd()), isolation, 0); err != nil {
		return fmt.Errorf("remove network evidence staging %q: %w", isolation, err)
	}
	if err := syncer.Sync(directory); err != nil {
		return fmt.Errorf("sync network evidence staging removal: %w", err)
	}
	return nil
}

func evidenceIsolationName(name string) (string, error) {
	prefix := claimDiscardPrefix
	if name == activationTemporaryName || strings.HasPrefix(name, activationDiscardPrefix) {
		prefix = activationDiscardPrefix
	}
	for {
		var value [16]byte
		if _, err := rand.Read(value[:]); err != nil {
			return "", fmt.Errorf("name network evidence isolation: %w", err)
		}
		candidate := prefix + hex.EncodeToString(value[:])
		if candidate != name {
			return candidate, nil
		}
	}
}

func preserveEvidenceStaging(directory *os.File, isolation, original string, cause error) error {
	if err := renameNoReplaceAt(int(directory.Fd()), isolation, original); err != nil {
		return errors.Join(cause, fmt.Errorf("preserve network evidence staging: %w", err))
	}
	return cause
}

func unlinkEvidence(directory *os.File, name string, syncer fileSync) error {
	if err := unix.Unlinkat(int(directory.Fd()), name, 0); err != nil {
		return fmt.Errorf("remove network evidence %q: %w", name, err)
	}
	if err := syncer.Sync(directory); err != nil {
		return fmt.Errorf("sync network evidence %q removal: %w", name, err)
	}
	return nil
}

func removeEmptyNetworkDirectory(root, directory *os.File, syncer fileSync) error {
	if err := unix.Unlinkat(int(root.Fd()), networkDirectoryName, unix.AT_REMOVEDIR); err != nil {
		return fmt.Errorf("remove empty network directory: %w", err)
	}
	if err := syncer.Sync(root); err != nil {
		return fmt.Errorf("sync empty network directory removal: %w", err)
	}
	return nil
}

func openNetworkDirectory(root *os.File, create bool) (*os.File, bool, error) {
	created := false
	if create {
		err := unix.Mkdirat(int(root.Fd()), networkDirectoryName, 0o700)
		if err == nil {
			created = true
		} else if !errors.Is(err, unix.EEXIST) {
			return nil, false, fmt.Errorf("create network directory: %w", err)
		}
	}
	fd, err := unix.Openat(int(root.Fd()), networkDirectoryName,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, false, os.ErrNotExist
		}
		return nil, false, fmt.Errorf("open network directory: %w", err)
	}
	directory := os.NewFile(uintptr(fd), networkDirectoryName)
	info, err := directory.Stat()
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		_ = directory.Close()
		return nil, false, errors.New("network directory has unsafe identity or permissions")
	}
	var status unix.Stat_t
	if err := unix.Fstat(fd, &status); err != nil || status.Uid != uint32(os.Geteuid()) {
		_ = directory.Close()
		return nil, false, errors.New("network directory has unsafe ownership")
	}
	return directory, created, nil
}

func directoryNames(directory *os.File) ([]string, error) {
	fd, err := unix.Dup(int(directory.Fd()))
	if err != nil {
		return nil, fmt.Errorf("duplicate network directory descriptor: %w", err)
	}
	copy := os.NewFile(uintptr(fd), "network-directory-scan")
	defer copy.Close()
	names, err := copy.Readdirnames(-1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read network directory: %w", err)
	}
	sort.Strings(names)
	return names, nil
}
