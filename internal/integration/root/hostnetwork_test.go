//go:build linux && rootintegration

package root_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/networkcatalog"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

type hostNetworkFixture struct {
	t            *testing.T
	prefix       string
	filterTable  string
	natTable     string
	router       string
	providers    []string
	peers        []string
	sources      []netip.Addr
	sourceLinks  []int
	gateway      []netip.Addr
	tables       []int
	priorities   []int
	remote       netip.Addr
	sysctls      map[string][]byte
	server       *http.Server
	serverDone   chan error
	observed     chan netip.Addr
	measured     chan sourceObservation
	defaultRoute *mainDefaultRouteReceipt
	remoteRoute  *remoteReachabilityRouteReceipt
}

type mainDefaultRouteReceipt struct {
	route netlink.Route
}

type remoteReachabilityRouteReceipt struct {
	route netlink.Route
}

type sourceObservation struct {
	source   netip.Addr
	at       time.Time
	finished bool
}

func TestCloseRestoresHostNetworkSurface(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	before := fixture.hostSurface()
	egresses := fixture.resolveEgresses()
	runs := rundirectory.New()
	live, id := fixture.createRun(runs)
	namespacesBefore := fixture.run("ip", "netns", "list")

	session, err := hostnetwork.New().Open(context.Background(), live, egresses)
	if err != nil {
		t.Fatalf("open host network: %v", err)
	}
	disableCleanup := fixture.registerSessionCleanup(session)
	fixture.assertTraffic(session)
	links := fixture.observeTransferLinks(session)
	fixture.assertKernelRouteLookups(links)
	fixture.assertOwnedConntrackPresent()
	fixture.assertOwnedRulesAndNFT(id, links)
	if namespacesAfter := fixture.run("ip", "netns", "list"); !sameNamedNamespaces(namespacesBefore, namespacesAfter) {
		t.Fatalf("Transfer Lanes created a named namespace\nbefore:\n%s\nafter:\n%s",
			namespacesBefore, namespacesAfter)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close host network: %v", err)
	}
	disableCleanup()
	if err := live.Complete(context.Background()); err != nil {
		t.Fatalf("complete live run: %v", err)
	}
	fixture.assertSurface(before)
}

func TestOpenUsesPerInterfaceForwarding(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	egresses := fixture.resolveEgresses()
	fixture.writeSysctl("/proc/sys/net/ipv4/ip_forward", "0\n")
	for _, provider := range fixture.providers {
		fixture.writeSysctl("/proc/sys/net/ipv4/conf/"+provider+"/forwarding", "1\n")
	}
	disabledSurface := fixture.hostSurface()
	live, _ := fixture.createRun(rundirectory.New())

	session, err := hostnetwork.New().Open(context.Background(), live, egresses)
	if err != nil {
		t.Fatalf("open with global forwarding disabled: %v", err)
	}
	disableCleanup := fixture.registerSessionCleanup(session)
	fixture.assertTraffic(session)
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("close with global forwarding disabled: %v", err)
	}
	disableCleanup()
	if err := live.Complete(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.assertSurface(disabledSurface)
}

func TestOpenRejectsPreemptingSourceRule(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	egresses := fixture.resolveEgresses()
	priority := 9000
	fixture.run("ip", "rule", "add", "priority", strconv.Itoa(priority),
		"from", "198.18.0.0/15", "blackhole")
	defer fixture.runCleanup("ip", "rule", "del", "priority", strconv.Itoa(priority))
	withConflict := fixture.hostSurface()
	live, _ := fixture.createRun(rundirectory.New())

	session, err := hostnetwork.New().Open(context.Background(), live, egresses)
	disableCleanup := func() {}
	if session != nil {
		disableCleanup = fixture.registerSessionCleanup(session)
	}
	if err == nil || session != nil {
		if session != nil {
			if closeErr := session.Close(context.Background()); closeErr == nil {
				disableCleanup()
			}
		}
		t.Fatal("preempting wide RPDB rule was accepted")
	}
	if err := live.Complete(context.Background()); err != nil {
		t.Fatalf("complete preflight-rejected run: %v", err)
	}
	fixture.assertSurface(withConflict)
}

func TestPreflightRejectsInvertedTOSRuleBeforeClaimPublication(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	egresses := fixture.resolveEgresses()
	for index, source := range fixture.sources {
		table := fixture.tables[fixture.sourceLinks[index]]
		priority := 8995 + index
		fixture.run("ip", "rule", "add", "priority", strconv.Itoa(priority),
			"from", source.String()+"/32", "table", strconv.Itoa(table))
		defer fixture.runCleanup("ip", "rule", "del", "priority", strconv.Itoa(priority))
	}
	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V4
	rule.Priority = 8999
	rule.Src = &net.IPNet{IP: net.ParseIP("198.18.0.0"), Mask: net.CIDRMask(15, 32)}
	rule.Tos = 0x10
	rule.Invert = true
	rule.Type = uint8(nl.FR_ACT_UNREACHABLE)
	if err := netlink.RuleAdd(rule); err != nil {
		t.Fatalf("add inverted TOS rule: %v", err)
	}
	defer func() {
		if err := netlink.RuleDel(rule); err != nil {
			t.Errorf("remove inverted TOS rule: %v", err)
		}
	}()
	if _, err := netlink.RouteGetWithOptions(net.ParseIP("1.1.1.1"), &netlink.RouteGetOptions{
		Iif: fixture.providers[0], SrcAddr: net.ParseIP("198.18.0.2")}); err == nil {
		t.Fatal("ordinary TOS=0 lookup did not match inverted TOS rule")
	}
	withConflict := fixture.hostSurface()
	live, id := fixture.createRun(rundirectory.New())
	watch := fixture.watchRunRootCreations(id)

	session, err := hostnetwork.New().Open(context.Background(), live, egresses)
	disableCleanup := func() {}
	if session != nil {
		disableCleanup = fixture.registerSessionCleanup(session)
	}
	published := watch()
	if err == nil || session != nil {
		if session != nil {
			if closeErr := session.Close(context.Background()); closeErr == nil {
				disableCleanup()
			}
		}
		t.Fatal("preempting inverted TOS rule was accepted")
	}
	if published {
		t.Fatal("preflight rejection occurred after network claim publication")
	}
	if err := live.Complete(context.Background()); err != nil {
		t.Fatalf("complete inverted-TOS-rejected run: %v", err)
	}
	fixture.assertSurface(withConflict)
}

