package hostnetwork

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const (
	firstPriority      = 20000
	lastPriority       = 29999
	firstReturnTable   = 30000
	lastReturnTable    = 39999
	firstClaimProtocol = 100
	lastClaimProtocol  = 252
)

var temporaryRange = netip.MustParsePrefix("198.18.0.0/15")

type egressSelection struct {
	transfer      transfernumber.Number
	source        netip.Addr
	providerName  string
	providerIndex int
	routeTable    int
	gateway       netip.Addr
	hasGateway    bool
}

type hostInventory struct {
	linkNames                map[string]struct{}
	firewall                 firewallInventory
	priorities               map[int]struct{}
	usedRouteTables          map[int]struct{}
	localAddresses           map[netip.Addr]struct{}
	routePrefixes            []netip.Prefix
	routingRules             []routingRule
	usedProtocols            map[uint8]struct{}
	conntrackOriginalSources map[netip.Addr]struct{}
}

type ruleAction uint8

const (
	ruleActionLookup      ruleAction = 1
	ruleActionUnreachable ruleAction = 7
)

type routingRule struct {
	priority        int
	table           int
	action          ruleAction
	source          *netip.Prefix
	destination     *netip.Prefix
	inputInterface  string
	inverted        bool
	unknownSelector bool
	unknownTypes    []uint16
	builtinLocal    bool
	protocol        uint8
}

type networkAllocation struct {
	runID                  runid.ID
	ownerToken             string
	protocol               uint8
	backend                firewallKind
	iptablesFrontend       iptablesFrontend
	firewallTable          string
	nftPostroutingPriority int32
	returnTable            int
	forwardChains          []nftForwardChain
	transfers              []transferAllocation
}

type transferAllocation struct {
	transfer         transfernumber.Number
	number           int
	source           netip.Addr
	providerName     string
	providerIndex    int
	routeTable       int
	gateway          netip.Addr
	hasGateway       bool
	subnet           netip.Prefix
	hostIP           netip.Addr
	namespaceIP      netip.Addr
	outboundPriority int
	returnPriority   int
	linkOwner        linkOwnerID
	hostVeth         string
	hostMAC          linkMAC
	peerVeth         string
	peerMAC          linkMAC
}

type allocationStarts struct {
	ownerToken      string
	protocol        int
	returnTable     int
	priority        int
	temporarySubnet int
}

func allocateClaimNetwork(id runid.ID, ownerToken string, selected []egressSelection,
	inventory hostInventory) (networkAllocation, error) {
	if !validOwnerToken(ownerToken) {
		return networkAllocation{}, fmt.Errorf("network owner token is invalid")
	}
	starts := startsFromOwnerToken(ownerToken)
	return allocateNetworkWithStarts(id, selected, inventory, starts)
}

func allocateNetworkWithStarts(id runid.ID, selected []egressSelection,
	inventory hostInventory, starts allocationStarts) (networkAllocation, error) {
	ordered, err := orderedSelections(selected)
	if err != nil {
		return networkAllocation{}, err
	}
	result, err := allocateRunNetworkIdentity(id, ordered, inventory, starts)
	if err != nil {
		return networkAllocation{}, err
	}
	return appendTransferAllocations(result, ordered, inventory, starts)
}

func allocateRunNetworkIdentity(id runid.ID, selected []egressSelection,
	inventory hostInventory, starts allocationStarts,
) (networkAllocation, error) {
	usedRouteTables := cloneSet(inventory.usedRouteTables)
	for _, selection := range selected {
		usedRouteTables[selection.routeTable] = struct{}{}
	}
	returnTable, err := nextReturnTableFrom(usedRouteTables, starts.returnTable)
	if err != nil {
		return networkAllocation{}, err
	}
	result := networkAllocation{runID: id, ownerToken: starts.ownerToken,
		backend: inventory.firewall.kind, returnTable: returnTable}
	result.protocol, err = nextProtocolFrom(inventory.usedProtocols, starts.protocol)
	if err != nil {
		return networkAllocation{}, err
	}
	switch inventory.firewall.kind {
	case firewallNFTables:
		result.firewallTable = firewallName(id)
		result.forwardChains = append([]nftForwardChain(nil), inventory.firewall.nftForwardChains...)
		result.nftPostroutingPriority, err = nextNFTPostroutingPriority(
			inventory.firewall.nftPostroutingNATChains)
		if err != nil {
			return networkAllocation{}, err
		}
	case firewallIPTables:
		if !knownIPTablesFrontend(inventory.firewall.iptablesFrontend) {
			return networkAllocation{}, fmt.Errorf("host inventory has no verified iptables frontend")
		}
		result.iptablesFrontend = inventory.firewall.iptablesFrontend
	default:
		return networkAllocation{}, fmt.Errorf("host inventory has no selected firewall backend")
	}
	if err := rejectFirewallIdentityCollisions(id, inventory); err != nil {
		return networkAllocation{}, err
	}
	return result, nil
}

