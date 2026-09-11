package networkcatalog

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// Linux uses LOOPBACK_IFINDEX as flowi4_iif for locally originated IPv4 lookups.
const localIPv4OutputInputInterfaceIndex = 1

type localOutputRuleMatch uint8

const (
	ruleDoesNotMatchLocalOutput localOutputRuleMatch = iota
	ruleMatchesLocalOutput
	ruleMayMatchLocalOutput
)

func (snapshot Snapshot) Resolve(sources []netip.Addr) []Egress {
	result := make([]Egress, len(sources))
	for index, source := range sources {
		result[index] = snapshot.resolveOne(source)
	}
	return result
}

func (snapshot Snapshot) resolveOne(source netip.Addr) Egress {
	egress := Egress{localIP: source}
	if !source.Is4() {
		egress.reason = fmt.Sprintf("selected source %s is not IPv4", source)
		return egress
	}
	address, diagnostics, reason := snapshot.localAddress(source)
	egress.diagnostics = append(egress.diagnostics, diagnostics...)
	if reason != "" {
		egress.reason = reason
		return egress
	}
	if reason = snapshot.unsupportedAddressTopology(address); reason != "" {
		egress.reason = reason
		return egress
	}
	return snapshot.resolveRules(egress, source)
}

func (snapshot Snapshot) localAddress(source netip.Addr) (addressObservation, []string, string) {
	matches := make([]addressObservation, 0, 1)
	for _, address := range snapshot.addresses {
		if address.prefix.Addr() == source {
			matches = append(matches, address)
		}
	}
	if len(matches) == 0 {
		return addressObservation{}, nil, fmt.Sprintf("selected source %s is not a local address", source)
	}
	first := matches[0]
	for _, match := range matches[1:] {
		if match.linkIndex != first.linkIndex || match.prefix.Bits() != first.prefix.Bits() {
			return addressObservation{}, nil,
				fmt.Sprintf("selected source %s has conflicting facts on multiple interfaces or prefixes", source)
		}
	}
	diagnostics := []string(nil)
	if len(matches) > 1 {
		link := snapshot.linkIndex()[first.linkIndex]
		diagnostics = append(diagnostics,
			fmt.Sprintf("duplicate address observation on %s(ifindex=%d)", link.name, link.index))
	}
	return first, diagnostics, ""
}

func (snapshot Snapshot) unsupportedAddressTopology(address addressObservation) string {
	links := snapshot.linkIndex()
	link, found := links[address.linkIndex]
	if !found {
		return fmt.Sprintf("local address refers to missing interface index %d", address.linkIndex)
	}
	if reason := unavailableInterfaceReason(link, "local interface "+link.name); reason != "" {
		return reason
	}
	if link.flags&net.FlagLoopback != 0 {
		return fmt.Sprintf("local interface %s is loopback", link.name)
	}
	if link.kind == "vrf" {
		return fmt.Sprintf("local interface %s uses unsupported VRF topology", link.name)
	}
	if master, present := links[link.masterIndex]; present && master.kind == "vrf" {
		return fmt.Sprintf("local interface %s is attached to unsupported VRF %s", link.name, master.name)
	}
	return ""
}

func (snapshot Snapshot) resolveRules(egress Egress, source netip.Addr) Egress {
	for index := 0; index < len(snapshot.rules); {
		end := equalPriorityEnd(snapshot.rules, index)
		applicable := make([]policyRule, 0, end-index)
		for _, rule := range snapshot.rules[index:end] {
			match, undecidable := matchLocalOutputRule(rule, source)
			if match == ruleMayMatchLocalOutput {
				egress.diagnostics = append(egress.diagnostics,
					fmt.Sprintf("policy rule priority=%d table=%d", rule.priority, rule.table))
				egress.reason = "policy rule depends on selector not determined for local output: " +
					strings.Join(undecidable, ", ")
				return egress
			}
			if match == ruleMatchesLocalOutput {
				applicable = append(applicable, rule)
			}
		}
		if len(applicable) > 1 && !equivalentRules(applicable) {
			egress.reason = fmt.Sprintf("multiple policy rules at priority %d are ambiguous", applicable[0].priority)
			return egress
		}
		if len(applicable) > 0 {
			var finished bool
			egress, finished = snapshot.applyRule(egress, applicable[0])
			if finished {
				return egress
			}
		}
		index = end
	}
	egress.reason = "no usable default route for selected source"
	return egress
}

