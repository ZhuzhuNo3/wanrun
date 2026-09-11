package hostnetwork

import (
	"net/netip"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func emptyInventory() hostInventory {
	return hostInventory{linkNames: map[string]struct{}{},
		firewall:   firewallInventory{kind: firewallNFTables, nftTableNames: map[string]struct{}{}},
		priorities: map[int]struct{}{}, usedRouteTables: map[int]struct{}{}, usedProtocols: map[uint8]struct{}{},
		conntrackOriginalSources: map[netip.Addr]struct{}{}}
}

func emptyIPTablesInventory() hostInventory {
	return hostInventory{linkNames: map[string]struct{}{},
		firewall: firewallInventory{kind: firewallIPTables, transferlanesOwnerReferences: map[string]struct{}{},
			iptablesFrontend: iptablesFrontendNFT},
		priorities: map[int]struct{}{}, usedRouteTables: map[int]struct{}{}, usedProtocols: map[uint8]struct{}{},
		conntrackOriginalSources: map[netip.Addr]struct{}{}}
}

func fixtureSelection(t *testing.T, number int, source, provider string, index, table int) egressSelection {
	t.Helper()
	id, err := transfernumber.New(number)
	if err != nil {
		t.Fatal(err)
	}
	return egressSelection{transfer: id, source: netip.MustParseAddr(source), providerName: provider,
		providerIndex: index, routeTable: table}
}

func allocateNetwork(id runid.ID, selected []egressSelection,
	inventory hostInventory) (networkAllocation, error) {
	return allocateNetworkWithStarts(id, selected, inventory, allocationStarts{
		ownerToken: testOwnerToken, protocol: firstClaimProtocol, returnTable: firstReturnTable,
		priority: firstPriority,
	})
}

func testTransferLinks(t *testing.T, id runid.ID, number int) linkAllocation {
	t.Helper()
	transfer, err := transfernumber.New(number)
	if err != nil {
		t.Fatal(err)
	}
	links, err := transferLinks(id, testOwnerToken, transfer)
	if err != nil {
		t.Fatal(err)
	}
	return links
}

func mustRunID(t *testing.T, value string) runid.ID {
	t.Helper()
	id, err := runid.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