func TestRecoveryPreservesWrongOwnerEvidence(t *testing.T) {
	for _, test := range []struct {
		name     string
		evidence string
	}{
		{name: "temporary claim", evidence: ".claim.tmp"},
		{name: "discarded claim", evidence: ".claim.discard.0123456789abcdef0123456789abcdef"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHostNetworkFixture(t)
			fixture.install()
			before := fixture.hostSurface()
			runs := rundirectory.New()
			live, id := fixture.createRun(runs)
			directory := filepath.Join(rundirectory.AuthorityRoot, id.String(), "network")
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			evidence := filepath.Join(directory, test.evidence)
			if err := os.WriteFile(evidence, []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chown(evidence, 65534, 65534); err != nil {
				t.Fatal(err)
			}
			if err := live.Close(); err != nil {
				t.Fatal(err)
			}
			recover := func(stale *rundirectory.StaleRun) error {
				return hostnetwork.New().Recover(context.Background(), stale)
			}

			if err := runs.RecoverStale(context.Background(), recover); err == nil {
				t.Fatalf("wrong-owner unpublished claim %q was removed", test.evidence)
			}
			var status unix.Stat_t
			if err := unix.Lstat(evidence, &status); err != nil {
				t.Fatalf("wrong-owner unpublished evidence %q was not preserved: %v", test.evidence, err)
			}
			if status.Uid != 65534 {
				t.Fatalf("wrong-owner unpublished evidence %q changed owner to %d", test.evidence, status.Uid)
			}
			if err := os.Chown(evidence, os.Geteuid(), os.Getegid()); err != nil {
				t.Fatal(err)
			}
			if err := runs.RecoverStale(context.Background(), recover); err != nil {
				t.Fatalf("recover repaired unpublished evidence %q: %v", test.evidence, err)
			}
			fixture.assertSurface(before)
		})
	}
}

func TestCloseRejectsMismatchedNFTTable(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	before := fixture.hostSurface()
	live, id := fixture.createRun(rundirectory.New())
	session, err := hostnetwork.New().Open(context.Background(), live, fixture.resolveEgresses())
	if err != nil {
		t.Fatal(err)
	}
	disableCleanup := fixture.registerSessionCleanup(session)
	table := "transferlanes_" + id.String()
	fixture.run("nft", "add", "chain", "ip", table, "foreign")
	removeForeign := true
	t.Cleanup(func() {
		if removeForeign {
			fixture.runCleanup("nft", "delete", "chain", "ip", table, "foreign")
		}
	})

	if err := session.Close(context.Background()); err == nil {
		t.Fatal("identity mismatch was cleaned instead of failing closed")
	}
	if !strings.Contains(fixture.run("nft", "list", "table", "ip", table), "chain foreign") {
		t.Fatal("failed close changed the mismatched nft table")
	}
	fixture.run("nft", "delete", "chain", "ip", table, "foreign")
	removeForeign = false
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("retry close after identity repair: %v", err)
	}
	disableCleanup()
	if err := live.Complete(context.Background()); err != nil {
		t.Fatal(err)
	}
	fixture.assertSurface(before)
}

func TestHostNetworkAssertionsFollowConfiguredSourceLinks(t *testing.T) {
	for _, test := range []struct {
		name    string
		fixture func(*testing.T) *hostNetworkFixture
	}{
		{name: "one provider", fixture: newSingleProviderFixture},
		{name: "three providers", fixture: newThreeProviderFixture},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := test.fixture(t)
			fixture.install()
			before := fixture.hostSurface()
			runs := rundirectory.New()
			live, id := fixture.createRun(runs)
			namespacesBefore := fixture.run("ip", "netns", "list")
			session, err := hostnetwork.New().Open(context.Background(), live, fixture.resolveEgresses())
			if err != nil {
				t.Fatalf("open host network: %v", err)
			}
			disableCleanup := fixture.registerSessionCleanup(session)
			fixture.assertTraffic(session)
			links := fixture.observeTransferLinks(session)
			fixture.assertKernelRouteLookups(links)
			fixture.assertOwnedConntrackPresent()
			fixture.assertOwnedRulesAndNFT(id, links)
			if namespacesAfter := fixture.run("ip", "netns", "list"); !sameNamedNamespaces(namespacesBefore, namespacesAfter) {
				t.Fatalf("Transfer Lanes created a named namespace\nbefore:\n%s\nafter:\n%s",
					namespacesBefore, namespacesAfter)
			}
			if err := session.Close(context.Background()); err != nil {
				t.Fatalf("close host network: %v", err)
			}
			disableCleanup()
			if err := live.Complete(context.Background()); err != nil {
				t.Fatalf("complete live run: %v", err)
			}
			fixture.assertSurface(before)
		})
	}
}

func newHostNetworkFixture(t *testing.T) *hostNetworkFixture {
	return newHostNetworkFixtureWithSourceLinks(t, []int{0, 0, 1})
}

func newSingleProviderFixture(t *testing.T) *hostNetworkFixture {
	return newHostNetworkFixtureWithSourceLinks(t, []int{0, 0, 0})
}

