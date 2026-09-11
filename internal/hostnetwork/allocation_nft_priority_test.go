package hostnetwork

import (
	"fmt"
	"testing"
)

func TestAllocateNativeNFTPriorityPrecedesEveryActivePostroutingNATChain(t *testing.T) {
	id := mustRunID(t, "22112233445566778899aabbccddeeff")
	inventory := emptyInventory()
	inventory.firewall.nftPostroutingNATChains = []nftPostroutingNATChain{
		{Family: "ip", Table: "nat", Name: "POSTROUTING", Priority: 100, HasRules: true},
		{Family: "inet", Table: "host", Name: "postrouting", Priority: 99, HasRules: true},
		{Family: "inet", Table: "empty", Name: "postrouting", Priority: -50},
	}
	allocation, err := allocateNetwork(id, testSelections(), inventory)
	if err != nil {
		t.Fatal(err)
	}
	if allocation.nftPostroutingPriority != 98 {
		t.Fatalf("native nft postrouting priority = %d, want 98", allocation.nftPostroutingPriority)
	}

	inventory.firewall.nftPostroutingNATChains = append(inventory.firewall.nftPostroutingNATChains,
		nftPostroutingNATChain{Family: "ip", Table: firewallName(id), Name: "postrouting",
			Priority: allocation.nftPostroutingPriority, HasRules: true})
	second, err := allocateNetwork(mustRunID(t, "23112233445566778899aabbccddeeff"), testSelections(), inventory)
	if err != nil {
		t.Fatal(err)
	}
	if second.nftPostroutingPriority != 97 {
		t.Fatalf("concurrent native nft postrouting priority = %d, want 97", second.nftPostroutingPriority)
	}
}

func TestAllocateNativeNFTPriorityFailsAtKernelPriorityLowerBound(t *testing.T) {
	for _, priority := range []int32{-199, -200} {
		t.Run(fmt.Sprintf("priority_%d", priority), func(t *testing.T) {
			inventory := emptyInventory()
			inventory.firewall.nftPostroutingNATChains = []nftPostroutingNATChain{{Family: "inet", Table: "host",
				Name: "postrouting", Priority: priority, HasRules: true}}
			if _, err := allocateNetwork(mustRunID(t, "24112233445566778899aabbccddeeff"),
				testSelections(), inventory); err == nil {
				t.Fatalf("native nft allocation accepted active postrouting NAT priority %d", priority)
			}
		})
	}
}
