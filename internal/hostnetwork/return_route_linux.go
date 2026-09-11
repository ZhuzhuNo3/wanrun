//go:build linux

package hostnetwork

import (
	"fmt"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func returnRoute(returnTable int, value transferAllocation, linkIndex int) netlink.Route {
	return netlink.Route{LinkIndex: linkIndex, Dst: ipNet(value.subnet), Family: unix.AF_INET,
		Table: returnTable,
		Scope: netlink.SCOPE_LINK, Protocol: unix.RTPROT_STATIC, Type: unix.RTN_UNICAST}
}

func routesForReturnPrefix(returnTable int, value transferAllocation) ([]netlink.Route, error) {
	filter := &netlink.Route{Table: returnTable, Dst: ipNet(value.subnet)}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, filter,
		netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
	if err != nil {
		return nil, fmt.Errorf("list return routes: %w", err)
	}
	return routes, nil
}

func exactReturnRoute(actual, expected netlink.Route) bool {
	return sameReturnRouteIdentity(actual, expected) && plainReturnRoutePath(actual) &&
		plainReturnRouteMetrics(actual)
}

func sameReturnRouteIdentity(actual, expected netlink.Route) bool {
	return actual.Family == unix.AF_INET && expected.Family == unix.AF_INET &&
		actual.Table == expected.Table && actual.LinkIndex == expected.LinkIndex &&
		actual.Type == unix.RTN_UNICAST && actual.Scope == netlink.SCOPE_LINK &&
		actual.Protocol == expected.Protocol && equalIPNet(actual.Dst, expected.Dst) &&
		len(actual.Gw) == 0 && len(actual.Src) == 0 && actual.Priority == 0
}

func plainReturnRoutePath(route netlink.Route) bool {
	return route.ILinkIndex == 0 && len(route.MultiPath) == 0 && route.Tos == 0 &&
		route.Flags == 0 && route.MPLSDst == nil && route.NewDst == nil &&
		route.Encap == nil && route.Via == nil && route.Realm == 0
}

func plainReturnRouteMetrics(route netlink.Route) bool {
	return route.MTU == 0 && !route.MTULock && route.Window == 0 && route.Rtt == 0 &&
		route.RttVar == 0 && route.Ssthresh == 0 && route.Cwnd == 0 && route.AdvMSS == 0 &&
		route.Reordering == 0 && route.Hoplimit == 0 && route.InitCwnd == 0 &&
		route.Features == 0 && route.RtoMin == 0 && !route.RtoMinLock && route.InitRwnd == 0 &&
		route.QuickACK == 0 && route.Congctl == "" && route.FastOpenNoCookie == 0
}

func ownedHostVeth(value transferAllocation) (netlink.Link, error) {
	host, err := netlink.LinkByName(value.hostVeth)
	if err != nil {
		return nil, err
	}
	if !exactHostVethMarker(host, value) {
		return nil, ambiguousTemporaryVeth("host veth identity mismatch")
	}
	return host, nil
}

func verifyNoMainTemporaryRoute(value transferAllocation) error {
	filter := &netlink.Route{Table: unix.RT_TABLE_MAIN, Dst: ipNet(value.subnet)}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, filter,
		netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
	if err != nil {
		return fmt.Errorf("list main-table temporary routes: %w", err)
	}
	if len(routes) != 0 {
		return fmt.Errorf("temporary prefix %s leaked into main route table: %#v", value.subnet, routes)
	}
	return nil
}