func newThreeProviderFixture(t *testing.T) *hostNetworkFixture {
	return newHostNetworkFixtureWithSourceLinks(t, []int{0, 1, 2})
}

func newHostNetworkFixtureWithSourceLinks(t *testing.T, sourceLinks []int) *hostNetworkFixture {
	t.Helper()
	requireRoot(t)
	requireRootTools(t)
	sequence := int(labSequence.Add(1) & 0x0fff)
	prefix := fmt.Sprintf("hn%04x", sequence)
	providerCount := slices.Max(sourceLinks) + 1
	providers := make([]string, providerCount)
	peers := make([]string, providerCount)
	gateways := make([]netip.Addr, providerCount)
	tables := make([]int, providerCount)
	for index := range providerCount {
		providers[index] = fmt.Sprintf("%sp%d", prefix, index+1)
		peers[index] = fmt.Sprintf("%sr%d", prefix, index+1)
		gateways[index] = netip.MustParseAddr(fmt.Sprintf("10.%d.1.1", 201+index))
		tables[index] = 31000 + sequence*3 + index
	}
	sources := make([]netip.Addr, len(sourceLinks))
	hosts := make([]int, providerCount)
	for index, provider := range sourceLinks {
		hosts[provider]++
		sources[index] = netip.MustParseAddr(fmt.Sprintf("10.%d.1.%d", 201+provider, hosts[provider]+1))
	}
	return &hostNetworkFixture{t: t, prefix: prefix, router: prefix + "r",
		filterTable: prefix + "f", natTable: prefix + "n",
		providers: providers, peers: peers, sources: sources, sourceLinks: slices.Clone(sourceLinks),
		gateway:    gateways,
		tables:     tables,
		priorities: []int{10000 + sequence*3, 10001 + sequence*3, 10002 + sequence*3},
		remote:     netip.MustParseAddr("203.0.113.200")}
}

func (fixture *hostNetworkFixture) install() {
	fixture.t.Helper()
	originalSurface := fixture.hostSurface()
	fixture.t.Cleanup(func() { fixture.assertSurface(originalSurface) })
	fixture.t.Cleanup(fixture.remove)
	fixture.snapshotSysctls()
	fixture.run("ip", "netns", "add", fixture.router)
	fixture.run("ip", "-n", fixture.router, "link", "set", "lo", "up")
	fixture.run("ip", "-n", fixture.router, "addr", "add", fixture.remote.String()+"/32", "dev", "lo")
	for index := range fixture.providers {
		fixture.installProvider(index)
	}
	fixture.installDefaultReachabilityRoute()
	fixture.installRemoteReachabilityRoute()
	fixture.installForwardDrop()
	fixture.installEarlierForeignSNAT()
	fixture.installSourceRules()
	fixture.configureSysctls()
	fixture.startServer()
}

func (fixture *hostNetworkFixture) installDefaultReachabilityRoute() {
	fixture.t.Helper()
	routes, err := fixture.mainDefaultRoutes()
	if err != nil {
		fixture.t.Fatal(err)
	}
	if len(routes) != 0 {
		return
	}
	provider, err := netlink.LinkByName(fixture.providers[0])
	if err != nil {
		fixture.t.Fatalf("open default-route provider: %v", err)
	}
	route := netlink.Route{LinkIndex: provider.Attrs().Index,
		Gw: net.IP(fixture.gateway[0].AsSlice()), Table: unix.RT_TABLE_MAIN,
		Protocol: unix.RTPROT_STATIC, Scope: netlink.SCOPE_UNIVERSE, Type: unix.RTN_UNICAST}
	if err := netlink.RouteAdd(&route); err != nil {
		fixture.t.Fatalf("add default reachability route: %v", err)
	}
	fixture.defaultRoute = &mainDefaultRouteReceipt{route: route}
	fixture.assertDefaultReachabilityRoute()
}

func (fixture *hostNetworkFixture) installRemoteReachabilityRoute() {
	fixture.t.Helper()
	routes, err := fixture.remoteReachabilityRoutes()
	if err != nil {
		fixture.t.Fatal(err)
	}
	if len(routes) != 0 {
		fixture.t.Fatalf("remote reachability prefix already has %d route(s): %#v", len(routes), routes)
	}
	provider, err := netlink.LinkByName(fixture.providers[0])
	if err != nil {
		fixture.t.Fatalf("open remote-route provider: %v", err)
	}
	route := netlink.Route{LinkIndex: provider.Attrs().Index, Dst: rootPrefixIPNet(netip.PrefixFrom(fixture.remote, 32)),
		Gw: net.IP(fixture.gateway[0].AsSlice()), Table: unix.RT_TABLE_MAIN,
		Protocol: unix.RTPROT_STATIC, Scope: netlink.SCOPE_UNIVERSE, Type: unix.RTN_UNICAST}
	if err := netlink.RouteAdd(&route); err != nil {
		fixture.t.Fatalf("add remote reachability route: %v", err)
	}
	fixture.remoteRoute = &remoteReachabilityRouteReceipt{route: route}
	fixture.assertRemoteReachabilityRoute()
}

func (fixture *hostNetworkFixture) assertDefaultReachabilityRoute() {
	fixture.t.Helper()
	if fixture.defaultRoute == nil {
		fixture.t.Fatal("default reachability route has no successful add receipt")
	}
	routes, err := fixture.mainDefaultRoutes()
	if err != nil || len(routes) != 1 || !sameFixtureRoute(routes[0], fixture.defaultRoute.route) {
		fixture.t.Fatalf("default reachability route identity mismatch: %#v, %v", routes, err)
	}
}

