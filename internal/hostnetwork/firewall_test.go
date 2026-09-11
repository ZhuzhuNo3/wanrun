package hostnetwork

import (
	"context"
	"errors"
	"testing"
)

func TestFirewallSelectionFallsBackOnlyBeforeMutation(t *testing.T) {
	tests := []struct {
		name           string
		nftErr         error
		iptablesErr    error
		nftChains      []nftForwardChain
		frontend       iptablesFrontend
		iptablesActive bool
		want           firewallKind
		wantErr        bool
	}{
		{name: "nftables preferred", frontend: iptablesFrontendNFT, want: firewallNFTables},
		{name: "nftables unavailable", nftErr: errFirewallUnavailable,
			frontend: iptablesFrontendLegacy, want: firewallIPTables},
		{name: "nftables unsupported with active legacy forward", nftErr: errFirewallUnavailable,
			frontend: iptablesFrontendLegacy, iptablesActive: true, want: firewallIPTables},
		{name: "both unavailable", nftErr: errFirewallUnavailable,
			iptablesErr: errFirewallUnavailable, wantErr: true},
		{name: "iptables preflight failed", nftErr: errFirewallUnavailable,
			iptablesErr: errors.New("iptables inventory malformed"), wantErr: true},
		{name: "malformed iptables inventory does not fall through to nftables",
			iptablesErr: errors.New("iptables inventory malformed"), wantErr: true},
		{name: "iptables nft frontend canonical FORWARD uses iptables",
			nftChains:      []nftForwardChain{{Family: "ip", Table: "filter", Name: "FORWARD"}},
			frontend:       iptablesFrontendNFT,
			iptablesActive: true, want: firewallIPTables},
		{name: "nft frontend canonical activity missing from iptables inventory fails closed",
			nftChains: []nftForwardChain{{Family: "ip", Table: "filter", Name: "FORWARD"}},
			frontend:  iptablesFrontendNFT, wantErr: true},
		{name: "iptables nft activity missing from nft inventory fails closed",
			frontend: iptablesFrontendNFT, iptablesActive: true, wantErr: true},
		{name: "iptables legacy plus native canonical FORWARD fails closed",
			nftChains:      []nftForwardChain{{Family: "ip", Table: "filter", Name: "FORWARD"}},
			frontend:       iptablesFrontendLegacy,
			iptablesActive: true, wantErr: true},
		{name: "active legacy iptables without native nftables uses iptables",
			frontend: iptablesFrontendLegacy, iptablesActive: true, want: firewallIPTables},
		{name: "native nftables forward chain uses nftables",
			nftChains: []nftForwardChain{{Family: "inet", Table: "host", Name: "forward"}},
			frontend:  iptablesFrontendNFT, want: firewallNFTables},
		{name: "active nft frontend and additional native chain fail before mutation",
			nftChains: []nftForwardChain{{Family: "ip", Table: "filter", Name: "FORWARD"},
				{Family: "inet", Table: "host", Name: "forward"}},
			frontend: iptablesFrontendNFT, iptablesActive: true, wantErr: true},
		{name: "successful iptables inventory without frontend identity fails closed", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nft := &recordingFirewall{kind: firewallNFTables, inspectErr: test.nftErr,
				inventory: firewallInventory{kind: firewallNFTables, nftForwardChains: test.nftChains}}
			iptables := &recordingFirewall{kind: firewallIPTables, inspectErr: test.iptablesErr,
				inventory: firewallInventory{kind: firewallIPTables,
					iptablesFrontend:      test.frontend,
					iptablesForwardActive: test.iptablesActive}}
			legacy := legacyForwardFacts{}
			if test.frontend == iptablesFrontendLegacy {
				legacy = legacyForwardFacts{exists: true, active: test.iptablesActive}
			}
			inventory, err := (firewallBackends{nftables: nft, iptables: iptables,
				legacyIPTables: staticLegacyIPTablesReader{forward: legacy}}).Inspect(context.Background())
			if (err != nil) != test.wantErr {
				t.Fatalf("Inspect error = %v, want error %v", err, test.wantErr)
			}
			if !test.wantErr && inventory.kind != test.want {
				t.Fatalf("selected backend = %q, want %q", inventory.kind, test.want)
			}
			if nft.inspectCalls != 1 || iptables.inspectCalls != 1 {
				t.Fatalf("inventory calls nft/iptables = %d/%d, want 1/1",
					nft.inspectCalls, iptables.inspectCalls)
			}
			if test.wantErr && (nft.installCalls != 0 || iptables.installCalls != 0) {
				t.Fatalf("failed inventory mutated nftables/iptables: %d/%d",
					nft.installCalls, iptables.installCalls)
			}
		})
	}
}

