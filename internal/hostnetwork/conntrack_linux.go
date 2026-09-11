//go:build linux

package hostnetwork

import (
	"fmt"
	"net"
	"net/netip"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type netlinkConntrack struct{}

func (netlinkConntrack) ObserveOriginalSources() ([]netip.Addr, error) {
	if err := requireNetAdminCapability(); err != nil {
		return nil, err
	}
	flows, err := netlink.ConntrackTableList(netlink.ConntrackTable, netlink.FAMILY_V4)
	if err != nil {
		return nil, fmt.Errorf("list IPv4 conntrack entries: %w", err)
	}
	result := make([]netip.Addr, 0, len(flows))
	for _, flow := range flows {
		if flow == nil {
			return nil, fmt.Errorf("conntrack returned an entry without identity")
		}
		if source, valid := netip.AddrFromSlice(flow.Forward.SrcIP); valid && source.Unmap().Is4() {
			result = append(result, source.Unmap())
		}
	}
	return result, nil
}

func (netlinkConntrack) DeleteOriginalSources(sources []netip.Addr) error {
	filters := make([]netlink.CustomConntrackFilter, 0, len(sources))
	for _, source := range sources {
		if !source.Is4() {
			return fmt.Errorf("build conntrack filter for non-IPv4 source %s", source)
		}
		filter := &netlink.ConntrackFilter{}
		if err := filter.AddIP(netlink.ConntrackOrigSrcIP, net.IP(source.AsSlice())); err != nil {
			return fmt.Errorf("build conntrack filter for %s: %w", source, err)
		}
		filters = append(filters, filter)
	}
	if _, err := netlink.ConntrackDeleteFilters(netlink.ConntrackTable, netlink.FAMILY_V4, filters...); err != nil {
		return err
	}
	return nil
}

func requireNetAdminCapability() error {
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	data := [2]unix.CapUserData{}
	if err := unix.Capget(&header, &data[0]); err != nil {
		return fmt.Errorf("read process capabilities: %w", err)
	}
	word := unix.CAP_NET_ADMIN / 32
	bit := uint(unix.CAP_NET_ADMIN % 32)
	if data[word].Effective&(uint32(1)<<bit) == 0 {
		return fmt.Errorf("CAP_NET_ADMIN is required for exact conntrack cleanup")
	}
	return nil
}
