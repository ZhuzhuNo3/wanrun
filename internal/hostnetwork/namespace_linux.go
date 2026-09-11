//go:build linux

package hostnetwork

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

const namespaceInterface = "eth0"

func createAnonymousNamespace() (*os.File, error) {
	result := make(chan namespaceCreation, 1)
	go createNamespaceOnLockedThread(result)
	created := <-result
	if created.err != nil {
		return nil, fmt.Errorf("create anonymous network namespace: %w", created.err)
	}
	return os.NewFile(uintptr(created.handle), "transferlanes-network-namespace"), nil
}

func createNamespaceOnLockedThread(result chan<- namespaceCreation) {
	runtime.LockOSThread()
	original, err := netns.Get()
	if err != nil {
		runtime.UnlockOSThread()
		result <- namespaceCreation{handle: -1, err: err}
		return
	}
	created, createErr := netns.New()
	restoreErr := netns.Set(original)
	closeErr := original.Close()
	if restoreErr == nil {
		runtime.UnlockOSThread()
	}
	if err := errors.Join(createErr, restoreErr, closeErr); err != nil {
		_ = created.Close()
		result <- namespaceCreation{handle: -1, err: err}
		return
	}
	result <- namespaceCreation{handle: created}
}

type namespaceCreation struct {
	handle netns.NsHandle
	err    error
}

func installTransferLinks(value transferAllocation, namespace *os.File) (linkReceipt, error) {
	if namespace == nil {
		return linkReceipt{}, errors.New("anonymous namespace descriptor is missing")
	}
	receipt, err := establishVethPair(systemVethCreation{}, value, int(namespace.Fd()))
	if err != nil {
		return receipt, err
	}
	if err := configureHostVeth(value); err != nil {
		return receipt, err
	}
	return receipt, configureNamespaceVeth(value, netns.NsHandle(namespace.Fd()))
}

type systemVethCreation struct{}

func (systemVethCreation) CreateAtomic(hostName string, hostMAC linkMAC, peerName string,
	peerMAC linkMAC, mtu, namespaceFD int) error {
	return netlink.LinkAdd(&netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: hostName, HardwareAddr: hostMAC.hardwareAddr(), MTU: mtu},
		PeerName:  peerName, PeerHardwareAddr: peerMAC.hardwareAddr(), PeerMTU: uint32(mtu),
		PeerNamespace: netlink.NsFd(namespaceFD),
	})
}

func (systemVethCreation) HostLink(name string) (vethIdentity, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return vethIdentity{}, err
	}
	identity, err := identityOfLink(link)
	if err != nil || link.Type() != "veth" {
		return identity, err
	}
	peerIndex, err := netlink.VethPeerIndex(&netlink.Veth{LinkAttrs: *link.Attrs()})
	if err != nil {
		return vethIdentity{}, fmt.Errorf("observe veth peer identity: %w", err)
	}
	identity.peerIndex = peerIndex
	return identity, nil
}

func (systemVethCreation) NamespaceLink(namespaceFD int, name string) (vethIdentity, error) {
	handle, err := netlink.NewHandleAt(netns.NsHandle(namespaceFD))
	if err != nil {
		return vethIdentity{}, err
	}
	defer handle.Close()
	link, err := handle.LinkByName(name)
	if err != nil {
		return vethIdentity{}, err
	}
	return identityOfLink(link)
}

func identityOfLink(link netlink.Link) (vethIdentity, error) {
	if link == nil || link.Attrs() == nil {
		return vethIdentity{}, errors.New("netlink returned a link without identity")
	}
	attributes := link.Attrs()
	mac, err := linkMACFromHardwareAddr(attributes.HardwareAddr)
	if err != nil {
		return vethIdentity{}, err
	}
	return vethIdentity{name: attributes.Name, mac: mac, kind: link.Type(),
		index: attributes.Index, peerIndex: attributes.ParentIndex}, nil
}

