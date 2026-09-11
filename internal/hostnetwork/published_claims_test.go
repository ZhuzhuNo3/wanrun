//go:build linux || darwin

package hostnetwork

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"golang.org/x/sys/unix"
)

func TestPublishedClaimReservesEveryHostAllocationIdentity(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	inventory, err := mergePublishedClaimReservations(emptyInventory(), []networkClaim{claim},
		firewallsAbsent(claim))
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := inventory.usedRouteTables[claim.returnTable]; !exists {
		t.Fatalf("return table %d was not reserved", claim.returnTable)
	}
	if _, exists := inventory.usedProtocols[claim.protocol]; !exists {
		t.Fatalf("protocol %d was not reserved", claim.protocol)
	}
	if _, exists := inventory.firewall.nftTableNames[claim.firewallTable]; !exists {
		t.Fatalf("nftables table %q was not reserved", claim.firewallTable)
	}
	if _, exists := inventory.firewall.transferlanesOwnerReferences[claim.runID.String()]; !exists {
		t.Fatalf("firewall owner %s was not reserved", claim.runID)
	}
	if !slices.ContainsFunc(inventory.firewall.nftPostroutingNATChains, func(chain nftPostroutingNATChain) bool {
		return chain.Table == claim.firewallTable && chain.Priority == claim.nftPostroutingPriority
	}) {
		t.Fatalf("nftables postrouting priority %d was not reserved", claim.nftPostroutingPriority)
	}
	for _, transfer := range claim.transfers {
		if _, exists := inventory.linkNames[transfer.hostVeth]; !exists {
			t.Fatalf("host link identity %q was not reserved", transfer.hostVeth)
		}
		if _, exists := inventory.priorities[transfer.outboundPriority]; !exists {
			t.Fatalf("outbound priority %d was not reserved", transfer.outboundPriority)
		}
		if _, exists := inventory.priorities[transfer.returnPriority]; !exists {
			t.Fatalf("return priority %d was not reserved", transfer.returnPriority)
		}
		if _, exists := inventory.localAddresses[transfer.hostIP]; !exists {
			t.Fatalf("temporary host address %s was not reserved", transfer.hostIP)
		}
		if _, exists := inventory.localAddresses[transfer.namespaceIP]; !exists {
			t.Fatalf("temporary namespace address %s was not reserved", transfer.namespaceIP)
		}
		if !slices.Contains(inventory.routePrefixes, transfer.subnet) {
			t.Fatalf("temporary subnet %s was not reserved", transfer.subnet)
		}
	}
}

func TestPublishedClaimMergesAnExactObservedNFTChainOnce(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	inventory := emptyInventory()
	inventory.firewall.nftTableNames[claim.firewallTable] = struct{}{}
	inventory.firewall.transferlanesOwnerReferences = make(map[string]struct{})
	inventory.firewall.transferlanesOwnerReferences[claim.runID.String()] = struct{}{}
	inventory.firewall.nftPostroutingNATChains = []nftPostroutingNATChain{{
		Family: "ip", Table: claim.firewallTable, Name: "postrouting",
		Priority: claim.nftPostroutingPriority, HasRules: true,
	}}

	merged, err := mergePublishedClaimReservations(inventory, []networkClaim{claim},
		firewallsPresent(claim))
	if err != nil {
		t.Fatal(err)
	}
	if got := matchingNFTPostroutingChains(merged.firewall.nftPostroutingNATChains,
		claim.firewallTable); got != 1 {
		t.Fatalf("merged exact observed nftables chains = %d, want 1", got)
	}
}

func TestPublishedClaimRejectsAConflictingObservedNFTChain(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	inventory := emptyInventory()
	inventory.firewall.nftTableNames[claim.firewallTable] = struct{}{}
	inventory.firewall.nftPostroutingNATChains = []nftPostroutingNATChain{{
		Family: "ip", Table: claim.firewallTable, Name: "postrouting",
		Priority: claim.nftPostroutingPriority + 1, HasRules: true,
	}}

	if _, err := mergePublishedClaimReservations(inventory, []networkClaim{claim},
		firewallsPresent(claim)); err == nil {
		t.Fatal("conflicting observed nftables chain was accepted")
	}
}

