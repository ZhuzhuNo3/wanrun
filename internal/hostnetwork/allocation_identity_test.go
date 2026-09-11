package hostnetwork

import (
	"net/netip"
	"testing"
)

func TestAllocateNetworkRejectsIdentityCollision(t *testing.T) {
	id := mustRunID(t, "20112233445566778899aabbccddeeff")
	selected := []egressSelection{fixtureSelection(t, 1, "192.0.2.10", "wan0", 2, 254)}
	baseline, err := allocateNetwork(id, selected, emptyInventory())
	if err != nil {
		t.Fatal(err)
	}

	for name, mutate := range map[string]func(*hostInventory){
		"link identity": func(value *hostInventory) {
			value.linkNames[baseline.transfers[0].hostVeth] = struct{}{}
		},
		"nft identity": func(value *hostInventory) {
			value.firewall.nftTableNames[baseline.firewallTable] = struct{}{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			inventory := emptyInventory()
			mutate(&inventory)
			if _, err := allocateNetwork(id, selected, inventory); err == nil {
				t.Fatal("identity collision was accepted")
			}
		})
	}
	peerNameOnHost := emptyInventory()
	peerNameOnHost.linkNames[baseline.transfers[0].peerVeth] = struct{}{}
	if _, err := allocateNetwork(id, selected, peerNameOnHost); err != nil {
		t.Fatalf("host link sharing the anonymous-namespace peer name blocked allocation: %v", err)
	}
}

func TestAllocateNetworkRejectsResourceExhaustion(t *testing.T) {
	id := mustRunID(t, "20112233445566778899aabbccddeeff")
	selected := []egressSelection{fixtureSelection(t, 1, "192.0.2.10", "wan0", 2, 254)}
	routesExhausted := emptyInventory()
	routesExhausted.routePrefixes = []netip.Prefix{netip.MustParsePrefix("198.18.0.0/15")}
	if _, err := allocateNetwork(id, selected, routesExhausted); err == nil {
		t.Fatal("temporary address exhaustion was accepted")
	}
	prioritiesExhausted := emptyInventory()
	for priority := 20000; priority <= 29999; priority++ {
		prioritiesExhausted.priorities[priority] = struct{}{}
	}
	if _, err := allocateNetwork(id, selected, prioritiesExhausted); err == nil {
		t.Fatal("priority exhaustion was accepted")
	}
	tablesExhausted := emptyInventory()
	for table := firstReturnTable; table <= lastReturnTable; table++ {
		tablesExhausted.usedRouteTables[table] = struct{}{}
	}
	if _, err := allocateNetwork(id, selected, tablesExhausted); err == nil {
		t.Fatal("return-table exhaustion was accepted")
	}
}

func TestAllocateNetworkRejectsIPTablesOwnerCollision(t *testing.T) {
	id := mustRunID(t, "21112233445566778899aabbccddeeff")
	selected := []egressSelection{fixtureSelection(t, 1, "192.0.2.10", "wan0", 2, 254)}
	inventory := emptyIPTablesInventory()
	inventory.firewall.transferlanesOwnerReferences[id.String()] = struct{}{}
	if _, err := allocateNetwork(id, selected, inventory); err == nil {
		t.Fatal("iptables owner collision was accepted")
	}
}
