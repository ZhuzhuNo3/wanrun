package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
)

func TestResolverOpenFailureCompletesRunAfterHostNetworkCloseError(t *testing.T) {
	resolverOpenErr := errors.New("open resolver group")
	networkCloseErr := errors.New("close host network")
	source := sourceTree(t)
	networks := &observedHostNetworkOwner{closeErr: networkCloseErr}
	directory := newStartupTransferDirectory(t, &observedEgressResolver{}, networks, &observedWeightMeasurer{},
		&observedCommandGroupStarter{}, time.Second)
	directory.resolvers.openErr = resolverOpenErr

	result := runTransfer(t, directory.TransferDirectory, context.Background(), source,
		validManualRequest(t, source))
	if !errors.Is(result.RunError(), resolverOpenErr) ||
		!errors.Is(result.CleanupError(), networkCloseErr) {
		t.Fatalf("resolver/network failure result=%#v", result)
	}
	if networks.closeContexts != 1 {
		t.Fatalf("host network close attempts=%d", networks.closeContexts)
	}
	assertRunRootAbsent(t, directory.authority, result)
}

func TestStartupFailureAddsNewSupervisionEvidenceOnlyToRunFailure(t *testing.T) {
	source := sourceTree(t)
	request := validManualRequest(t, source)
	startupErr := errors.New("startup failed")
	supervisionErr := errors.New("supervision failed during bounded stop")
	contained := make(chan struct{})
	close(contained)
	group := &observedSupervisedProcesses{done: contained, result: successfulProcessResult(request),
		waitErr: supervisionErr}
	commands := &observedCommandGroupStarter{startErr: startupErr, startOwner: group}
	networks := &observedHostNetworkOwner{closeDone: make(chan struct{})}
	directory := newStartupTransferDirectory(t, &observedEgressResolver{}, networks, &observedWeightMeasurer{}, commands, time.Second)

	result := runTransfer(t, directory.TransferDirectory, context.Background(), source, request)
	if !errors.Is(result.RunError(), startupErr) || !errors.Is(result.RunError(), supervisionErr) ||
		result.CleanupError() != nil {
		t.Fatalf("bounded startup stop result=%#v", result)
	}
	if len(result.Transfers()) != 0 || networks.closed != 1 {
		t.Fatalf("startup transfer/network evidence=%d/%d", len(result.Transfers()), networks.closed)
	}
	assertRunRootAbsent(t, directory.authority, result)
}

func TestStartupFailureSeparatesSupervisionFromUnconfirmedContainment(t *testing.T) {
	source := sourceTree(t)
	request := validManualRequest(t, source)
	startupErr := errors.New("startup failed")
	supervisionErr := errors.New("supervision failed during bounded stop")
	contained := make(chan struct{})
	group := &observedSupervisedProcesses{done: contained, result: successfulProcessResult(request),
		waitErr: supervisionErr}
	commands := &observedCommandGroupStarter{startErr: startupErr, startOwner: group}
	networks := &observedHostNetworkOwner{closeDone: make(chan struct{})}
	directory := newStartupTransferDirectory(t, &observedEgressResolver{}, networks, &observedWeightMeasurer{}, commands,
		25*time.Millisecond)

	result := runTransfer(t, directory.TransferDirectory, context.Background(), source, request)
	if !errors.Is(result.RunError(), startupErr) || !errors.Is(result.RunError(), supervisionErr) ||
		result.CleanupError() == nil || errors.Is(result.CleanupError(), supervisionErr) {
		t.Fatalf("unconfirmed startup stop result=%#v", result)
	}
	if len(result.Transfers()) != 0 || networks.closed != 0 {
		t.Fatalf("unconfirmed transfer/network evidence=%d/%d", len(result.Transfers()), networks.closed)
	}
	id, _ := result.RunID()
	close(contained)
	waitForTestSignal(t, networks.closeDone, time.Second, "host network cleanup")
	waitForTransferredRunLivenessRelease(t, directory.authority, id)
	callbacks, err := recoverExactRun(directory.roots, networks, directory.views, id)
	if err != nil || callbacks != 1 {
		t.Fatalf("recover exact startup-failure run: callbacks=%d error=%v", callbacks, err)
	}
	assertExactRunAbsent(t, directory.authority, id)
}

