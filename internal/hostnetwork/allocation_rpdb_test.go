package hostnetwork

import (
	"net/netip"
	"testing"
)

func TestAllocateNetworkRejectsOrAvoidsPreemptingRPDBRules(t *testing.T) {
	id := mustRunID(t, "11112233445566778899aabbccddeeff")
	selected := []egressSelection{fixtureSelection(t, 1, "192.0.2.10", "wan0", 2, 254)}

	for name, rule := range map[string]routingRule{
		"wide lookup":        {priority: 100, table: 100},
		"unreachable":        {priority: 101, action: ruleActionUnreachable},
		"unknown selector":   {priority: 102, table: 100, unknownSelector: true},
		"matching interface": {priority: 103, table: 100, inputInterface: testTransferLinks(t, id, 1).hostName},
	} {
		t.Run(name, func(t *testing.T) {
			inventory := emptyInventory()
			inventory.routingRules = []routingRule{rule}
			if _, err := allocateNetwork(id, selected, inventory); err == nil {
				t.Fatal("preempting RPDB rule was accepted")
			}
		})
	}

	avoid := emptyInventory()
	first := netip.MustParsePrefix("198.18.0.0/30")
	avoid.routingRules = []routingRule{{priority: 100, table: 100, source: &first,
		inputInterface: testTransferLinks(t, id, 1).hostName}}
	allocation, err := allocateNetwork(id, selected, avoid)
	if err != nil {
		t.Fatalf("allocate around source-specific rule: %v", err)
	}
	if got, want := allocation.transfers[0].subnet, netip.MustParsePrefix("198.18.0.4/30"); got != want {
		t.Fatalf("allocated subnet = %s, want %s", got, want)
	}

	avoidReturn := emptyInventory()
	avoidReturn.routingRules = []routingRule{{priority: 100, table: 100,
		destination: &first, inputInterface: "wan0"}}
	allocation, err = allocateNetwork(id, selected, avoidReturn)
	if err != nil {
		t.Fatalf("allocate around destination-specific return rule: %v", err)
	}
	if got, want := allocation.transfers[0].subnet, netip.MustParsePrefix("198.18.0.4/30"); got != want {
		t.Fatalf("return-safe subnet = %s, want %s", got, want)
	}

	nonmatching := emptyInventory()
	source := netip.MustParsePrefix("192.0.2.10/32")
	nonmatching.routingRules = []routingRule{{priority: 100, table: 100, source: &source}}
	if _, err := allocateNetwork(id, selected, nonmatching); err != nil {
		t.Fatalf("nonmatching source rule blocked allocation: %v", err)
	}

	unknownReturnSource := emptyInventory()
	remoteSource := netip.MustParsePrefix("203.0.113.0/24")
	unknownReturnSource.routingRules = []routingRule{{priority: 100, table: 100, source: &remoteSource}}
	if _, err := allocateNetwork(id, selected, unknownReturnSource); err == nil {
		t.Fatal("source rule that can match return traffic was accepted")
	}

	builtinLocal := emptyInventory()
	builtinLocal.routingRules = []routingRule{{priority: 0, table: 255, action: ruleActionLookup, builtinLocal: true}}
	if _, err := allocateNetwork(id, selected, builtinLocal); err != nil {
		t.Fatalf("proven fallthrough local rule blocked allocation: %v", err)
	}

	inverted := emptyInventory()
	inverted.routingRules = []routingRule{{priority: 100, table: 100, source: &source, inverted: true}}
	if _, err := allocateNetwork(id, selected, inverted); err == nil {
		t.Fatal("inverted source mismatch that can select temporary traffic was accepted")
	}
}