func matchingNFTPostroutingChains(chains []nftPostroutingNATChain, table string) int {
	count := 0
	for _, chain := range chains {
		if chain.Family == "ip" && chain.Table == table && chain.Name == "postrouting" {
			count++
		}
	}
	return count
}

func TestConflictingPublishedClaimsFailClosedForEveryUniqueDomain(t *testing.T) {
	first := claimFromAllocation(testAllocation(t, firewallNFTables))
	second := claimFromAllocation(testAllocationForRun(t,
		"2f212233445566778899aabbccddeeff", strings.Repeat("2", 64), firewallNFTables))
	second.nftPostroutingPriority = first.nftPostroutingPriority - 1
	if _, err := mergePublishedClaimReservations(emptyInventory(), []networkClaim{first, second},
		firewallsAbsent(first, second)); err != nil {
		t.Fatalf("non-conflicting fixture claims: %v", err)
	}
	tests := map[string]func(*networkClaim){
		"temporary subnet and addresses": func(claim *networkClaim) {
			claim.transfers[0].subnet = first.transfers[0].subnet
			claim.transfers[0].hostIP = first.transfers[0].hostIP
			claim.transfers[0].namespaceIP = first.transfers[0].namespaceIP
		},
		"outbound priority": func(claim *networkClaim) {
			claim.transfers[0].outboundPriority = first.transfers[0].outboundPriority
		},
		"return priority": func(claim *networkClaim) {
			claim.transfers[0].returnPriority = first.transfers[0].returnPriority
		},
		"return table": func(claim *networkClaim) { claim.returnTable = first.returnTable },
		"protocol":     func(claim *networkClaim) { claim.protocol = first.protocol },
		"host link identity": func(claim *networkClaim) {
			claim.ownerToken = first.ownerToken
			links, err := transferLinks(claim.runID, claim.ownerToken, claim.transfers[0].transfer)
			if err != nil {
				t.Fatal(err)
			}
			claim.transfers[0].linkOwner = links.owner
			claim.transfers[0].hostVeth = links.hostName
			claim.transfers[0].hostMAC = links.hostMAC
			claim.transfers[0].peerMAC = links.peerMAC
		},
		"nftables postrouting priority": func(claim *networkClaim) {
			claim.nftPostroutingPriority = first.nftPostroutingPriority
		},
	}
	for name, collide := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := second
			candidate.transfers = append([]transferAllocation(nil), second.transfers...)
			collide(&candidate)
			if err := validateClaim(candidate, candidate.runID); err != nil {
				t.Fatalf("collision fixture is not a valid claim: %v", err)
			}
			if _, err := mergePublishedClaimReservations(emptyInventory(),
				[]networkClaim{first, candidate}, firewallsAbsent(first, candidate)); err == nil {
				t.Fatal("conflicting published claims were accepted")
			}
		})
	}
}

func TestOpenRejectsPublishedClaimCollisionAbsentFromKernelInventory(t *testing.T) {
	directory, _, err := rundirectory.CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.ClosePrivateAuthority(context.Background()) })
	reserved := createPublishedRun(t, directory, "3f112233445566778899aabbccddeeff")
	current := createPublishedRun(t, directory, "40112233445566778899aabbccddeeff")
	t.Cleanup(func() {
		_ = reserved.Close()
		_ = current.Close()
	})
	reservedRoot, err := reserved.OpenRoot()
	if err != nil {
		t.Fatal(err)
	}
	claim, err := allocateClaimNetwork(reserved.ID(), testOwnerToken, testSelections(), emptyInventory())
	if err != nil {
		t.Fatal(err)
	}
	if err := publishClaim(reservedRoot, claimFromAllocation(claim), systemFileSync{}); err != nil {
		_ = reservedRoot.Close()
		t.Fatal(err)
	}
	_ = reservedRoot.Close()

	host := &recordingHost{}
	owner := testOwner(t, host, systemFileSync{})
	owner.ownerTokens = fixedNetworkOwnerTokens{testOwnerToken}
	if session, err := owner.open(context.Background(), current, testSelections()); err == nil || session != nil {
		t.Fatalf("published identity collision = %#v, %v; want nil/error", session, err)
	}
	if host.installCalls != 0 {
		t.Fatalf("published identity collision reached %d kernel installs", host.installCalls)
	}
	root, err := current.OpenRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if network, _, err := openNetworkDirectory(root, false); err == nil {
		_ = network.Close()
		t.Fatal("collision published a new claim")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("collision published a new claim: %v", err)
	}
}

