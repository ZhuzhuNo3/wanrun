package hostnetwork

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestAllocateNetworkIsDeterministic(t *testing.T) {
	id := mustRunID(t, "00112233445566778899aabbccddeeff")
	selected := []egressSelection{
		fixtureSelection(t, 2, "192.0.2.10", "wan0", 2, 100),
		fixtureSelection(t, 1, "192.0.2.10", "wan0", 2, 100),
		fixtureSelection(t, 3, "198.51.100.10", "wan1", 3, 200),
	}
	first, err := allocateNetwork(id, selected, emptyInventory())
	if err != nil {
		t.Fatalf("allocateNetwork: %v", err)
	}
	second, err := allocateNetwork(id, selected, emptyInventory())
	if err != nil {
		t.Fatalf("allocateNetwork again: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("allocation is not deterministic:\n%#v\n%#v", first, second)
	}
	if len(first.transfers) != 3 || first.transfers[0].number != 1 || first.transfers[1].number != 2 {
		t.Fatalf("transfers are not sorted by stable number: %#v", first.transfers)
	}
	for index, allocation := range first.transfers {
		wantPrefix := netip.MustParsePrefix([]string{
			"198.18.0.0/30", "198.18.0.4/30", "198.18.0.8/30",
		}[index])
		if allocation.subnet != wantPrefix || allocation.outboundPriority != 20000+index*2 ||
			allocation.returnPriority != 20001+index*2 {
			t.Errorf("transfer %d allocation = %s/%d/%d, want %s/%d/%d",
				allocation.number, allocation.subnet, allocation.outboundPriority, allocation.returnPriority,
				wantPrefix, 20000+index*2, 20001+index*2)
		}
		if len(allocation.hostVeth) != 15 || allocation.hostVeth != allocation.linkOwner.hostName() ||
			allocation.hostMAC != allocation.linkOwner.hostMAC() {
			t.Errorf("host marker %q/%s does not preserve its link owner", allocation.hostVeth,
				allocation.hostMAC)
		}
		if len(allocation.peerVeth) >= 16 || !strings.Contains(allocation.peerVeth, id.String()[:10]) {
			t.Errorf("derived peer name %q is not IFNAMSIZ-safe/run-bound", allocation.peerVeth)
		}
	}
}

func TestAllocateNetworkAllowsRepeatedSource(t *testing.T) {
	id := mustRunID(t, "01112233445566778899aabbccddeeff")
	selected := []egressSelection{
		fixtureSelection(t, 1, "192.0.2.10", "wan0", 2, 100),
		fixtureSelection(t, 2, "192.0.2.10", "wan0", 2, 100),
	}
	allocation, err := allocateNetwork(id, selected, emptyInventory())
	if err != nil || len(allocation.transfers) != len(selected) {
		t.Fatalf("repeated source allocation=%#v error=%v", allocation, err)
	}
}

func TestAllocatedLinkNamesCoverRepresentationMaximumWithinIFNAMSIZ(t *testing.T) {
	id := mustRunID(t, "00112233445566778899aabbccddeeff")
	seen := make(map[string]struct{}, transfernumber.Maximum*2)
	for number := 1; number <= transfernumber.Maximum; number++ {
		links := testTransferLinks(t, id, number)
		for _, name := range []string{links.hostName, links.peerName} {
			if len(name) > 15 {
				t.Fatalf("transfer %d identity %q exceeds IFNAMSIZ", number, name)
			}
			if _, duplicate := seen[name]; duplicate {
				t.Fatalf("transfer %d repeats identity %q", number, name)
			}
			seen[name] = struct{}{}
		}
	}
}

func TestAllocateNetworkSkipsRouteAndPriorityCollisions(t *testing.T) {
	id := mustRunID(t, "10112233445566778899aabbccddeeff")
	inventory := emptyInventory()
	inventory.routePrefixes = []netip.Prefix{
		netip.MustParsePrefix("198.18.0.0/29"),
		netip.MustParsePrefix("198.18.0.8/30"),
	}
	inventory.priorities[20000] = struct{}{}
	inventory.priorities[20001] = struct{}{}

	allocation, err := allocateNetwork(id,
		[]egressSelection{fixtureSelection(t, 1, "192.0.2.10", "wan0", 2, 254)}, inventory)
	if err != nil {
		t.Fatalf("allocateNetwork: %v", err)
	}
	transferAllocation := allocation.transfers[0]
	if transferAllocation.subnet != netip.MustParsePrefix("198.18.0.12/30") ||
		transferAllocation.outboundPriority != 20002 || transferAllocation.returnPriority != 20003 {
		t.Fatalf("collision allocation = %s/%d/%d", transferAllocation.subnet,
			transferAllocation.outboundPriority, transferAllocation.returnPriority)
	}
}

func TestAllocateNetworkSkipsUsedReturnTablesAndAllocatesTwoRulePriorities(t *testing.T) {
	id := mustRunID(t, "10912233445566778899aabbccddeeff")
	inventory := emptyInventory()
	inventory.usedRouteTables[firstReturnTable] = struct{}{}
	inventory.usedRouteTables[firstReturnTable+1] = struct{}{}
	inventory.priorities[firstPriority] = struct{}{}

	allocation, err := allocateNetwork(id,
		[]egressSelection{fixtureSelection(t, 1, "192.0.2.10", "wan0", 2, 254)}, inventory)
	if err != nil {
		t.Fatalf("allocateNetwork: %v", err)
	}
	transfer := allocation.transfers[0]
	if allocation.returnTable != firstReturnTable+2 || transfer.outboundPriority != firstPriority+1 ||
		transfer.returnPriority != firstPriority+2 || transfer.outboundPriority == transfer.returnPriority {
		t.Fatalf("return allocation = table %d, priorities %d/%d", allocation.returnTable,
			transfer.outboundPriority, transfer.returnPriority)
	}
}

func TestAllocateNetworkRejectsIncompleteTransferSet(t *testing.T) {
	id := mustRunID(t, "21112233445566778899aabbccddeeff")
	selected := []egressSelection{
		fixtureSelection(t, 1, "192.0.2.10", "wan0", 2, 254),
		fixtureSelection(t, 3, "198.51.100.10", "wan1", 3, 200),
	}
	if _, err := allocateNetwork(id, selected, emptyInventory()); err == nil {
		t.Fatal("incomplete transfer set was accepted")
	}
}
