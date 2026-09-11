//go:build linux && rootintegration

package root_test

import (
	"crypto/sha256"
	"fmt"
	"io/fs"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type hostStateSnapshot struct {
	network             string
	resolver            string
	namedNetNS          string
	legacyIPv4          string
	legacyIPv6          string
	authority           string
	transferlanesMounts string
	markedProcesses     string
	temporaryFlows      string
}

func captureHostState(t *testing.T, fixture *hostNetworkFixture, marker string) hostStateSnapshot {
	t.Helper()
	return hostStateSnapshot{
		network:             fixture.hostSurface(),
		resolver:            resolverIdentity(t),
		namedNetNS:          fixture.run("ip", "netns", "list"),
		legacyIPv4:          normalizeFirewallCounters(fixture.run("iptables-save", "-c")),
		legacyIPv6:          normalizeFirewallCounters(fixture.run("ip6tables-save", "-c")),
		authority:           pathTree(t, rundirectory.AuthorityRoot),
		transferlanesMounts: transferlanesMounts(t),
		markedProcesses:     markedProcessSnapshot(t, marker),
		temporaryFlows:      temporaryConntrack(t),
	}
}

func assertHostState(t *testing.T, fixture *hostNetworkFixture, marker string,
	want hostStateSnapshot,
) {
	t.Helper()
	got := captureHostState(t, fixture, marker)
	for _, field := range []struct {
		name      string
		want, got string
	}{
		{name: "network", want: want.network, got: got.network},
		{name: "resolver", want: want.resolver, got: got.resolver},
		{name: "named network namespaces", want: want.namedNetNS, got: got.namedNetNS},
		{name: "legacy IPv4 firewall", want: want.legacyIPv4, got: got.legacyIPv4},
		{name: "legacy IPv6 firewall", want: want.legacyIPv6, got: got.legacyIPv6},
		{name: "run authority", want: want.authority, got: got.authority},
		{name: "Transfer Lanes mounts", want: want.transferlanesMounts, got: got.transferlanesMounts},
		{name: "marked processes", want: want.markedProcesses, got: got.markedProcesses},
		{name: "temporary conntrack", want: want.temporaryFlows, got: got.temporaryFlows},
	} {
		if field.got != field.want {
			t.Errorf("host %s changed across public acceptance\nwant:\n%s\ngot:\n%s",
				field.name, field.want, field.got)
		}
	}
}

func resolverIdentity(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	var entry, target unix.Stat_t
	if err := unix.Lstat("/etc/resolv.conf", &entry); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat("/etc/resolv.conf", &target); err != nil {
		t.Fatal(err)
	}
	link := ""
	if entry.Mode&unix.S_IFMT == unix.S_IFLNK {
		link, err = os.Readlink("/etc/resolv.conf")
		if err != nil {
			t.Fatal(err)
		}
	}
	return fmt.Sprintf("entry=%d:%d:%#o target=%d:%d:%#o:%d:%d link=%q sha256=%x",
		entry.Dev, entry.Ino, entry.Mode, target.Dev, target.Ino, target.Mode, target.Uid, target.Gid,
		link, sha256.Sum256(content))
}

var (
	legacyCounterPattern = regexp.MustCompile(`\[[0-9]+:[0-9]+\]`)
	nftCounterPattern    = regexp.MustCompile(`"(packets|bytes)":[0-9]+`)
)

func normalizeFirewallCounters(value string) string {
	value = legacyCounterPattern.ReplaceAllString(value, "[#:#]")
	return nftCounterPattern.ReplaceAllString(value, `"$1":#`)
}

func pathTree(t *testing.T, root string) string {
	t.Helper()
	var entries []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if os.IsNotExist(walkErr) {
			return fs.SkipDir
		}
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." {
			return nil
		}
		value := fmt.Sprintf("%s mode=%s", filepath.ToSlash(relative), info.Mode())
		if info.Mode().IsRegular() {
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			value += fmt.Sprintf(" size=%d sha256=%x", info.Size(), sha256.Sum256(content))
		} else if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			value += " target=" + target
		}
		entries = append(entries, value)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	slices.Sort(entries)
	return strings.Join(entries, "\n")
}

func transferlanesMounts(t *testing.T) string {
	t.Helper()
	content, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	var mounts []string
	for _, line := range strings.Split(string(content), "\n") {
		if strings.Contains(line, rundirectory.AuthorityRoot) || strings.Contains(line, "/run/transferlanes") {
			mounts = append(mounts, line)
		}
	}
	return strings.Join(mounts, "\n")
}

func markedProcessSnapshot(t *testing.T, marker string) string {
	t.Helper()
	var processes []string
	for _, process := range markedProcesses(t, marker) {
		namespace, _ := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", process.pid))
		processes = append(processes, fmt.Sprintf("%d %s %q", process.pid, namespace, process.command))
	}
	slices.Sort(processes)
	return strings.Join(processes, "\n")
}

func temporaryConntrack(t *testing.T) string {
	t.Helper()
	flows, err := netlink.ConntrackTableList(netlink.ConntrackTable, netlink.FAMILY_V4)
	if err != nil {
		t.Fatal(err)
	}
	temporary := netip.MustParsePrefix("198.18.0.0/15")
	var result []string
	for _, flow := range flows {
		if conntrackContainsPrefix(flow, temporary) {
			result = append(result, fmt.Sprintf("%s:%d>%s:%d proto=%d",
				flow.Forward.SrcIP, flow.Forward.SrcPort, flow.Forward.DstIP,
				flow.Forward.DstPort, flow.Forward.Protocol))
		}
	}
	slices.Sort(result)
	return strings.Join(result, "\n")
}

func conntrackContainsPrefix(flow *netlink.ConntrackFlow, prefix netip.Prefix) bool {
	for _, value := range []net.IP{
		flow.Forward.SrcIP, flow.Forward.DstIP, flow.Reverse.SrcIP, flow.Reverse.DstIP,
	} {
		address, valid := netip.AddrFromSlice(value)
		if valid && prefix.Contains(address.Unmap()) {
			return true
		}
	}
	return false
}
