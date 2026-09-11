//go:build linux

package hostnetwork

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/nftables"
	"golang.org/x/sys/unix"
)

func TestNFTLiveCleanupRequiresItsPositiveReceiptBeforeMutation(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	empty := newFirewallReceipts(firewallNFTables)
	if err := (nftFirewall{}).Remove(context.Background(), claim, empty); err != nil {
		t.Fatalf("receipt-free rollback should be a no-op: %v", err)
	}
	empty.add("nft:another-owner")
	if err := (nftFirewall{}).Remove(context.Background(), claim, empty); err == nil {
		t.Fatal("live cleanup accepted a foreign nftables receipt")
	}
}

func TestNFTSuccessfulBatchKeepsReceiptWhenObservationFails(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	observedErr := errors.New("injected nftables observation failure")
	backend := nftFirewall{
		inspectInventory: func(context.Context) (firewallInventory, error) { return emptyInventory().firewall, nil },
		commitProgram:    func(nftProgram) error { return nil },
		observeClaim: func(context.Context, networkClaim, bool) error {
			return observedErr
		},
	}
	receipts, err := backend.Install(context.Background(), claim)
	if !errors.Is(err, observedErr) {
		t.Fatalf("post-batch observation error = %v, want %v", err, observedErr)
	}
	owner := "nft:" + claim.firewallTable
	if len(receipts.owners) != 1 || !receipts.has(owner) {
		t.Fatalf("successful batch receipts = %v, want %q", receipts.owners, owner)
	}
}

func TestNFTFamilyUnsupportedClassificationIsNarrow(t *testing.T) {
	for _, err := range []error{unix.EAFNOSUPPORT, unix.EPROTONOSUPPORT, unix.ENOPROTOOPT,
		unix.EOPNOTSUPP, fmt.Errorf("wrapped: %w", unix.EOPNOTSUPP)} {
		if !nftFamilyIsUnsupported(err) {
			t.Errorf("unsupported kernel error %v was not recognized", err)
		}
	}
	for _, err := range []error{unix.EPERM, unix.EBUSY, unix.ENOENT, errors.New("malformed response")} {
		if nftFamilyIsUnsupported(err) {
			t.Errorf("inventory failure %v was treated as unsupported", err)
		}
	}
}

func TestNFTPostroutingNATInventoryRequiresCompleteUniqueIdentity(t *testing.T) {
	table := &nftables.Table{Family: nftables.TableFamilyINet, Name: "host"}
	chain := &nftables.Chain{Table: table, Name: "postrouting", Hooknum: nftables.ChainHookPostrouting,
		Type: nftables.ChainTypeNAT}
	if _, found, err := nftPostroutingNATFacts(chain, true); err == nil || found {
		t.Fatalf("priority-less postrouting NAT facts = found %v, error %v", found, err)
	}
	priority := nftables.ChainPriority(99)
	chain.Priority = &priority
	facts, found, err := nftPostroutingNATFacts(chain, true)
	if err != nil || !found || facts.Priority != 99 || !facts.HasRules {
		t.Fatalf("postrouting NAT facts = %#v, found %v, error %v", facts, found, err)
	}
	if err := validateNFTPostroutingNATChains([]nftPostroutingNATChain{facts, facts}); err == nil {
		t.Fatal("duplicate postrouting NAT chain identity was accepted")
	}
	facts.Priority = -200
	if err := validateNFTPostroutingNATChains([]nftPostroutingNATChain{facts}); err == nil {
		t.Fatal("kernel-unsupported postrouting NAT chain priority was accepted")
	}
	facts.Priority = -199
	if err := validateNFTPostroutingNATChains([]nftPostroutingNATChain{facts}); err != nil {
		t.Fatalf("earliest kernel-supported postrouting NAT chain priority was rejected: %v", err)
	}
}
