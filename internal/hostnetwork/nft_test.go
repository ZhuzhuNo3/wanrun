package hostnetwork

import (
	"slices"
	"strings"
	"testing"
)

func TestNFTProgramUsesExactForwardAndSNATWithoutMarks(t *testing.T) {
	id := mustRunID(t, "70112233445566778899aabbccddeeff")
	allocation, err := allocateClaimNetwork(id, testOwnerToken, []egressSelection{
		fixtureSelection(t, 1, "192.0.2.10", "wan0", 2, 254),
		fixtureSelection(t, 2, "198.51.100.10", "wan1", 3, 100),
	}, emptyInventory())
	if err != nil {
		t.Fatal(err)
	}
	program, err := buildNFTProgram(claimFromAllocation(allocation))
	if err != nil {
		t.Fatalf("buildNFTProgram: %v", err)
	}
	if program.table != allocation.firewallTable || !slices.Equal(program.chains, []string{"forward", "postrouting"}) {
		t.Fatalf("program identity/chains = %#v", program)
	}
	if program.postroutingPriority != allocation.nftPostroutingPriority {
		t.Fatalf("program postrouting priority = %d, want %d",
			program.postroutingPriority, allocation.nftPostroutingPriority)
	}
	if len(program.rules) != 10 {
		t.Fatalf("rule count = %d, want 10", len(program.rules))
	}
	wantOwnerPrefix := "transferlanes:" + id.String() + ":" + testOwnerToken + ":"
	if program.ownerPrefix != wantOwnerPrefix {
		t.Fatalf("nft owner prefix = %q, want %q", program.ownerPrefix, wantOwnerPrefix)
	}
	for _, rule := range program.rules {
		if !strings.HasPrefix(rule.owner, wantOwnerPrefix) {
			t.Fatalf("nft rule lacks full owner token: %q", rule.owner)
		}
		if rule.kind == "mark" {
			t.Fatalf("nft program contains packet mark: %#v", rule)
		}
		if rule.input == "" && rule.output == "" {
			t.Fatalf("nft rule is not interface-bound: %#v", rule)
		}
	}
	for _, transfer := range allocation.transfers {
		if !hasNFTRule(program, "snat", transfer.hostVeth, transfer.providerName, transfer.source.String()) ||
			!hasNFTRule(program, "forward-out", transfer.hostVeth, transfer.providerName, "") ||
			!hasNFTRule(program, "return", transfer.providerName, transfer.hostVeth, "") {
			t.Errorf("transfer %d lacks exact forward/SNAT rules: %#v", transfer.number, program.rules)
		}
		for _, rule := range program.rules {
			if rule.kind == "snat" && rule.input == transfer.hostVeth && rule.source != transfer.namespaceIP.String() {
				t.Errorf("transfer %d SNAT source match = %q, want %s",
					transfer.number, rule.source, transfer.namespaceIP)
			}
			if rule.kind == "forward-out" && rule.input == transfer.hostVeth &&
				rule.source != transfer.subnet.String() {
				t.Errorf("transfer %d forward source = %q, want %s", transfer.number, rule.source, transfer.subnet)
			}
			if rule.kind == "return" && rule.output == transfer.hostVeth &&
				rule.destination != transfer.subnet.String() {
				t.Errorf("transfer %d return destination = %q, want %s",
					transfer.number, rule.destination, transfer.subnet)
			}
		}
	}
}

func TestNFTProgramAddsExactLeadingRulesToRecordedHostForwardChains(t *testing.T) {
	id := mustRunID(t, "71112233445566778899aabbccddeeff")
	inventory := emptyInventory()
	inventory.firewall.nftForwardChains = []nftForwardChain{
		{Family: "inet", Table: "host_filter", Name: "forward"},
		{Family: "ip", Table: "security", Name: "FORWARD"},
	}
	allocation, err := allocateClaimNetwork(id, testOwnerToken, []egressSelection{
		fixtureSelection(t, 1, "192.0.2.10", "wan0", 2, 254),
	}, inventory)
	if err != nil {
		t.Fatal(err)
	}
	program, err := buildNFTProgram(claimFromAllocation(allocation))
	if err != nil {
		t.Fatal(err)
	}
	if len(program.forwardRules) != 4 {
		t.Fatalf("host forward rule count = %d, want 4", len(program.forwardRules))
	}
	for _, target := range inventory.firewall.nftForwardChains {
		if !hasNFTForwardRule(program, target, "forward-out", allocation.transfers[0].subnet.String()) ||
			!hasNFTForwardRule(program, target, "return", allocation.transfers[0].subnet.String()) {
			t.Fatalf("host forward target %#v lacks exact bidirectional rules: %#v",
				target, program.forwardRules)
		}
	}
	for _, forward := range program.forwardRules {
		if !strings.HasPrefix(forward.rule.owner, "transferlanes:"+id.String()+":"+testOwnerToken+":") {
			t.Fatalf("host forward rule lacks full owner token: %q", forward.rule.owner)
		}
	}
}

func hasNFTForwardRule(program nftProgram, target nftForwardChain, kind, prefix string) bool {
	for _, forward := range program.forwardRules {
		if forward.target == target && forward.rule.kind == kind &&
			(forward.rule.source == prefix || forward.rule.destination == prefix) {
			return true
		}
	}
	return false
}

func hasNFTRule(program nftProgram, kind, input, output, address string) bool {
	for _, rule := range program.rules {
		if rule.kind == kind && rule.input == input && rule.output == output && rule.address == address {
			return true
		}
	}
	return false
}
