package hostnetwork

import (
	"context"
	"errors"
	"fmt"
)

type firewallKind string
type iptablesFrontend string

const (
	firewallNFTables firewallKind = "nftables"
	firewallIPTables firewallKind = "iptables"

	iptablesFrontendNFT    iptablesFrontend = "nf_tables"
	iptablesFrontendLegacy iptablesFrontend = "legacy"
)

func knownIPTablesFrontend(value iptablesFrontend) bool {
	return value == iptablesFrontendNFT || value == iptablesFrontendLegacy
}

var errFirewallUnavailable = errors.New("firewall backend is unavailable")

type firewallInventory struct {
	kind                         firewallKind
	nftTableNames                map[string]struct{}
	nftForwardChains             []nftForwardChain
	nftPostroutingNATChains      []nftPostroutingNATChain
	transferlanesOwnerReferences map[string]struct{}
	iptablesFrontend             iptablesFrontend
	iptablesForwardActive        bool
	iptablesPostroutingActive    bool
}

type firewallBackend interface {
	Kind() firewallKind
	Inspect(context.Context) (firewallInventory, error)
	Preflight(context.Context, networkClaim) error
	Install(context.Context, networkClaim) (firewallReceipts, error)
	Verify(context.Context, networkClaim, bool) error
	Remove(context.Context, networkClaim, firewallReceipts) error
	Recover(context.Context, networkClaim) error
	VerifyAbsent(context.Context, networkClaim) error
	ObservePublished(context.Context, []networkClaim) ([]bool, error)
}

type firewallReceipts struct {
	kind   firewallKind
	owners map[string]struct{}
}

func newFirewallReceipts(kind firewallKind) firewallReceipts {
	return firewallReceipts{kind: kind, owners: make(map[string]struct{})}
}

func (receipts *firewallReceipts) add(owner string) {
	if receipts.owners == nil {
		receipts.owners = make(map[string]struct{})
	}
	receipts.owners[owner] = struct{}{}
}

func (receipts firewallReceipts) has(owner string) bool {
	_, exists := receipts.owners[owner]
	return exists
}

func (receipts firewallReceipts) requireComplete(claim networkClaim) error {
	expected, err := firewallReceiptOwners(claim)
	if err != nil {
		return err
	}
	if receipts.kind != firewallKind(claim.backend) || len(receipts.owners) != len(expected) {
		return errors.New("live firewall receipt set is incomplete")
	}
	for _, owner := range expected {
		if !receipts.has(owner) {
			return errors.New("live firewall receipt set is incomplete")
		}
	}
	return nil
}

func firewallReceiptOwners(claim networkClaim) ([]string, error) {
	switch firewallKind(claim.backend) {
	case firewallNFTables:
		return []string{"nft:" + claim.firewallTable}, nil
	case firewallIPTables:
		program, err := buildIPTablesProgram(claim)
		if err != nil {
			return nil, err
		}
		owners := make([]string, 0, len(program.rules))
		for _, rule := range program.rules {
			owners = append(owners, rule.owner)
		}
		return owners, nil
	default:
		return nil, fmt.Errorf("unsupported firewall receipt backend %q", claim.backend)
	}
}

func (backends firewallBackends) Preflight(ctx context.Context, claim networkClaim) error {
	backend, err := backends.forClaim(claim)
	if err != nil {
		return err
	}
	return backend.Preflight(ctx, claim)
}

type firewallBackends struct {
	nftables       firewallBackend
	iptables       firewallBackend
	legacyIPTables legacyIPTablesReader
}

