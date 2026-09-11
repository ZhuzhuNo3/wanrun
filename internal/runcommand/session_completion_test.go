package runcommand

import (
	"context"
	"errors"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestRunSessionCompletionWinsQueuedTermination(t *testing.T) {
	lifecycle := &sessionLifecycleRecorder{}
	client := newSessionClientRecorder()
	client.lifecycle = lifecycle
	logs := &sessionLogRecorder{lifecycle: lifecycle}
	driver := &outputJoinedSessionDriver{newCompletionSessionDriver(&staticSessionDisplay{})}
	driver.lifecycle = lifecycle
	signals := &sessionSignalRecorder{resize: make(chan struct{}, 1), lifecycle: lifecycle}
	inbox := newTerminationInbox()
	inbox.publish(terminationRequest{reason: runsupervisor.CancelUser})
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{})
	client.events <- final

	result := (&runSession{ctx: context.Background(), client: client, logs: logs,
		driver: driver, signals: signals, inbox: inbox}).run()
	cancels, _, _ := client.actions()
	if len(cancels) != 0 {
		t.Fatalf("late termination caused cancellations: %v", cancels)
	}
	wantOrder := []string{"projection", "driver", "signals", "logs", "lifeline", "wait"}
	gotOrder := lifecycle.Events()
	if len(gotOrder) != len(wantOrder) {
		t.Fatalf("cleanup order=%v, want %v", gotOrder, wantOrder)
	}
	for index := range wantOrder {
		if gotOrder[index] != wantOrder[index] {
			t.Fatalf("cleanup order=%v, want %v", gotOrder, wantOrder)
		}
	}
	if _, cancelled := result.Cancellation(); cancelled {
		t.Fatal("late termination replaced normal completion")
	}
}

func TestRunSessionCompletionIgnoresLateCallerAndControllerLoss(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	callerFailure := errors.New("caller after Final")
	client := newSessionClientRecorder()
	projection := newSessionPhaseGate()
	t.Cleanup(projection.Release)
	driver := newCompletionSessionDriver(&staticSessionDisplay{})
	driver.projection = projection
	inbox := newTerminationInbox()
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{})
	client.events <- final
	done := make(chan Result, 1)
	go func() {
		done <- (&runSession{ctx: ctx, client: client, driver: driver, inbox: inbox}).run()
	}()

	waitForSessionPhase(t, projection.reached, "final projection started")
	cancel(callerFailure)
	inbox.publish(terminationRequest{reason: runsupervisor.CancelLifeline, controllerLost: true})
	projection.Release()
	result := waitForSessionResult(t, done, "completed result after late termination")
	if _, cancelled := result.Cancellation(); cancelled || errors.Is(result.ParentError(), callerFailure) {
		t.Fatalf("late events changed completed result: cancellation=%t parent=%v", cancelled,
			result.ParentError())
	}
	cancels, closeCalls, waitCalls := client.actions()
	if len(cancels) != 0 || closeCalls != 1 || waitCalls != 1 {
		t.Fatalf("actions cancel/close/wait=%v/%d/%d", cancels, closeCalls, waitCalls)
	}
}

func TestRunSessionFinalProjectionUsesDeepClone(t *testing.T) {
	id, _ := transfernumber.New(1)
	result := Result{finalReceived: true, final: runsupervisor.Final{
		Transfers: []runsupervisor.TransferResult{{Transfer: id}},
		Source:    &runsupervisor.SourceSummary{IgnoredSymlinks: 2},
	}}
	first, _ := result.Final()
	first.Transfers[0].ExitCode = 19
	first.Source.IgnoredSymlinks = 99
	second, _ := result.Final()
	if second.Transfers[0].ExitCode != 0 || second.Source.IgnoredSymlinks != 2 {
		t.Fatalf("saved Final mutated through Result getter: %+v", second)
	}
}
