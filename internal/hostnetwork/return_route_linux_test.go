//go:build linux

package hostnetwork

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type returnRouteModification func(*netlink.Route)

func TestExactReturnRouteAcceptsOnlyPlainIPv4LinkRoute(t *testing.T) {
	expected := returnRoute(41001, testAllocation(t, firewallNFTables).transfers[0], 17)
	if expected.Family != unix.AF_INET {
		t.Fatalf("return-route family = %d, want IPv4", expected.Family)
	}
	if !exactReturnRoute(expected, expected) {
		t.Fatal("plain IPv4 link return route was rejected")
	}
	assertReturnRouteModificationsRejected(t, expected, returnRouteIdentityModifications())
	assertReturnRouteModificationsRejected(t, expected, returnRoutePathModifications())
	assertReturnRouteModificationsRejected(t, expected, returnRouteMetricModifications())
}

func returnRouteIdentityModifications() map[string]returnRouteModification {
	return map[string]returnRouteModification{
		"wrong output link": func(route *netlink.Route) { route.LinkIndex++ },
		"wrong scope":       func(route *netlink.Route) { route.Scope = netlink.SCOPE_UNIVERSE },
		"wrong destination": func(route *netlink.Route) {
			route.Dst = &net.IPNet{IP: net.IPv4(198, 51, 100, 0), Mask: net.CIDRMask(24, 32)}
		},
		"preferred source": func(route *netlink.Route) { route.Src = net.IPv4(192, 0, 2, 2) },
		"gateway":          func(route *netlink.Route) { route.Gw = net.IPv4(192, 0, 2, 1) },
		"wrong protocol":   func(route *netlink.Route) { route.Protocol++ },
		"priority":         func(route *netlink.Route) { route.Priority = 1 },
		"wrong family":     func(route *netlink.Route) { route.Family = unix.AF_INET6 },
		"wrong table":      func(route *netlink.Route) { route.Table++ },
		"wrong type":       func(route *netlink.Route) { route.Type = unix.RTN_BLACKHOLE },
	}
}

func returnRoutePathModifications() map[string]returnRouteModification {
	mplsDestination := 101
	return map[string]returnRouteModification{
		"input link": func(route *netlink.Route) { route.ILinkIndex = 19 },
		"multipath":  func(route *netlink.Route) { route.MultiPath = []*netlink.NexthopInfo{{LinkIndex: 17}} },
		"tos":        func(route *netlink.Route) { route.Tos = 1 },
		"flags":      func(route *netlink.Route) { route.Flags = int(netlink.FLAG_ONLINK) },
		"mpls destination": func(route *netlink.Route) {
			route.MPLSDst = &mplsDestination
		},
		"new destination": func(route *netlink.Route) {
			route.NewDst = &netlink.MPLSDestination{Labels: []int{101}}
		},
		"encapsulation": func(route *netlink.Route) {
			route.Encap = &netlink.MPLSEncap{Labels: []int{101}}
		},
		"via": func(route *netlink.Route) {
			route.Via = &netlink.Via{AddrFamily: unix.AF_INET, Addr: net.IPv4(192, 0, 2, 1)}
		},
		"realm": func(route *netlink.Route) { route.Realm = 1 },
	}
}

func returnRouteMetricModifications() map[string]returnRouteModification {
	return map[string]returnRouteModification{
		"mtu":                 func(route *netlink.Route) { route.MTU = 1400 },
		"mtu lock":            func(route *netlink.Route) { route.MTULock = true },
		"window":              func(route *netlink.Route) { route.Window = 1 },
		"round trip time":     func(route *netlink.Route) { route.Rtt = 1 },
		"round trip variance": func(route *netlink.Route) { route.RttVar = 1 },
		"slow-start threshold": func(route *netlink.Route) {
			route.Ssthresh = 1
		},
		"congestion window": func(route *netlink.Route) { route.Cwnd = 1 },
		"advertised mss":    func(route *netlink.Route) { route.AdvMSS = 1200 },
		"reordering":        func(route *netlink.Route) { route.Reordering = 1 },
		"hop limit":         func(route *netlink.Route) { route.Hoplimit = 1 },
		"initial cwnd":      func(route *netlink.Route) { route.InitCwnd = 1 },
		"features":          func(route *netlink.Route) { route.Features = 1 },
		"minimum rto":       func(route *netlink.Route) { route.RtoMin = 1 },
		"minimum rto lock":  func(route *netlink.Route) { route.RtoMinLock = true },
		"initial receive window": func(route *netlink.Route) {
			route.InitRwnd = 1
		},
		"quick ack":           func(route *netlink.Route) { route.QuickACK = 1 },
		"congestion control":  func(route *netlink.Route) { route.Congctl = "reno" },
		"fast-open no cookie": func(route *netlink.Route) { route.FastOpenNoCookie = 1 },
	}
}

func assertReturnRouteModificationsRejected(t *testing.T, expected netlink.Route,
	modifications map[string]returnRouteModification,
) {
	t.Helper()
	for name, modify := range modifications {
		t.Run(name, func(t *testing.T) {
			actual := expected
			modify(&actual)
			if exactReturnRoute(actual, expected) {
				t.Fatal("return route with an unexpected semantic attribute was accepted")
			}
		})
	}
}
