//go:build linux && rootintegration

package hostnetwork

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
)

func TestOpenRollsBackRouteConflict(t *testing.T) {
	fixture := newOpenVerificationFixture(t, "69112233445566778899aabbccddeeff")
	host := &routeConflictHost{kernelHost: fixture.owner.host}
	t.Cleanup(func() {
		if err := host.removeConflict(); err != nil {
			t.Errorf("remove injected route conflict: %v", err)
		}
	})
	fixture.owner.host = host

	session, err := fixture.owner.open(context.Background(), fixture.run, fixture.selections)
	if err == nil || session != nil || !strings.Contains(err.Error(), "outbound route") {
		if session != nil {
			_ = session.Close(context.Background())
		}
		t.Fatalf("route verification conflict result = %#v, %v", session, err)
	}
	if host.injections != 1 {
		t.Fatalf("route conflict injections = %d, want 1", host.injections)
	}
	assertNoNetworkEvidence(t, fixture.run.root)
	fixture.network.assertSurface(fixture.baseline)
}

func TestOpenRejectsChangedVethMarker(t *testing.T) {
	fixture := newOpenVerificationFixture(t, "6a112233445566778899aabbccddeeff")
	host := &vethMarkerTakeoverHost{kernelHost: fixture.owner.host}
	t.Cleanup(func() {
		if err := host.restoreMarker(true); err != nil {
			t.Errorf("restore injected veth marker: %v", err)
		}
	})
	fixture.owner.host = host

	session, err := fixture.owner.open(context.Background(), fixture.run, fixture.selections)
	if err == nil || session != nil ||
		!strings.Contains(err.Error(), "privileged network managers must ignore Transfer Lanes temporary veths") {
		if session != nil {
			_ = session.Close(context.Background())
		}
		t.Fatalf("changed veth marker result = %#v, %v", session, err)
	}
	if host.injections != 1 {
		t.Fatalf("veth marker injections = %d, want 1", host.injections)
	}
	assertNoNetworkEvidence(t, fixture.run.root)
	fixture.network.assertSurface(fixture.baseline)
}

type openVerificationFixture struct {
	network    *iptablesRootFixture
	owner      *Owner
	run        testRun
	selections []egressSelection
	baseline   string
}

func newOpenVerificationFixture(t *testing.T, id string) *openVerificationFixture {
	t.Helper()
	network := newIPTablesRootFixture(t, iptablesFrontendNFT)
	network.install()
	selections := append([]egressSelection(nil), network.selections[:1]...)
	return &openVerificationFixture{
		network: network, owner: network.ownerFor(netlinkRoutingKernel{}, selections),
		run: network.newRun(t, id), selections: selections, baseline: network.surface(),
	}
}

type routeConflictHost struct {
	kernelHost
	injections int
	conflict   *routeConflictReceipt
}

type routeConflictReceipt struct {
	rule    netlink.Rule
	pending bool
}

func (host *routeConflictHost) Verify(ctx context.Context, claim networkClaim,
	installation *hostInstallation,
) error {
	if len(claim.transfers) == 0 {
		return errors.New("route conflict injection requires one transfer")
	}
	transfer := claim.transfers[0]
	rule := netlink.NewRule()
	rule.Family = netlink.FAMILY_V4
	rule.Priority = transfer.outboundPriority - 1
	rule.IifName = transfer.hostVeth
	rule.Src = ipNet(transfer.subnet)
	rule.Type = uint8(nl.FR_ACT_UNREACHABLE)
	if err := netlink.RuleAdd(rule); err != nil {
		return fmt.Errorf("add exact route conflict: %w", err)
	}
	host.conflict = &routeConflictReceipt{rule: *rule, pending: true}
	host.injections++

	verifyErr := host.kernelHost.Verify(ctx, claim, installation)
	restoreErr := host.removeConflict()
	if restoreErr != nil {
		restoreErr = fmt.Errorf("remove exact route conflict before rollback: %w", restoreErr)
	}
	return errors.Join(verifyErr, restoreErr)
}

func (host *routeConflictHost) removeConflict() error {
	if host.conflict == nil || !host.conflict.pending {
		return nil
	}
	if err := netlink.RuleDel(&host.conflict.rule); err != nil {
		return err
	}
	host.conflict.pending = false
	return nil
}

type vethMarkerTakeoverHost struct {
	kernelHost
	injections int
	change     *vethMarkerReceipt
}

type vethMarkerReceipt struct {
	name        string
	index       int
	peerIndex   int
	originalMAC net.HardwareAddr
	changedMAC  net.HardwareAddr
	pending     bool
}

func (host *vethMarkerTakeoverHost) Verify(ctx context.Context, claim networkClaim,
	installation *hostInstallation,
) error {
	if len(claim.transfers) == 0 {
		return errors.New("veth marker injection requires one transfer")
	}
	if err := host.changeMarker(claim.transfers[0]); err != nil {
		return err
	}
	verifyErr := host.kernelHost.Verify(ctx, claim, installation)
	restoreErr := host.restoreMarker(false)
	if restoreErr != nil {
		restoreErr = fmt.Errorf("restore exact veth marker before rollback: %w", restoreErr)
	}
	return errors.Join(verifyErr, restoreErr)
}

func (host *vethMarkerTakeoverHost) changeMarker(transfer transferAllocation) error {
	link, err := netlink.LinkByName(transfer.hostVeth)
	if err != nil {
		return fmt.Errorf("open exact host veth: %w", err)
	}
	attributes := link.Attrs()
	if attributes == nil || link.Type() != "veth" || len(attributes.HardwareAddr) != 6 {
		return fmt.Errorf("host veth %q has invalid identity", transfer.hostVeth)
	}
	original := append(net.HardwareAddr(nil), attributes.HardwareAddr...)
	changed := append(net.HardwareAddr(nil), original...)
	changed[len(changed)-1] ^= 0xff
	host.change = &vethMarkerReceipt{
		name: transfer.hostVeth, index: attributes.Index, peerIndex: attributes.ParentIndex,
		originalMAC: original, changedMAC: changed, pending: true,
	}
	if err := netlink.LinkSetHardwareAddr(link, changed); err != nil {
		host.change.pending = false
		return fmt.Errorf("change exact host veth marker: %w", err)
	}
	host.injections++
	return nil
}

func (host *vethMarkerTakeoverHost) restoreMarker(allowMissing bool) error {
	if host.change == nil || !host.change.pending {
		return nil
	}
	link, err := netlink.LinkByName(host.change.name)
	if err != nil {
		if allowMissing && isLinkNotFound(err) {
			host.change.pending = false
			return nil
		}
		return err
	}
	attributes := link.Attrs()
	if attributes == nil || link.Type() != "veth" || attributes.Index != host.change.index ||
		attributes.ParentIndex != host.change.peerIndex ||
		!bytes.Equal(attributes.HardwareAddr, host.change.changedMAC) {
		return fmt.Errorf("host veth %q identity changed before marker restore", host.change.name)
	}
	if err := netlink.LinkSetHardwareAddr(link, host.change.originalMAC); err != nil {
		return err
	}
	host.change.pending = false
	return nil
}