func TestFirewallMutationNeverFallsBack(t *testing.T) {
	nft := &recordingFirewall{kind: firewallNFTables, installErr: errors.New("nft mutation failed")}
	iptables := &recordingFirewall{kind: firewallIPTables}
	backends := firewallBackends{nftables: nft, iptables: iptables}
	id := mustRunID(t, "28112233445566778899aabbccddeeff")
	allocation, err := allocateClaimNetwork(id, testOwnerToken, testSelections(), emptyInventory())
	if err != nil {
		t.Fatal(err)
	}
	claim := claimFromAllocation(allocation)
	claim.backend = string(firewallNFTables)

	if _, err := backends.Install(context.Background(), claim); err == nil {
		t.Fatal("failed nftables mutation unexpectedly succeeded")
	}
	if nft.installCalls != 1 || iptables.installCalls != 0 {
		t.Fatalf("install calls nft/iptables = %d/%d, want 1/0",
			nft.installCalls, iptables.installCalls)
	}
}

func TestClaimedFirewallObservationRequiresCompletePresenceOrAbsence(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	absentErr := errors.New("claimed firewall is not absent")
	presentErr := errors.New("claimed firewall is not complete")
	tests := []struct {
		name         string
		firewall     *recordingFirewall
		wantPresent  bool
		wantErr      bool
		wantVerifies int
		wantAbsences int
	}{
		{name: "absent", firewall: &recordingFirewall{kind: firewallNFTables}, wantAbsences: 1},
		{name: "complete", firewall: &recordingFirewall{kind: firewallNFTables,
			verifyAbsentErr: absentErr}, wantPresent: true, wantVerifies: 1, wantAbsences: 1},
		{name: "partial or conflicting", firewall: &recordingFirewall{kind: firewallNFTables,
			verifyAbsentErr: absentErr, verifyErr: presentErr}, wantErr: true,
			wantVerifies: 1, wantAbsences: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observations, err := (firewallBackends{nftables: test.firewall}).ObservePublished(
				context.Background(), []networkClaim{claim})
			if observations[claim.runID] != test.wantPresent || (err != nil) != test.wantErr {
				t.Fatalf("claimed firewall observation = present %t, error %v",
					observations[claim.runID], err)
			}
			if test.firewall.verifyCalls != test.wantVerifies ||
				test.firewall.verifyAbsentCalls != test.wantAbsences {
				t.Fatalf("verify/absence calls = %d/%d, want %d/%d",
					test.firewall.verifyCalls, test.firewall.verifyAbsentCalls,
					test.wantVerifies, test.wantAbsences)
			}
		})
	}
}

func TestIncompleteNFTInventoryFailsBeforeOtherBackendOrMutation(t *testing.T) {
	nft := &recordingFirewall{kind: firewallNFTables, inspectErr: errors.New("nftables rule read failed")}
	iptables := &recordingFirewall{kind: firewallIPTables,
		inventory: firewallInventory{kind: firewallIPTables, iptablesFrontend: iptablesFrontendLegacy}}
	_, err := (firewallBackends{nftables: nft, iptables: iptables,
		legacyIPTables: staticLegacyIPTablesReader{}}).Inspect(context.Background())
	if err == nil || errors.Is(err, errFirewallUnavailable) {
		t.Fatalf("incomplete nftables inventory error = %v", err)
	}
	if nft.inspectCalls != 1 || iptables.inspectCalls != 0 || nft.installCalls != 0 || iptables.installCalls != 0 {
		t.Fatalf("incomplete nftables inventory calls inspect=%d/%d install=%d/%d",
			nft.inspectCalls, iptables.inspectCalls, nft.installCalls, iptables.installCalls)
	}
}

