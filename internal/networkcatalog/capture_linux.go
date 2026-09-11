//go:build linux

package networkcatalog

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type linuxNetworkReader interface {
	readAddresses(context.Context) ([]addressObservation, error)
	readPolicyRules(context.Context) ([]policyRule, error)
	readRoutes(context.Context) ([]fibRoute, error)
	readLinkByIndex(context.Context, int) (linkObservation, bool, error)
	readLinkByName(context.Context, string) (linkObservation, bool, error)
}

type netlinkNetworkReader struct{}

func capture(ctx context.Context) (Snapshot, error) {
	return captureLinuxNetwork(ctx, netlinkNetworkReader{})
}

func captureLinuxNetwork(ctx context.Context, reader linuxNetworkReader) (Snapshot, error) {
	addresses, err := reader.readAddresses(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	rules, err := reader.readPolicyRules(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	routes, err := reader.readRoutes(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	resolvedRules, links, err := resolveRulesAndObserveLinks(ctx, reader, addresses, rules, routes)
	if err != nil {
		return Snapshot{}, err
	}
	confirmedRawRules, err := reader.readPolicyRules(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if !samePolicyRules(rules, confirmedRawRules) {
		return Snapshot{}, fmt.Errorf("Linux policy rules changed while capturing network catalog")
	}
	confirmedRules, confirmedLinks, err := resolveRulesAndObserveLinks(
		ctx, reader, addresses, confirmedRawRules, routes)
	if err != nil {
		return Snapshot{}, err
	}
	if !samePolicyRules(resolvedRules, confirmedRules) {
		return Snapshot{}, fmt.Errorf("Linux policy rule interface identity changed while capturing network catalog")
	}
	stableLinks, stableAddresses := stableLinkFacts(links, addresses, confirmedLinks)
	if err := requireStableRuleInputInterfaces(resolvedRules, stableLinks); err != nil {
		return Snapshot{}, err
	}
	return newSnapshot(stableLinks, stableAddresses, resolvedRules, routes), nil
}

func (netlinkNetworkReader) readAddresses(ctx context.Context) ([]addressObservation, error) {
	return observeAddresses(ctx)
}

func (netlinkNetworkReader) readPolicyRules(ctx context.Context) ([]policyRule, error) {
	return observePolicyRules(ctx)
}

func (netlinkNetworkReader) readRoutes(ctx context.Context) ([]fibRoute, error) {
	return observeRoutes(ctx)
}

func (netlinkNetworkReader) readLinkByIndex(ctx context.Context,
	index int,
) (linkObservation, bool, error) {
	return observeLinkByIndex(ctx, index)
}

func (netlinkNetworkReader) readLinkByName(ctx context.Context,
	name string,
) (linkObservation, bool, error) {
	return observeLinkByName(ctx, name)
}

func resolveRulesAndObserveLinks(ctx context.Context, reader linuxNetworkReader,
	addresses []addressObservation, rules []policyRule, routes []fibRoute,
) ([]policyRule, []linkObservation, error) {
	resolved, err := resolveRuleInputInterfaces(ctx, reader, rules)
	if err != nil {
		return nil, nil, err
	}
	required := ruleInputInterfaceIndexes(resolved)
	indexes := referencedLinkIndexes(addresses, routes, resolved)
	links, err := observeReferencedLinks(ctx, reader, indexes, required)
	if err != nil {
		return nil, nil, err
	}
	if err := validateRuleInputInterfaces(resolved, links); err != nil {
		return nil, nil, err
	}
	return resolved, links, nil
}

func ruleInputInterfaceIndexes(rules []policyRule) map[int]struct{} {
	result := make(map[int]struct{}, len(rules))
	for _, rule := range rules {
		if rule.inputInterface.state == inputInterfaceAttached && rule.inputInterface.index > 0 {
			result[rule.inputInterface.index] = struct{}{}
		}
	}
	return result
}

func resolveRuleInputInterfaces(ctx context.Context, reader linuxNetworkReader,
	rules []policyRule,
) ([]policyRule, error) {
	resolved := cloneRules(rules)
	for index := range resolved {
		selector := resolved[index].inputInterface
		if selector == (inputInterfaceSelector{}) {
			continue
		}
		if selector.name == "" {
			return nil, fmt.Errorf("policy rule has input interface identity without a name")
		}
		link, present, err := reader.readLinkByName(ctx, selector.name)
		if err != nil {
			return nil, err
		}
		switch selector.state {
		case inputInterfaceUnresolved:
			if !present {
				return nil, fmt.Errorf("attached policy rule input interface %s is unavailable", selector.name)
			}
			if link.index <= 0 || link.name != selector.name {
				return nil, fmt.Errorf("attached policy rule input interface %s has inconsistent identity", selector.name)
			}
			resolved[index].inputInterface.state = inputInterfaceAttached
			resolved[index].inputInterface.index = link.index
			resolved[index].inputInterface.isL3Master = linkIsL3Master(link)
		case inputInterfaceDetached:
			if selector.index != -1 || present {
				return nil, fmt.Errorf("detached policy rule input interface %s has conflicting link identity", selector.name)
			}
		default:
			return nil, fmt.Errorf("policy rule input interface %s is already resolved before capture", selector.name)
		}
	}
	return resolved, nil
}

func referencedLinkIndexes(addresses []addressObservation, routes []fibRoute,
	rules []policyRule,
) []int {
	indexes := make(map[int]struct{}, len(addresses)+len(routes)+len(rules))
	for _, address := range addresses {
		indexes[address.linkIndex] = struct{}{}
	}
	for _, route := range routes {
		indexes[route.linkIndex] = struct{}{}
	}
	for _, rule := range rules {
		if rule.inputInterface.state == inputInterfaceAttached {
			indexes[rule.inputInterface.index] = struct{}{}
		}
	}
	return sortedPositiveIndexes(indexes)
}

func sortedPositiveIndexes(values map[int]struct{}) []int {
	result := make([]int, 0, len(values))
	for value := range values {
		if value > 0 {
			result = append(result, value)
		}
	}
	sort.Ints(result)
	return result
}

func observeReferencedLinks(ctx context.Context, reader linuxNetworkReader,
	requested []int, required map[int]struct{},
) ([]linkObservation, error) {
	indexes := append([]int(nil), requested...)
	queued := make(map[int]struct{}, len(indexes))
	for _, index := range indexes {
		queued[index] = struct{}{}
	}
	result := make([]linkObservation, 0, len(indexes))
	for next := 0; next < len(indexes); next++ {
		link, present, err := reader.readLinkByIndex(ctx, indexes[next])
		if err != nil {
			return nil, err
		}
		if !present {
			if _, mustExist := required[indexes[next]]; mustExist {
				return nil, fmt.Errorf("policy rule input interface index %d is unavailable", indexes[next])
			}
			continue
		}
		if link.index != indexes[next] || link.name == "" {
			return nil, fmt.Errorf("referenced Linux interface %d has inconsistent identity", indexes[next])
		}
		result = append(result, link)
		if link.masterIndex > 0 {
			if _, known := queued[link.masterIndex]; !known {
				queued[link.masterIndex] = struct{}{}
				indexes = append(indexes, link.masterIndex)
			}
		}
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].index != result[right].index {
			return result[left].index < result[right].index
		}
		return result[left].name < result[right].name
	})
	return result, nil
}

func validateRuleInputInterfaces(rules []policyRule, links []linkObservation) error {
	byIndex := make(map[int]linkObservation, len(links))
	for _, link := range links {
		byIndex[link.index] = link
	}
	for _, rule := range rules {
		selector := rule.inputInterface
		if selector.state != inputInterfaceAttached {
			continue
		}
		link, present := byIndex[selector.index]
		if !present || link.name != selector.name || linkIsL3Master(link) != selector.isL3Master {
			return fmt.Errorf("policy rule input interface %s changed during link observation", selector.name)
		}
	}
	return nil
}

func requireStableRuleInputInterfaces(rules []policyRule, links []linkObservation) error {
	if err := validateRuleInputInterfaces(rules, links); err != nil {
		return fmt.Errorf("policy rule input interface was unstable across link observations: %w", err)
	}
	return nil
}

func linkIsL3Master(link linkObservation) bool {
	return link.kind == "vrf"
}

func samePolicyRules(left, right []policyRule) bool {
	left = cloneRules(left)
	right = cloneRules(right)
	sort.Slice(left, func(first, second int) bool { return lessRule(left[first], left[second]) })
	sort.Slice(right, func(first, second int) bool { return lessRule(right[first], right[second]) })
	return slices.EqualFunc(left, right, func(first, second policyRule) bool {
		return comparePolicyRules(first, second) == 0
	})
}

func stableLinkFacts(before []linkObservation, addresses []addressObservation,
	after []linkObservation,
) ([]linkObservation, []addressObservation) {
	confirmed := make(map[int]linkObservation, len(after))
	for _, link := range after {
		confirmed[link.index] = link
	}
	stableIndexes := make(map[int]struct{}, len(before))
	stableLinks := make([]linkObservation, 0, len(before))
	for _, link := range before {
		if confirmed[link.index] == link {
			stableLinks = append(stableLinks, link)
			stableIndexes[link.index] = struct{}{}
		}
	}
	stableAddresses := make([]addressObservation, 0, len(addresses))
	for _, address := range addresses {
		if _, stable := stableIndexes[address.linkIndex]; stable {
			stableAddresses = append(stableAddresses, address)
		}
	}
	return stableLinks, stableAddresses
}

func observeLinkByIndex(ctx context.Context, index int) (linkObservation, bool, error) {
	if err := ctx.Err(); err != nil {
		return linkObservation{}, false, err
	}
	link, err := netlink.LinkByIndex(index)
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		return linkObservation{}, false, nil
	}
	if err != nil {
		return linkObservation{}, false, fmt.Errorf("read Linux interface %d: %w", index, err)
	}
	attributes := link.Attrs()
	if attributes == nil {
		return linkObservation{}, false, fmt.Errorf("Linux interface %d has no attributes", index)
	}
	return linkObservation{index: attributes.Index, name: attributes.Name, kind: link.Type(),
		mtu: attributes.MTU, flags: attributes.Flags, masterIndex: attributes.MasterIndex,
		operationalState: attributes.OperState.String()}, true, ctx.Err()
}

func observeLinkByName(ctx context.Context, name string) (linkObservation, bool, error) {
	if err := ctx.Err(); err != nil {
		return linkObservation{}, false, err
	}
	link, err := netlink.LinkByName(name)
	var missing netlink.LinkNotFoundError
	if errors.As(err, &missing) {
		return linkObservation{}, false, nil
	}
	if err != nil {
		return linkObservation{}, false, fmt.Errorf("read Linux interface %s: %w", name, err)
	}
	attributes := link.Attrs()
	if attributes == nil {
		return linkObservation{}, false, fmt.Errorf("Linux interface %s has no attributes", name)
	}
	return linkObservation{index: attributes.Index, name: attributes.Name, kind: link.Type(),
		mtu: attributes.MTU, flags: attributes.Flags, masterIndex: attributes.MasterIndex,
		operationalState: attributes.OperState.String()}, true, ctx.Err()
}

func observeAddresses(ctx context.Context) ([]addressObservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	addresses, err := netlink.AddrList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("list Linux IPv4 addresses: %w", err)
	}
	result := make([]addressObservation, 0, len(addresses))
	for _, address := range addresses {
		if address.LinkIndex <= 0 {
			return nil, fmt.Errorf("Linux IPv4 address has invalid interface index %d", address.LinkIndex)
		}
		prefix, err := linuxIPv4Prefix(address.IPNet)
		if err != nil {
			return nil, fmt.Errorf("decode IPv4 address on interface %d: %w", address.LinkIndex, err)
		}
		result = append(result, addressObservation{linkIndex: address.LinkIndex, prefix: prefix})
	}
	return result, ctx.Err()
}

