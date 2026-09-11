//go:build linux || darwin

package hostnetwork

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentOwnersReobserveAndKeepSharedSourceObjectsDisjoint(t *testing.T) {
	lockDirectory := t.TempDir()
	if err := os.Chmod(lockDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	host := newConcurrentKernelHost()
	owner := newOwner(currentSelections{testSelections()}, host, systemFileSync{},
		filepath.Join(lockDirectory, "host-network.lock"))
	runs := []testRun{
		{mustRunID(t, "2f112233445566778899aabbccddeeff"), openTestRunRoot(t)},
		{mustRunID(t, "30122233445566778899aabbccddeeff"), openTestRunRoot(t)},
	}
	sessions := make([]*Session, len(runs))
	errorsByRun := make([]error, len(runs))
	start := make(chan struct{})
	var wait sync.WaitGroup
	for index := range runs {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			sessions[index], errorsByRun[index] = owner.open(context.Background(), runs[index], testSelections())
		}(index)
	}
	close(start)
	wait.Wait()
	for index, err := range errorsByRun {
		if err != nil || sessions[index] == nil {
			t.Fatalf("Open run %d = %#v, %v", index, sessions[index], err)
		}
	}
	left, right := sessions[0].claim, sessions[1].claim
	if left.transfers[0].source != right.transfers[0].source || left.transfers[0].routeTable != right.transfers[0].routeTable {
		t.Fatalf("fixture did not reuse source/table: left=%#v right=%#v", left.transfers[0], right.transfers[0])
	}
	if left.runID == right.runID || left.firewallTable == right.firewallTable ||
		left.returnTable == right.returnTable || left.transfers[0].subnet == right.transfers[0].subnet ||
		left.transfers[0].outboundPriority == right.transfers[0].outboundPriority ||
		left.transfers[0].returnPriority == right.transfers[0].returnPriority ||
		left.transfers[0].hostVeth == right.transfers[0].hostVeth || left.protocol == right.protocol {
		t.Fatalf("concurrent run identities overlap: left=%#v right=%#v", left, right)
	}
	if err := sessions[0].Close(context.Background()); err != nil {
		t.Fatalf("Close first run: %v", err)
	}
	if !host.has(right.runID.String()) {
		t.Fatal("closing one run changed the other active run")
	}
	if err := sessions[1].Close(context.Background()); err != nil {
		t.Fatalf("Close second run: %v", err)
	}
	if host.count() != 0 {
		t.Fatalf("active kernel owners after close = %d", host.count())
	}
}

type concurrentKernelHost struct {
	mu     sync.Mutex
	active map[string]networkClaim
}

func newConcurrentKernelHost() *concurrentKernelHost {
	return &concurrentKernelHost{active: make(map[string]networkClaim)}
}

func (host *concurrentKernelHost) Inspect(context.Context, []egressSelection) (hostInventory, error) {
	host.mu.Lock()
	defer host.mu.Unlock()
	inventory := emptyInventory()
	ensureInventoryMaps(&inventory)
	for _, claim := range host.active {
		inventory.firewall.nftTableNames[claim.firewallTable] = struct{}{}
		inventory.firewall.transferlanesOwnerReferences[claim.runID.String()] = struct{}{}
		inventory.firewall.nftPostroutingNATChains = append(inventory.firewall.nftPostroutingNATChains,
			nftPostroutingNATChain{Family: "ip", Table: claim.firewallTable, Name: "postrouting",
				Priority: claim.nftPostroutingPriority, HasRules: true})
		inventory.usedRouteTables[claim.returnTable] = struct{}{}
		inventory.usedProtocols[claim.protocol] = struct{}{}
		for _, value := range claim.transfers {
			inventory.linkNames[value.hostVeth] = struct{}{}
			inventory.routePrefixes = append(inventory.routePrefixes, value.subnet)
			inventory.priorities[value.outboundPriority] = struct{}{}
			inventory.priorities[value.returnPriority] = struct{}{}
		}
	}
	return inventory, nil
}

func (host *concurrentKernelHost) ObservePublishedFirewalls(_ context.Context,
	claims []networkClaim,
) (publishedFirewallObservations, error) {
	result := make(publishedFirewallObservations, len(claims))
	for _, claim := range claims {
		result[claim.runID] = host.has(claim.runID.String())
	}
	return result, nil
}

func (host *concurrentKernelHost) Install(_ context.Context, claim networkClaim) (*hostInstallation, error) {
	host.mu.Lock()
	defer host.mu.Unlock()
	key := claim.runID.String()
	if _, exists := host.active[key]; exists {
		return nil, fmt.Errorf("duplicate active run %s", claim.runID)
	}
	host.active[key] = claim
	return completeTestInstallation(claim), nil
}

func (host *concurrentKernelHost) Verify(_ context.Context, claim networkClaim, _ *hostInstallation) error {
	if !host.has(claim.runID.String()) {
		return fmt.Errorf("run %s is absent", claim.runID)
	}
	return nil
}

func (*concurrentKernelHost) ConfirmConntrackEmpty(context.Context, networkClaim) error { return nil }

func (host *concurrentKernelHost) Rollback(ctx context.Context, claim networkClaim,
	_ *hostInstallation, _ bool) error {
	return host.Cleanup(ctx, claim, false)
}

func (host *concurrentKernelHost) Close(ctx context.Context, claim networkClaim,
	_ *hostInstallation) error {
	return host.Cleanup(ctx, claim, true)
}

func (host *concurrentKernelHost) Cleanup(_ context.Context, claim networkClaim, _ bool) error {
	host.mu.Lock()
	defer host.mu.Unlock()
	key := claim.runID.String()
	active, exists := host.active[key]
	if !exists {
		return nil
	}
	if !claimsEqual(active, claim) {
		return fmt.Errorf("run %s ownership changed", claim.runID)
	}
	delete(host.active, key)
	return nil
}

func (host *concurrentKernelHost) has(key string) bool {
	host.mu.Lock()
	defer host.mu.Unlock()
	_, exists := host.active[key]
	return exists
}

func (host *concurrentKernelHost) count() int {
	host.mu.Lock()
	defer host.mu.Unlock()
	return len(host.active)
}