func TestIPTablesSelectionRejectsOnlyNativeNATThatCanMapFirst(t *testing.T) {
	for _, test := range []struct {
		name     string
		priority int32
		wantErr  bool
	}{
		{name: "earlier", priority: 99, wantErr: true},
		{name: "same", priority: 100, wantErr: true},
		{name: "later", priority: 101},
	} {
		t.Run(test.name, func(t *testing.T) {
			nft := firewallInventory{kind: firewallNFTables,
				nftPostroutingNATChains: []nftPostroutingNATChain{{Family: "inet", Table: "host",
					Name: "postrouting", Priority: test.priority, HasRules: true}}}
			iptables := firewallInventory{kind: firewallIPTables,
				iptablesFrontend: iptablesFrontendLegacy, iptablesForwardActive: true}
			_, err := selectFirewallInventory(nft, nil, iptables, nil, legacyForwardFacts{exists: true, active: true})
			if (err != nil) != test.wantErr {
				t.Fatalf("selection error = %v, want error %v", err, test.wantErr)
			}
		})
	}
}

func TestNFTFrontendCanonicalPostroutingIsNotAnIndependentNATSurface(t *testing.T) {
	canonicalForward := nftForwardChain{Family: "ip", Table: "filter", Name: "FORWARD"}
	canonicalNAT := nftPostroutingNATChain{Family: "ip", Table: "nat", Name: "POSTROUTING",
		Priority: 100, HasRules: true}
	nft := firewallInventory{kind: firewallNFTables, nftForwardChains: []nftForwardChain{canonicalForward},
		nftPostroutingNATChains: []nftPostroutingNATChain{canonicalNAT}}
	iptables := firewallInventory{kind: firewallIPTables, iptablesFrontend: iptablesFrontendNFT,
		iptablesForwardActive: true, iptablesPostroutingActive: true}
	selected, err := selectFirewallInventory(nft, nil, iptables, nil, legacyForwardFacts{})
	if err != nil {
		t.Fatalf("canonical iptables-nft postrouting was treated as independent: %v", err)
	}
	if selected.kind != firewallIPTables {
		t.Fatalf("selected backend = %s, want iptables", selected.kind)
	}
}

func TestIPTablesNFTSelectionRejectsActiveLegacyPostrouting(t *testing.T) {
	nft := &recordingFirewall{kind: firewallNFTables, inventory: firewallInventory{
		kind:             firewallNFTables,
		nftForwardChains: []nftForwardChain{{Family: "ip", Table: "filter", Name: "FORWARD"}},
	}}
	iptables := &recordingFirewall{kind: firewallIPTables, inventory: firewallInventory{
		kind: firewallIPTables, iptablesFrontend: iptablesFrontendNFT,
		iptablesForwardActive: true,
	}}
	legacy := fixedLegacyIPTablesReader{
		tables: staticLegacyIPTablesTables{"nat"},
		runner: &scriptedArgvRunner{responses: []argvResponse{
			{output: []byte("iptables v1.8.9 (legacy)\n")},
			{output: []byte("iptables-save v1.8.9 (legacy)\n")},
			{output: []byte("*nat\n:POSTROUTING ACCEPT [0:0]\n-A POSTROUTING -j MASQUERADE\nCOMMIT\n")},
		}},
	}

	if _, err := (firewallBackends{nftables: nft, iptables: iptables,
		legacyIPTables: legacy}).Inspect(context.Background()); err == nil {
		t.Fatal("iptables-nft selection ignored an active legacy POSTROUTING surface")
	}
	if nft.installCalls != 0 || iptables.installCalls != 0 {
		t.Fatalf("unsafe mixed NAT inventory mutated nftables/iptables: %d/%d",
			nft.installCalls, iptables.installCalls)
	}
}