func observeRoutes(ctx context.Context) ([]fibRoute, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{Table: unix.RT_TABLE_UNSPEC}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, fmt.Errorf("list Linux IPv4 routes: %w", err)
	}
	result := make([]fibRoute, 0, len(routes))
	for _, route := range routes {
		fact, err := decodeRoute(route)
		if err != nil {
			return nil, err
		}
		result = append(result, fact)
	}
	return result, ctx.Err()
}

func decodeRoute(route netlink.Route) (fibRoute, error) {
	destination := netip.MustParsePrefix("0.0.0.0/0")
	var err error
	if route.Dst != nil {
		destination, err = linuxIPv4Prefix(route.Dst)
		if err != nil {
			return fibRoute{}, fmt.Errorf("decode route table %d destination: %w", route.Table, err)
		}
		destination = destination.Masked()
	}
	fact := fibRoute{table: route.Table, destination: destination, kind: linuxRouteKind(route.Type),
		linkIndex: route.LinkIndex, metric: route.Priority}
	if len(route.MultiPath) != 0 {
		fact.kind, fact.unsupported = routeUnsupported, "multipath route requires destination-dependent selection"
	}
	if route.Tos != 0 || route.ILinkIndex != 0 || route.Encap != nil || route.Via != nil || route.NewDst != nil {
		fact.kind, fact.unsupported = routeUnsupported, "route has selectors or encapsulation not determined by source"
	}
	if len(route.Gw) != 0 {
		gateway, present := netip.AddrFromSlice(route.Gw)
		if !present || !gateway.Unmap().Is4() {
			return fibRoute{}, fmt.Errorf("route table %d has non-IPv4 gateway", route.Table)
		}
		fact.gateway, fact.hasGateway = gateway.Unmap(), true
	}
	return fact, nil
}

func linuxIPv4Prefix(value *net.IPNet) (netip.Prefix, error) {
	if value == nil {
		return netip.Prefix{}, fmt.Errorf("address is empty")
	}
	address, ok := netip.AddrFromSlice(value.IP)
	if !ok || !address.Unmap().Is4() {
		return netip.Prefix{}, fmt.Errorf("address %v is not IPv4", value.IP)
	}
	ones, bits := value.Mask.Size()
	if bits == net.IPv6len*8 && address.Is4In6() {
		ones, bits = ones-96, 32
	}
	if bits != 32 || ones < 0 || ones > 32 {
		return netip.Prefix{}, fmt.Errorf("mask is %d/%d, want IPv4", ones, bits)
	}
	return netip.PrefixFrom(address.Unmap(), ones), nil
}

func linuxRouteKind(kind int) routeKind {
	switch kind {
	case unix.RTN_UNICAST:
		return routeUnicast
	case unix.RTN_THROW:
		return routeThrow
	case unix.RTN_UNREACHABLE:
		return routeUnreachable
	case unix.RTN_BLACKHOLE:
		return routeBlackhole
	case unix.RTN_PROHIBIT:
		return routeProhibit
	default:
		return routeUnsupported
	}
}