func appendTransferAllocations(result networkAllocation, selected []egressSelection,
	inventory hostInventory, starts allocationStarts,
) (networkAllocation, error) {
	usedLinkNames := cloneLinkNames(inventory.linkNames)
	usedPrefixes := append([]netip.Prefix(nil), inventory.routePrefixes...)
	usedPriorities := cloneSet(inventory.priorities)
	localAddresses := cloneAddressSet(inventory.localAddresses)
	for _, selection := range selected {
		localAddresses[selection.source] = struct{}{}
	}
	for _, selectedTransfer := range selected {
		links, err := transferLinks(result.runID, starts.ownerToken, selectedTransfer.transfer)
		if err != nil {
			return networkAllocation{}, err
		}
		if err := reserveHostLinkName(usedLinkNames, links.hostName); err != nil {
			return networkAllocation{}, err
		}
		outboundPriority, err := nextPriorityFrom(usedPriorities, starts.priority)
		if err != nil {
			return networkAllocation{}, err
		}
		usedPriorities[outboundPriority] = struct{}{}
		returnPriority, err := nextPriorityFrom(usedPriorities, starts.priority)
		if err != nil {
			return networkAllocation{}, err
		}
		usedPriorities[returnPriority] = struct{}{}
		subnet, err := nextTemporarySubnetForTransferFrom(result.runID, selectedTransfer, links, outboundPriority,
			returnPriority, usedPrefixes, inventory.routingRules, localAddresses,
			inventory.conntrackOriginalSources, starts.temporarySubnet)
		if err != nil {
			return networkAllocation{}, err
		}
		allocation := makeTransferAllocation(selectedTransfer, links, subnet, outboundPriority, returnPriority)
		result.transfers = append(result.transfers, allocation)
		usedPrefixes = append(usedPrefixes, subnet)
	}
	return result, nil
}

func startsFromOwnerToken(ownerToken string) allocationStarts {
	digest := sha256.Sum256([]byte(ownerToken))
	return allocationStarts{
		ownerToken:      ownerToken,
		protocol:        int(binary.BigEndian.Uint32(digest[0:4])),
		returnTable:     int(binary.BigEndian.Uint32(digest[4:8])),
		priority:        int(binary.BigEndian.Uint32(digest[8:12])),
		temporarySubnet: int(binary.BigEndian.Uint32(digest[12:16])),
	}
}

func nextNFTPostroutingPriority(chains []nftPostroutingNATChain) (int32, error) {
	if err := validateNFTPostroutingNATChains(chains); err != nil {
		return 0, err
	}
	earliest := iptablesSourceNATPriority
	for _, chain := range chains {
		if chain.HasRules && chain.Priority < earliest {
			earliest = chain.Priority
		}
	}
	if earliest <= nftConntrackPriority+1 {
		return 0, fmt.Errorf("active nftables postrouting NAT priority %d has no earlier kernel-supported NAT priority",
			earliest)
	}
	return earliest - 1, nil
}

func nextTemporarySubnetForTransferFrom(id runid.ID, selected egressSelection, links linkAllocation, outboundPriority,
	returnPriority int, used []netip.Prefix, rules []routingRule,
	localAddresses map[netip.Addr]struct{}, conntrackSources map[netip.Addr]struct{}, start int,
) (netip.Prefix, error) {
	base := temporaryRange.Addr()
	const candidateCount = 1 << 17 / 4
	for visited := 0; visited < candidateCount; visited++ {
		index := (normalizedStart(start, candidateCount) + visited) % candidateCount
		offset := uint32(index * 4)
		candidate := netip.PrefixFrom(addIPv4(base, offset), 30)
		if overlapsAny(candidate, used) || containsAnyAddress(candidate, conntrackSources) {
			continue
		}
		allocation := makeTransferAllocation(selected, links, candidate, outboundPriority, returnPriority)
		if !hasPreemptingRule(allocation, rules) &&
			!hasPreemptingReturnRule(allocation, rules, localAddresses) {
			return candidate, nil
		}
	}
	return netip.Prefix{}, fmt.Errorf("temporary IPv4 /30 range has no allocation safe from higher-priority RPDB rules")
}