func equalPriorityEnd(rules []policyRule, start int) int {
	end := start + 1
	for end < len(rules) && rules[end].priority == rules[start].priority {
		end++
	}
	return end
}

func matchLocalOutputRule(rule policyRule, source netip.Addr) (localOutputRuleMatch, []string) {
	nonmatching := rule.source.IsValid() && !rule.source.Contains(source)
	undecidable := make([]string, 0, len(rule.unresolvedSelectors)+2)
	if rule.destination.IsValid() && rule.destination.Bits() > 0 {
		undecidable = append(undecidable, "destination prefix "+rule.destination.String())
	}
	if rule.inputInterface.name != "" {
		matches, known := inputInterfaceMatchesLocalOutput(rule.inputInterface)
		if known && !matches {
			nonmatching = true
		} else if !known {
			undecidable = append(undecidable, "input interface "+rule.inputInterface.name)
		}
	} else if rule.inputInterface != (inputInterfaceSelector{}) {
		undecidable = append(undecidable, "invalid input interface identity")
	}
	if rule.outputInterface.name != "" {
		undecidable = append(undecidable, "output interface "+rule.outputInterface.name)
	} else if rule.outputInterface != (outputInterfaceSelector{}) {
		undecidable = append(undecidable, "invalid output interface identity")
	}
	for _, selector := range rule.unresolvedSelectors {
		undecidable = appendUniqueDescription(undecidable, selector.description())
	}
	if nonmatching {
		if rule.inverted {
			return ruleMatchesLocalOutput, nil
		}
		return ruleDoesNotMatchLocalOutput, nil
	}
	if len(undecidable) != 0 {
		return ruleMayMatchLocalOutput, undecidable
	}
	if rule.inverted {
		return ruleDoesNotMatchLocalOutput, nil
	}
	return ruleMatchesLocalOutput, nil
}

func inputInterfaceMatchesLocalOutput(selector inputInterfaceSelector) (bool, bool) {
	if selector.isL3Master {
		return false, false
	}
	switch selector.state {
	case inputInterfaceAttached:
		if selector.index <= 0 {
			return false, false
		}
		return selector.index == localIPv4OutputInputInterfaceIndex, true
	case inputInterfaceDetached:
		if selector.index != -1 {
			return false, false
		}
		return false, true
	default:
		return false, false
	}
}