func (backends firewallBackends) Inspect(ctx context.Context) (firewallInventory, error) {
	if backends.nftables == nil || backends.iptables == nil || backends.legacyIPTables == nil {
		return firewallInventory{}, errors.New("firewall backend dependencies are incomplete")
	}
	legacyForward, err := backends.legacyIPTables.Forward(ctx)
	if err != nil {
		return firewallInventory{}, fmt.Errorf("inspect legacy FORWARD surface: %w", err)
	}
	nftInventory, nftErr := backends.nftables.Inspect(ctx)
	if nftErr == nil {
		if _, err := requireFirewallInventory(nftInventory, firewallNFTables); err != nil {
			return firewallInventory{}, err
		}
	} else if !errors.Is(nftErr, errFirewallUnavailable) {
		return firewallInventory{}, fmt.Errorf("preflight nftables backend: %w", nftErr)
	}
	iptablesInventory, iptablesErr := backends.iptables.Inspect(ctx)
	if iptablesErr == nil {
		if _, err := requireFirewallInventory(iptablesInventory, firewallIPTables); err != nil {
			return firewallInventory{}, err
		}
	}
	selected, err := selectFirewallInventory(nftInventory, nftErr, iptablesInventory, iptablesErr, legacyForward)
	if err != nil {
		return firewallInventory{}, err
	}
	if err := backends.confirmNoActiveLegacyPostrouting(ctx, selected); err != nil {
		return firewallInventory{}, err
	}
	return selected, nil
}

func (backends firewallBackends) confirmNoActiveLegacyPostrouting(ctx context.Context,
	selected firewallInventory,
) error {
	if selected.kind != firewallIPTables || selected.iptablesFrontend != iptablesFrontendNFT {
		return nil
	}
	facts, err := backends.legacyIPTables.Postrouting(ctx)
	if err != nil {
		return fmt.Errorf("inspect legacy POSTROUTING surface: %w", err)
	}
	if facts.active {
		return errors.New("active nft and legacy iptables POSTROUTING surfaces cannot be modified safely together")
	}
	return nil
}

func selectFirewallInventory(nftInventory firewallInventory, nftErr error,
	iptablesInventory firewallInventory, iptablesErr error, legacyForward legacyForwardFacts,
) (firewallInventory, error) {
	if err := checkLegacyForwardAgainstIPTables(iptablesInventory, iptablesErr,
		nftErr, legacyForward); err != nil {
		return firewallInventory{}, err
	}
	if nftErr != nil {
		return selectWithoutNFTables(nftErr, iptablesInventory, iptablesErr)
	}
	if iptablesErr != nil {
		if !errors.Is(iptablesErr, errFirewallUnavailable) {
			return firewallInventory{}, fmt.Errorf("preflight iptables backend: %w", iptablesErr)
		}
		return nftInventory, nil
	}
	return selectFromCompleteFirewallInventories(nftInventory, iptablesInventory)
}

func checkLegacyForwardAgainstIPTables(inventory firewallInventory, iptablesErr, nftErr error,
	legacy legacyForwardFacts,
) error {
	if iptablesErr == nil {
		return checkLegacyForwardFacts(inventory, legacy)
	}
	if legacy.active && errors.Is(iptablesErr, errFirewallUnavailable) && nftErr == nil {
		return errors.New("active legacy iptables FORWARD surface cannot be combined with native nftables")
	}
	return nil
}

func selectWithoutNFTables(nftErr error, iptablesInventory firewallInventory,
	iptablesErr error,
) (firewallInventory, error) {
	if iptablesErr != nil {
		return firewallInventory{}, errors.Join(fmt.Errorf("preflight nftables backend: %w", nftErr),
			fmt.Errorf("preflight iptables backend: %w", iptablesErr))
	}
	return iptablesInventory, nil
}

func selectFromCompleteFirewallInventories(nftInventory,
	iptablesInventory firewallInventory,
) (firewallInventory, error) {
	native, err := directlyManagedNFTForwardChains(nftInventory.nftForwardChains, iptablesInventory)
	if err != nil {
		return firewallInventory{}, err
	}
	if iptablesInventory.iptablesForwardActive && len(native) > 0 {
		return firewallInventory{}, errors.New("active iptables and native nftables forward chains cannot be modified safely together")
	}
	if iptablesInventory.iptablesForwardActive {
		independentNAT, err := independentNFTPostroutingChains(
			nftInventory.nftPostroutingNATChains, iptablesInventory)
		if err != nil {
			return firewallInventory{}, err
		}
		if err := rejectNativeNATThatCanMapBeforeIPTables(independentNAT); err != nil {
			return firewallInventory{}, err
		}
		return iptablesInventory, nil
	}
	nftInventory.nftForwardChains = native
	return nftInventory, nil
}