func containsAnyAddress(prefix netip.Prefix, addresses map[netip.Addr]struct{}) bool {
	for address := range addresses {
		if prefix.Contains(address.Unmap()) {
			return true
		}
	}
	return false
}

func hasPreemptingReturnRule(allocation transferAllocation, rules []routingRule,
	localAddresses map[netip.Addr]struct{},
) bool {
	for _, rule := range rules {
		if rule.builtinLocal || rule.priority < 0 || rule.priority >= allocation.returnPriority {
			continue
		}
		destinationMatches := rule.destination == nil || rule.destination.Contains(allocation.namespaceIP)
		interfaceMatches := rule.inputInterface == "" || rule.inputInterface == allocation.providerName
		if rule.inverted {
			if !destinationMatches || !interfaceMatches || rule.source != nil || rule.unknownSelector {
				return true
			}
			continue
		}
		if returnSourceMayMatch(rule.source, localAddresses) && destinationMatches && interfaceMatches {
			return true
		}
	}
	return false
}

func returnSourceMayMatch(source *netip.Prefix, localAddresses map[netip.Addr]struct{}) bool {
	if source == nil {
		return true
	}
	if source.Bits() != 32 {
		return true
	}
	_, local := localAddresses[source.Addr()]
	return !local
}

func hasPreemptingRule(allocation transferAllocation, rules []routingRule) bool {
	for _, rule := range rules {
		if rule.builtinLocal || rule.priority < 0 || rule.priority >= allocation.outboundPriority {
			continue
		}
		sourceMatches := rule.source == nil || rule.source.Contains(allocation.namespaceIP)
		interfaceMatches := rule.inputInterface == "" || rule.inputInterface == allocation.hostVeth
		if rule.inverted {
			if !sourceMatches || !interfaceMatches || rule.unknownSelector {
				return true
			}
			continue
		}
		if sourceMatches && interfaceMatches {
			return true
		}
	}
	return false
}

func orderedSelections(selected []egressSelection) ([]egressSelection, error) {
	if len(selected) == 0 || len(selected) > transfernumber.Maximum {
		return nil, fmt.Errorf("host network requires 1..%d selected egresses", transfernumber.Maximum)
	}
	result := append([]egressSelection(nil), selected...)
	sort.Slice(result, func(left, right int) bool {
		return result[left].transfer.Value() < result[right].transfer.Value()
	})
	for index, value := range result {
		if value.transfer.Value() != uint8(index+1) {
			return nil, fmt.Errorf("selected egress set is missing transfer %d", index+1)
		}
		if !validProviderName(value.providerName) || !value.source.Is4() || value.source.IsUnspecified() ||
			value.providerIndex <= 0 || value.routeTable <= 0 || value.hasGateway && !value.gateway.Is4() {
			return nil, fmt.Errorf("transfer %d has incomplete egress facts", value.transfer.Value())
		}
	}
	return result, nil
}

func validProviderName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 15 &&
		!strings.ContainsAny(name, "/\x00")
}

func rejectFirewallIdentityCollisions(id runid.ID, inventory hostInventory) error {
	switch inventory.firewall.kind {
	case firewallNFTables:
		if _, exists := inventory.firewall.nftTableNames[firewallName(id)]; exists {
			return fmt.Errorf("nft table identity %q already exists", firewallName(id))
		}
		if _, exists := inventory.firewall.transferlanesOwnerReferences[id.String()]; exists {
			return fmt.Errorf("nftables ownership comment for run %s already exists", id)
		}
	case firewallIPTables:
		if _, exists := inventory.firewall.transferlanesOwnerReferences[id.String()]; exists {
			return fmt.Errorf("iptables ownership comment for run %s already exists", id)
		}
	default:
		return fmt.Errorf("host inventory has unsupported firewall backend %q", inventory.firewall.kind)
	}
	return nil
}

type linkAllocation struct {
	owner    linkOwnerID
	hostName string
	hostMAC  linkMAC
	peerName string
	peerMAC  linkMAC
}

