//go:build linux

package runsupervisor

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"golang.org/x/sys/unix"
)

func TestFailedRunSupervisorLaunchReleasesTheConsumedSourceCapability(t *testing.T) {
	source, err := sourcefiles.OpenSourceRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing-supervisor")
	if client, err := Launch(context.Background(), missing, nil, source); err == nil {
		_ = client.CloseLifeline()
		_ = client.Wait()
		t.Fatal("supervisor launch unexpectedly succeeded")
	}
	if source.StableRoot() != "" {
		t.Fatal("failed supervisor launch retained a source descriptor")
	}
}

func TestCancelledHandshakePreservesIndependentRunSupervisorExit(t *testing.T) {
	for range 20 {
		client, closePeer := exitedLaunchClient(t, nil, 94)
		cause := errors.New("parent SIGINT")
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		launched, err := client.finish(ctx, nil)
		closePeer()
		if launched != nil {
			t.Fatal("independently failed supervisor was returned as live")
		}
		if err == cause || !strings.Contains(err.Error(), "exit status 94") {
			t.Fatalf("cancelled handshake error=%v, want independent exit 94", err)
		}
	}
}

func TestCancelledHandshakePreservesMalformedRunSupervisorEvidence(t *testing.T) {
	client, closePeer := exitedLaunchClient(t, []byte("not-a-wire-frame"), 94)
	defer closePeer()
	cause := errors.New("parent SIGINT")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	launched, err := client.finish(ctx, nil)
	if launched != nil {
		t.Fatal("supervisor with malformed handshake was returned as live")
	}
	if err == cause || !strings.Contains(err.Error(), "supervisor ready handshake is invalid") ||
		!strings.Contains(err.Error(), "exit status 94") {
		t.Fatalf("cancelled malformed handshake error=%v", err)
	}
}

func TestCancelledReadyHandshakePreservesIndependentRunSupervisorExit(t *testing.T) {
	ready, err := encodeFrame(frameReady, nil)
	if err != nil {
		t.Fatal(err)
	}
	client, closePeer := exitedLaunchClient(t, ready, 94)
	defer closePeer()
	cause := errors.New("parent SIGINT")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	launched, err := client.finish(ctx, nil)
	if launched != nil || err == cause || !strings.Contains(err.Error(), "exit status 94") {
		t.Fatalf("ready then exit handshake launch=%v error=%v", launched, err)
	}
}

func TestCancelledHandshakePreservesIndependentRunSupervisorSignal(t *testing.T) {
	client, eventWriter, closePeer := launchClientForCommand(t,
		exec.Command("/bin/sh", "-c", "kill -SEGV $$"))
	defer closePeer()
	if err := eventWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Waitid(unix.P_PID, client.process.command.Process.Pid, nil,
		unix.WEXITED|unix.WNOWAIT, nil); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("parent SIGINT")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	launched, err := client.finish(ctx, nil)
	if launched != nil || err == cause || !strings.Contains(err.Error(), "segmentation fault") {
		t.Fatalf("signalled supervisor handshake launch=%v error=%v", launched, err)
	}
}

