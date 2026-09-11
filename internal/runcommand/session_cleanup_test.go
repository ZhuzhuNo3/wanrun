package runcommand

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestRunSessionOwnsLifelineCloseFailureOnceAndPreservesWaitFailure(t *testing.T) {
	client := newSessionClientRecorder()
	cancelFailure := errors.New("cancel failed")
	closeFailure := errors.New("lifeline close failed")
	waitFailure := errors.New("wait failed")
	client.cancelErr, client.closeErr, client.waitErr = cancelFailure, closeFailure, waitFailure
	inbox := newTerminationInbox()
	done := make(chan Result, 1)
	go func() {
		done <- (&runSession{ctx: context.Background(), client: client,
			driver: newCompletionSessionDriver(&staticSessionDisplay{}), inbox: inbox}).run()
	}()

	inbox.publish(terminationRequest{reason: runsupervisor.CancelUser})
	if reason := waitForSessionCancel(t, client.cancelAccepted, "cleanup error cancellation"); reason != runsupervisor.CancelUser {
		t.Fatalf("cancel reason = %v, want %v", reason, runsupervisor.CancelUser)
	}
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{Cancelled: true,
		Reason: runsupervisor.CancelUser})
	client.events <- final
	result := waitForSessionResult(t, done, "cleanup errors")
	for _, failure := range []error{cancelFailure, closeFailure, waitFailure} {
		if countErrorOccurrence(result.ParentError(), failure) != 1 {
			t.Fatalf("parent error occurrence for %v=%d in %v", failure,
				countErrorOccurrence(result.ParentError(), failure), result.ParentError())
		}
	}
	_, closeCalls, waitCalls := client.actions()
	if closeCalls != 1 || waitCalls != 1 {
		t.Fatalf("close/wait calls=%d/%d", closeCalls, waitCalls)
	}
}

func TestRunSessionKeepsDisplayDriverWaitAndLogFailuresInOwnedSlots(t *testing.T) {
	id, _ := transfernumber.New(1)
	displayFailure := errors.New("display request failed")
	driverFailure := errors.New("restore failed")
	waitFailure := errors.New("wait failed")
	logCloseFailure := errors.New("raw log close failed")
	client := newSessionClientRecorder()
	client.waitErr = waitFailure
	logs := &sessionLogRecorder{closeErr: logCloseFailure}
	driver := newCompletionSessionDriver(&staticSessionDisplay{showErr: displayFailure})
	driver.err = driverFailure
	output, _ := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStdout, []byte("output"))
	client.events <- output
	done := make(chan Result, 1)
	go func() {
		done <- (&runSession{ctx: context.Background(), client: client, logs: logs,
			driver: driver, inbox: newTerminationInbox()}).run()
	}()

	if reason := waitForSessionCancel(t, client.cancelAccepted, "display failure"); reason != runsupervisor.CancelInternal {
		t.Fatalf("cancel reason = %v, want %v", reason, runsupervisor.CancelInternal)
	}
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{Cancelled: true,
		Reason: runsupervisor.CancelInternal, Transfers: []runsupervisor.TransferResult{{Transfer: id}}})
	client.events <- final
	result := waitForSessionResult(t, done, "owned failure slots")
	for _, failure := range []error{displayFailure, driverFailure, waitFailure} {
		if countErrorOccurrence(result.ParentError(), failure) != 1 {
			t.Fatalf("parent occurrence for %v=%d in %v", failure,
				countErrorOccurrence(result.ParentError(), failure), result.ParentError())
		}
		if errors.Is(result.LogError(), failure) {
			t.Fatalf("parent failure %v entered LogError: %v", failure, result.LogError())
		}
	}
	if countErrorOccurrence(result.LogError(), logCloseFailure) != 1 {
		t.Fatalf("log occurrence for %v=%d in %v", logCloseFailure,
			countErrorOccurrence(result.LogError(), logCloseFailure), result.LogError())
	}
	if errors.Is(result.ParentError(), logCloseFailure) {
		t.Fatalf("log failure %v entered ParentError: %v", logCloseFailure, result.ParentError())
	}
}