func configureHostVeth(value transferAllocation) error {
	host, err := ownedHostVeth(value)
	if err != nil {
		return err
	}
	address := addressFor(value.hostIP, value.subnet.Bits())
	address.Flags = unix.IFA_F_NOPREFIXROUTE
	if err := netlink.AddrAdd(host, address); err != nil {
		return fmt.Errorf("add host veth address: %w", err)
	}
	if err := netlink.LinkSetUp(host); err != nil {
		return fmt.Errorf("set host veth up: %w", err)
	}
	path := filepath.Join("/proc/sys/net/ipv4/conf", value.hostVeth, "forwarding")
	if err := os.WriteFile(path, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable IPv4 forwarding on owned host veth: %w", err)
	}
	return nil
}

func configureNamespaceVeth(value transferAllocation, namespace netns.NsHandle) error {
	handle, err := netlink.NewHandleAt(namespace)
	if err != nil {
		return err
	}
	defer handle.Close()
	loopback, err := handle.LinkByName("lo")
	if err != nil {
		return err
	}
	if err := handle.LinkSetUp(loopback); err != nil {
		return err
	}
	peer, err := handle.LinkByName(value.peerVeth)
	if err != nil {
		return err
	}
	if err := handle.LinkSetName(peer, namespaceInterface); err != nil {
		return fmt.Errorf("rename namespace veth: %w", err)
	}
	peer, err = handle.LinkByName(namespaceInterface)
	if err != nil {
		return err
	}
	if err := handle.AddrAdd(peer, addressFor(value.namespaceIP, value.subnet.Bits())); err != nil {
		return fmt.Errorf("add namespace address: %w", err)
	}
	if err := handle.LinkSetUp(peer); err != nil {
		return err
	}
	defaultRoute := netlink.Route{LinkIndex: peer.Attrs().Index, Dst: defaultIPv4Network(),
		Gw: net.IP(value.hostIP.AsSlice()), Table: unix.RT_TABLE_MAIN}
	if err := handle.RouteAdd(&defaultRoute); err != nil {
		return fmt.Errorf("add namespace default route: %w", err)
	}
	return nil
}

func verifyClaimedTransfer(claim networkClaim, value transferAllocation, namespace *os.File) error {
	if namespace == nil {
		return errors.New("anonymous namespace descriptor is missing")
	}
	if err := verifyVethPair(systemVethCreation{}, value, int(namespace.Fd()), namespaceInterface); err != nil {
		return err
	}
	if err := verifyOwnedHostVeth(value, false); err != nil {
		return err
	}
	if err := verifyNamespace(value, netns.NsHandle(namespace.Fd()), false); err != nil {
		return err
	}
	if err := verifyClaimedRule(claimedOutboundRule(claim, value), false); err != nil {
		return err
	}
	if err := verifyClaimedRule(claimedReturnRule(claim, value), false); err != nil {
		return err
	}
	if err := verifyClaimedReturnRoute(claim, value, false); err != nil {
		return err
	}
	if err := verifyNoMainTemporaryRoute(value); err != nil {
		return err
	}
	if err := verifyOutboundRoute(value); err != nil {
		return err
	}
	return verifyReturnPath(claim.returnTable, value)
}

func verifyOwnedHostVeth(value transferAllocation, allowPartial bool) error {
	host, err := netlink.LinkByName(value.hostVeth)
	if err != nil {
		return fmt.Errorf("open host veth: %w", err)
	}
	addresses, err := netlink.AddrList(host, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("host veth addresses: %w", err)
	}
	return verifyObservedOwnedHostVeth(host, addresses, value, allowPartial)
}

func verifyObservedOwnedHostVeth(host netlink.Link, addresses []netlink.Addr,
	value transferAllocation, allowPartial bool,
) error {
	if !exactHostVethMarker(host, value) {
		return ambiguousTemporaryVeth("host veth owner marker or type is ambiguous")
	}
	want := prefixFor(value.hostIP, value.subnet.Bits())
	if err := verifyIPv4AddressSet(addresses, want, allowPartial, unix.IFA_F_NOPREFIXROUTE); err != nil {
		return fmt.Errorf("host veth addresses: %w", err)
	}
	if !allowPartial && host.Attrs().Flags&net.FlagUp == 0 {
		return errors.New("host veth is down")
	}
	if !allowPartial {
		return requireOwnedVethForwarding(value.hostVeth)
	}
	return nil
}