func TestIPTablesNFTFallbackRejectsLegacyPostroutingWithoutActiveForward(t *testing.T) {
	postroutingCalls := 0
	nft := &recordingFirewall{kind: firewallNFTables, inspectErr: errFirewallUnavailable}
	iptables := &recordingFirewall{kind: firewallIPTables, inventory: firewallInventory{
		kind: firewallIPTables, iptablesFrontend: iptablesFrontendNFT,
	}}
	legacy := staticLegacyIPTablesReader{
		postrouting:      legacyPostroutingFacts{exists: true, active: true},
		postroutingCalls: &postroutingCalls,
	}

	if _, err := (firewallBackends{nftables: nft, iptables: iptables,
		legacyIPTables: legacy}).Inspect(context.Background()); err == nil {
		t.Fatal("iptables-nft fallback ignored active legacy POSTROUTING without active FORWARD")
	}
	if postroutingCalls != 1 || nft.installCalls != 0 || iptables.installCalls != 0 {
		t.Fatalf("unsafe fallback calls postrouting=%d install=%d/%d",
			postroutingCalls, nft.installCalls, iptables.installCalls)
	}
}

func TestIPTablesNFTSelectionReadsLegacyPostroutingOnlyWhenItsTableExists(t *testing.T) {
	for _, test := range []struct {
		name      string
		tables    staticLegacyIPTablesTables
		responses []argvResponse
		wantCalls int
	}{
		{name: "nat table absent"},
		{name: "no direct postrouting rule", tables: staticLegacyIPTablesTables{"nat"},
			responses: []argvResponse{
				{output: []byte("iptables v1.8.9 (legacy)\n")},
				{output: []byte("iptables-save v1.8.9 (legacy)\n")},
				{output: []byte("*nat\n:POSTROUTING ACCEPT [0:0]\n:FOREIGN - [0:0]\n-A FOREIGN -j MASQUERADE\nCOMMIT\n")},
			}, wantCalls: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &scriptedArgvRunner{responses: test.responses}
			nft := &recordingFirewall{kind: firewallNFTables, inventory: firewallInventory{
				kind:             firewallNFTables,
				nftForwardChains: []nftForwardChain{{Family: "ip", Table: "filter", Name: "FORWARD"}},
			}}
			iptables := &recordingFirewall{kind: firewallIPTables, inventory: firewallInventory{
				kind: firewallIPTables, iptablesFrontend: iptablesFrontendNFT,
				iptablesForwardActive: true,
			}}
			selected, err := (firewallBackends{nftables: nft, iptables: iptables,
				legacyIPTables: fixedLegacyIPTablesReader{runner: runner, tables: test.tables}}).
				Inspect(context.Background())
			if err != nil || selected.kind != firewallIPTables {
				t.Fatalf("iptables-nft selection = %s, %v", selected.kind, err)
			}
			if len(runner.calls) != test.wantCalls {
				t.Fatalf("legacy command calls = %q, want %d calls", runner.calls, test.wantCalls)
			}
		})
	}
}

func TestNativeNFTSelectionDoesNotReadLegacyPostrouting(t *testing.T) {
	postroutingCalls := 0
	legacy := staticLegacyIPTablesReader{
		postroutingErr:   errors.New("legacy NAT commands are unavailable"),
		postroutingCalls: &postroutingCalls,
	}
	nft := &recordingFirewall{kind: firewallNFTables, inventory: firewallInventory{
		kind:             firewallNFTables,
		nftForwardChains: []nftForwardChain{{Family: "inet", Table: "host", Name: "forward"}},
	}}
	iptables := &recordingFirewall{kind: firewallIPTables, inventory: firewallInventory{
		kind: firewallIPTables, iptablesFrontend: iptablesFrontendNFT,
	}}
	selected, err := (firewallBackends{nftables: nft, iptables: iptables,
		legacyIPTables: legacy}).Inspect(context.Background())
	if err != nil || selected.kind != firewallNFTables {
		t.Fatalf("native nft selection = %s, %v", selected.kind, err)
	}
	if postroutingCalls != 0 {
		t.Fatalf("native nft selection read legacy POSTROUTING %d times", postroutingCalls)
	}
}