func (fixture *hostNetworkFixture) assertRemoteReachabilityRoute() {
	fixture.t.Helper()
	if fixture.remoteRoute == nil {
		fixture.t.Fatal("remote reachability route has no successful add receipt")
	}
	routes, err := fixture.remoteReachabilityRoutes()
	if err != nil || len(routes) != 1 || !sameFixtureRoute(routes[0], fixture.remoteRoute.route) {
		fixture.t.Fatalf("remote reachability route identity mismatch: %#v, %v", routes, err)
	}
}

func (fixture *hostNetworkFixture) mainDefaultRoutes() ([]netlink.Route, error) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, fmt.Errorf("list main-table default routes: %w", err)
	}
	defaults := make([]netlink.Route, 0, 1)
	for _, route := range routes {
		if route.Dst == nil || route.Dst.String() == "0.0.0.0/0" {
			defaults = append(defaults, route)
		}
	}
	return defaults, nil
}

func (fixture *hostNetworkFixture) remoteReachabilityRoutes() ([]netlink.Route, error) {
	filter := netlink.Route{Table: unix.RT_TABLE_MAIN,
		Dst: rootPrefixIPNet(netip.PrefixFrom(fixture.remote, 32))}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &filter,
		netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
	if err != nil {
		return nil, fmt.Errorf("list remote reachability route: %w", err)
	}
	return routes, nil
}

func rootPrefixIPNet(prefix netip.Prefix) *net.IPNet {
	prefix = prefix.Masked()
	return &net.IPNet{IP: net.IP(prefix.Addr().AsSlice()), Mask: net.CIDRMask(prefix.Bits(), 32)}
}

func sameFixtureRoute(actual, expected netlink.Route) bool {
	return actual.Table == expected.Table && actual.LinkIndex == expected.LinkIndex &&
		actual.Type == unix.RTN_UNICAST && actual.Scope == netlink.SCOPE_UNIVERSE &&
		actual.Protocol == expected.Protocol && sameFixtureDestination(actual.Dst, expected.Dst) &&
		net.IP(actual.Gw).Equal(expected.Gw) && actual.Src == nil && actual.Priority == 0
}

func sameFixtureDestination(left, right *net.IPNet) bool {
	if rootDefaultRoute(left) && rootDefaultRoute(right) {
		return true
	}
	return sameRootIPNet(left, right)
}

func rootDefaultRoute(destination *net.IPNet) bool {
	return destination == nil || destination.String() == "0.0.0.0/0"
}

func (fixture *hostNetworkFixture) installEarlierForeignSNAT() {
	fixture.run("nft", "add", "table", "inet", fixture.natTable)
	fixture.run("nft", "add", "chain", "inet", fixture.natTable, "postrouting",
		"{ type nat hook postrouting priority 99; policy accept; }")
	fixture.run("nft", "add", "rule", "inet", fixture.natTable, "postrouting",
		"oifname", fixture.providers[0], "ip", "saddr", "198.18.0.0/15",
		"snat", "to", fixture.sources[1].String(), "comment", "foreign-earlier-snat")
}

func (fixture *hostNetworkFixture) installForwardDrop() {
	fixture.run("nft", "add", "table", "inet", fixture.filterTable)
	fixture.run("nft", "add", "chain", "inet", fixture.filterTable, "forward",
		"{ type filter hook forward priority 0; policy drop; }")
	fixture.run("nft", "add", "rule", "inet", fixture.filterTable, "forward",
		"iifname", "unrelated0", "accept", "comment", "foreign-forward-rule")
}

func (fixture *hostNetworkFixture) installProvider(index int) {
	provider, peer := fixture.providers[index], fixture.peers[index]
	fixture.run("ip", "link", "add", provider, "type", "veth", "peer", "name", peer)
	fixture.run("ip", "link", "set", peer, "netns", fixture.router)
	fixture.run("ip", "link", "set", provider, "up")
	fixture.run("ip", "-n", fixture.router, "link", "set", peer, "up")
	fixture.run("ip", "-n", fixture.router, "addr", "add", fixture.gateway[index].String()+"/24", "dev", peer)
	for sourceIndex, source := range fixture.sources {
		if fixture.sourceLinks[sourceIndex] == index {
			fixture.run("ip", "addr", "add", source.String()+"/24", "dev", provider)
		}
	}
	fixture.run("ip", "route", "add", "table", strconv.Itoa(fixture.tables[index]), "default",
		"via", fixture.gateway[index].String(), "dev", provider)
}

func (fixture *hostNetworkFixture) installSourceRules() {
	for index, source := range fixture.sources {
		table := fixture.tables[fixture.sourceLinks[index]]
		fixture.run("ip", "rule", "add", "priority", strconv.Itoa(fixture.priorities[index]),
			"from", source.String()+"/32", "table", strconv.Itoa(table))
	}
}

func (fixture *hostNetworkFixture) snapshotSysctls() {
	fixture.sysctls = make(map[string][]byte)
	for _, path := range fixture.sysctlPaths() {
		data, err := os.ReadFile(path)
		if err == nil {
			fixture.sysctls[path] = append([]byte(nil), data...)
		}
	}
}

func (fixture *hostNetworkFixture) configureSysctls() {
	fixture.writeSysctl("/proc/sys/net/ipv4/ip_forward", "1\n")
	fixture.writeSysctl("/proc/sys/net/ipv4/conf/all/rp_filter", "2\n")
	fixture.writeSysctl("/proc/sys/net/ipv4/conf/default/rp_filter", "2\n")
	fixture.writeSysctl("/proc/sys/net/ipv4/conf/all/src_valid_mark", "0\n")
	fixture.writeSysctl("/proc/sys/net/ipv4/conf/default/src_valid_mark", "0\n")
	for _, provider := range fixture.providers {
		fixture.writeSysctl("/proc/sys/net/ipv4/conf/"+provider+"/forwarding", "1\n")
		fixture.writeSysctl("/proc/sys/net/ipv4/conf/"+provider+"/rp_filter", "2\n")
		fixture.writeSysctl("/proc/sys/net/ipv6/conf/"+provider+"/disable_ipv6", "1\n")
	}
}