func exactHostVethMarker(host netlink.Link, value transferAllocation) bool {
	if host == nil || host.Type() != "veth" || host.Attrs() == nil {
		return false
	}
	attributes := host.Attrs()
	mac, err := linkMACFromHardwareAddr(attributes.HardwareAddr)
	if err != nil {
		return false
	}
	owner, err := linkOwnerIDFromHost(attributes.Name, mac)
	return err == nil && owner == value.linkOwner && attributes.Name == value.hostVeth && mac == value.hostMAC
}

func verifyNamespace(value transferAllocation, namespace netns.NsHandle, allowPartial bool) error {
	handle, err := netlink.NewHandleAt(namespace)
	if err != nil {
		return err
	}
	defer handle.Close()
	links, err := handle.LinkList()
	if err != nil {
		return err
	}
	for _, link := range links {
		name := link.Attrs().Name
		if expectedType, baseline := kernelNamespaceLinks()[name]; baseline {
			if link.Type() != expectedType {
				return fmt.Errorf("namespace baseline link %q has type %q, want %q", name, link.Type(), expectedType)
			}
			continue
		}
		if name != namespaceInterface && name != value.peerVeth {
			return fmt.Errorf("namespace contains unknown link %q", name)
		}
	}
	peer, err := namespacePeer(handle, value)
	if err != nil {
		if allowPartial && isLinkNotFound(err) {
			return nil
		}
		return err
	}
	if peer.Type() != "veth" || peer.Attrs() == nil ||
		!hardwareAddrEquals(peer.Attrs().HardwareAddr, value.peerMAC) {
		return errors.New("namespace veth identity mismatch")
	}
	want := prefixFor(value.namespaceIP, value.subnet.Bits())
	if err := verifyIPv4Addresses(handle, peer, want, allowPartial, 0); err != nil {
		return err
	}
	if !allowPartial {
		return verifyNamespaceReady(handle, peer, value)
	}
	return nil
}

func kernelNamespaceLinks() map[string]string {
	return map[string]string{"lo": "device", "tunl0": "ipip", "gre0": "gre", "gretap0": "gretap",
		"erspan0": "erspan", "ip_vti0": "vti", "ip6_vti0": "vti6", "sit0": "sit",
		"ip6tnl0": "ip6tnl", "ip6gre0": "ip6gre"}
}

func namespacePeer(handle *netlink.Handle, value transferAllocation) (netlink.Link, error) {
	peer, err := handle.LinkByName(namespaceInterface)
	if err == nil {
		return peer, nil
	}
	return handle.LinkByName(value.peerVeth)
}