func TestStartupEventDrainDeadlineRetainsOwnersUntilRealContainment(t *testing.T) {
	source := sourceTree(t)
	request := validManualRequest(t, source)
	owner := newDelayedCommandProcessOwner()
	processContextReleased := make(chan struct{})
	var releaseOnce sync.Once
	group := newCommandProcessGroup(owner, discardCommandEvents{}, func() {
		releaseOnce.Do(func() { close(processContextReleased) })
	})
	commands := &observedCommandGroupStarter{startErr: errors.New("startup failed"), startOwner: group}
	networks := &observedHostNetworkOwner{closeDone: make(chan struct{})}
	directory := newStartupTransferDirectory(t, &observedEgressResolver{}, networks, &observedWeightMeasurer{}, commands,
		25*time.Millisecond)
	delayedDrain := time.AfterFunc(250*time.Millisecond, owner.closeEventStreams)

	started := time.Now()
	result := runTransfer(t, directory.TransferDirectory, context.Background(), source, request)
	elapsed := time.Since(started)
	delayedDrain.Stop()
	waitForTestSignal(t, processContextReleased, time.Second, "command process context release")
	select {
	case <-group.done:
		t.Fatal("command event drain stopped before its source streams closed")
	default:
	}

	if networks.closed != 0 {
		t.Fatalf("host network closed before process containment: %d", networks.closed)
	}
	id, _ := result.RunID()
	runRoot := filepath.Join(directory.authority, id.String())
	for _, name := range []string{"views", "liveness.lock"} {
		if _, err := os.Stat(filepath.Join(runRoot, name)); err != nil {
			t.Fatalf("unconfirmed startup lost %s: %v", name, err)
		}
	}
	callbacks := 0
	if err := directory.roots.RecoverStale(context.Background(), func(*rundirectory.StaleRun) error {
		callbacks++
		return nil
	}); err != nil || callbacks != 0 {
		t.Fatalf("uncontained command exposed live run to recovery: callbacks=%d error=%v", callbacks, err)
	}
	owner.closeEventStreams()
	waitForTestSignal(t, group.done, time.Second, "command event drain")
	close(owner.contained)
	waitForTestSignal(t, networks.closeDone, time.Second, "host network cleanup")
	waitForTransferredRunLivenessRelease(t, directory.authority, id)
	callbacks, err := recoverExactRun(directory.roots, networks, directory.views, id)
	if err != nil || callbacks != 1 {
		t.Fatalf("recover exact event-drain run: callbacks=%d error=%v", callbacks, err)
	}
	assertExactRunAbsent(t, directory.authority, id)

	if elapsed >= 150*time.Millisecond {
		t.Fatalf("failed-start event drain exceeded cleanup deadline: %v", elapsed)
	}
	if !errors.Is(result.RunError(), context.DeadlineExceeded) ||
		!strings.Contains(result.RunError().Error(), "command event forwarding") ||
		!errors.Is(result.CleanupError(), context.DeadlineExceeded) {
		t.Fatalf("failed-start deadline evidence is incomplete: %#v", result)
	}
}

func TestUnconfirmedStartupRetainsLivenessAndEveryRecoveryAnchor(t *testing.T) {
	networks := &observedHostNetworkOwner{closeDone: make(chan struct{})}
	contained := make(chan struct{})
	group := &observedSupervisedProcesses{done: contained}
	commands := &observedCommandGroupStarter{startErr: errors.New("startup compensation unconfirmed"),
		startOwner: group}
	directory := newStartupTransferDirectory(t, &observedEgressResolver{}, networks, &observedWeightMeasurer{}, commands,
		25*time.Millisecond)
	source := sourceTree(t)
	result := runTransfer(t, directory.TransferDirectory, context.Background(), source,
		validManualRequest(t, source))
	if result.RunError() == nil || !errors.Is(result.CleanupError(), context.DeadlineExceeded) {
		t.Fatalf("unconfirmed startup result=%#v", result)
	}
	if networks.closed != 0 {
		t.Fatalf("host network closed before process containment: %d", networks.closed)
	}
	id, _ := result.RunID()
	runRoot := filepath.Join(directory.authority, id.String())
	for _, name := range []string{"views", "liveness.lock"} {
		if _, err := os.Stat(filepath.Join(runRoot, name)); err != nil {
			t.Fatalf("unconfirmed startup lost %s: %v", name, err)
		}
	}
	callbacks := 0
	if err := directory.roots.RecoverStale(context.Background(), func(*rundirectory.StaleRun) error {
		callbacks++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if callbacks != 0 {
		t.Fatalf("concurrent recoverer observed active run %d times", callbacks)
	}
	close(contained)
	waitForTestSignal(t, networks.closeDone, time.Second, "host network cleanup")
	waitForTransferredRunLivenessRelease(t, directory.authority, id)
	callbacks, err := recoverExactRun(directory.roots, networks, directory.views, id)
	if err != nil || callbacks != 1 {
		t.Fatalf("recover exact unconfirmed-startup run: callbacks=%d error=%v", callbacks, err)
	}
	assertExactRunAbsent(t, directory.authority, id)
}

func TestConfirmedStartupFailureCanCloseEveryLaterOwner(t *testing.T) {
	networks := &observedHostNetworkOwner{}
	commands := &observedCommandGroupStarter{startErr: errors.New("barrier preparation failed")}
	directory := newStartupTransferDirectory(t, &observedEgressResolver{}, networks, &observedWeightMeasurer{}, commands, time.Second)
	source := sourceTree(t)
	result := runTransfer(t, directory.TransferDirectory, context.Background(), source,
		validManualRequest(t, source))
	if result.RunError() == nil || result.CleanupError() != nil || networks.closed != 1 {
		t.Fatalf("confirmed startup failure result=%#v network closes=%d", result, networks.closed)
	}
	assertRunRootAbsent(t, directory.authority, result)
}

func newStartupTransferDirectory(t *testing.T, egresses egressResolver, networks hostNetworkLifecycle,
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