func appendUniqueDescription(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func equivalentRules(rules []policyRule) bool {
	first := rules[0]
	for _, rule := range rules[1:] {
		if comparePolicyRules(rule, first) != 0 {
			return false
		}
	}
	return true
}

func (snapshot Snapshot) applyRule(egress Egress, rule policyRule) (Egress, bool) {
	egress.diagnostics = append(egress.diagnostics,
		fmt.Sprintf("policy rule priority=%d table=%d", rule.priority, rule.table))
	if len(rule.unsupportedEffects) != 0 {
		descriptions := make([]string, len(rule.unsupportedEffects))
		for index, effect := range rule.unsupportedEffects {
			descriptions[index] = effect.description()
		}
		egress.reason = "policy rule has unsupported effect: " + strings.Join(descriptions, ", ")
		return egress, true
	}
	switch rule.action {
	case ruleContinue:
		return egress, false
	case ruleBlackhole, ruleUnreachable, ruleProhibit:
		egress.reason = "policy rule action is " + rule.action.String()
		return egress, true
	case ruleLookup:
		return snapshot.lookupTable(egress, rule.table)
	default:
		egress.reason = "policy rule has unsupported action"
		return egress, true
	}
}

func (selector unresolvedRuleSelector) description() string {
	switch selector.kind {
	case ruleSelectorTypeOfService:
		return "type-of-service"
	case ruleSelectorFirewallMark:
		return "fwmark"
	case ruleSelectorUIDRange:
		return "UID range"
	case ruleSelectorTransport:
		return "transport selector"
	case ruleSelectorL3Master:
		return "VRF/l3mdev"
	case ruleSelectorTunnelID:
		return "tunnel ID"
	case ruleSelectorFlowRealm:
		return "flow realm"
	default:
		return fmt.Sprintf("rule attribute %d", selector.attribute)
	}
}

func (effect unsupportedRuleEffect) description() string {
	switch effect.kind {
	case ruleEffectRouteSuppression:
		return "route suppression"
	case ruleEffectGoto:
		return "goto"
	case ruleEffectHeaderFlags:
		return fmt.Sprintf("rule flags %#x", effect.flags)
	default:
		return "unknown rule effect"
	}
}

func (snapshot Snapshot) lookupTable(egress Egress, table int) (Egress, bool) {
	candidates := snapshot.defaultRoutes(table)
	if len(candidates) == 0 {
		egress.diagnostics = append(egress.diagnostics, fmt.Sprintf("table %d has no default route", table))
		return egress, false
	}
	best := equalMetricRoutes(candidates)
	if len(best) > 1 && !equivalentRoutes(best) {
		egress.reason = fmt.Sprintf("table %d has ambiguous equal-cost default routes", table)
		return egress, true
	}
	return snapshot.applyRoute(egress, best[0])
}

func (snapshot Snapshot) defaultRoutes(table int) []fibRoute {
	result := make([]fibRoute, 0, 1)
	for _, route := range snapshot.routes {
		if route.table == table && route.destination.IsValid() && route.destination.Bits() == 0 {
			result = append(result, route)
		}
	}
	return result
}

func equalMetricRoutes(routes []fibRoute) []fibRoute {
	end := 1
	for end < len(routes) && routes[end].metric == routes[0].metric {
		end++
	}
	return routes[:end]
}

func equivalentRoutes(routes []fibRoute) bool {
	first := routes[0]
	for _, route := range routes[1:] {
		if route.kind != first.kind || route.linkIndex != first.linkIndex ||
			route.gateway != first.gateway || route.hasGateway != first.hasGateway {
			return false
		}
	}
	return true
}

func (snapshot Snapshot) applyRoute(egress Egress, route fibRoute) (Egress, bool) {
	egress.diagnostics = append(egress.diagnostics,
		fmt.Sprintf("default route table=%d metric=%d type=%s", route.table, route.metric, route.kind))
	switch route.kind {
	case routeThrow:
		return egress, false
	case routeUnreachable, routeBlackhole, routeProhibit:
		egress.reason = "default route action is " + route.kind.String()
		return egress, true
	case routeUnicast:
		return snapshot.runnableRoute(egress, route), true
	default:
		egress.reason = "default route is unsupported"
		if route.unsupported != "" {
			egress.reason += ": " + route.unsupported
		}
		return egress, true
	}
}

func (snapshot Snapshot) runnableRoute(egress Egress, route fibRoute) Egress {
	link, present := snapshot.linkIndex()[route.linkIndex]
	if !present || link.index <= 0 || link.name == "" {
		egress.reason = fmt.Sprintf("default route output interface index %d is unavailable", route.linkIndex)
		return egress
	}
	if reason := unavailableInterfaceReason(link, "default route output interface "+link.name); reason != "" {
		egress.reason = reason
		return egress
	}
	if link.kind == "vrf" {
		egress.reason = fmt.Sprintf("default route uses unsupported VRF interface %s", link.name)
		return egress
	}
	if master, exists := snapshot.linkIndex()[link.masterIndex]; exists && master.kind == "vrf" {
		egress.reason = fmt.Sprintf("default route interface %s is attached to unsupported VRF %s", link.name, master.name)
		return egress
	}
	if route.hasGateway && invalidGateway(route.gateway) {
		egress.reason = fmt.Sprintf("default route gateway %s is invalid", route.gateway)
		return egress
	}
	egress.interfaceValue = publicInterface(link)
	egress.table = route.table
	egress.gateway, egress.hasGateway = route.gateway, route.hasGateway
	egress.runnable = true
	return egress
}

func invalidGateway(address netip.Addr) bool {
	return !address.Is4() || address.IsUnspecified() || address.IsLoopback() || address.IsMulticast() ||
		address == netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

func (action ruleAction) String() string {
	return [...]string{"lookup", "blackhole", "unreachable", "prohibit", "continue"}[action]
}

func (kind routeKind) String() string {
	return [...]string{"unicast", "throw", "unreachable", "blackhole", "prohibit", "unsupported"}[kind]
}