func checkLegacyForwardFacts(inventory firewallInventory, legacy legacyForwardFacts) error {
	switch inventory.iptablesFrontend {
	case iptablesFrontendNFT:
		if legacy.active {
			return errors.New("active nft and legacy iptables FORWARD surfaces cannot be modified safely together")
		}
	case iptablesFrontendLegacy:
		if inventory.iptablesForwardActive != legacy.active {
			return errors.New("legacy iptables frontend and fixed legacy FORWARD observations disagree")
		}
	default:
		return errors.New("iptables inventory has no verified frontend identity")
	}
	return nil
}

func independentNFTPostroutingChains(chains []nftPostroutingNATChain,
	iptablesInventory firewallInventory,
) ([]nftPostroutingNATChain, error) {
	if iptablesInventory.iptablesFrontend != iptablesFrontendNFT {
		return append([]nftPostroutingNATChain(nil), chains...), nil
	}
	canonicalActive := false
	for _, chain := range chains {
		if chain.canonicalIPTablesPostroutingChain() {
			canonicalActive = canonicalActive || chain.HasRules
		}
	}
	if canonicalActive != iptablesInventory.iptablesPostroutingActive {
		return nil, errors.New("iptables nft frontend and canonical nftables POSTROUTING observations disagree")
	}
	result := make([]nftPostroutingNATChain, 0, len(chains))
	for _, chain := range chains {
		if !chain.canonicalIPTablesPostroutingChain() {
			result = append(result, chain)
		}
	}
	return result, nil
}

func rejectNativeNATThatCanMapBeforeIPTables(chains []nftPostroutingNATChain) error {
	for _, chain := range chains {
		if chain.HasRules && chain.Priority <= iptablesSourceNATPriority {
			return fmt.Errorf("active native nftables postrouting NAT chain %s/%s/%s priority %d can map before iptables source NAT",
				chain.Family, chain.Table, chain.Name, chain.Priority)
		}
	}
	return nil
}

func directlyManagedNFTForwardChains(chains []nftForwardChain,
	iptablesInventory firewallInventory,
) ([]nftForwardChain, error) {
	if iptablesInventory.iptablesFrontend != iptablesFrontendNFT {
		return append([]nftForwardChain(nil), chains...), nil
	}
	canonicalActive := false
	for _, chain := range chains {
		canonicalActive = canonicalActive || chain.canonicalIPTablesForwardChain()
	}
	if canonicalActive != iptablesInventory.iptablesForwardActive {
		return nil, errors.New("iptables nft frontend and canonical nftables FORWARD observations disagree")
	}
	result := make([]nftForwardChain, 0, len(chains))
	for _, chain := range chains {
		if !chain.canonicalIPTablesForwardChain() {
			result = append(result, chain)
		}
	}
	return result, nil
}

func requireFirewallInventory(inventory firewallInventory, expected firewallKind) (firewallInventory, error) {
	if inventory.kind != expected {
		return firewallInventory{}, fmt.Errorf("firewall backend returned inventory for %q, want %q",
			inventory.kind, expected)
	}
	if expected == firewallIPTables && !knownIPTablesFrontend(inventory.iptablesFrontend) {
		return firewallInventory{}, errors.New("iptables inventory has no verified frontend identity")
	}
	if expected == firewallNFTables {
		if err := validateNFTPostroutingNATChains(inventory.nftPostroutingNATChains); err != nil {
			return firewallInventory{}, err
		}
	}
	return inventory, nil
}