func (fixture *hostNetworkFixture) resolveEgresses() map[transfernumber.Number]networkcatalog.Egress {
	snapshot, err := networkcatalog.New().Capture(context.Background())
	if err != nil {
		fixture.t.Fatal(err)
	}
	resolved := snapshot.Resolve(fixture.sources)
	result := make(map[transfernumber.Number]networkcatalog.Egress, len(resolved))
	for index, egress := range resolved {
		if !egress.Runnable() {
			fixture.t.Fatalf("source %s is not runnable: %s", fixture.sources[index], egress.Reason())
		}
		id, _ := transfernumber.New(index + 1)
		result[id] = egress
	}
	return result
}

func sameNamedNamespaces(left, right string) bool {
	return strings.Join(namedNamespaces(left), "\x00") == strings.Join(namedNamespaces(right), "\x00")
}

func namedNamespaces(output string) []string {
	var names []string
	for _, raw := range strings.Split(output, "\n") {
		if fields := strings.Fields(raw); len(fields) > 0 {
			names = append(names, fields[0])
		}
	}
	return names
}

func (fixture *hostNetworkFixture) watchRunRootCreations(id runid.ID) func() bool {
	fixture.t.Helper()
	descriptor, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		fixture.t.Fatalf("start run-root publication watch: %v", err)
	}
	if _, err := unix.InotifyAddWatch(descriptor,
		filepath.Join(rundirectory.AuthorityRoot, id.String()), unix.IN_CREATE|unix.IN_MOVED_TO); err != nil {
		_ = unix.Close(descriptor)
		fixture.t.Fatalf("watch run-root publication: %v", err)
	}
	return func() bool {
		defer unix.Close(descriptor)
		buffer := make([]byte, 4096)
		count, err := unix.Read(descriptor, buffer)
		if err != nil && !errors.Is(err, unix.EAGAIN) {
			fixture.t.Fatalf("read run-root publication watch: %v", err)
		}
		return count > 0
	}
}

func (fixture *hostNetworkFixture) registerSessionCleanup(session *hostnetwork.Session) func() {
	fixture.t.Helper()
	enabled := true
	fixture.t.Cleanup(func() {
		if !enabled {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := session.Close(ctx); err != nil {
			fixture.t.Errorf("close successful root-test network session: %v", err)
		}
	})
	return func() { enabled = false }
}

func sameRootIPNet(left, right *net.IPNet) bool {
	return left != nil && right != nil && left.String() == right.String()
}

type observedTransferLink struct {
	hostName string
}

func (fixture *hostNetworkFixture) createRun(owner *rundirectory.Owner) (*rundirectory.LiveRun, runid.ID) {
	id, err := runid.New()
	if err != nil {
		fixture.t.Fatal(err)
	}
	live, err := owner.Create(id)
	if err != nil {
		fixture.t.Fatalf("create run: %v", err)
	}
	return live, id
}

func (fixture *hostNetworkFixture) assertTraffic(session *hostnetwork.Session) {
	for index, expected := range fixture.sources {
		id, _ := transfernumber.New(index + 1)
		lease, err := session.OpenNamespace(id)
		if err != nil {
			fixture.t.Fatal(err)
		}
		observed, err := fetchFromNamespace(lease.Descriptor(), fixture.remote)
		closeErr := lease.Close()
		if err != nil || closeErr != nil {
			fixture.t.Fatalf("transfer %d fetch/close: %v / %v", index+1, err, closeErr)
		}
		if observed != expected.String() {
			fixture.t.Errorf("transfer %d observed source %s, want %s\nrules:\n%s\nnft:\n%s", index+1,
				observed, expected, fixture.run("ip", "-details", "rule", "show"),
				fixture.run("nft", "list", "ruleset"))
		}
	}
}

func (fixture *hostNetworkFixture) observeTransferLinks(
	session *hostnetwork.Session) []observedTransferLink {
	observed := make([]observedTransferLink, len(fixture.sources))
	for index := range observed {
		id, _ := transfernumber.New(index + 1)
		lease, err := session.OpenNamespace(id)
		if err != nil {
			fixture.t.Fatal(err)
		}
		handle, handleErr := netlink.NewHandleAt(netns.NsHandle(lease.Descriptor()))
		if handleErr != nil {
			_ = lease.Close()
			fixture.t.Fatal(handleErr)
		}
		peer, peerErr := handle.LinkByName("eth0")
		handle.Close()
		closeErr := lease.Close()
		if peerErr != nil || closeErr != nil || peer == nil || peer.Attrs() == nil {
			fixture.t.Fatalf("observe transfer %d namespace link: %v / %v", index+1, peerErr, closeErr)
		}
		host, hostErr := netlink.LinkByIndex(peer.Attrs().ParentIndex)
		if hostErr != nil || host == nil || host.Attrs() == nil {
			fixture.t.Fatalf("observe transfer %d host link: %v", index+1, hostErr)
		}
		attributes := host.Attrs()
		if host.Type() != "veth" || len(attributes.Name) != 15 ||
			!strings.HasPrefix(attributes.Name, "w") || len(attributes.HardwareAddr) != 6 ||
			attributes.HardwareAddr[0] != 0x02 || attributes.ParentIndex != peer.Attrs().Index {
			fixture.t.Fatalf("transfer %d owner-marked veth graph is invalid: host=%+v peer=%+v",
				index+1, attributes, peer.Attrs())
		}
		observed[index] = observedTransferLink{hostName: attributes.Name}
	}
	return observed
}

func (fixture *hostNetworkFixture) assertOwnedRulesAndNFT(id runid.ID,
	links []observedTransferLink) {
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		fixture.t.Fatal(err)
	}
	protocol, returnTable := uint8(0), 0
	for number := 1; number <= len(fixture.sources); number++ {
		link := links[number-1]
		var outbound *netlink.Rule
		for index := range rules {
			candidate := &rules[index]
			if candidate.IifName == link.hostName && candidate.Src != nil {
				outbound = candidate
				break
			}
		}
		if outbound == nil || outbound.Protocol == 0 {
			fixture.t.Fatalf("transfer %d has no protocol-tagged outbound rule", number)
		}
		if protocol == 0 {
			protocol = outbound.Protocol
		} else if protocol != outbound.Protocol {
			fixture.t.Fatalf("transfer %d protocol = %d, want %d", number, outbound.Protocol, protocol)
		}
		provider := fixture.providers[fixture.sourceLinks[number-1]]
		var returning *netlink.Rule
		for index := range rules {
			candidate := &rules[index]
			if candidate.IifName == provider && sameRootIPNet(candidate.Dst, outbound.Src) {
				returning = candidate
				break
			}
		}
		if returning == nil || returning.Protocol != protocol {
			fixture.t.Fatalf("transfer %d has no matching protocol-tagged return rule", number)
		}
		if returnTable == 0 {
			returnTable = returning.Table
		} else if returnTable != returning.Table {
			fixture.t.Fatalf("transfer %d return table = %d, want %d", number, returning.Table, returnTable)
		}
		routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
			&netlink.Route{Table: returnTable, Dst: outbound.Src}, netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
		if err != nil || len(routes) != 1 || uint8(routes[0].Protocol) != protocol {
			fixture.t.Fatalf("transfer %d return routes = %#v, error=%v", number, routes, err)
		}
	}
	if text := fixture.run("ip", "-details", "rule", "show"); strings.Contains(text, "fwmark") {
		for _, link := range links {
			if strings.Contains(text, "iif "+link.hostName) {
				fixture.t.Fatalf("owned rule uses fwmark:\n%s", text)
			}
		}
	}
	table := "transferlanes_" + id.String()
	nft := fixture.run("nft", "list", "table", "ip", table)
	for _, source := range fixture.sources {
		if !strings.Contains(nft, "snat to "+source.String()) {
			fixture.t.Fatalf("owned nft program lacks SNAT to %s:\n%s", source, nft)
		}
	}
	if !strings.Contains(nft, "chain forward") || !strings.Contains(nft, "chain postrouting") ||
		strings.Contains(nft, "meta mark") {
		fixture.t.Fatalf("unexpected owned nft rules:\n%s", nft)
	}
}

