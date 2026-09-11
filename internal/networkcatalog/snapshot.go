package networkcatalog

import (
	"cmp"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"
)

func newSnapshot(links []linkObservation, addresses []addressObservation,
	rules []policyRule, routes []fibRoute) Snapshot {
	snapshot := Snapshot{
		links:     append([]linkObservation(nil), links...),
		addresses: append([]addressObservation(nil), addresses...),
		rules:     cloneRules(rules),
		routes:    append([]fibRoute(nil), routes...),
	}
	snapshot.sortFacts()
	return snapshot
}

func cloneRules(values []policyRule) []policyRule {
	result := make([]policyRule, len(values))
	for index, value := range values {
		value.unresolvedSelectors = append([]unresolvedRuleSelector(nil), value.unresolvedSelectors...)
		value.unsupportedEffects = append([]unsupportedRuleEffect(nil), value.unsupportedEffects...)
		slices.SortFunc(value.unresolvedSelectors, compareUnresolvedSelector)
		slices.SortFunc(value.unsupportedEffects, compareUnsupportedEffect)
		result[index] = value
	}
	return result
}

func (snapshot *Snapshot) sortFacts() {
	sort.SliceStable(snapshot.links, func(left, right int) bool {
		if snapshot.links[left].index != snapshot.links[right].index {
			return snapshot.links[left].index < snapshot.links[right].index
		}
		return snapshot.links[left].name < snapshot.links[right].name
	})
	sort.SliceStable(snapshot.addresses, func(left, right int) bool {
		return lessAddress(snapshot.addresses[left], snapshot.addresses[right])
	})
	sort.SliceStable(snapshot.rules, func(left, right int) bool {
		return lessRule(snapshot.rules[left], snapshot.rules[right])
	})
	sort.SliceStable(snapshot.routes, func(left, right int) bool {
		return lessRoute(snapshot.routes[left], snapshot.routes[right])
	})
}

func lessAddress(left, right addressObservation) bool {
	if comparison := left.prefix.Addr().Compare(right.prefix.Addr()); comparison != 0 {
		return comparison < 0
	}
	if left.linkIndex != right.linkIndex {
		return left.linkIndex < right.linkIndex
	}
	return left.prefix.Bits() < right.prefix.Bits()
}

func lessRule(left, right policyRule) bool {
	return comparePolicyRules(left, right) < 0
}

func comparePolicyRules(left, right policyRule) int {
	if result := firstRuleDifference(
		cmp.Compare(left.priority, right.priority),
		cmp.Compare(left.table, right.table),
		comparePrefix(left.source, right.source),
		comparePrefix(left.destination, right.destination),
		compareInputInterface(left.inputInterface, right.inputInterface),
		compareOutputInterface(left.outputInterface, right.outputInterface),
		cmp.Compare(boolValue(left.inverted), boolValue(right.inverted)),
		cmp.Compare(left.action, right.action),
	); result != 0 {
		return result
	}
	if result := slices.CompareFunc(left.unresolvedSelectors, right.unresolvedSelectors,
		compareUnresolvedSelector); result != 0 {
		return result
	}
	return slices.CompareFunc(left.unsupportedEffects, right.unsupportedEffects, compareUnsupportedEffect)
}

func compareOutputInterface(left, right outputInterfaceSelector) int {
	return firstRuleDifference(
		cmp.Compare(left.name, right.name),
		cmp.Compare(left.state, right.state),
	)
}

