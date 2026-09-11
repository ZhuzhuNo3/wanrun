//go:build linux

package hostnetwork

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestLinuxHostRejectsUnavailableClaimedRulePlaneBeforeTransferMutation(t *testing.T) {
	claim := iptablesFixtureClaim(t, "2e312233445566778899aabbccddeeff")
	runner := &scriptedArgvRunner{responses: []argvResponse{{err: exec.ErrNotFound}, {err: exec.ErrNotFound}}}
	host := linuxHost{firewalls: firewallBackends{
		nftables: &recordingFirewall{kind: firewallNFTables},
		iptables: testIPTablesFirewall(runner),
	}}
	assertTransferKernelObjectsAbsent(t, claim.transfers[0])
	if _, err := host.Install(context.Background(), claim); err == nil {
		t.Fatal("host install accepted an unavailable claimed iptables rule plane")
	}
	assertTransferKernelObjectsAbsent(t, claim.transfers[0])
	if len(runner.calls) != 2 || runner.calls[0].name != iptablesNFTRulesCommand ||
		runner.calls[1].name != iptablesNFTSaveCommand {
		t.Fatalf("preflight did not stay on the claimed rule plane: %q", runner.calls)
	}
}

func TestNFTInstallTopologyRequiresKernelSupportedNATPriority(t *testing.T) {
	for _, test := range []struct {
		priority int32
		wantErr  bool
	}{
		{priority: -200, wantErr: true},
		{priority: -199},
	} {
		t.Run(fmt.Sprintf("priority_%d", test.priority), func(t *testing.T) {
			inventory := firewallInventory{kind: firewallNFTables, nftTableNames: map[string]struct{}{}}
			program := nftProgram{table: "transferlanes_example", postroutingPriority: test.priority}
			err := requireNFTInstallTopology(inventory, program)
			if (err != nil) != test.wantErr {
				t.Fatalf("topology preflight error = %v, want error %v", err, test.wantErr)
			}
		})
	}
}

func TestStaleActivatedWeakFIBCleanupWaitsForKernelLinkCascade(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	kernel := &linkCascadeRoutingKernel{pendingReturnRouteObservations: 2}
	if err := removeActivatedWeakFIBDuringLinkCascade(context.Background(), kernel, claim,
		100*time.Millisecond, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if kernel.returnRouteDeletes != 3 {
		t.Fatalf("return-route observations = %d, want 3", kernel.returnRouteDeletes)
	}
}

func TestStaleActivatedWeakFIBCleanupDoesNotRetryOtherFailures(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	wantErr := errors.New("foreign route shape")
	kernel := &linkCascadeRoutingKernel{returnRouteError: exactReturnRouteDeleteResult(wantErr)}
	err := removeActivatedWeakFIBDuringLinkCascade(context.Background(), kernel, claim,
		100*time.Millisecond, time.Millisecond)
	if !errors.Is(err, wantErr) {
		t.Fatalf("cleanup error = %v, want %v", err, wantErr)
	}
	if kernel.returnRouteDeletes != 1 {
		t.Fatalf("return-route observations = %d, want 1", kernel.returnRouteDeletes)
	}
}

func TestStaleActivatedWeakFIBCleanupReobservesAfterExactRouteDeleteLosesCascadeRace(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	kernel := &linkCascadeRoutingKernel{returnRouteError: exactReturnRouteDeleteResult(unix.ESRCH)}
	kernel.clearReturnRouteErrorAfterFirstObservation = true
	if err := removeActivatedWeakFIBDuringLinkCascade(context.Background(), kernel, claim,
		100*time.Millisecond, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if kernel.returnRouteDeletes != 2 {
		t.Fatalf("return-route observations = %d, want exact delete then absence", kernel.returnRouteDeletes)
	}
}

func TestStaleActivatedWeakFIBCleanupBoundsKernelCascadeWait(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	kernel := &linkCascadeRoutingKernel{pendingReturnRouteObservations: -1}
	started := time.Now()
	err := removeActivatedWeakFIBDuringLinkCascade(context.Background(), kernel, claim,
		20*time.Millisecond, time.Millisecond)
	if !errors.Is(err, errReturnRouteLinkCascadePending) {
		t.Fatalf("cleanup error = %v, want pending link cascade", err)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("bounded cleanup elapsed = %s", elapsed)
	}
	if kernel.returnRouteDeletes < 2 {
		t.Fatalf("return-route observations = %d, want repeated observation", kernel.returnRouteDeletes)
	}
}

type linkCascadeRoutingKernel struct {
	pendingReturnRouteObservations             int
	returnRouteError                           error
	returnRouteDeletes                         int
	clearReturnRouteErrorAfterFirstObservation bool
}

func (*linkCascadeRoutingKernel) addReturnRoute(networkClaim, transferAllocation) error { return nil }

func (*linkCascadeRoutingKernel) addOutboundRule(networkClaim, transferAllocation) error { return nil }

func (*linkCascadeRoutingKernel) addReturnRule(networkClaim, transferAllocation) error { return nil }

func (kernel *linkCascadeRoutingKernel) deleteReturnRoute(networkClaim, transferAllocation) error {
	kernel.returnRouteDeletes++
	if kernel.pendingReturnRouteObservations != 0 {
		if kernel.pendingReturnRouteObservations > 0 {
			kernel.pendingReturnRouteObservations--
		}
		return errReturnRouteLinkCascadePending
	}
	err := kernel.returnRouteError
	if kernel.clearReturnRouteErrorAfterFirstObservation {
		kernel.returnRouteError = nil
	}
	return err
}

func (*linkCascadeRoutingKernel) deleteOutboundRule(networkClaim, transferAllocation) error {
	return nil
}

func (*linkCascadeRoutingKernel) deleteReturnRule(networkClaim, transferAllocation) error { return nil }

func assertTransferKernelObjectsAbsent(t *testing.T, transfer transferAllocation) {
	t.Helper()
	for _, name := range []string{transfer.hostVeth, transfer.peerVeth} {
		if _, err := netlink.LinkByName(name); err == nil {
			t.Fatalf("unexpected link %s exists", name)
		} else {
			var missing netlink.LinkNotFoundError
			if !errors.As(err, &missing) {
				t.Fatalf("inspect link %s: %v", name, err)
			}
		}
	}
}
