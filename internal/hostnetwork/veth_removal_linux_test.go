//go:build linux

package hostnetwork

import (
	"errors"
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestRemoveOwnedVethConvergesWhenNamespaceTeardownWinsAfterObservation(t *testing.T) {
	for _, test := range []struct {
		name      string
		addresses []netlink.Addr
	}{
		{name: "after partial shape"},
		{name: "after full shape", addresses: []netlink.Addr{ownedVethAddress(t)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := testAllocation(t, firewallNFTables).transfers[0]
			observed := observedOwnedVeth(value, 41)
			links := &recordingOwnedVethRemoval{
				lookups:   []ownedVethLookup{{link: observed}, {err: unix.ENODEV}},
				addresses: test.addresses,
				deleteErr: unix.ENODEV,
			}
			if err := removeOwnedVethUsing(links, value); err != nil {
				t.Fatal(err)
			}
			if links.lookupCalls != 2 || links.addressCalls != 1 || links.addressed != observed ||
				links.deleteCalls != 1 || links.deleted != observed {
				t.Fatalf("veth teardown calls = lookups %d addresses %d deletes %d",
					links.lookupCalls, links.addressCalls, links.deleteCalls)
			}
		})
	}
}

func TestRemoveOwnedVethRejectsAmbiguousInitialObservation(t *testing.T) {
	value := testAllocation(t, firewallNFTables).transfers[0]
	wrongMarker := observedOwnedVeth(value, 41)
	wrongMarker.LinkAttrs.HardwareAddr = net.HardwareAddr{0x02, 1, 2, 3, 4, 5}
	wrongAddress := ownedVethAddress(t)
	wrongAddress.Flags = 0
	for _, test := range []struct {
		name       string
		lookup     ownedVethLookup
		addresses  []netlink.Addr
		addressErr error
	}{
		{name: "lookup failure", lookup: ownedVethLookup{err: unix.EPERM}},
		{name: "wrong marker", lookup: ownedVethLookup{link: wrongMarker}},
		{name: "address observation failure", lookup: ownedVethLookup{link: observedOwnedVeth(value, 42)},
			addressErr: unix.EPERM},
		{name: "wrong partial shape", lookup: ownedVethLookup{link: observedOwnedVeth(value, 42)},
			addresses: []netlink.Addr{wrongAddress}},
	} {
		t.Run(test.name, func(t *testing.T) {
			links := &recordingOwnedVethRemoval{lookups: []ownedVethLookup{test.lookup},
				addresses: test.addresses, addressErr: test.addressErr}
			if err := removeOwnedVethUsing(links, value); err == nil {
				t.Fatal("ambiguous host veth was accepted for deletion")
			}
			if links.deleteCalls != 0 {
				t.Fatalf("ambiguous host veth delete calls = %d", links.deleteCalls)
			}
		})
	}
}

func TestRemoveOwnedVethDoesNotSwallowDeleteFailureOrNameReuse(t *testing.T) {
	value := testAllocation(t, firewallNFTables).transfers[0]
	observed := observedOwnedVeth(value, 41)
	foreign := observedOwnedVeth(value, 42)
	foreign.LinkAttrs.HardwareAddr = net.HardwareAddr{0x02, 1, 2, 3, 4, 5}
	for _, test := range []struct {
		name        string
		deleteErr   error
		afterDelete ownedVethLookup
		wantLookup  int
		wantCause   error
	}{
		{name: "permission failure", deleteErr: unix.EPERM, wantLookup: 1, wantCause: unix.EPERM},
		{name: "same name reused", deleteErr: unix.ENODEV,
			afterDelete: ownedVethLookup{link: foreign}, wantLookup: 2},
		{name: "absence check failure", deleteErr: unix.ENODEV,
			afterDelete: ownedVethLookup{err: unix.EPERM}, wantLookup: 2, wantCause: unix.EPERM},
	} {
		t.Run(test.name, func(t *testing.T) {
			links := &recordingOwnedVethRemoval{
				lookups:   []ownedVethLookup{{link: observed}, test.afterDelete},
				addresses: []netlink.Addr{ownedVethAddress(t)}, deleteErr: test.deleteErr,
			}
			err := removeOwnedVethUsing(links, value)
			if err == nil {
				t.Fatal("veth deletion ambiguity was accepted")
			}
			if test.wantCause != nil && !errors.Is(err, test.wantCause) {
				t.Fatalf("deletion error = %v, want cause %v", err, test.wantCause)
			}
			if links.lookupCalls != test.wantLookup || links.addressCalls != 1 ||
				links.addressed != observed || links.deleteCalls != 1 || links.deleted != observed {
				t.Fatalf("ambiguous deletion calls = lookups %d deletes %d deleted %#v",
					links.lookupCalls, links.deleteCalls, links.deleted)
			}
		})
	}
}

func TestRemoveOwnedVethUsesSingleObservationForOwnershipProofAndDelete(t *testing.T) {
	value := testAllocation(t, firewallNFTables).transfers[0]
	observed := observedOwnedVeth(value, 41)
	links := &recordingOwnedVethRemoval{
		lookups:   []ownedVethLookup{{link: observed}},
		addresses: []netlink.Addr{ownedVethAddress(t)},
	}
	if err := removeOwnedVethUsing(links, value); err != nil {
		t.Fatal(err)
	}
	if links.lookupCalls != 1 || links.addressCalls != 1 || links.addressed != observed ||
		links.deleteCalls != 1 || links.deleted != observed {
		t.Fatalf("veth removal calls = lookups %d addresses %d deletes %d",
			links.lookupCalls, links.addressCalls, links.deleteCalls)
	}
}

func TestRemoveOwnedVethAcceptsInitialAbsence(t *testing.T) {
	value := testAllocation(t, firewallNFTables).transfers[0]
	links := &recordingOwnedVethRemoval{lookups: []ownedVethLookup{{err: unix.ENODEV}}}
	if err := removeOwnedVethUsing(links, value); err != nil {
		t.Fatal(err)
	}
	if links.lookupCalls != 1 || links.addressCalls != 0 || links.deleteCalls != 0 {
		t.Fatalf("absent veth calls = lookups %d addresses %d deletes %d",
			links.lookupCalls, links.addressCalls, links.deleteCalls)
	}
}

type ownedVethLookup struct {
	link netlink.Link
	err  error
}

type recordingOwnedVethRemoval struct {
	lookups      []ownedVethLookup
	addresses    []netlink.Addr
	addressErr   error
	deleteErr    error
	lookupCalls  int
	addressCalls int
	addressed    netlink.Link
	deleteCalls  int
	deleted      netlink.Link
}

func (links *recordingOwnedVethRemoval) HostLink(string) (netlink.Link, error) {
	index := links.lookupCalls
	links.lookupCalls++
	if index >= len(links.lookups) {
		return nil, errors.New("unexpected host-veth lookup")
	}
	return links.lookups[index].link, links.lookups[index].err
}

func (links *recordingOwnedVethRemoval) IPv4Addresses(link netlink.Link) ([]netlink.Addr, error) {
	links.addressCalls++
	links.addressed = link
	return links.addresses, links.addressErr
}

func (links *recordingOwnedVethRemoval) Delete(link netlink.Link) error {
	links.deleteCalls++
	links.deleted = link
	return links.deleteErr
}

func observedOwnedVeth(value transferAllocation, index int) *netlink.Veth {
	return &netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: value.hostVeth,
		HardwareAddr: value.hostMAC.hardwareAddr(), Index: index}}
}

func ownedVethAddress(t *testing.T) netlink.Addr {
	t.Helper()
	value := testAllocation(t, firewallNFTables).transfers[0]
	address := *addressFor(value.hostIP, value.subnet.Bits())
	address.Flags = unix.IFA_F_NOPREFIXROUTE
	return address
}
