//go:build linux && rootintegration

package root_test

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/networkcatalog"
	"github.com/vishvananda/netlink"
)

type catalogRootFixture struct {
	t            *testing.T
	firstLink    string
	secondLink   string
	ruleOnlyLink string
	firstTable   int
	secondTable  int
	priorities   []int
}

func TestRootIntegrationNetworkCatalogReadOnlyEgresses(t *testing.T) {
	requireRoot(t)
	requireRootTools(t)
	fixture := newCatalogRootFixture(t)
	fixture.install()
	before := fixture.mutationSurface()

	snapshot, err := networkcatalog.New().Capture(context.Background())
	if err != nil {
		t.Fatalf("capture network catalog: %v", err)
	}
	if !catalogContainsInterface(snapshot.Interfaces(), fixture.ruleOnlyLink) {
		t.Fatalf("capture omitted rule-only input interface %s", fixture.ruleOnlyLink)
	}
	resolved := snapshot.Resolve([]netip.Addr{
		netip.MustParseAddr("192.0.2.10"),
		netip.MustParseAddr("192.0.2.11"),
		netip.MustParseAddr("198.51.100.10"),
	})
	fixture.assertEgresses(resolved)

	after := fixture.mutationSurface()
	if before != after {
		t.Fatalf("read-only capture changed host network\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestRootIntegrationNetworkCatalogKeepsStableEgressDuringUnrelatedLinkChurn(t *testing.T) {
	requireRoot(t)
	requireRootTools(t)
	fixture := newCatalogRootFixture(t)
	fixture.install()

	churnName := fixture.firstLink + "x"
	t.Cleanup(func() {
		if link, err := netlink.LinkByName(churnName); err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	stop := make(chan struct{})
	ready := make(chan struct{})
	churnErr := make(chan error, 1)
	go churnUnrelatedAddressedLink(churnName, stop, ready, churnErr)
	select {
	case <-ready:
	case err := <-churnErr:
		t.Fatalf("start unrelated link churn: %v", err)
	}
	for attempt := 0; attempt < 100; attempt++ {
		snapshot, err := networkcatalog.New().Capture(context.Background())
		if err != nil {
			close(stop)
			<-churnErr
			t.Fatalf("capture stable egress during unrelated link churn: %v", err)
		}
		resolved := snapshot.Resolve([]netip.Addr{netip.MustParseAddr("192.0.2.10")})
		if len(resolved) != 1 || !resolved[0].Runnable() ||
			resolved[0].Interface().Name() != fixture.firstLink {
			close(stop)
			<-churnErr
			t.Fatalf("stable egress during unrelated link churn = %#v", resolved)
		}
	}
	close(stop)
	if err := <-churnErr; err != nil {
		t.Fatal(err)
	}
}

func churnUnrelatedAddressedLink(name string, stop <-chan struct{}, ready chan<- struct{}, result chan<- error) {
	announced := false
	var churnErr error
	defer func() { result <- churnErr }()
	for {
		select {
		case <-stop:
			return
		default:
		}
		link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: name}}
		if err := netlink.LinkAdd(link); err != nil {
			churnErr = fmt.Errorf("add churn link: %w", err)
			return
		}
		address := &netlink.Addr{IPNet: &net.IPNet{IP: net.ParseIP("198.18.0.1"),
			Mask: net.CIDRMask(24, 32)}}
		if err := netlink.AddrAdd(link, address); err != nil {
			_ = netlink.LinkDel(link)
			churnErr = fmt.Errorf("address churn link: %w", err)
			return
		}
		if !announced {
			close(ready)
			announced = true
		}
		time.Sleep(100 * time.Microsecond)
		if err := netlink.LinkDel(link); err != nil {
			churnErr = fmt.Errorf("delete churn link: %w", err)
			return
		}
		time.Sleep(100 * time.Microsecond)
	}
}

func newCatalogRootFixture(t *testing.T) *catalogRootFixture {
	t.Helper()
	sequence := int(labSequence.Add(1) & 0x0fff)
	prefix := fmt.Sprintf("nwc%03x", sequence)
	fixture := &catalogRootFixture{t: t, firstLink: prefix + "a", secondLink: prefix + "b",
		ruleOnlyLink: prefix + "r",
		firstTable:   30000 + sequence*2, secondTable: 30001 + sequence*2}
	fixture.priorities = []int{20000 + sequence*4, 20001 + sequence*4,
		20002 + sequence*4, 20003 + sequence*4}
	return fixture
}

func (fixture *catalogRootFixture) install() {
	fixture.t.Helper()
	fixture.t.Cleanup(fixture.remove)
	fixture.run("ip", "link", "add", fixture.firstLink, "type", "dummy")
	fixture.run("ip", "link", "add", fixture.secondLink, "type", "dummy")
	fixture.run("ip", "link", "add", fixture.ruleOnlyLink, "type", "dummy")
	fixture.run("ip", "addr", "add", "192.0.2.10/24", "dev", fixture.firstLink)
	fixture.run("ip", "addr", "add", "192.0.2.11/24", "dev", fixture.firstLink)
	fixture.run("ip", "addr", "add", "198.51.100.10/24", "dev", fixture.secondLink)
	fixture.run("ip", "link", "set", fixture.firstLink, "up")
	fixture.run("ip", "link", "set", fixture.secondLink, "up")
	fixture.run("ip", "link", "set", fixture.ruleOnlyLink, "up")
	fixture.addDefault(fixture.firstTable, fixture.firstLink, "192.0.2.1")
	fixture.addDefault(fixture.secondTable, fixture.secondLink, "198.51.100.1")
	fixture.addReturnRule(fixture.priorities[0])
	fixture.addRule(fixture.priorities[1], "192.0.2.10", fixture.firstTable)
	fixture.addRule(fixture.priorities[2], "192.0.2.11", fixture.firstTable)
	fixture.addRule(fixture.priorities[3], "198.51.100.10", fixture.secondTable)
}

func (fixture *catalogRootFixture) addDefault(table int, link, gateway string) {
	fixture.run("ip", "route", "add", "table", strconv.Itoa(table), "default",
		"via", gateway, "dev", link, "onlink")
}

func (fixture *catalogRootFixture) addRule(priority int, source string, table int) {
	fixture.run("ip", "rule", "add", "priority", strconv.Itoa(priority),
		"from", source+"/32", "table", strconv.Itoa(table))
}

func (fixture *catalogRootFixture) addReturnRule(priority int) {
	fixture.run("ip", "rule", "add", "priority", strconv.Itoa(priority), "iif", fixture.ruleOnlyLink,
		"to", "198.18.0.0/30", "table", strconv.Itoa(fixture.firstTable))
}

func catalogContainsInterface(values []networkcatalog.Interface, name string) bool {
	for _, value := range values {
		if value.Name() == name {
			return true
		}
	}
	return false
}

func (fixture *catalogRootFixture) assertEgresses(values []networkcatalog.Egress) {
	fixture.t.Helper()
	want := []struct {
		ip, link, gateway string
		table             int
	}{
		{ip: "192.0.2.10", link: fixture.firstLink, gateway: "192.0.2.1", table: fixture.firstTable},
		{ip: "192.0.2.11", link: fixture.firstLink, gateway: "192.0.2.1", table: fixture.firstTable},
		{ip: "198.51.100.10", link: fixture.secondLink, gateway: "198.51.100.1", table: fixture.secondTable},
	}
	if len(values) != len(want) {
		fixture.t.Fatalf("resolved %d egresses, want %d", len(values), len(want))
	}
	for index, expected := range want {
		gateway, present := values[index].Gateway()
		if !values[index].Runnable() || values[index].LocalIP().String() != expected.ip ||
			values[index].Interface().Name() != expected.link || values[index].Table() != expected.table ||
			!present || gateway.String() != expected.gateway {
			fixture.t.Errorf("egress[%d] = %#v gateway=%s/%t, want %+v", index, values[index], gateway, present, expected)
		}
	}
}

func (fixture *catalogRootFixture) mutationSurface() string {
	fixture.t.Helper()
	commands := [][]string{
		{"ip", "-j", "-details", "link", "show"},
		{"ip", "-j", "-details", "address", "show"},
		{"ip", "-j", "-details", "rule", "show"},
		{"ip", "-j", "-details", "route", "show", "table", "all"},
		{"nft", "-j", "list", "ruleset"},
		{"iptables-save"},
	}
	var result strings.Builder
	for _, command := range commands {
		result.WriteString(strings.Join(command, " "))
		result.WriteByte('\n')
		result.WriteString(fixture.run(command[0], command[1:]...))
		result.WriteByte('\n')
	}
	for _, path := range catalogSysctlPaths() {
		value, err := os.ReadFile(path)
		if err != nil {
			fixture.t.Fatalf("read sysctl %s: %v", path, err)
		}
		result.WriteString(path)
		result.WriteByte('=')
		result.Write(value)
	}
	return result.String()
}

func catalogSysctlPaths() []string {
	return []string{
		"/proc/sys/net/ipv4/ip_forward",
		"/proc/sys/net/ipv4/conf/all/rp_filter",
		"/proc/sys/net/ipv4/conf/default/rp_filter",
		"/proc/sys/net/ipv4/conf/all/src_valid_mark",
		"/proc/sys/net/ipv4/conf/default/src_valid_mark",
	}
}

func (fixture *catalogRootFixture) remove() {
	for index := len(fixture.priorities) - 1; index >= 0; index-- {
		fixture.runCleanup("ip", "rule", "del", "priority", strconv.Itoa(fixture.priorities[index]))
	}
	fixture.runCleanup("ip", "route", "flush", "table", strconv.Itoa(fixture.secondTable))
	fixture.runCleanup("ip", "route", "flush", "table", strconv.Itoa(fixture.firstTable))
	fixture.runCleanup("ip", "link", "del", fixture.ruleOnlyLink)
	fixture.runCleanup("ip", "link", "del", fixture.secondLink)
	fixture.runCleanup("ip", "link", "del", fixture.firstLink)
}

func (fixture *catalogRootFixture) run(name string, arguments ...string) string {
	fixture.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, arguments...).CombinedOutput()
	if err != nil {
		fixture.t.Fatalf("run %s %q: %v: %s", name, arguments, err, strings.TrimSpace(string(output)))
	}
	return string(output)
}

func (fixture *catalogRootFixture) runCleanup(name string, arguments ...string) {
	fixture.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, arguments...).CombinedOutput()
	if err != nil && !cleanupOutputIsAbsent(string(output)) {
		fixture.t.Errorf("cleanup %s %q: %v: %s", name, arguments, err, strings.TrimSpace(string(output)))
	}
}
