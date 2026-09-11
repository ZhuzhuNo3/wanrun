package hostnetwork

import (
	"errors"
	"net/netip"
	"testing"
)

func TestConntrackCapabilityPreflightFailsClosed(t *testing.T) {
	access := &recordingConntrack{observeErr: errors.New("operation not supported")}
	if err := requireConntrackCapability(access); err == nil {
		t.Fatal("unobservable conntrack state was accepted")
	}
	if len(access.deleteBatches) != 0 || access.observeCalls != 1 {
		t.Fatalf("capability calls = observe %d, delete batches %d",
			access.observeCalls, len(access.deleteBatches))
	}
}

func TestConntrackCleanupRequiresExactDeleteAndConfirmedAbsence(t *testing.T) {
	claim := conntrackTestClaim(t)
	claim.transfers = append(claim.transfers, claim.transfers[0])
	for name, access := range map[string]*recordingConntrack{
		"delete unsupported":       {deleteErr: errors.New("operation not supported")},
		"verification unavailable": {observeErr: errors.New("permission denied")},
		"entry remains":            {sources: []netip.Addr{claim.transfers[0].namespaceIP}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := clearOwnedConntrack(access, claim); err == nil {
				t.Fatal("unconfirmed conntrack cleanup was accepted")
			}
			if len(access.deleteBatches) != 1 || access.observeCalls > 1 {
				t.Fatalf("failed cleanup calls = batch %d, observe %d",
					len(access.deleteBatches), access.observeCalls)
			}
		})
	}

	access := &recordingConntrack{}
	if err := clearOwnedConntrack(access, claim); err != nil {
		t.Fatalf("confirmed conntrack cleanup: %v", err)
	}
	if len(access.deleteBatches) != 1 || access.observeCalls != 1 {
		t.Fatalf("delete/observe calls = batch %d, observe %d",
			len(access.deleteBatches), access.observeCalls)
	}
	want := make(map[netip.Addr]struct{})
	for _, transfer := range claim.transfers {
		want[transfer.namespaceIP] = struct{}{}
	}
	wantCount := len(want)
	for _, source := range access.deleteBatches[0] {
		delete(want, source)
	}
	if len(access.deleteBatches[0]) != wantCount || len(want) != 0 {
		t.Fatalf("batch sources = %v; want complete deduplicated claim sources", access.deleteBatches[0])
	}
}

type recordingConntrack struct {
	sources                 []netip.Addr
	observeErr              error
	observeCallsBeforeError int
	deleteErr               error
	observeCalls            int
	deleteBatches           [][]netip.Addr
}

func (access *recordingConntrack) ObserveOriginalSources() ([]netip.Addr, error) {
	access.observeCalls++
	if access.observeErr != nil && access.observeCalls > access.observeCallsBeforeError {
		return nil, access.observeErr
	}
	return append([]netip.Addr(nil), access.sources...), nil
}

func (access *recordingConntrack) DeleteOriginalSources(sources []netip.Addr) error {
	access.deleteBatches = append(access.deleteBatches, append([]netip.Addr(nil), sources...))
	return access.deleteErr
}

func conntrackTestClaim(t *testing.T) networkClaim {
	t.Helper()
	return claimFromAllocation(testAllocation(t, firewallNFTables))
}