func firstRuleDifference(values ...int) int {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func comparePrefix(left, right netip.Prefix) int {
	return cmp.Compare(left.String(), right.String())
}

func compareInputInterface(left, right inputInterfaceSelector) int {
	return firstRuleDifference(
		cmp.Compare(left.name, right.name),
		cmp.Compare(left.state, right.state),
		cmp.Compare(left.index, right.index),
		cmp.Compare(boolValue(left.isL3Master), boolValue(right.isL3Master)),
	)
}

func compareUnresolvedSelector(left, right unresolvedRuleSelector) int {
	return firstRuleDifference(
		cmp.Compare(left.kind, right.kind),
		cmp.Compare(left.attribute, right.attribute),
		cmp.Compare(left.value, right.value),
	)
}

func compareUnsupportedEffect(left, right unsupportedRuleEffect) int {
	return firstRuleDifference(
		cmp.Compare(left.kind, right.kind),
		cmp.Compare(left.attribute, right.attribute),
		cmp.Compare(left.value, right.value),
		cmp.Compare(left.flags, right.flags),
	)
}

func boolValue(value bool) uint8 {
	if value {
		return 1
	}
	return 0
}

func lessRoute(left, right fibRoute) bool {
	if left.table != right.table {
		return left.table < right.table
	}
	if left.destination.Bits() != right.destination.Bits() {
		return left.destination.Bits() > right.destination.Bits()
	}
	if left.metric != right.metric {
		return left.metric < right.metric
	}
	if left.kind != right.kind {
		return left.kind < right.kind
	}
	return left.linkIndex < right.linkIndex
}

func (snapshot Snapshot) Interfaces() []Interface {
	result := make([]Interface, len(snapshot.links))
	for index, link := range snapshot.links {
		result[index] = publicInterface(link)
	}
	return result
}

func (snapshot Snapshot) LocalAddresses() []LocalAddress {
	links := snapshot.linkIndex()
	result := make([]LocalAddress, 0, len(snapshot.addresses))
	for _, address := range snapshot.addresses {
		link, found := links[address.linkIndex]
		reason := defaultExclusion(link, found, links)
		result = append(result, publicAddress(address, link, reason))
	}
	return result
}

func (snapshot Snapshot) DefaultCandidates() []LocalAddress {
	addresses := snapshot.LocalAddresses()
	result := make([]LocalAddress, 0, len(addresses))
	for start := 0; start < len(addresses); {
		end := addressGroupEnd(addresses, start)
		if candidate, ok := defaultGroupCandidate(addresses[start:end]); ok {
			result = append(result, candidate)
		}
		start = end
	}
	return result
}

func addressGroupEnd(addresses []LocalAddress, start int) int {
	end := start + 1
	for end < len(addresses) && addresses[end].ip == addresses[start].ip {
		end++
	}
	return end
}

func defaultGroupCandidate(group []LocalAddress) (LocalAddress, bool) {
	first := group[0]
	if !first.IncludedByDefault() || !first.ip.IsGlobalUnicast() {
		return LocalAddress{}, false
	}
	for _, current := range group[1:] {
		if !current.IncludedByDefault() || current.interfaceValue != first.interfaceValue ||
			current.prefixBits != first.prefixBits {
			return LocalAddress{}, false
		}
	}
	return first, true
}

func (snapshot Snapshot) linkIndex() map[int]linkObservation {
	result := make(map[int]linkObservation, len(snapshot.links))
	for _, link := range snapshot.links {
		result[link.index] = link
	}
	return result
}

func publicInterface(link linkObservation) Interface {
	return Interface{index: link.index, name: link.name, kind: link.kind, mtu: link.mtu,
		flags: link.flags, masterIndex: link.masterIndex, operationalState: link.operationalState}
}

func publicAddress(address addressObservation, link linkObservation, reason string) LocalAddress {
	return LocalAddress{ip: address.prefix.Addr(), prefixBits: address.prefix.Bits(),
		interfaceValue: publicInterface(link), exclusionReason: reason}
}

func defaultExclusion(link linkObservation, found bool, links map[int]linkObservation) string {
	if !found {
		return "address refers to a missing interface"
	}
	if reason := unavailableInterfaceReason(link, "interface"); reason != "" {
		return reason
	}
	switch {
	case link.flags&net.FlagLoopback != 0:
		return "interface is loopback"
	case link.kind == "bridge":
		return "interface is a bridge"
	case link.kind == "veth":
		return "interface is a veth endpoint"
	}
	if master, present := links[link.masterIndex]; present && master.kind == "bridge" {
		return fmt.Sprintf("interface is attached to bridge %s", master.name)
	}
	return ""
}

func unavailableInterfaceReason(link linkObservation, subject string) string {
	if link.flags&net.FlagUp == 0 {
		return subject + " is administratively down"
	}
	switch link.operationalState {
	case "", "up", "unknown":
		return ""
	default:
		return fmt.Sprintf("%s operational state is %s", subject, link.operationalState)
	}
}
