package runcommand

import (
	"context"
	"errors"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
)

func TestRunSessionCallerContextWinsQueuedTermination(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("caller stopped")
	cancel(cause)
	client := newSessionClientRecorder()
	client.cancelErr = errors.New("cancel pipe failed")
	inbox := newTerminationInbox()
	inbox.publish(terminationRequest{reason: runsupervisor.CancelUser})
	done := make(chan Result, 1)
	go func() {
		done <- (&runSession{ctx: ctx, client: client,
			driver: newCompletionSessionDriver(&staticSessionDisplay{}), inbox: inbox}).run()
	}()

	if reason := waitForSessionCancel(t, client.cancelAccepted, "caller cancellation"); reason != runsupervisor.CancelInternal {
		t.Fatalf("cancel reason = %v, want %v", reason, runsupervisor.CancelInternal)
	}
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{})
	client.events <- final
	result := waitForSessionResult(t, done, "caller cancellation")
	reason, exists := result.Cancellation()
	if !exists || reason != runsupervisor.CancelInternal || !errors.Is(result.ParentError(), cause) {
		t.Fatalf("reason=%v/%v parent=%v", reason, exists, result.ParentError())
	}
	cancels, closeCalls, waitCalls := client.actions()
	if len(cancels) != 1 || closeCalls != 1 || waitCalls != 1 {
		t.Fatalf("actions=%v close=%d wait=%d", cancels, closeCalls, waitCalls)
	}
}

func TestRunSessionControllerLossDoesNotReplaceAcceptedTermination(t *testing.T) {
	client := newSessionClientRecorder()
	inbox := newTerminationInbox()
	done := make(chan Result, 1)
	go func() {
		done <- (&runSession{ctx: context.Background(), client: client,
			driver: newCompletionSessionDriver(&staticSessionDisplay{}), inbox: inbox}).run()
	}()

	inbox.publish(terminationRequest{reason: runsupervisor.CancelUser})
	if reason := waitForSessionCancel(t, client.cancelAccepted, "user termination"); reason != runsupervisor.CancelUser {
		t.Fatalf("cancel reason = %v, want %v", reason, runsupervisor.CancelUser)
	}
	inbox.publish(terminationRequest{reason: runsupervisor.CancelLifeline, controllerLost: true})
	waitForSessionPhase(t, client.lifelineClosed, "controller loss accepted")
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{Cancelled: true,
		Reason: runsupervisor.CancelUser})
	client.events <- final
	result := waitForSessionResult(t, done, "controller loss after accepted termination")

	reason, _ := result.Cancellation()
	if reason != runsupervisor.CancelUser {
		t.Fatalf("first reason=%v", reason)
	}
	cancels, closeCalls, waitCalls := client.actions()
	if len(cancels) != 1 || closeCalls != 1 || waitCalls != 1 {
		t.Fatalf("actions=%v close=%d wait=%d", cancels, closeCalls, waitCalls)
	}
}

func TestRunSessionAcceptedTerminationIgnoresLateCallerCancellation(t *testing.T) {
	for _, controllerLost := range []bool{false, true} {
		name := "signal"
		if controllerLost {
			name = "controller-loss"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			callerFailure := errors.New("late caller cancellation")
			client := newSessionClientRecorder()
			inbox := newTerminationInbox()
			done := make(chan Result, 1)
			go func() {
				done <- (&runSession{ctx: ctx, client: client,
					driver: newCompletionSessionDriver(&staticSessionDisplay{}), inbox: inbox}).run()
			}()
			request := terminationRequest{reason: runsupervisor.CancelUser, controllerLost: controllerLost}
			if controllerLost {
				request.reason = runsupervisor.CancelLifeline
			}
			inbox.publish(request)
			if controllerLost {
				waitForSessionPhase(t, client.lifelineClosed, "controller loss accepted")
			} else if reason := waitForSessionCancel(t, client.cancelAccepted, "signal accepted"); reason != request.reason {
				t.Fatalf("cancel reason = %v, want %v", reason, request.reason)
			}

			cancel(callerFailure)
			final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{Cancelled: true, Reason: request.reason})
			client.events <- final
			result := waitForSessionResult(t, done, name)
			reason, exists := result.Cancellation()
			if !exists || reason != request.reason || errors.Is(result.ParentError(), callerFailure) {
				t.Fatalf("reason=%v/%v parent=%v", reason, exists, result.ParentError())
			}
			cancels, closeCalls, waitCalls := client.actions()
			wantCancels := 1
			if controllerLost {
				wantCancels = 0
			}
			if len(cancels) != wantCancels || closeCalls != 1 || waitCalls != 1 {
				t.Fatalf("actions cancel/close/wait=%v/%d/%d", cancels, closeCalls, waitCalls)
			}
		})
	}
}