func transferLinks(id runid.ID, ownerToken string, transfer transfernumber.Number) (linkAllocation, error) {
	owner, err := deriveLinkOwnerID(ownerToken, transfer)
	if err != nil {
		return linkAllocation{}, err
	}
	peerMAC, err := derivePeerMAC(ownerToken, transfer)
	if err != nil {
		return linkAllocation{}, err
	}
	return linkAllocation{owner: owner, hostName: owner.hostName(), hostMAC: owner.hostMAC(),
		peerName: derivedPeerName(id, int(transfer.Value())), peerMAC: peerMAC}, nil
}

func reserveHostLinkName(used map[string]struct{}, name string) error {
	if _, exists := used[name]; exists {
		return fmt.Errorf("host link identity %q already exists", name)
	}
	used[name] = struct{}{}
	return nil
}

func cloneLinkNames(source map[string]struct{}) map[string]struct{} {
	result := make(map[string]struct{}, len(source))
	for name := range source {
		result[name] = struct{}{}
	}
	return result
}

func derivedPeerName(id runid.ID, number int) string {
	suffix := id.String()[:10] + fmt.Sprintf("%02x", number)
	return "wrp" + suffix
}

func firewallName(id runid.ID) string { return "transferlanes_" + id.String() }

func makeTransferAllocation(selected egressSelection, links linkAllocation,
	subnet netip.Prefix, outboundPriority, returnPriority int) transferAllocation {
	number := int(selected.transfer.Value())
	return transferAllocation{transfer: selected.transfer, number: number, source: selected.source,
		providerName: selected.providerName, providerIndex: selected.providerIndex,
		routeTable: selected.routeTable, gateway: selected.gateway, hasGateway: selected.hasGateway,
		subnet: subnet, hostIP: subnet.Addr().Next(), namespaceIP: subnet.Addr().Next().Next(),
		outboundPriority: outboundPriority, returnPriority: returnPriority,
		linkOwner: links.owner, hostVeth: links.hostName, hostMAC: links.hostMAC,
		peerVeth: links.peerName, peerMAC: links.peerMAC}
}

func overlapsAny(candidate netip.Prefix, used []netip.Prefix) bool {
	for _, prefix := range used {
		if prefix.IsValid() && prefix.Addr().Is4() && prefix.Bits() > 0 && candidate.Overlaps(prefix.Masked()) {
			return true
		}
	}
	return false
}

func addIPv4(address netip.Addr, offset uint32) netip.Addr {
	bytes := address.As4()
	value := uint32(bytes[0])<<24 | uint32(bytes[1])<<16 | uint32(bytes[2])<<8 | uint32(bytes[3])
	value += offset
	return netip.AddrFrom4([4]byte{byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value)})
}

func nextPriorityFrom(used map[int]struct{}, start int) (int, error) {
	count := lastPriority - firstPriority + 1
	for visited := 0; visited < count; visited++ {
		priority := firstPriority + (normalizedStart(start, count)+visited)%count
		if _, exists := used[priority]; !exists {
			return priority, nil
		}
	}
	return 0, fmt.Errorf("policy priority range is exhausted")
}

func nextReturnTableFrom(used map[int]struct{}, start int) (int, error) {
	count := lastReturnTable - firstReturnTable + 1
	for visited := 0; visited < count; visited++ {
		table := firstReturnTable + (normalizedStart(start, count)+visited)%count
		if _, exists := used[table]; !exists {
			return table, nil
		}
	}
	return 0, fmt.Errorf("return route table range is exhausted")
}

func nextProtocolFrom(used map[uint8]struct{}, start int) (uint8, error) {
	count := lastClaimProtocol - firstClaimProtocol + 1
	for visited := 0; visited < count; visited++ {
		protocol := uint8(firstClaimProtocol + (normalizedStart(start, count)+visited)%count)
		if _, exists := used[protocol]; !exists {
			return protocol, nil
		}
	}
	return 0, fmt.Errorf("route/rule protocol range is exhausted")
}

func normalizedStart(start, count int) int {
	value := start % count
	if value < 0 {
		value += count
	}
	return value
}

func cloneSet(input map[int]struct{}) map[int]struct{} {
	result := make(map[int]struct{}, len(input))
	for value := range input {
		result[value] = struct{}{}
	}
	return result
}

func cloneAddressSet(input map[netip.Addr]struct{}) map[netip.Addr]struct{} {
	result := make(map[netip.Addr]struct{}, len(input))
	for value := range input {
		result[value] = struct{}{}
	}
	return result
}