func verifyNamespaceReady(handle *netlink.Handle, peer netlink.Link, value transferAllocation) error {
	if peer.Attrs().Name != namespaceInterface || peer.Attrs().Flags&net.FlagUp == 0 {
		return errors.New("namespace veth is not ready")
	}
	routes, err := handle.RouteList(peer, netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	found := 0
	for _, route := range routes {
		if isDefaultRoute(route.Dst) && route.Table == unix.RT_TABLE_MAIN &&
			route.LinkIndex == peer.Attrs().Index && net.IP(value.hostIP.AsSlice()).Equal(route.Gw) {
			found++
		}
	}
	if found != 1 {
		return fmt.Errorf("namespace has %d exact default routes", found)
	}
	return nil
}

type ownedVethRemoval interface {
	HostLink(string) (netlink.Link, error)
	IPv4Addresses(netlink.Link) ([]netlink.Addr, error)
	Delete(netlink.Link) error
}

type systemOwnedVethRemoval struct{}

func (systemOwnedVethRemoval) HostLink(name string) (netlink.Link, error) {
	return netlink.LinkByName(name)
}

func (systemOwnedVethRemoval) IPv4Addresses(link netlink.Link) ([]netlink.Addr, error) {
	return netlink.AddrList(link, netlink.FAMILY_V4)
}

func (systemOwnedVethRemoval) Delete(link netlink.Link) error {
	return netlink.LinkDel(link)
}

func removeOwnedVeth(value transferAllocation) error {
	return removeOwnedVethUsing(systemOwnedVethRemoval{}, value)
}

func removeOwnedVethUsing(links ownedVethRemoval, value transferAllocation) error {
	host, err := links.HostLink(value.hostVeth)
	if isLinkNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !exactHostVethMarker(host, value) {
		return ambiguousTemporaryVeth("host veth owner marker or type is ambiguous before deletion")
	}
	addresses, err := links.IPv4Addresses(host)
	if err != nil {
		return fmt.Errorf("host veth addresses before deletion: %w", err)
	}
	if err := verifyObservedOwnedHostVeth(host, addresses, value, true); err != nil {
		return err
	}
	if err := links.Delete(host); err != nil {
		if isLinkNotFound(err) {
			return confirmOwnedVethNameAbsent(links, value, err)
		}
		return fmt.Errorf("delete owned host veth: %w", err)
	}
	return nil
}

func confirmOwnedVethNameAbsent(links ownedVethRemoval, value transferAllocation,
	deleteErr error,
) error {
	_, err := links.HostLink(value.hostVeth)
	if isLinkNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("confirm host veth absence after %v: %w", deleteErr, err)
	}
	return fmt.Errorf("host veth %s exists after delete reported %v", value.hostVeth, deleteErr)
}

func verifyOwnedVethAbsent(value transferAllocation) error {
	if _, err := netlink.LinkByName(value.hostVeth); !isLinkNotFound(err) {
		if err == nil {
			return fmt.Errorf("host veth %s remains", value.hostVeth)
		}
		return err
	}
	return nil
}

func isDefaultRoute(destination *net.IPNet) bool {
	if destination == nil {
		return true
	}
	ones, bits := destination.Mask.Size()
	return bits == 32 && ones == 0
}

func verifyIPv4Addresses(handle *netlink.Handle, link netlink.Link,
	want string, allowNone bool, requiredFlags int) error {
	var addresses []netlink.Addr
	var err error
	if handle == nil {
		addresses, err = netlink.AddrList(link, netlink.FAMILY_V4)
	} else {
		addresses, err = handle.AddrList(link, netlink.FAMILY_V4)
	}
	if err != nil {
		return err
	}
	return verifyIPv4AddressSet(addresses, want, allowNone, requiredFlags)
}

func verifyIPv4AddressSet(addresses []netlink.Addr, want string,
	allowNone bool, requiredFlags int,
) error {
	if allowNone && len(addresses) == 0 {
		return nil
	}
	if len(addresses) != 1 || addresses[0].IPNet == nil || addresses[0].IPNet.String() != want ||
		addresses[0].Flags&requiredFlags != requiredFlags {
		return fmt.Errorf("got %v, want only %s", addresses, want)
	}
	return nil
}

func requireOwnedVethForwarding(name string) error {
	data, err := os.ReadFile(filepath.Join("/proc/sys/net/ipv4/conf", name, "forwarding"))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(data)) != "1" {
		return errors.New("owned host veth forwarding is disabled")
	}
	return nil
}

func addressFor(address netip.Addr, bits int) *netlink.Addr {
	return &netlink.Addr{IPNet: &net.IPNet{IP: net.IP(address.AsSlice()), Mask: net.CIDRMask(bits, 32)}}
}

func prefixFor(address netip.Addr, bits int) string {
	return (&net.IPNet{IP: net.IP(address.AsSlice()), Mask: net.CIDRMask(bits, 32)}).String()
}

func defaultIPv4Network() *net.IPNet {
	return &net.IPNet{IP: net.IPv4zero, Mask: net.CIDRMask(0, 32)}
}

func isLinkNotFound(err error) bool {
	var notFound netlink.LinkNotFoundError
	return errors.Is(err, unix.ENODEV) || errors.Is(err, unix.ENOENT) || errors.As(err, &notFound)
}
