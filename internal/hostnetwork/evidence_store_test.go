//go:build linux || darwin

package hostnetwork

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestNetworkClaimPublicationIsDurableAndImmutable(t *testing.T) {
	root := openTestRunRoot(t)
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	syncer := &observingSync{}
	if err := publishClaim(root, claim, syncer); err != nil {
		t.Fatal(err)
	}
	evidence, exists, err := loadNetworkEvidence(root, claim.runID, systemFileSync{})
	if err != nil || !exists || evidence.activated || !claimsEqual(evidence.claim, claim) {
		t.Fatalf("loaded evidence = %#v, exists=%t, error=%v", evidence, exists, err)
	}
	if !slices.Contains(syncer.names, claimTemporaryName) ||
		!slices.Contains(syncer.names, networkDirectoryName) {
		t.Fatalf("claim durability syncs = %v", syncer.names)
	}
	if err := publishClaim(root, claim, systemFileSync{}); err == nil {
		t.Fatal("second claim publication replaced immutable evidence")
	}
}

func TestTrafficActivationBecomesDurableCommitCertificate(t *testing.T) {
	root := openTestRunRoot(t)
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	if err := publishClaim(root, claim, systemFileSync{}); err != nil {
		t.Fatal(err)
	}
	syncer := &observingSync{}
	if err := publishTrafficActivation(root, claim, syncer); err != nil {
		t.Fatal(err)
	}
	evidence, exists, err := loadNetworkEvidence(root, claim.runID, systemFileSync{})
	if err != nil || !exists || !evidence.activated || !claimsEqual(evidence.claim, claim) {
		t.Fatalf("loaded evidence = %#v, exists=%t, error=%v", evidence, exists, err)
	}
	if !slices.Contains(syncer.names, activationTemporaryName) ||
		!slices.Contains(syncer.names, networkDirectoryName) {
		t.Fatalf("activation durability syncs = %v", syncer.names)
	}
	if err := publishTrafficActivation(root, claim, systemFileSync{}); err == nil {
		t.Fatal("second activation publication replaced durable evidence")
	}
}

