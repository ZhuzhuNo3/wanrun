//go:build linux || darwin

package hostnetwork

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
)

type networkOwnerTokens interface {
	New() (string, error)
}

type cryptographicNetworkOwnerTokens struct{}

func (cryptographicNetworkOwnerTokens) New() (string, error) { return newOwnerToken() }

func (owner *Owner) reservePublishedClaims(ctx context.Context, run runAccess,
	inventory hostInventory,
) (hostInventory, error) {
	roots, err := run.OpenPublishedRunRoots(ctx)
	if err != nil {
		return hostInventory{}, fmt.Errorf("snapshot published run roots: %w", err)
	}
	claims, readErr := readPublishedClaims(roots)
	if err := errors.Join(readErr, roots.Close()); err != nil {
		return hostInventory{}, err
	}
	observations, err := owner.observePublishedFirewalls(ctx, claims)
	if err != nil {
		return hostInventory{}, err
	}
	return mergePublishedClaimReservations(inventory, claims, observations)
}

func readPublishedClaims(roots rundirectory.PublishedRunRoots) ([]networkClaim, error) {
	var claims []networkClaim
	for _, published := range roots.Roots() {
		root, err := published.OpenRoot()
		if err != nil {
			return nil, fmt.Errorf("open published run %s: %w", published.ID(), err)
		}
		claim, exists, readErr := readPublishedClaim(root, published.ID())
		closeErr := root.Close()
		if readErr != nil {
			return nil, errors.Join(fmt.Errorf("read published run %s network claim: %w",
				published.ID(), readErr), closeErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close published run %s root: %w", published.ID(), closeErr)
		}
		if exists {
			claims = append(claims, claim)
		}
	}
	return claims, nil
}

type publishedFirewallObservations map[runid.ID]bool

func (owner *Owner) observePublishedFirewalls(ctx context.Context,
	claims []networkClaim,
) (publishedFirewallObservations, error) {
	observations, err := owner.host.ObservePublishedFirewalls(ctx, claims)
	if err != nil {
		return nil, fmt.Errorf("observe published run firewalls: %w", err)
	}
	return observations, nil
}

type publishedClaimReservations struct {
	subnets          []netip.Prefix
	priorities       map[int]runid.ID
	returnTables     map[int]runid.ID
	protocols        map[uint8]runid.ID
	hostLinks        map[string]linkMAC
	firewallOwners   map[string]runid.ID
	nftTables        map[string]runid.ID
	nftNATPriorities map[int32]runid.ID
}

func newPublishedClaimReservations() *publishedClaimReservations {
	return &publishedClaimReservations{
		priorities:       make(map[int]runid.ID),
		returnTables:     make(map[int]runid.ID),
		protocols:        make(map[uint8]runid.ID),
		hostLinks:        make(map[string]linkMAC),
		firewallOwners:   make(map[string]runid.ID),
		nftTables:        make(map[string]runid.ID),
		nftNATPriorities: make(map[int32]runid.ID),
	}
}

func mergePublishedClaimReservations(inventory hostInventory,
	claims []networkClaim, firewalls publishedFirewallObservations,
) (hostInventory, error) {
	reservations := newPublishedClaimReservations()
	for _, claim := range claims {
		if err := validateClaim(claim, claim.runID); err != nil {
			return hostInventory{}, fmt.Errorf("invalid published network claim for %s: %w", claim.runID, err)
		}
		if err := reservations.add(claim); err != nil {
			return hostInventory{}, err
		}
		if _, observed := firewalls[claim.runID]; !observed {
			return hostInventory{}, fmt.Errorf("published run %s firewall was not observed", claim.runID)
		}
	}
	return reservations.merge(inventory, claims, firewalls)
}

func (reservations *publishedClaimReservations) add(claim networkClaim) error {
	if err := reserveClaimInt(reservations.returnTables, claim.returnTable, claim.runID, "return table"); err != nil {
		return err
	}
	if err := reserveClaimProtocol(reservations.protocols, claim.protocol, claim.runID); err != nil {
		return err
	}
	if err := reserveClaimString(reservations.firewallOwners, claim.runID.String(), claim.runID,
		"firewall owner"); err != nil {
		return err
	}
	if firewallKind(claim.backend) == firewallNFTables {
		if err := reservations.addNFTables(claim); err != nil {
			return err
		}
	}
	for _, transfer := range claim.transfers {
		if err := reservations.addTransfer(claim.runID, transfer); err != nil {
			return err
		}
	}
	return nil
}

func (reservations *publishedClaimReservations) addNFTables(claim networkClaim) error {
	if err := reserveClaimString(reservations.nftTables, claim.firewallTable, claim.runID,
		"nftables table"); err != nil {
		return err
	}
	return reserveClaimNFTPriority(reservations.nftNATPriorities, claim.nftPostroutingPriority, claim.runID)
}

func (reservations *publishedClaimReservations) addTransfer(id runid.ID,
	transfer transferAllocation,
) error {
	if err := reserveClaimInt(reservations.priorities, transfer.outboundPriority, id,
		"outbound priority"); err != nil {
		return err
	}
	if err := reserveClaimInt(reservations.priorities, transfer.returnPriority, id,
		"return priority"); err != nil {
		return err
	}
	if previous, exists := reservations.hostLinks[transfer.hostVeth]; exists {
		return fmt.Errorf("published network claims conflict on host link %q (MAC %s and %s)",
			transfer.hostVeth, previous, transfer.hostMAC)
	}
	reservations.hostLinks[transfer.hostVeth] = transfer.hostMAC
	for _, subnet := range reservations.subnets {
		if subnet.Overlaps(transfer.subnet) {
			return fmt.Errorf("published network claims conflict on temporary subnet %s", transfer.subnet)
		}
	}
	reservations.subnets = append(reservations.subnets, transfer.subnet)
	return nil
}

func reserveClaimInt(reserved map[int]runid.ID, value int, id runid.ID, description string) error {
	if previous, exists := reserved[value]; exists {
		return fmt.Errorf("published network claims %s and %s conflict on %s %v",
			previous, id, description, value)
	}
	reserved[value] = id
	return nil
}

func reserveClaimString(reserved map[string]runid.ID, value string, id runid.ID,
	description string,
) error {
	if previous, exists := reserved[value]; exists {
		return fmt.Errorf("published network claims %s and %s conflict on %s %s",
			previous, id, description, value)
	}
	reserved[value] = id
	return nil
}

func reserveClaimProtocol(reserved map[uint8]runid.ID, value uint8, id runid.ID) error {
	if previous, exists := reserved[value]; exists {
		return fmt.Errorf("published network claims %s and %s conflict on protocol %d", previous, id, value)
	}
	reserved[value] = id
	return nil
}

func reserveClaimNFTPriority(reserved map[int32]runid.ID, value int32, id runid.ID) error {
	if previous, exists := reserved[value]; exists {
		return fmt.Errorf("published network claims %s and %s conflict on nftables postrouting priority %d",
			previous, id, value)
	}
	reserved[value] = id
	return nil
}

func (reservations *publishedClaimReservations) merge(inventory hostInventory,
	claims []networkClaim, firewalls publishedFirewallObservations,
) (hostInventory, error) {
	ensureInventoryMaps(&inventory)
	for _, claim := range claims {
		inventory.usedRouteTables[claim.returnTable] = struct{}{}
		inventory.usedProtocols[claim.protocol] = struct{}{}
		inventory.firewall.transferlanesOwnerReferences[claim.runID.String()] = struct{}{}
		if firewallKind(claim.backend) == firewallNFTables {
			if err := mergeClaimedNFTChain(&inventory.firewall, claim, firewalls[claim.runID]); err != nil {
				return hostInventory{}, err
			}
		}
		mergeClaimTransfers(&inventory, claim.transfers)
	}
	return inventory, nil
}

func mergeClaimedNFTChain(inventory *firewallInventory, claim networkClaim, present bool) error {
	expected := nftPostroutingNATChain{Family: "ip", Table: claim.firewallTable,
		Name: "postrouting", Priority: claim.nftPostroutingPriority, HasRules: true}
	matches := 0
	for _, observed := range inventory.nftPostroutingNATChains {
		if observed.Family != expected.Family || observed.Table != expected.Table ||
			observed.Name != expected.Name {
			continue
		}
		matches++
		if observed != expected {
			return fmt.Errorf("published claim %s conflicts with observed nftables chain %s/%s/%s",
				claim.runID, observed.Family, observed.Table, observed.Name)
		}
	}
	if matches > 1 {
		return fmt.Errorf("published claim %s has duplicated observed nftables chain %s/%s/%s",
			claim.runID, expected.Family, expected.Table, expected.Name)
	}
	if present && matches != 1 {
		return fmt.Errorf("published claim %s exact nftables program is absent from the host inventory",
			claim.runID)
	}
	if !present && matches != 0 {
		return fmt.Errorf("published claim %s absent nftables program conflicts with the host inventory",
			claim.runID)
	}
	if !present {
		inventory.nftPostroutingNATChains = append(inventory.nftPostroutingNATChains, expected)
	}
	inventory.nftTableNames[claim.firewallTable] = struct{}{}
	return nil
}

func mergeClaimTransfers(inventory *hostInventory, transfers []transferAllocation) {
	for _, transfer := range transfers {
		inventory.linkNames[transfer.hostVeth] = struct{}{}
		inventory.priorities[transfer.outboundPriority] = struct{}{}
		inventory.priorities[transfer.returnPriority] = struct{}{}
		inventory.localAddresses[transfer.hostIP] = struct{}{}
		inventory.localAddresses[transfer.namespaceIP] = struct{}{}
		inventory.routePrefixes = append(inventory.routePrefixes, transfer.subnet)
	}
}

func ensureInventoryMaps(inventory *hostInventory) {
	if inventory.linkNames == nil {
		inventory.linkNames = make(map[string]struct{})
	}
	if inventory.priorities == nil {
		inventory.priorities = make(map[int]struct{})
	}
	if inventory.usedRouteTables == nil {
		inventory.usedRouteTables = make(map[int]struct{})
	}
	if inventory.localAddresses == nil {
		inventory.localAddresses = make(map[netip.Addr]struct{})
	}
	if inventory.usedProtocols == nil {
		inventory.usedProtocols = make(map[uint8]struct{})
	}
	if inventory.firewall.nftTableNames == nil {
		inventory.firewall.nftTableNames = make(map[string]struct{})
	}
	if inventory.firewall.transferlanesOwnerReferences == nil {
		inventory.firewall.transferlanesOwnerReferences = make(map[string]struct{})
	}
}

func readPublishedClaim(root *os.File, id runid.ID) (networkClaim, bool, error) {
	directory, _, err := openNetworkDirectory(root, false)
	if errors.Is(err, os.ErrNotExist) {
		return networkClaim{}, false, nil
	}
	if err != nil {
		return networkClaim{}, false, err
	}
	defer directory.Close()
	observed, err := inspectEvidenceDirectory(directory)
	if err != nil {
		return networkClaim{}, false, err
	}
	if !observed.claim {
		return networkClaim{}, false, nil
	}
	claim, err := readClaimFile(directory, id)
	if err != nil {
		return networkClaim{}, false, err
	}
	if observed.activation {
		if err := readActivationFile(directory, claim); err != nil {
			return networkClaim{}, false, err
		}
	}
	return claim, true, nil
}