func (fixture *hostNetworkFixture) assertKernelRouteLookups(links []observedTransferLink) {
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		fixture.t.Fatal(err)
	}
	for number := 1; number <= len(fixture.sources); number++ {
		link := links[number-1]
		var source net.IP
		var prefix *net.IPNet
		for _, rule := range rules {
			if rule.IifName == link.hostName && rule.Src != nil {
				source = append(net.IP(nil), rule.Src.IP.To4()...)
				prefix = rule.Src
				break
			}
		}
		if source == nil {
			fixture.t.Fatalf("transfer %d has no observable input rule", number)
		}
		source[3] += 2
		providerIndex := fixture.sourceLinks[number-1]
		lookup := fixture.run("ip", "route", "get", "1.1.1.1", "from", source.String(), "iif", link.hostName)
		for _, fragment := range []string{"dev " + fixture.providers[providerIndex],
			"table " + strconv.Itoa(fixture.tables[providerIndex]), "via " + fixture.gateway[providerIndex].String()} {
			if !strings.Contains(lookup, fragment) {
				fixture.t.Fatalf("transfer %d route lookup lacks %q: %s", number, fragment, lookup)
			}
		}
		if main := strings.TrimSpace(fixture.run("ip", "route", "show", "table", "main",
			"exact", prefix.String())); main != "" {
			fixture.t.Fatalf("transfer %d temporary prefix leaked into main table: %s", number, main)
		}
		returnTable := 0
		for _, rule := range rules {
			if rule.IifName == fixture.providers[providerIndex] && sameRootIPNet(rule.Dst, prefix) {
				returnTable = rule.Table
				break
			}
		}
		if returnTable == 0 {
			fixture.t.Fatalf("transfer %d has no observable return rule", number)
		}
		returnLookup := fixture.run("ip", "route", "get", source.String(), "from", "1.1.1.1",
			"iif", fixture.providers[providerIndex])
		for _, fragment := range []string{"dev " + link.hostName, "table " + strconv.Itoa(returnTable)} {
			if !strings.Contains(returnLookup, fragment) {
				fixture.t.Fatalf("transfer %d return route lookup lacks %q: %s", number, fragment, returnLookup)
			}
		}
	}
}

func (fixture *hostNetworkFixture) assertOwnedConntrackPresent() {
	flows, err := netlink.ConntrackTableList(netlink.ConntrackTable, netlink.FAMILY_V4)
	if err != nil {
		fixture.t.Fatalf("list active conntrack entries: %v", err)
	}
	found := make(map[netip.Addr]struct{})
	temporary := netip.MustParsePrefix("198.18.0.0/15")
	for _, flow := range flows {
		if source, valid := netip.AddrFromSlice(flow.Forward.SrcIP); valid {
			source = source.Unmap()
			if temporary.Contains(source) {
				found[source] = struct{}{}
			}
		}
	}
	if len(found) < len(fixture.sources) {
		fixture.t.Fatalf("real traffic created only %d temporary-source conntrack entries: %v",
			len(found), found)
	}
}

