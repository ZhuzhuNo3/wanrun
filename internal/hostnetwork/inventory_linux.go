//go:build linux

package hostnetwork

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func inspectHostInventory(ctx context.Context) (hostInventory, error) {
	inventory := emptyHostInventory()
	links, err := netlink.LinkList()
	if err != nil {
		return inventory, fmt.Errorf("list host links: %w", err)
	}
	if err := inventoryLinks(ctx, &inventory, links); err != nil {
		return inventory, err
	}
	if err := inventoryRoutesAndRules(&inventory); err != nil {
		return inventory, err
	}
	return inventory, nil
}

func emptyHostInventory() hostInventory {
	return hostInventory{linkNames: map[string]struct{}{},
		priorities: map[int]struct{}{}, usedRouteTables: map[int]struct{}{},
		localAddresses: map[netip.Addr]struct{}{}, usedProtocols: map[uint8]struct{}{},
		conntrackOriginalSources: map[netip.Addr]struct{}{}}
}

func inventoryLinks(ctx context.Context, inventory *hostInventory, links []netlink.Link) error {
	for _, link := range links {
		if err := ctx.Err(); err != nil {
			return err
		}
		attributes := link.Attrs()
		if attributes == nil || attributes.Name == "" {
			return errors.New("host link has no stable identity")
		}
		inventory.linkNames[attributes.Name] = struct{}{}
		addresses, err := netlink.AddrList(link, netlink.FAMILY_V4)
		if err != nil {
			return fmt.Errorf("list addresses on %s: %w", attributes.Name, err)
		}
		for _, address := range addresses {
			if local, valid := netip.AddrFromSlice(address.IP); valid && local.Unmap().Is4() {
				inventory.localAddresses[local.Unmap()] = struct{}{}
			}
			if prefix, valid := prefixFromIPNet(address.IPNet); valid {
				inventory.routePrefixes = append(inventory.routePrefixes, prefix)
			}
		}
	}
	return nil
}

func inventoryRoutesAndRules(inventory *hostInventory) error {
	filter := &netlink.Route{Table: unix.RT_TABLE_UNSPEC}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, filter, netlink.RT_FILTER_TABLE)
	if err != nil {
		return fmt.Errorf("list host IPv4 routes: %w", err)
	}
	localHasDefault := false
	for _, route := range routes {
		if route.Table > 0 {
			inventory.usedRouteTables[route.Table] = struct{}{}
		}
		if prefix, valid := prefixFromIPNet(route.Dst); valid && prefix.Bits() > 0 {
			inventory.routePrefixes = append(inventory.routePrefixes, prefix)
		}
		if route.Table == unix.RT_TABLE_LOCAL && isDefaultRoute(route.Dst) {
			localHasDefault = true
		}
		if route.Protocol != 0 {
			inventory.usedProtocols[uint8(route.Protocol)] = struct{}{}
		}
	}
	rules, err := listRoutingRules()
	if err != nil {
		return fmt.Errorf("list host IPv4 rules: %w", err)
	}
	for _, rule := range rules {
		if rule.priority >= 0 {
			inventory.priorities[rule.priority] = struct{}{}
		}
		if rule.table > 0 {
			inventory.usedRouteTables[rule.table] = struct{}{}
		}
		if rule.protocol != 0 {
			inventory.usedProtocols[rule.protocol] = struct{}{}
		}
	}
	if localHasDefault {
		for index := range rules {
			rules[index].builtinLocal = false
		}
	}
	inventory.routingRules = rules
	return nil
}

func verifyProviderForwarding(selected []egressSelection) error {
	checked := make(map[string]struct{})
	for _, value := range selected {
		if _, exists := checked[value.providerName]; exists {
			continue
		}
		link, err := netlink.LinkByIndex(value.providerIndex)
		if err != nil || link.Attrs() == nil || link.Attrs().Name != value.providerName {
			return fmt.Errorf("provider interface %s identity changed", value.providerName)
		}
		if link.Attrs().Flags&net.FlagUp == 0 {
			return fmt.Errorf("provider interface %s is administratively down", value.providerName)
		}
		if err := requireIPv4Forwarding(value.providerName); err != nil {
			return err
		}
		checked[value.providerName] = struct{}{}
	}
	return nil
}

func requireIPv4Forwarding(name string) error {
	if filepath.Base(name) != name || name == "." || name == ".." {
		return fmt.Errorf("unsafe provider interface identity %q", name)
	}
	data, err := os.ReadFile(filepath.Join("/proc/sys/net/ipv4/conf", name, "forwarding"))
	if err != nil {
		return fmt.Errorf("read IPv4 forwarding for %s: %w", name, err)
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || value != 1 {
		return fmt.Errorf("provider interface %s does not have IPv4 forwarding enabled", name)
	}
	return nil
}

func prefixFromIPNet(value *net.IPNet) (netip.Prefix, bool) {
	if value == nil {
		return netip.Prefix{}, false
	}
	address, valid := netip.AddrFromSlice(value.IP)
	ones, bits := value.Mask.Size()
	if !valid || !address.Unmap().Is4() || bits != 32 || ones < 0 {
		return netip.Prefix{}, false
	}
	return netip.PrefixFrom(address.Unmap(), ones).Masked(), true
}
