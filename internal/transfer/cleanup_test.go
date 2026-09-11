package transfer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
)

func TestUnconfirmedResolverCleanupRetainsHostNetworkAndRunEvidence(t *testing.T) {
	roots, authority := isolatedRunRoots(t)
	id, err := runid.New()
	if err != nil {
		t.Fatal(err)
	}
	live, err := roots.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	networks := &observedHostNetworkOwner{closeDone: make(chan struct{})}
	resolvers := newObservedResolvers(t)
	retryEntered := make(chan struct{})
	releaseRetry := make(chan struct{})
	resolvers.closeFunc = func(ctx context.Context, call int32) (bool, error) {
		if call == 1 {
			<-ctx.Done()
			return false, ctx.Err()
		}
		if call == 2 {
			close(retryEntered)
		}
		select {
		case <-releaseRetry:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	directory := &TransferDirectory{networks: networks, cleanupLimit: 80 * time.Millisecond}
	start := time.Now()
	err = directory.cleanupResolvers(live, nil, resolvers)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) || networks.closeContexts != 0 ||
		resolvers.closed.Load() != 0 || elapsed > 150*time.Millisecond {
		t.Fatalf("unconfirmed resolver cleanup error=%v network closes=%d resolver closes=%d duration=%v",
			err, networks.closeContexts, resolvers.closed.Load(), elapsed)
	}
	waitForTestSignal(t, retryEntered, time.Second, "retained resolver cleanup")
	callbacks := 0
	if err := roots.RecoverStale(context.Background(), func(*rundirectory.StaleRun) error {
		callbacks++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if callbacks != 0 {
		t.Fatalf("active resolver cleanup exposed run evidence as stale %d times", callbacks)
	}
	close(releaseRetry)
	waitForTestSignal(t, networks.closeDone, time.Second, "host network cleanup")
	waitForTransferredRunLivenessRelease(t, authority, id)
	callbacks, err = recoverExactRun(roots, networks, newObservedFileViews(), id)
	if err != nil || callbacks != 1 {
		t.Fatalf("recover exact resolver-cleanup run: callbacks=%d error=%v", callbacks, err)
	}
	assertExactRunAbsent(t, authority, id)
}

func TestEachCleanupOwnerReceivesFreshDeadline(t *testing.T) {
	networks := &observedHostNetworkOwner{closeDelay: 25 * time.Millisecond}
	done := make(chan struct{})
	close(done)
	commands := &observedCommandGroupStarter{group: &observedSupervisedProcesses{
		done: done, confirmDelay: 25 * time.Millisecond,
	}}
	directory := newCleanupTransferDirectory(t, &observedEgressResolver{}, networks, &observedWeightMeasurer{}, commands,
		40*time.Millisecond)
	source := sourceTree(t)
	result := runTransfer(t, directory.TransferDirectory, context.Background(), source,
		validManualRequest(t, source))
	if result.CleanupError() != nil || networks.closeContexts != 1 || networks.closed != 1 {
		t.Fatalf("independent cleanup deadlines result=%#v network calls=%d closes=%d",
			result, networks.closeContexts, networks.closed)
	}
	assertRunRootAbsent(t, directory.authority, result)
}

func TestIndependentOwnerChainsAggregateFailures(t *testing.T) {
	networkErr := errors.New("host network cleanup did not finish")
	viewErr := errors.New("file-view cleanup did not finish")
	networks := &observedHostNetworkOwner{closeErr: networkErr}
	commands := &observedCommandGroupStarter{}
	directory := newCleanupTransferDirectory(t, &observedEgressResolver{}, networks, &observedWeightMeasurer{}, commands,
		time.Second)
	directory.views.closeErr = viewErr
	source := sourceTree(t)
	result := runTransfer(t, directory.TransferDirectory, context.Background(), source,
		validManualRequest(t, source))
	if !errors.Is(result.CleanupError(), networkErr) || !errors.Is(result.CleanupError(), viewErr) ||
		networks.closeContexts != 1 || directory.views.closeCalls != 1 {
		t.Fatalf("independent cleanup result=%#v network calls=%d view calls=%d",
			result, networks.closeContexts, directory.views.closeCalls)
	}
	id, _ := result.RunID()
	if !directory.views.hasSet(id) {
		t.Fatal("failed file-view cleanup lost its live owner evidence")
	}
	directory.views.closeErr = nil
	callbacks, err := recoverExactRun(directory.roots, networks, directory.views, id)
	if err != nil || callbacks != 1 {
		t.Fatalf("recover exact cleanup-failure run: callbacks=%d error=%v", callbacks, err)
	}
	assertExactRunAbsent(t, directory.authority, id)
}

func newCleanupTransferDirectory(t *testing.T, egresses egressResolver, networks hostNetworkLifecycle,
	weights weightMeasurer, commands commandGroupStarter, cleanup time.Duration,
) *isolatedTransferDirectory {
	t.Helper()
	roots, authority := isolatedRunRoots(t)
	resolvers := newObservedResolvers(t)
	views := newObservedFileViews()
	return &isolatedTransferDirectory{
		TransferDirectory: newTransferDirectory(roots, egresses, networks, resolvers, weights, views, commands, cleanup),
		roots:             roots, authority: authority, resolvers: resolvers, views: views,
	}
}