func TestOpenAllocatesAroundPublishedClaimAbsentFromKernelInventory(t *testing.T) {
	directory, _, err := rundirectory.CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.ClosePrivateAuthority(context.Background()) })
	reserved := createPublishedRun(t, directory, "3f212233445566778899aabbccddeeff")
	current := createPublishedRun(t, directory, "40212233445566778899aabbccddeeff")
	t.Cleanup(func() {
		_ = reserved.Close()
		_ = current.Close()
	})
	reservedRoot, err := reserved.OpenRoot()
	if err != nil {
		t.Fatal(err)
	}
	reservedAllocation := testAllocationForRun(t, reserved.ID().String(), testOwnerToken, firewallNFTables)
	reservedClaim := claimFromAllocation(reservedAllocation)
	if err := publishClaim(reservedRoot, reservedClaim, systemFileSync{}); err != nil {
		_ = reservedRoot.Close()
		t.Fatal(err)
	}
	_ = reservedRoot.Close()

	host := &recordingHost{}
	owner := testOwner(t, host, systemFileSync{})
	owner.ownerTokens = fixedNetworkOwnerTokens{strings.Repeat("2", 64)}
	session, err := owner.open(context.Background(), current, testSelections())
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close(context.Background())
	if session.claim.nftPostroutingPriority >= reservedClaim.nftPostroutingPriority {
		t.Fatalf("new nftables priority = %d, want before reserved %d",
			session.claim.nftPostroutingPriority, reservedClaim.nftPostroutingPriority)
	}
	assertClaimsUseDisjointAllocationIdentities(t, reservedClaim, session.claim)
}

func TestOpenFailsClosedOnCorruptPublishedClaim(t *testing.T) {
	directory, _, err := rundirectory.CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.ClosePrivateAuthority(context.Background()) })
	corrupt := createPublishedRun(t, directory, "41112233445566778899aabbccddeeff")
	current := createPublishedRun(t, directory, "42112233445566778899aabbccddeeff")
	t.Cleanup(func() {
		_ = corrupt.Close()
		_ = current.Close()
	})
	root, err := corrupt.OpenRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkdirat(int(root.Fd()), networkDirectoryName, 0o700); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	network, _, err := openNetworkDirectory(root, false)
	if err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	claimFile, err := openTestEvidenceFile(network, claimFileName)
	if err != nil {
		_ = network.Close()
		_ = root.Close()
		t.Fatal(err)
	}
	if _, err := claimFile.Write([]byte("not-json")); err != nil {
		_ = claimFile.Close()
		_ = network.Close()
		_ = root.Close()
		t.Fatal(err)
	}
	_ = claimFile.Close()
	_ = network.Close()
	_ = root.Close()
	host := &recordingHost{}
	owner := testOwner(t, host, systemFileSync{})
	if session, err := owner.open(context.Background(), current, testSelections()); err == nil || session != nil {
		t.Fatalf("corrupt published claim = %#v, %v; want nil/error", session, err)
	}
	if host.installCalls != 0 {
		t.Fatalf("corrupt published claim reached %d kernel installs", host.installCalls)
	}
}

