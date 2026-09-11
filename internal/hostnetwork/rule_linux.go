//go:build linux

package hostnetwork

import (
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

var policyRouteProbe = netip.MustParseAddr("1.1.1.1")

func basePolicyRule(priority, table int) netlink.Rule {
	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V4
	rule.Priority = priority
	rule.Table = table
	rule.Type = uint8(nl.FR_ACT_TO_TBL)
	rule.Protocol = uint8(unix.RTPROT_STATIC)
	return *rule
}

func exactPolicyRule(actual, expected netlink.Rule) bool {
	return actual.Family == expected.Family && actual.Priority == expected.Priority &&
		actual.Table == expected.Table && actual.IifName == expected.IifName &&
		equalIPNet(actual.Src, expected.Src) && equalIPNet(actual.Dst, expected.Dst) &&
		actual.Mark == 0 && actual.Mask == nil && actual.OifName == "" && !actual.Invert && actual.Tos == 0 &&
		actual.TunID == 0 && actual.Goto == -1 && actual.Flow == -1 &&
		actual.SuppressIfgroup == -1 && actual.SuppressPrefixlen == -1 && actual.Dport == nil &&
		actual.Sport == nil && actual.IPProto == 0 && actual.UIDRange == nil &&
		(actual.Type == 0 || actual.Type == uint8(nl.FR_ACT_TO_TBL)) && actual.Protocol == expected.Protocol
}

func equalIPNet(left, right *net.IPNet) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.String() == right.String()
}

func verifyOutboundRoute(value transferAllocation) error {
	routes, err := netlink.RouteGetWithOptions(net.IP(policyRouteProbe.AsSlice()), &netlink.RouteGetOptions{
		Iif: value.hostVeth, SrcAddr: net.IP(value.namespaceIP.AsSlice()),
	})
	if err != nil {
		return fmt.Errorf("lookup outbound route for transfer %d: %w", value.number, err)
	}
	if err := requireSingleRoute(routes, value.routeTable, value.providerIndex); err != nil {
		return fmt.Errorf("outbound route for transfer %d: %w", value.number, err)
	}
	route := routes[0]
	if value.hasGateway {
		if !net.IP(value.gateway.AsSlice()).Equal(route.Gw) {
			return fmt.Errorf("outbound route lookup used gateway %s, want %s", route.Gw, value.gateway)
		}
	} else if len(route.Gw) != 0 {
		return fmt.Errorf("outbound route lookup unexpectedly used gateway %s", route.Gw)
	}
	return nil
}

func verifyReturnPath(returnTable int, value transferAllocation) error {
	host, err := netlink.LinkByName(value.hostVeth)
	if err != nil {
		return fmt.Errorf("open host veth for return route lookup: %w", err)
	}
	if host.Attrs() == nil {
		return errors.New("host veth for return route lookup has no identity")
	}
	routes, err := netlink.RouteGetWithOptions(net.IP(value.namespaceIP.AsSlice()), &netlink.RouteGetOptions{
		Iif: value.providerName, SrcAddr: net.IP(policyRouteProbe.AsSlice()),
	})
	if err != nil {
		return fmt.Errorf("lookup return route for transfer %d: %w", value.number, err)
	}
	if err := requireSingleRoute(routes, returnTable, host.Attrs().Index); err != nil {
		return fmt.Errorf("return route for transfer %d: %w", value.number, err)
	}
	return nil
}

func requireSingleRoute(routes []netlink.Route, table, linkIndex int) error {
	if len(routes) != 1 {
		return fmt.Errorf("route lookup returned %d routes, want one: %#v", len(routes), routes)
	}
	route := routes[0]
	if route.Type != unix.RTN_UNICAST || route.Table != table || route.LinkIndex != linkIndex {
		return fmt.Errorf("route lookup used type/table/interface %d/%d/%d, want %d/%d/%d",
			route.Type, route.Table, route.LinkIndex, unix.RTN_UNICAST, table, linkIndex)
	}
	return nil
}

func ipNet(prefix netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: net.IP(prefix.Addr().AsSlice()), Mask: net.CIDRMask(prefix.Bits(), 32)}
}
