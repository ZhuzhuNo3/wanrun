package transfer

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
)

func TestManualTransferUsesRealRunAndViewsAndKeepsSiblingResults(t *testing.T) {
	source := sourceTree(t)
	request := validManualRequest(t, source)
	selectedNetworks := request.selectedNetworks()
	networks := &observedHostNetworkOwner{closeDone: make(chan struct{})}
	commands := &observedCommandGroupStarter{result: childprocesses.Result{Transfers: []childprocesses.TransferResult{
		{Transfer: selectedNetworks[1].transfer, ExitCode: 7}, {Transfer: selectedNetworks[0].transfer, ExitCode: 0},
	}}}
	directory := newExecutionTransferDirectory(t, &observedEgressResolver{}, networks, &observedWeightMeasurer{}, commands, time.Second)
	result := runTransfer(t, directory.TransferDirectory, context.Background(), source, request)
	if result.CleanupError() != nil || result.RunError() != nil {
		t.Fatalf("manual transfer result=%#v", result)
	}
	transfers := result.Transfers()
	if len(transfers) != 2 || transfers[0].Transfer != selectedNetworks[0].transfer ||
		transfers[1].Transfer != selectedNetworks[1].transfer || len(commands.executions) != 2 || networks.closed != 1 {
		t.Fatalf("result/command/close counts=%d/%d/%d",
			len(transfers), len(commands.executions), networks.closed)
	}
	for _, execution := range commands.executions {
		argument := execution.Argv()[2]
		if filepath.Base(argument) != filepath.Base(source) || execution.ViewPath() != argument {
			t.Errorf("transfer %d descriptor alias=%q view=%q", execution.Number().Value(), argument,
				execution.ViewPath())
		}
	}
	assertRunRootAbsent(t, directory.authority, result)
}

func TestResolverDiagnosticsReachTheRunErrorBoundary(t *testing.T) {
	want := errors.New("oversized DNS request was rejected")
	source := sourceTree(t)
	request := validManualRequest(t, source)
	commands := &observedCommandGroupStarter{result: successfulProcessResult(request)}
	directory := newExecutionTransferDirectory(t, &observedEgressResolver{}, &observedHostNetworkOwner{}, &observedWeightMeasurer{}, commands,
		time.Second)
	directory.resolvers.diagnosticErr = want
	result := runTransfer(t, directory.TransferDirectory, context.Background(), source, request)
	if !errors.Is(result.RunError(), want) {
		t.Fatalf("resolver diagnostic did not reach run result: %#v", result)
	}
}

func TestTransferExecutionFreezesTheCompleteJoinedCommand(t *testing.T) {
	source := sourceTree(t)
	request := validManualRequest(t, source)
	commands := &observedCommandGroupStarter{result: successfulProcessResult(request)}
	directory := newExecutionTransferDirectory(t, &observedEgressResolver{}, &observedHostNetworkOwner{},
		&observedWeightMeasurer{}, commands, time.Second)
	result := runTransfer(t, directory.TransferDirectory, context.Background(), source, request)
	if result.Err() != nil || len(commands.executions) != 2 {
		t.Fatalf("transfer result=%v executions=%d", result.Err(), len(commands.executions))
	}
	selectedNetworks := request.selectedNetworks()
	for index, execution := range commands.executions {
		wantNumber := selectedNetworks[index].transfer
		wantShortcut := byte('1' + index)
		if execution.Number() != wantNumber || execution.Shortcut() != wantShortcut ||
			filepath.Base(execution.ViewPath()) != filepath.Base(source) {
			t.Fatalf("execution %d = %#v", index, execution)
		}
		argv := execution.Argv()
		argv[0] = "changed"
		if execution.Argv()[0] == "changed" {
			t.Fatal("execution argv remained mutable")
		}
	}
}

func TestObservedSupervisionFailureIsOnlyARunFailureAfterContainment(t *testing.T) {
	source := sourceTree(t)
	request := validManualRequest(t, source)
	supervisionErr := errors.New("process supervision failed after reaping")
	done := make(chan struct{})
	close(done)
	group := &observedSupervisedProcesses{done: done, result: successfulProcessResult(request),
		waitErr: supervisionErr}
	networks := &observedHostNetworkOwner{}
	directory := newExecutionTransferDirectory(t, &observedEgressResolver{}, networks, &observedWeightMeasurer{},
		&observedCommandGroupStarter{group: group}, time.Second)

	result := runTransfer(t, directory.TransferDirectory, context.Background(), source, request)
	if !errors.Is(result.RunError(), supervisionErr) || result.CleanupError() != nil {
		t.Fatalf("contained supervision result=%#v", result)
	}
	if networks.closed != 1 {
		t.Fatalf("host network cleanup count=%d", networks.closed)
	}
	assertRunRootAbsent(t, directory.authority, result)
}

