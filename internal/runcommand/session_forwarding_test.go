package runcommand

import (
	"context"
	"errors"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestRunSessionStartsForwarderBeforePotentiallyBlockingDisplayDriver(t *testing.T) {
	client := newSessionClientRecorder()
	display := newBlockingBeginDisplay()
	t.Cleanup(display.phase.Release)
	driver := newCompletionSessionDriver(display)
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{})
	client.events <- final
	done := make(chan Result, 1)
	go func() {
		done <- (&runSession{ctx: context.Background(), client: client, driver: driver,
			inbox: newTerminationInbox()}).run()
	}()

	waitForSessionPhase(t, client.nextStarted, "event forwarding started")
	waitForSessionPhase(t, display.phase.reached, "display Begin entered")
	display.phase.Release()
	result := waitForSessionResult(t, done, "blocked display startup")
	if _, ok := result.Final(); !ok {
		t.Fatal("Final was not preserved")
	}
}

func TestRunSessionKeepsLogsOpenWhileBlockedForwarderDrainsAfterCancellation(t *testing.T) {
	id, _ := transfernumber.New(1)
	client := newSessionClientRecorder()
	writePhase := newSessionPhaseGate()
	t.Cleanup(writePhase.Release)
	logs := &sessionLogRecorder{writePhase: writePhase}
	driver := newCompletionSessionDriver(&staticSessionDisplay{})
	inbox := newTerminationInbox()
	output, _ := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStdout, []byte("tail"))
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{Cancelled: true,
		Reason: runsupervisor.CancelUser, Transfers: []runsupervisor.TransferResult{{Transfer: id}}})
	client.events <- output
	client.events <- final
	done := make(chan Result, 1)
	go func() {
		done <- (&runSession{ctx: context.Background(), client: client, logs: logs,
			driver: driver, inbox: inbox}).run()
	}()

	waitForSessionPhase(t, writePhase.reached, "raw log write blocked")
	inbox.publish(terminationRequest{reason: runsupervisor.CancelUser})
	if reason := waitForSessionCancel(t, client.cancelAccepted, "log drain"); reason != runsupervisor.CancelUser {
		t.Fatalf("cancel reason = %v, want %v", reason, runsupervisor.CancelUser)
	}
	if logs.isClosed() {
		t.Fatal("logs closed before the forwarder drained")
	}
	writePhase.Release()

	result := waitForSessionResult(t, done, "blocked log drain")
	if reason, ok := result.Cancellation(); !ok || reason != runsupervisor.CancelUser {
		t.Fatalf("cancellation=%v/%v", reason, ok)
	}
	cancels, closeCalls, waitCalls := client.actions()
	if len(cancels) != 1 || closeCalls != 1 || waitCalls != 1 {
		t.Fatalf("actions cancel/close/wait=%v/%d/%d", cancels, closeCalls, waitCalls)
	}
}

func TestRunSessionDisplayFailureRequestsOneImmediateCancelAndStillDrainsFinal(t *testing.T) {
	id, _ := transfernumber.New(1)
	client := newSessionClientRecorder()
	failure := errors.New("display failed")
	driver := newCompletionSessionDriver(&staticSessionDisplay{showErr: failure})
	output, _ := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStdout, []byte("output"))
	client.events <- output
	done := make(chan Result, 1)
	go func() {
		done <- (&runSession{ctx: context.Background(), client: client, driver: driver,
			inbox: newTerminationInbox()}).run()
	}()

	if reason := waitForSessionCancel(t, client.cancelAccepted, "display failure"); reason != runsupervisor.CancelInternal {
		t.Fatalf("cancel reason = %v, want %v", reason, runsupervisor.CancelInternal)
	}
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{Cancelled: true,
		Reason: runsupervisor.CancelInternal, Transfers: []runsupervisor.TransferResult{{Transfer: id}}})
	client.events <- final
	result := waitForSessionResult(t, done, "display failure final drain")

	if !errors.Is(result.ParentError(), failure) {
		t.Fatalf("parent error=%v", result.ParentError())
	}
	if _, available := result.Final(); !available {
		t.Fatal("Final was not drained after display failure")
	}
	cancels, _, _ := client.actions()
	if len(cancels) != 1 || cancels[0] != runsupervisor.CancelInternal {
		t.Fatalf("cancellations=%v", cancels)
	}
}