func TestRunSessionRawLogWriteAndCloseFailuresRemainLogErrors(t *testing.T) {
	id, _ := transfernumber.New(1)
	writeFailure := errors.New("raw log write failed")
	closeFailure := errors.New("raw log close failed")
	client := newSessionClientRecorder()
	logs := &sessionLogRecorder{writeErr: writeFailure, closeErr: closeFailure}
	output, _ := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStdout, []byte("output"))
	client.events <- output
	done := make(chan Result, 1)
	go func() {
		done <- (&runSession{ctx: context.Background(), client: client, logs: logs,
			driver: newCompletionSessionDriver(&staticSessionDisplay{}), inbox: newTerminationInbox()}).run()
	}()

	if reason := waitForSessionCancel(t, client.cancelAccepted, "raw log write failure"); reason != runsupervisor.CancelInternal {
		t.Fatalf("cancel reason = %v, want %v", reason, runsupervisor.CancelInternal)
	}
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{Cancelled: true,
		Reason: runsupervisor.CancelInternal, Transfers: []runsupervisor.TransferResult{{Transfer: id}}})
	client.events <- final
	result := waitForSessionResult(t, done, "raw log failures")
	for _, failure := range []error{writeFailure, closeFailure} {
		if countErrorOccurrence(result.LogError(), failure) != 1 || errors.Is(result.ParentError(), failure) {
			t.Fatalf("failure %v parent/log=%v/%v", failure, result.ParentError(), result.LogError())
		}
	}
}

func TestRunSessionPlainDriverPublishesWriteAndFlushFailuresOnce(t *testing.T) {
	id, _ := transfernumber.New(1)
	writeFailure := errors.New("plain write failed")
	flushFailure := errors.New("plain final flush failed")
	stdout := &failOnWrite{failAt: 2, failure: writeFailure}
	stderr := &failOnWrite{failAt: 1, failure: flushFailure}
	driver, err := newPlainDisplayDriver(preparationRequest(t), stdout, stderr)
	if err != nil {
		t.Fatal(err)
	}
	client := newSessionClientRecorder()
	pending, _ := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStderr, []byte("pending"))
	failing, _ := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStdout, []byte("line\n"))
	client.events <- pending
	client.events <- failing
	done := make(chan Result, 1)
	go func() {
		done <- (&runSession{ctx: context.Background(), client: client, driver: driver,
			inbox: newTerminationInbox()}).run()
	}()

	if reason := waitForSessionCancel(t, client.cancelAccepted, "plain display write failure"); reason != runsupervisor.CancelInternal {
		t.Fatalf("cancel reason = %v, want %v", reason, runsupervisor.CancelInternal)
	}
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{Cancelled: true,
		Reason: runsupervisor.CancelInternal, Transfers: []runsupervisor.TransferResult{{Transfer: id}}})
	client.events <- final
	result := waitForSessionResult(t, done, "plain output failures")
	for _, failure := range []error{writeFailure, flushFailure} {
		if countErrorOccurrence(result.ParentError(), failure) != 1 {
			t.Fatalf("failure %v occurrence=%d in %v", failure,
				countErrorOccurrence(result.ParentError(), failure), result.ParentError())
		}
	}
	if result.LogError() != nil {
		t.Fatalf("plain output failure entered LogError: %v", result.LogError())
	}
}

func TestRunSessionEOFAndWaitFailureRemainParentEvidence(t *testing.T) {
	client := newSessionClientRecorder()
	close(client.events)
	waitFailure := errors.New("wait failed")
	client.waitErr = waitFailure
	result := (&runSession{ctx: context.Background(), client: client,
		driver: newCompletionSessionDriver(&staticSessionDisplay{}), inbox: newTerminationInbox()}).run()
	if _, ok := result.Final(); ok || !result.LiveSessionEstablished() ||
		!errors.Is(result.ParentError(), io.EOF) || !errors.Is(result.ParentError(), waitFailure) {
		t.Fatalf("result final/live/parent=%v/%v/%v", ok, result.LiveSessionEstablished(), result.ParentError())
	}
}

type failOnWrite struct {
	calls   int
	failAt  int
	failure error
}

func (writer *failOnWrite) Write(content []byte) (int, error) {
	writer.calls++
	if writer.calls == writer.failAt {
		return 0, writer.failure
	}
	return len(content), nil
}