func TestObservedSupervisionFailureRetainsOwnersWithoutContainment(t *testing.T) {
	source := sourceTree(t)
	request := validManualRequest(t, source)
	supervisionErr := errors.New("reaper stopped before containment")
	contained := make(chan struct{})
	group := &observedSupervisedProcesses{done: contained, result: successfulProcessResult(request),
		waitErr: supervisionErr}
	networks := &observedHostNetworkOwner{closeDone: make(chan struct{})}
	directory := newExecutionTransferDirectory(t, &observedEgressResolver{}, networks, &observedWeightMeasurer{},
		&observedCommandGroupStarter{group: group}, 25*time.Millisecond)

	result := runTransfer(t, directory.TransferDirectory, context.Background(), source, request)
	if !errors.Is(result.RunError(), supervisionErr) || result.CleanupError() == nil {
		t.Fatalf("uncontained supervision result=%#v", result)
	}
	if networks.closed != 0 {
		t.Fatalf("host network closed without containment: %d", networks.closed)
	}
	id, _ := result.RunID()
	close(contained)
	waitForTestSignal(t, networks.closeDone, time.Second, "host network cleanup")
	waitForTransferredRunLivenessRelease(t, directory.authority, id)
	callbacks, err := recoverExactRun(directory.roots, networks, directory.views, id)
	if err != nil || callbacks != 1 {
		t.Fatalf("recover exact uncontained-supervision run: callbacks=%d error=%v", callbacks, err)
	}
	assertExactRunAbsent(t, directory.authority, id)
}

func TestAutomaticMeasurementFailureCreatesNoViewsOrCommands(t *testing.T) {
	weights := &observedWeightMeasurer{err: errors.New("measure transfer 2")}
	networks := &observedHostNetworkOwner{closeDone: make(chan struct{})}
	commands := &observedCommandGroupStarter{}
	directory := newExecutionTransferDirectory(t, &observedEgressResolver{}, networks, weights, commands, time.Second)
	source := sourceTree(t)
	result := runTransfer(t, directory.TransferDirectory, context.Background(), source,
		validAutomaticRequest(t, source))
	if result.RunError() == nil || !strings.Contains(result.RunError().Error(), "measure transfer 2") ||
		commands.started != 0 || networks.closed != 1 {
		t.Fatalf("measurement gate result=%#v starts=%d closes=%d",
			result, commands.started, networks.closed)
	}
	assertRunRootAbsent(t, directory.authority, result)
}

func TestUnconfirmedMeasureHelpersRetainNetworkAndLiveness(t *testing.T) {
	networks := &observedHostNetworkOwner{closeDone: make(chan struct{})}
	contained := make(chan struct{})
	helperErr := errors.New("measure helper supervision failed")
	weights := &observedWeightMeasurer{err: helperErr,
		owner: &observedSupervisedProcesses{done: contained}}
	directory := newExecutionTransferDirectory(t, &observedEgressResolver{}, networks, weights, &observedCommandGroupStarter{},
		25*time.Millisecond)
	source := sourceTree(t)
	result := runTransfer(t, directory.TransferDirectory, context.Background(), source,
		validAutomaticRequest(t, source))
	if !errors.Is(result.RunError(), helperErr) ||
		!errors.Is(result.CleanupError(), context.DeadlineExceeded) ||
		errors.Is(result.CleanupError(), helperErr) || networks.closed != 0 {
		t.Fatalf("unconfirmed measure result=%#v network closes=%d", result, networks.closed)
	}
	id, _ := result.RunID()
	callbacks := 0
	if err := directory.roots.RecoverStale(context.Background(), func(*rundirectory.StaleRun) error {
		callbacks++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if callbacks != 0 {
		t.Fatalf("measure helper containment exposed stale run %d times", callbacks)
	}
	if directory.resolvers.closed.Load() != 0 {
		t.Fatal("namespace resolvers closed before measure helpers were contained")
	}
	close(contained)
	waitForResolverClose(t, directory.resolvers)
	waitForTestSignal(t, networks.closeDone, time.Second, "host network cleanup")
	waitForTransferredRunLivenessRelease(t, directory.authority, id)
	callbacks, err := recoverExactRun(directory.roots, networks, directory.views, id)
	if err != nil || callbacks != 1 {
		t.Fatalf("recover exact measurement run: callbacks=%d error=%v", callbacks, err)
	}
	assertExactRunAbsent(t, directory.authority, id)
}

func newExecutionTransferDirectory(t *testing.T, egresses egressResolver, networks hostNetworkLifecycle,
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