func TestIPTablesNFTSelectionRequiresCompleteLegacyPostroutingInventory(t *testing.T) {
	postroutingCalls := 0
	nft := &recordingFirewall{kind: firewallNFTables, inventory: firewallInventory{
		kind:             firewallNFTables,
		nftForwardChains: []nftForwardChain{{Family: "ip", Table: "filter", Name: "FORWARD"}},
	}}
	iptables := &recordingFirewall{kind: firewallIPTables, inventory: firewallInventory{
		kind: firewallIPTables, iptablesFrontend: iptablesFrontendNFT,
		iptablesForwardActive: true,
	}}
	legacy := staticLegacyIPTablesReader{
		postroutingErr:   errors.New("legacy NAT inventory failed"),
		postroutingCalls: &postroutingCalls,
	}
	if _, err := (firewallBackends{nftables: nft, iptables: iptables,
		legacyIPTables: legacy}).Inspect(context.Background()); err == nil {
		t.Fatal("iptables-nft selection accepted incomplete legacy POSTROUTING inventory")
	}
	if postroutingCalls != 1 || nft.installCalls != 0 || iptables.installCalls != 0 {
		t.Fatalf("incomplete legacy NAT inventory calls postrouting=%d install=%d/%d",
			postroutingCalls, nft.installCalls, iptables.installCalls)
	}
}

type recordingFirewall struct {
	kind              firewallKind
	inspectCalls      int
	installCalls      int
	verifyCalls       int
	verifyAbsentCalls int
	inspectErr        error
	installErr        error
	verifyErr         error
	verifyAbsentErr   error
	inventory         firewallInventory
}

type staticLegacyIPTablesReader struct {
	forward          legacyForwardFacts
	postrouting      legacyPostroutingFacts
	forwardErr       error
	postroutingErr   error
	postroutingCalls *int
}

func (reader staticLegacyIPTablesReader) Forward(context.Context) (legacyForwardFacts, error) {
	return reader.forward, reader.forwardErr
}

func (reader staticLegacyIPTablesReader) Postrouting(context.Context) (legacyPostroutingFacts, error) {
	if reader.postroutingCalls != nil {
		*reader.postroutingCalls = *reader.postroutingCalls + 1
	}
	return reader.postrouting, reader.postroutingErr
}

func (firewall *recordingFirewall) Kind() firewallKind { return firewall.kind }

func (firewall *recordingFirewall) Inspect(context.Context) (firewallInventory, error) {
	firewall.inspectCalls++
	if firewall.inventory.kind == "" {
		firewall.inventory.kind = firewall.kind
	}
	return firewall.inventory, firewall.inspectErr
}

func (firewall *recordingFirewall) Install(_ context.Context, claim networkClaim) (firewallReceipts, error) {
	firewall.installCalls++
	receipts := newFirewallReceipts(firewall.kind)
	if firewall.installErr == nil {
		owners, _ := firewallReceiptOwners(claim)
		for _, owner := range owners {
			receipts.add(owner)
		}
	}
	return receipts, firewall.installErr
}

func (*recordingFirewall) Preflight(context.Context, networkClaim) error { return nil }

func (firewall *recordingFirewall) Verify(context.Context, networkClaim, bool) error {
	firewall.verifyCalls++
	return firewall.verifyErr
}

func (*recordingFirewall) Remove(context.Context, networkClaim, firewallReceipts) error { return nil }

func (*recordingFirewall) Recover(context.Context, networkClaim) error { return nil }

func (firewall *recordingFirewall) VerifyAbsent(context.Context, networkClaim) error {
	firewall.verifyAbsentCalls++
	return firewall.verifyAbsentErr
}

func (firewall *recordingFirewall) ObservePublished(ctx context.Context,
	claims []networkClaim,
) ([]bool, error) {
	return observePublishedFirewallsIndependently(ctx, firewall, claims)
}