func fetchFromNamespace(descriptor uintptr, destination netip.Addr) (string, error) {
	connection, err := openNamespaceTCP(descriptor, destination, 18081)
	if err != nil {
		return "", err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(connection, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", destination); err != nil {
		return "", err
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return strings.TrimSpace(string(body)), err
}

func openNamespaceTCP(descriptor uintptr, destination netip.Addr, port int) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan fetchResult, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		original, err := netns.Get()
		if err != nil {
			result <- fetchResult{err: err}
			return
		}
		defer original.Close()
		if err := netns.Set(netns.NsHandle(descriptor)); err != nil {
			result <- fetchResult{err: err}
			return
		}
		fd, socketErr := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if socketErr == nil {
			address := destination.As4()
			socketErr = connectRootTestSocket(ctx, fd,
				&unix.SockaddrInet4{Port: port, Addr: address})
		}
		restoreErr := netns.Set(original)
		if socketErr != nil || restoreErr != nil {
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			result <- fetchResult{err: errors.Join(socketErr, restoreErr)}
			return
		}
		result <- fetchResult{fd: fd}
	}()
	value := <-result
	if value.err != nil {
		return nil, value.err
	}
	file := os.NewFile(uintptr(value.fd), "namespace-tcp")
	connection, err := net.FileConn(file)
	closeErr := file.Close()
	return connection, errors.Join(err, closeErr)
}

func connectRootTestSocket(ctx context.Context, descriptor int, address unix.Sockaddr) error {
	if err := unix.SetNonblock(descriptor, true); err != nil {
		return err
	}
	if err := unix.Connect(descriptor, address); err != nil && !errors.Is(err, unix.EINPROGRESS) {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := unix.Poll([]unix.PollFd{{Fd: int32(descriptor), Events: unix.POLLOUT}}, 50)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if ready == 0 {
			continue
		}
		connectionError, err := unix.GetsockoptInt(descriptor, unix.SOL_SOCKET, unix.SO_ERROR)
		if err != nil {
			return err
		}
		if connectionError != 0 {
			return unix.Errno(connectionError)
		}
		return unix.SetNonblock(descriptor, false)
	}
}

type fetchResult struct {
	fd  int
	err error
}

func (fixture *hostNetworkFixture) startServer() {
	listenerResult := make(chan struct {
		listener net.Listener
		err      error
	}, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		original, err := netns.Get()
		if err != nil {
			listenerResult <- struct {
				listener net.Listener
				err      error
			}{err: err}
			return
		}
		defer original.Close()
		router, err := netns.GetFromName(fixture.router)
		if err == nil {
			defer router.Close()
			err = netns.Set(router)
		}
		var listener net.Listener
		if err == nil {
			listener, err = net.Listen("tcp4", net.JoinHostPort(fixture.remote.String(), "18081"))
		}
		restoreErr := netns.Set(original)
		listenerResult <- struct {
			listener net.Listener
			err      error
		}{listener: listener, err: errors.Join(err, restoreErr)}
	}()
	value := <-listenerResult
	if value.err != nil {
		fixture.t.Fatalf("start namespace receiver: %v", value.err)
	}
	fixture.observed = make(chan netip.Addr, 1024)
	fixture.measured = make(chan sourceObservation, 1024)
	measureLimiters := make(map[netip.Addr]*rootPacedUpload, len(fixture.sources))
	for _, source := range fixture.sources {
		measureLimiters[source] = &rootPacedUpload{bitsPerSecond: 100_000_000}
	}
	fixture.server = &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		host, _, _ := net.SplitHostPort(request.RemoteAddr)
		address, addressErr := netip.ParseAddr(host)
		if addressErr == nil {
			fixture.observed <- address
			if request.URL.Path == "/measure" {
				observeSourceTime(fixture.measured, address, false)
				defer observeSourceTime(fixture.measured, address, true)
			}
		}
		if request.URL.Path == "/measure" {
			time.Sleep(20 * time.Millisecond)
			if limiter := measureLimiters[address]; limiter != nil {
				if err := limiter.consume(request.Context(), request.Body); err != nil {
					return
				}
			} else {
				_, _ = io.Copy(io.Discard, request.Body)
			}
		} else {
			_, _ = io.Copy(io.Discard, request.Body)
		}
		if request.URL.Path == "/measure-fail" {
			http.Error(writer, "controlled measure failure", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(writer, host)
	})}
	fixture.serverDone = make(chan error, 1)
	go func() { fixture.serverDone <- fixture.server.Serve(value.listener) }()
}

func observeSourceTime(observed chan<- sourceObservation, source netip.Addr, finished bool) {
	observed <- sourceObservation{source: source, at: time.Now(), finished: finished}
}

func (fixture *hostNetworkFixture) hostSurface() string {
	result, err := fixture.captureHostSurface()
	if err != nil {
		fixture.t.Fatal(err)
	}
	return result
}

func (fixture *hostNetworkFixture) captureHostSurface() (string, error) {
	commands := [][]string{{"ip", "-j", "-details", "link", "show"},
		{"ip", "-j", "-details", "address", "show"}, {"ip", "-j", "-details", "rule", "show"},
		{"ip", "-j", "-details", "route", "show", "table", "all"}, {"nft", "-j", "list", "ruleset"}}
	var result strings.Builder
	for _, command := range commands {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		bytes, err := exec.CommandContext(ctx, command[0], command[1:]...).CombinedOutput()
		cancel()
		if err != nil {
			return "", fmt.Errorf("capture host surface with %s %q: %w: %s",
				command[0], command[1:], err, bytes)
		}
		output := string(bytes)
		if command[0] == "nft" {
			output = normalizeFirewallCounters(output)
		}
		result.WriteString(output)
		result.WriteByte('\n')
	}
	for _, path := range fixture.sysctlPaths() {
		data, err := os.ReadFile(path)
		if err == nil {
			result.WriteString(path + "=" + string(data))
		}
	}
	return result.String(), nil
}