func (backends firewallBackends) Install(ctx context.Context, claim networkClaim) (firewallReceipts, error) {
	backend, err := backends.forClaim(claim)
	if err != nil {
		return firewallReceipts{}, err
	}
	return backend.Install(ctx, claim)
}

func (backends firewallBackends) Verify(ctx context.Context, claim networkClaim, allowAbsent bool) error {
	backend, err := backends.forClaim(claim)
	if err != nil {
		return err
	}
	return backend.Verify(ctx, claim, allowAbsent)
}

func (backends firewallBackends) Remove(ctx context.Context, claim networkClaim,
	receipts firewallReceipts) error {
	backend, err := backends.forClaim(claim)
	if err != nil {
		return err
	}
	return backend.Remove(ctx, claim, receipts)
}

func (backends firewallBackends) Recover(ctx context.Context, claim networkClaim) error {
	backend, err := backends.forClaim(claim)
	if err != nil {
		return err
	}
	return backend.Recover(ctx, claim)
}

func (backends firewallBackends) VerifyAbsent(ctx context.Context, claim networkClaim) error {
	backend, err := backends.forClaim(claim)
	if err != nil {
		return err
	}
	return backend.VerifyAbsent(ctx, claim)
}

func (backends firewallBackends) ObservePublished(ctx context.Context,
	claims []networkClaim,
) (publishedFirewallObservations, error) {
	observations := make(publishedFirewallObservations, len(claims))
	for _, kind := range []firewallKind{firewallNFTables, firewallIPTables} {
		group := claimsForFirewall(kind, claims)
		if len(group) == 0 {
			continue
		}
		backend, err := backends.forClaim(group[0])
		if err != nil {
			return nil, err
		}
		present, err := backend.ObservePublished(ctx, group)
		if err != nil {
			return nil, fmt.Errorf("observe %s published firewalls: %w", kind, err)
		}
		if len(present) != len(group) {
			return nil, fmt.Errorf("%s published firewall observation count = %d, want %d",
				kind, len(present), len(group))
		}
		for index, claim := range group {
			if _, duplicate := observations[claim.runID]; duplicate {
				return nil, fmt.Errorf("published firewall claim repeats run %s", claim.runID)
			}
			observations[claim.runID] = present[index]
		}
	}
	if len(observations) != len(claims) {
		return nil, fmt.Errorf("published firewall observations cover %d of %d claims",
			len(observations), len(claims))
	}
	return observations, nil
}

func claimsForFirewall(kind firewallKind, claims []networkClaim) []networkClaim {
	var result []networkClaim
	for _, claim := range claims {
		if firewallKind(claim.backend) == kind {
			result = append(result, claim)
		}
	}
	return result
}

func observePublishedFirewallsIndependently(ctx context.Context, backend firewallBackend,
	claims []networkClaim,
) ([]bool, error) {
	present := make([]bool, len(claims))
	for index, claim := range claims {
		absentErr := backend.VerifyAbsent(ctx, claim)
		if absentErr == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		presentErr := backend.Verify(ctx, claim, false)
		if presentErr != nil {
			return nil, errors.Join(
				fmt.Errorf("confirm claimed firewall absence: %w", absentErr),
				fmt.Errorf("confirm complete claimed firewall: %w", presentErr))
		}
		present[index] = true
	}
	return present, nil
}

func (backends firewallBackends) forClaim(claim networkClaim) (firewallBackend, error) {
	var backend firewallBackend
	switch firewallKind(claim.backend) {
	case firewallNFTables:
		backend = backends.nftables
	case firewallIPTables:
		backend = backends.iptables
	default:
		return nil, fmt.Errorf("network claim names unsupported firewall backend %q", claim.backend)
	}
	if backend == nil || backend.Kind() != firewallKind(claim.backend) {
		return nil, fmt.Errorf("firewall backend %q is unavailable for claimed ownership", claim.backend)
	}
	return backend, nil
}