func TestCancelledHandshakePreservesIndependentRunSupervisorSIGKILL(t *testing.T) {
	client, eventWriter, closePeer := launchClientForCommand(t,
		exec.Command("/bin/sh", "-c", "exec sleep 30"))
	defer closePeer()
	if err := writeFrame(eventWriter, frameReady, nil); err != nil {
		t.Fatal(err)
	}
	if err := eventWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Kill(client.process.command.Process.Pid, unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := unix.Waitid(unix.P_PID, client.process.command.Process.Pid, nil,
		unix.WEXITED|unix.WNOWAIT, nil); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("parent SIGINT")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	launched, err := client.finish(ctx, nil)
	if launched != nil || err == cause || !strings.Contains(err.Error(), "signal: killed") {
		t.Fatalf("independently killed supervisor launch=%v error=%v", launched, err)
	}
}

func TestCancelledHandshakeFailsClosedWhenExitObservationIsUnavailable(t *testing.T) {
	client, eventWriter, closePeer := launchClientForCommand(t,
		exec.Command("/bin/sh", "-c", "exit 0"))
	defer closePeer()
	if err := writeFrame(eventWriter, frameReady, nil); err != nil {
		t.Fatal(err)
	}
	if err := eventWriter.Close(); err != nil {
		t.Fatal(err)
	}
	var status unix.WaitStatus
	if _, err := unix.Wait4(client.process.command.Process.Pid, &status, 0, nil); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("parent SIGINT")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	launched, err := client.finish(ctx, nil)
	if launched != nil || err == cause ||
		!strings.Contains(err.Error(), "observe supervisor exit before termination") {
		t.Fatalf("unobservable supervisor exit launch=%v error=%v", launched, err)
	}
}

func TestCancelledBlockedHandshakeReportsOnlyCausalCancellation(t *testing.T) {
	client, closePeer := blockedLaunchClient(t)
	defer closePeer()
	cause := errors.New("parent SIGINT")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	launched, err := client.finish(ctx, nil)
	if launched != nil || err != cause {
		t.Fatalf("blocked handshake launch=%v error=%v, want exact cancellation", launched, err)
	}
}

func TestCancelledReadyHandshakeReportsOnlyCausalCancellation(t *testing.T) {
	client, eventWriter, closePeer := launchClientForCommand(t,
		exec.Command("/bin/sh", "-c", "exec sleep 30"))
	defer closePeer()
	if err := writeFrame(eventWriter, frameReady, nil); err != nil {
		t.Fatal(err)
	}
	if err := eventWriter.Close(); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("parent SIGINT")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	launched, err := client.finish(ctx, nil)
	if launched != nil || err != cause {
		t.Fatalf("ready handshake launch=%v error=%v, want exact cancellation", launched, err)
	}
}

func TestCancelledHandshakePreservesAbortCleanupFailure(t *testing.T) {
	client, closePeer := blockedLaunchClient(t)
	defer closePeer()
	if err := client.controlWrite.Close(); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("parent SIGINT")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	launched, err := client.finish(ctx, nil)
	if launched != nil || err == cause || !errors.Is(err, os.ErrClosed) {
		t.Fatalf("cleanup-failed handshake launch=%v error=%v", launched, err)
	}
}

func TestFinishLaunchCommitsAcceptedRunSupervisorBeforeReturning(t *testing.T) {
	client, controlReader, eventWriter := launchPreCommitClient(t)
	committed := make(chan wireFrame, 1)
	peerErr := make(chan error, 1)
	go func() {
		if err := writeFrame(eventWriter, frameReady, nil); err != nil {
			peerErr <- err
			return
		}
		start, err := readFrame(controlReader)
		if err != nil || start.kind != frameStart {
			peerErr <- errors.Join(errors.New("missing supervisor start"), err)
			return
		}
		if err := writeFrame(eventWriter, frameAccepted, nil); err != nil {
			peerErr <- err
			return
		}
		commit, err := readFrame(controlReader)
		if err != nil {
			peerErr <- err
			return
		}
		committed <- commit
		peerErr <- nil
	}()

	launched, err := client.finish(context.Background(), []byte("request"))
	if err != nil || launched == nil {
		t.Fatalf("accepted launch = %v, %v", launched, err)
	}
	commit := <-committed
	if commit.kind != frameCommit || len(commit.payload) != 0 {
		t.Fatalf("supervisor commit = %#v", commit)
	}
	if err := <-peerErr; err != nil {
		t.Fatal(err)
	}
	if err := launched.CloseLifeline(); err != nil {
		t.Fatal(err)
	}
	if err := launched.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptedRunSupervisorCancellationClosesSourcePhaseWithoutCommitOrSIGKILL(t *testing.T) {
	client, controlReader, eventWriter := launchPreCommitClient(t)
	accepted := observeRunSupervisorFrame(client.eventRead, frameAccepted)
	if err := writeFrame(eventWriter, frameAccepted, nil); err != nil {
		t.Fatal(err)
	}
	finalPayload, err := encodeFinal(Final{Cancelled: true, Reason: CancelLifeline})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(eventWriter, frameFinal, finalPayload); err != nil {
		t.Fatal(err)
	}
	<-accepted.done

	cause := errors.New("parent SIGINT after source acceptance")
	if err := client.cancelSourceHandoff(accepted, cause); err != cause {
		t.Fatalf("accepted cancellation error = %v, want exact cause", err)
	}
	if client.process.command.ProcessState == nil || !client.process.command.ProcessState.Success() {
		t.Fatalf("pre-commit supervisor was not reaped normally: %v", client.process.command.ProcessState)
	}
	if frame, err := readFrame(controlReader); !errors.Is(err, io.EOF) {
		t.Fatalf("parent wrote a pre-cancellation commit: %#v, %v", frame, err)
	}
}

func TestAcceptedRunSupervisorControlCorruptionIsReapedAndReported(t *testing.T) {
	client, controlReader, eventWriter := launchPreCommitClient(t)
	accepted := observeRunSupervisorFrame(client.eventRead, frameAccepted)
	if err := writeFrame(eventWriter, frameAccepted, nil); err != nil {
		t.Fatal(err)
	}
	diagnostic := "supervisor control reached EOF before final"
	finalPayload, err := encodeFinal(Final{Cancelled: true, Reason: CancelInternal,
		InternalError: diagnostic})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(eventWriter, frameFinal, finalPayload); err != nil {
		t.Fatal(err)
	}
	<-accepted.done

	err = client.abortSourceHandoff(accepted)
	if err == nil || !strings.Contains(err.Error(), diagnostic) ||
		strings.Contains(err.Error(), "did not report lifeline cancellation") {
		t.Fatalf("pre-commit control corruption error = %v", err)
	}
	if client.process.command.ProcessState == nil || !client.process.command.ProcessState.Success() {
		t.Fatalf("control-corrupted supervisor was not reaped normally: %v", client.process.command.ProcessState)
	}
	if frame, err := readFrame(controlReader); !errors.Is(err, io.EOF) {
		t.Fatalf("parent committed after control corruption: %#v, %v", frame, err)
	}
}

func TestParentCancellationAfterStartNeverCommitsOrSIGKILLs(t *testing.T) {
	client, controlReader, eventWriter := launchPreCommitClient(t)
	if err := writeFrame(eventWriter, frameReady, nil); err != nil {
		t.Fatal(err)
	}
	cause := errors.New("parent cancelled after start")
	ctx, cancel := context.WithCancelCause(context.Background())
	type finishResult struct {
		client *RunSupervisorClient
		err    error
	}
	finished := make(chan finishResult, 1)
	go func() {
		launched, err := client.finish(ctx, []byte("request"))
		finished <- finishResult{client: launched, err: err}
	}()

	startRead := make(chan handshakeRead, 1)
	go func() {
		frame, err := readFrame(controlReader)
		startRead <- handshakeRead{frame: frame, err: err}
	}()
	var start handshakeRead
	select {
	case start = <-startRead:
	case <-time.After(time.Second):
		cancel(context.DeadlineExceeded)
		result := <-finished
		t.Fatalf("timed out waiting for parent start frame; launch=%v error=%v", result.client, result.err)
	}
	if start.err != nil || start.frame.kind != frameStart {
		t.Fatalf("parent start frame = %#v, %v", start.frame, start.err)
	}
	cancel(cause)
	if err := writeFrame(eventWriter, frameAccepted, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(eventWriter, frameFinal,
		mustFinalPayload(t, Final{Cancelled: true, Reason: CancelLifeline})); err != nil {
		t.Fatal(err)
	}

	var result finishResult
	select {
	case result = <-finished:
	case <-time.After(time.Second):
		t.Fatal("timed out joining cancelled launch")
	}
	if result.client != nil || result.err != cause {
		t.Fatalf("cancelled launch=%v error=%v, want exact cause", result.client, result.err)
	}
	if client.process.command.ProcessState == nil || !client.process.command.ProcessState.Success() {
		t.Fatalf("accepted supervisor was not reaped normally: %v", client.process.command.ProcessState)
	}
	if frame, err := readFrame(controlReader); !errors.Is(err, io.EOF) {
		t.Fatalf("parent committed after cancellation: %#v, %v", frame, err)
	}
}

func mustFinalPayload(t *testing.T, final Final) []byte {
	t.Helper()
	payload, err := encodeFinal(final)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func launchPreCommitClient(t *testing.T) (*clientLaunch, *os.File, *os.File) {
	t.Helper()
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	eventRead, eventWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cancelRead, cancelWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	wake, err := controlWriteWake()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sh", "-c", "while IFS= read -r value <&3; do :; done")
	command.ExtraFiles = []*os.File{cancelRead}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cancelRead.Close(); err != nil {
		t.Fatal(err)
	}
	launch := &clientLaunch{process: newSupervisorProcess(command), controlWrite: controlWrite,
		cancelWrite: cancelWrite, eventRead: eventRead, wake: wake}
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		closeFiles(controlRead, controlWrite, eventRead, eventWrite, cancelRead, cancelWrite, wake)
	})
	return launch, controlRead, eventWrite
}

func exitedLaunchClient(t *testing.T, event []byte, exitCode int) (*clientLaunch, func()) {
	t.Helper()
	client, eventWriter, closePeer := launchClientForCommand(t,
		exec.Command("/bin/sh", "-c", "exit "+strconv.Itoa(exitCode)))
	if len(event) != 0 {
		if _, err := eventWriter.Write(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := eventWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := unix.Waitid(unix.P_PID, client.process.command.Process.Pid, nil,
		unix.WEXITED|unix.WNOWAIT, nil); err != nil {
		t.Fatal(err)
	}
	return client, closePeer
}

func blockedLaunchClient(t *testing.T) (*clientLaunch, func()) {
	t.Helper()
	client, eventWriter, closePeer := launchClientForCommand(t,
		exec.Command("/bin/sh", "-c", "exec sleep 30"))
	if err := eventWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return client, closePeer
}

func launchClientForCommand(t *testing.T, command *exec.Cmd) (*clientLaunch, *os.File, func()) {
	t.Helper()
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	eventRead, eventWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cancelRead, cancelWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	wake, err := controlWriteWake()
	if err != nil {
		t.Fatal(err)
	}
	command.ExtraFiles = append(command.ExtraFiles, eventWrite)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	launch := &clientLaunch{process: newSupervisorProcess(command), controlWrite: controlWrite,
		cancelWrite: cancelWrite, eventRead: eventRead, wake: wake}
	closePeer := func() { closeFiles(controlRead, eventWrite, cancelRead) }
	t.Cleanup(func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		closeFiles(controlRead, controlWrite, eventRead, eventWrite, cancelRead, cancelWrite, wake)
	})
	return launch, eventWrite, closePeer
}