func TestInterruptedTrafficActivationNeverOpensTheGate(t *testing.T) {
	root := openTestRunRoot(t)
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	if err := publishClaim(root, claim, systemFileSync{}); err != nil {
		t.Fatal(err)
	}
	if err := publishTrafficActivation(root, claim, &namedFailingSync{name: activationTemporaryName}); err == nil {
		t.Fatal("interrupted activation publication succeeded")
	}
	evidence, exists, err := loadNetworkEvidence(root, claim.runID, systemFileSync{})
	if err != nil || !exists || evidence.activated {
		t.Fatalf("interrupted activation = %#v, exists=%t, error=%v", evidence, exists, err)
	}
	if _, err := os.Lstat(filepath.Join(root.Name(), networkDirectoryName, activationTemporaryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("activation staging file remains: %v", err)
	}
}

func TestActivationDirectorySyncFailureRemainsRecoverableAsActivated(t *testing.T) {
	root := openTestRunRoot(t)
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	if err := publishClaim(root, claim, systemFileSync{}); err != nil {
		t.Fatal(err)
	}
	if err := publishTrafficActivation(root, claim,
		&namedFailingSync{name: networkDirectoryName}); err == nil {
		t.Fatal("activation directory sync failure was not reported")
	}
	evidence, exists, err := loadNetworkEvidence(root, claim.runID, systemFileSync{})
	if err != nil || !exists || !evidence.activated {
		t.Fatalf("activation after rename = %#v, exists=%t, error=%v", evidence, exists, err)
	}
}

func TestActivationWithoutClaimFailsClosed(t *testing.T) {
	root := openTestRunRoot(t)
	directory := filepath.Join(root.Name(), networkDirectoryName)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	mustWriteTestFile(t, filepath.Join(directory, activationFileName), 0o600)
	if _, _, err := loadNetworkEvidence(root,
		claimFromAllocation(testAllocation(t, firewallNFTables)).runID, systemFileSync{}); err == nil {
		t.Fatal("activation without claim was accepted")
	}
	if _, err := os.Lstat(filepath.Join(directory, activationFileName)); err != nil {
		t.Fatalf("ambiguous activation was changed: %v", err)
	}
}

func TestNetworkEvidenceRemovalChecksClaimAndGateIdentity(t *testing.T) {
	root := openTestRunRoot(t)
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	if err := publishClaim(root, claim, systemFileSync{}); err != nil {
		t.Fatal(err)
	}
	if err := publishTrafficActivation(root, claim, systemFileSync{}); err != nil {
		t.Fatal(err)
	}
	if err := removeNetworkEvidence(root, networkEvidence{claim: claim}, systemFileSync{}); err == nil {
		t.Fatal("inactive evidence expectation removed an activation")
	}
	if err := removeNetworkEvidence(root,
		networkEvidence{claim: claim, activated: true}, systemFileSync{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root.Name(), networkDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("network evidence directory remains: %v", err)
	}
}

func TestLoadNetworkEvidenceRemovesAnEmptyNetworkDirectory(t *testing.T) {
	root := openTestRunRoot(t)
	directory := filepath.Join(root.Name(), networkDirectoryName)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	syncer := &observingSync{}
	evidence, exists, err := loadNetworkEvidence(root,
		claimFromAllocation(testAllocation(t, firewallNFTables)).runID, syncer)
	if err != nil || exists {
		t.Fatalf("empty-directory evidence = %#v, exists=%t, error=%v", evidence, exists, err)
	}
	assertEvidenceDirectoryAbsent(t, root)
	if !slices.Equal(syncer.names, []string{root.Name()}) {
		t.Fatalf("empty-directory durability syncs = %v, want run root", syncer.names)
	}
}

func TestClaimUnlinkSyncFailureLeavesNoAuthorityAndLoadSelfHeals(t *testing.T) {
	root := openTestRunRoot(t)
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	if err := publishClaim(root, claim, systemFileSync{}); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("injected claim-removal directory sync failure")
	if err := removeNetworkEvidence(root, networkEvidence{claim: claim},
		&namedFailingSync{name: networkDirectoryName, cause: cause}); !errors.Is(err, cause) {
		t.Fatalf("remove error = %v, want %v", err, cause)
	}
	claimPath := filepath.Join(root.Name(), networkDirectoryName, claimFileName)
	if _, err := os.Lstat(claimPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claim authority remains after unlink: %v", err)
	}
	evidence, exists, err := loadNetworkEvidence(root, claim.runID, systemFileSync{})
	if err != nil || exists {
		t.Fatalf("evidence after self-heal = %#v, exists=%t, error=%v", evidence, exists, err)
	}
	assertEvidenceDirectoryAbsent(t, root)
}

func TestSuccessfulNetworkEvidenceRemovalSyncsRootAfterDirectoryRemoval(t *testing.T) {
	root := openTestRunRoot(t)
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	if err := publishClaim(root, claim, systemFileSync{}); err != nil {
		t.Fatal(err)
	}
	syncer := &observingSync{}
	if err := removeNetworkEvidence(root, networkEvidence{claim: claim}, syncer); err != nil {
		t.Fatal(err)
	}
	assertEvidenceDirectoryAbsent(t, root)
	if !slices.Equal(syncer.names, []string{networkDirectoryName, root.Name()}) {
		t.Fatalf("removal durability syncs = %v, want network directory then run root", syncer.names)
	}
}

func assertEvidenceDirectoryAbsent(t *testing.T, root *os.File) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root.Name(), networkDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("network evidence directory remains: %v", err)
	}
}

type namedFailingSync struct {
	name   string
	cause  error
	failed bool
}

type observingSync struct{ names []string }

func (syncer *observingSync) Sync(file *os.File) error {
	syncer.names = append(syncer.names, file.Name())
	return file.Sync()
}

func (syncer *namedFailingSync) Sync(file *os.File) error {
	if file.Name() == syncer.name && !syncer.failed {
		syncer.failed = true
		if syncer.cause != nil {
			return syncer.cause
		}
		return errors.New("injected evidence sync failure")
	}
	return file.Sync()
}

func mustWriteTestFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}