func (fixture *hostNetworkFixture) assertSurface(before string) {
	if err := fixture.checkHostSurface(before); err != nil {
		fixture.t.Fatal(err)
	}
	if err := checkTemporaryConntrackAbsent(); err != nil {
		fixture.t.Fatal(err)
	}
}

func (fixture *hostNetworkFixture) checkHostSurface(before string) error {
	after, err := fixture.captureHostSurface()
	if err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("host network did not return to exact fixture surface\nbefore:\n%s\nafter:\n%s",
			before, after)
	}
	return nil
}

func checkTemporaryConntrackAbsent() error {
	flows, err := netlink.ConntrackTableList(netlink.ConntrackTable, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list conntrack residue: %w", err)
	}
	temporary := netip.MustParsePrefix("198.18.0.0/15")
	for _, flow := range flows {
		address, valid := netip.AddrFromSlice(flow.Forward.SrcIP)
		if valid && temporary.Contains(address.Unmap()) {
			return fmt.Errorf("owned conntrack residue remains: %s", flow)
		}
	}
	return nil
}

func (fixture *hostNetworkFixture) sysctlPaths() []string {
	paths := []string{"/proc/sys/net/ipv4/ip_forward", "/proc/sys/net/ipv4/conf/all/rp_filter",
		"/proc/sys/net/ipv4/conf/default/rp_filter", "/proc/sys/net/ipv4/conf/all/src_valid_mark",
		"/proc/sys/net/ipv4/conf/default/src_valid_mark"}
	for _, provider := range fixture.providers {
		paths = append(paths, "/proc/sys/net/ipv4/conf/"+provider+"/forwarding",
			"/proc/sys/net/ipv4/conf/"+provider+"/rp_filter",
			"/proc/sys/net/ipv6/conf/"+provider+"/disable_ipv6")
	}
	return paths
}

func (fixture *hostNetworkFixture) writeSysctl(path, value string) {
	if err := os.WriteFile(path, []byte(value), 0); err != nil {
		fixture.t.Fatalf("write %s: %v", path, err)
	}
}

func (fixture *hostNetworkFixture) remove() {
	if fixture.server != nil {
		_ = fixture.server.Close()
		if err := <-fixture.serverDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
			fixture.t.Errorf("join namespace receiver: %v", err)
		}
		fixture.server, fixture.serverDone = nil, nil
	}
	fixture.runCleanup("nft", "delete", "table", "inet", fixture.filterTable)
	fixture.runCleanup("nft", "delete", "table", "inet", fixture.natTable)
	for index := len(fixture.priorities) - 1; index >= 0; index-- {
		fixture.runCleanup("ip", "rule", "del", "priority", strconv.Itoa(fixture.priorities[index]))
	}
	for _, table := range fixture.tables {
		fixture.runCleanup("ip", "route", "flush", "table", strconv.Itoa(table))
	}
	fixture.removeRemoteReachabilityRoute()
	fixture.removeDefaultReachabilityRoute()
	for _, provider := range fixture.providers {
		fixture.runCleanup("ip", "link", "del", provider)
	}
	fixture.runCleanup("ip", "netns", "del", fixture.router)
	for path, value := range fixture.sysctls {
		_ = os.WriteFile(path, value, 0)
	}
}

func (fixture *hostNetworkFixture) removeRemoteReachabilityRoute() {
	if fixture.remoteRoute == nil {
		return
	}
	routes, err := fixture.remoteReachabilityRoutes()
	if err != nil {
		fixture.t.Errorf("cleanup remote reachability route: %v", err)
		return
	}
	if len(routes) != 1 || !sameFixtureRoute(routes[0], fixture.remoteRoute.route) {
		fixture.t.Errorf("preserve changed remote reachability route: %#v", routes)
		return
	}
	if err := netlink.RouteDel(&routes[0]); err != nil {
		fixture.t.Errorf("delete remote reachability route: %v", err)
		return
	}
	remaining, err := fixture.remoteReachabilityRoutes()
	if err != nil || len(remaining) != 0 {
		fixture.t.Errorf("remote reachability route remains: %#v, %v", remaining, err)
		return
	}
	fixture.remoteRoute = nil
}

func (fixture *hostNetworkFixture) removeDefaultReachabilityRoute() {
	if fixture.defaultRoute == nil {
		return
	}
	routes, err := fixture.mainDefaultRoutes()
	if err != nil {
		fixture.t.Errorf("cleanup default reachability route: %v", err)
		return
	}
	if len(routes) != 1 || !sameFixtureRoute(routes[0], fixture.defaultRoute.route) {
		fixture.t.Errorf("preserve changed default reachability route: %#v", routes)
		return
	}
	if err := netlink.RouteDel(&routes[0]); err != nil {
		fixture.t.Errorf("delete default reachability route: %v", err)
		return
	}
	remaining, err := fixture.mainDefaultRoutes()
	if err != nil || len(remaining) != 0 {
		fixture.t.Errorf("default reachability route remains: %#v, %v", remaining, err)
		return
	}
	fixture.defaultRoute = nil
}

func (fixture *hostNetworkFixture) run(name string, arguments ...string) string {
	fixture.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, arguments...).CombinedOutput()
	if err != nil {
		fixture.t.Fatalf("run %s %q: %v: %s", name, arguments, err, output)
	}
	return string(output)
}

func (fixture *hostNetworkFixture) runCleanup(name string, arguments ...string) {
	fixture.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, arguments...).CombinedOutput()
	if err != nil && !cleanupOutputIsAbsent(string(output)) {
		fixture.t.Errorf("cleanup %s %q: %v: %s", name, arguments, err, output)
	}
}
