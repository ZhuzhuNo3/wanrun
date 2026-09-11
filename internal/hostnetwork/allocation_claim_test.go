package hostnetwork

import (
	"reflect"
	"strings"
	"testing"
)

func TestClaimAllocationUsesOwnerBoundRandomStarts(t *testing.T) {
	id := mustRunID(t, "10a12233445566778899aabbccddeeff")
	first, err := allocateClaimNetwork(id, testOwnerToken, testSelections(), emptyInventory())
	if err != nil {
		t.Fatal(err)
	}
	again, err := allocateClaimNetwork(id, testOwnerToken, testSelections(), emptyInventory())
	if err != nil {
		t.Fatal(err)
	}
	for _, transfer := range first.transfers {
		if transfer.hostVeth != transfer.linkOwner.hostName() || transfer.hostMAC != transfer.linkOwner.hostMAC() {
			t.Fatalf("host marker %s/%s does not encode link owner %s",
				transfer.hostVeth, transfer.hostMAC, transfer.linkOwner)
		}
	}
	if !reflect.DeepEqual(first, again) {
		t.Fatalf("same owner seed produced different allocations:\n%#v\n%#v", first, again)
	}
}

func TestClaimAllocationAvoidsObservedProtocol(t *testing.T) {
	id := mustRunID(t, "10d12233445566778899aabbccddeeff")
	first, err := allocateClaimNetwork(id, testOwnerToken, testSelections(), emptyInventory())
	if err != nil {
		t.Fatal(err)
	}
	inventory := emptyInventory()
	inventory.usedProtocols[first.protocol] = struct{}{}
	second, err := allocateClaimNetwork(id, testOwnerToken, testSelections(), inventory)
	if err != nil {
		t.Fatal(err)
	}
	if second.protocol == first.protocol {
		t.Fatalf("reused observed protocol %d", first.protocol)
	}
}

func TestClaimAllocationAvoidsObservedConntrackSource(t *testing.T) {
	id := mustRunID(t, "10e12233445566778899aabbccddeeff")
	first, err := allocateClaimNetwork(id, testOwnerToken, testSelections(), emptyInventory())
	if err != nil {
		t.Fatal(err)
	}
	inventory := emptyInventory()
	inventory.conntrackOriginalSources[first.transfers[0].namespaceIP] = struct{}{}
	second, err := allocateClaimNetwork(id, testOwnerToken, testSelections(), inventory)
	if err != nil {
		t.Fatal(err)
	}
	if second.transfers[0].subnet == first.transfers[0].subnet {
		t.Fatalf("reused subnet containing observed conntrack source %s", first.transfers[0].namespaceIP)
	}
}

func TestClaimAllocationFailsWhenProtocolDomainIsExhausted(t *testing.T) {
	inventory := emptyInventory()
	for protocol := firstClaimProtocol; protocol <= lastClaimProtocol; protocol++ {
		inventory.usedProtocols[uint8(protocol)] = struct{}{}
	}
	if _, err := allocateClaimNetwork(mustRunID(t, "10b12233445566778899aabbccddeeff"),
		testOwnerToken, testSelections(), inventory); err == nil {
		t.Fatal("exhausted route/rule protocol domain was accepted")
	}
}

func TestClaimAllocationRejectsZeroOwnerToken(t *testing.T) {
	if _, err := allocateClaimNetwork(mustRunID(t, "10c12233445566778899aabbccddeeff"),
		strings.Repeat("0", 64), testSelections(), emptyInventory()); err == nil {
		t.Fatal("zero network owner token was accepted")
	}
}