func TestOpenRejectsOutOfRangePublishedProtocolBeforeFirewallObservation(t *testing.T) {
	directory, _, err := rundirectory.CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = directory.ClosePrivateAuthority(context.Background()) })
	reserved := createPublishedRun(t, directory, "43312233445566778899aabbccddeeff")
	current := createPublishedRun(t, directory, "44312233445566778899aabbccddeeff")
	t.Cleanup(func() {
		_ = reserved.Close()
		_ = current.Close()
	})
	root, err := reserved.OpenRoot()
	if err != nil {
		t.Fatal(err)
	}
	claim := claimFromAllocation(testAllocationForRun(t, reserved.ID().String(),
		testOwnerToken, firewallNFTables))
	writeClaimEncoding(t, root, claimEncodingWithProtocol(t, claim, lastClaimProtocol+1))
	_ = root.Close()

	host := &recordingHost{}
	owner := testOwner(t, host, systemFileSync{})
	if session, err := owner.open(context.Background(), current, testSelections()); err == nil || session != nil {
		t.Fatalf("out-of-range published protocol = %#v, %v; want nil/error", session, err)
	}
	if host.claimedFirewallCalls != 0 || host.installCalls != 0 {
		t.Fatalf("invalid published protocol reached firewall observations/installs = %d/%d",
			host.claimedFirewallCalls, host.installCalls)
	}
}

func TestPublishedClaimReaderNeverCleansUnpublishedEvidence(t *testing.T) {
	root := openTestRunRoot(t)
	id := mustRunID(t, "43112233445566778899aabbccddeeff")
	directory, _, err := openNetworkDirectory(root, true)
	if err != nil {
		t.Fatal(err)
	}
	staging, err := openTestEvidenceFile(directory, claimTemporaryName)
	if err != nil {
		_ = directory.Close()
		t.Fatal(err)
	}
	if _, err := staging.Write([]byte("unpublished")); err != nil {
		_ = staging.Close()
		_ = directory.Close()
		t.Fatal(err)
	}
	_ = staging.Close()
	_ = directory.Close()

	if claim, exists, err := readPublishedClaim(root, id); err != nil || exists {
		t.Fatalf("unpublished claim read = %#v, exists=%t, error=%v", claim, exists, err)
	}
	directory, _, err = openNetworkDirectory(root, false)
	if err != nil {
		t.Fatalf("read-only claim reader removed network directory: %v", err)
	}
	defer directory.Close()
	var status unix.Stat_t
	if err := unix.Fstatat(int(directory.Fd()), claimTemporaryName, &status, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatalf("read-only claim reader removed staging evidence: %v", err)
	}
}

func createPublishedRun(t *testing.T, owner *rundirectory.Owner, id string) *rundirectory.LiveRun {
	t.Helper()
	live, err := owner.Create(mustRunID(t, id))
	if err != nil {
		t.Fatal(err)
	}
	return live
}

func assertClaimsUseDisjointAllocationIdentities(t *testing.T, left, right networkClaim) {
	t.Helper()
	if left.returnTable == right.returnTable || left.protocol == right.protocol {
		t.Fatalf("run identities overlap: left=%#v right=%#v", left, right)
	}
	for _, first := range left.transfers {
		for _, second := range right.transfers {
			if first.subnet.Overlaps(second.subnet) || first.outboundPriority == second.outboundPriority ||
				first.outboundPriority == second.returnPriority || first.returnPriority == second.outboundPriority ||
				first.returnPriority == second.returnPriority || first.hostVeth == second.hostVeth {
				t.Fatalf("transfer identities overlap: left=%#v right=%#v", first, second)
			}
		}
	}
}

func testAllocationForRun(t *testing.T, id, ownerToken string, kind firewallKind) networkAllocation {
	t.Helper()
	inventory := emptyInventory()
	if kind == firewallIPTables {
		inventory = emptyIPTablesInventory()
	}
	allocation, err := allocateClaimNetwork(mustRunID(t, id), ownerToken, testSelections(), inventory)
	if err != nil {
		t.Fatal(err)
	}
	return allocation
}

func openTestEvidenceFile(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

type fixedNetworkOwnerTokens struct{ value string }

func (tokens fixedNetworkOwnerTokens) New() (string, error) { return tokens.value, nil }

func firewallsAbsent(claims ...networkClaim) publishedFirewallObservations {
	result := make(publishedFirewallObservations, len(claims))
	for _, claim := range claims {
		result[claim.runID] = false
	}
	return result
}

func firewallsPresent(claims ...networkClaim) publishedFirewallObservations {
	result := make(publishedFirewallObservations, len(claims))
	for _, claim := range claims {
		result[claim.runID] = true
	}
	return result
}
