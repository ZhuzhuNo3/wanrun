//go:build linux

package runsupervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
)

// Launch consumes one descriptor-bound source capability, starts the same executable as a
// PID-namespace init, and returns only after the supervisor has accepted both the request and its
// inherited source descriptor. On failure Launch releases whichever side still owns the source.
func Launch(ctx context.Context, executable string, request []byte,
	source *sourcefiles.SourceRoot,
) (*RunSupervisorClient, error) {
	if source == nil {
		return nil, errors.New("supervisor source capability is required")
	}
	if ctx == nil || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return nil, errors.Join(errors.New("supervisor launch executable is invalid"), source.Close())
	}
	if err := validateStartRequest(request); err != nil {
		return nil, errors.Join(err, source.Close())
	}
	descriptor, err := source.TransferDescriptor()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("transfer supervisor source capability: %w", err), source.Close())
	}
	return launchRequest(ctx, executable, request, descriptor)
}

type clientLaunch struct {
	process *supervisorProcess

	controlRead  *os.File
	controlWrite *os.File
	eventRead    *os.File
	eventWrite   *os.File
	cancelRead   *os.File
	cancelWrite  *os.File
	wake         *os.File
	source       *os.File
}

func launchRequest(ctx context.Context, executable string, request []byte, source *os.File) (*RunSupervisorClient, error) {
	if ctx == nil || !filepath.IsAbs(executable) || filepath.Clean(executable) != executable {
		return nil, errors.Join(fmt.Errorf("supervisor launch executable is invalid"), closeSourceFile(source))
	}
	if err := validateStartRequest(request); err != nil {
		return nil, errors.Join(err, closeSourceFile(source))
	}
	launch, err := startClientLaunch(executable, source)
	if err != nil {
		return nil, err
	}
	return launch.finish(ctx, request)
}

func (launch *clientLaunch) finish(ctx context.Context, request []byte) (*RunSupervisorClient, error) {
	ready := observeRunSupervisorFrame(launch.eventRead, frameReady)
	select {
	case <-ready.done:
		if err := validateRunSupervisorHandshake(ready.result(), frameReady); err != nil {
			stopped := launch.stopBeforeStart(ready)
			return nil, errors.Join(err, stopped.failure())
		}
		if cause := cancellationCause(ctx); cause != nil {
			return nil, launch.stopBeforeStart(ready).cancellation(cause)
		}
	case <-ctx.Done():
		return nil, launch.stopBeforeStart(ready).cancellation(cancellationCause(ctx))
	}
	if err := writeLaunchStartFrame(ctx, launch.controlWrite, launch.wake, frameStart, request); err != nil {
		stopped := launch.stopBeforeStart(ready)
		if cause := cancellationCause(ctx); cause != nil {
			return nil, stopped.cancellation(cause)
		}
		return nil, errors.Join(fmt.Errorf("start supervisor work: %w", err), stopped.failure())
	}
	accepted := observeRunSupervisorFrame(launch.eventRead, frameAccepted)
	select {
	case <-accepted.done:
		read := accepted.result()
		if err := validateRunSupervisorHandshake(read, frameAccepted); err != nil {
			return nil, errors.Join(errors.New("supervisor source handoff was not accepted"), err,
				launch.abortSourceHandoff(accepted))
		}
		if cause := cancellationCause(ctx); cause != nil {
			return nil, launch.cancelSourceHandoff(accepted, cause)
		}
	case <-ctx.Done():
		return nil, launch.cancelSourceHandoff(accepted, cancellationCause(ctx))
	}
	if err := writeAtomicControlFile(launch.controlWrite, launch.wake, frameCommit, nil); err != nil {
		return nil, errors.Join(fmt.Errorf("commit supervisor work: %w", err), launch.abortSourceHandoff(accepted))
	}
	client := launch.takeClient()
	if err := launch.closeSource(); err != nil {
		stopErr := errors.Join(client.Cancel(CancelInternal), client.CloseLifeline(), client.Wait())
		return nil, errors.Join(fmt.Errorf("release parent source descriptor: %w", err), stopErr)
	}
	return client, nil
}

type handshakeRead struct {
	frame       wireFrame
	err         error
	beforeAbort bool
}

type handshakeObservation struct {
	mu       sync.Mutex
	aborting bool
	expected frameKind
	read     handshakeRead
	done     chan struct{}
}

func observeRunSupervisorFrame(events io.Reader, expected frameKind) *handshakeObservation {
	observation := &handshakeObservation{expected: expected, done: make(chan struct{})}
	go func() {
		frame, err := readFrame(events)
		observation.mu.Lock()
		observation.read = handshakeRead{frame: frame, err: err, beforeAbort: !observation.aborting}
		close(observation.done)
		observation.mu.Unlock()
	}()
	return observation
}

func (observation *handshakeObservation) beginAbort() {
	observation.mu.Lock()
	observation.aborting = true
	observation.mu.Unlock()
}

func (observation *handshakeObservation) result() handshakeRead {
	<-observation.done
	observation.mu.Lock()
	defer observation.mu.Unlock()
	return observation.read
}

func validateRunSupervisorHandshake(read handshakeRead, expected frameKind) error {
	if read.err != nil || read.frame.kind != expected || len(read.frame.payload) != 0 {
		name := "ready"
		if expected == frameAccepted {
			name = "source acceptance"
		}
		return errors.Join(fmt.Errorf("supervisor %s handshake is invalid", name), read.err)
	}
	return nil
}

func cancellationCause(ctx context.Context) error {
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}

type stoppedLaunch struct {
	ready             handshakeRead
	expected          frameKind
	exitedBeforeKill  bool
	exitObserveErr    error
	killAfterLiveRead bool
	killErr           error
	waitErr           error
	closeErr          error
}

func (launch *clientLaunch) abortSourceHandoff(accepted *handshakeObservation) error {
	accepted.beginAbort()
	closeErr := launch.closeCancellation()
	outcome := launch.process.waitForExit(supervisorReapLimit)
	read := accepted.result()
	acceptedErr := validateRunSupervisorHandshake(read, frameAccepted)
	finalErr := launch.preCommitFinalError(acceptedErr)
	closeErr = errors.Join(closeErr, launch.closeDescriptors())
	var shutdownErr error
	if outcome.timedOut {
		shutdownErr = errors.New("supervisor did not finish pre-commit source cleanup before timeout")
	} else if outcome.waitErr != nil {
		shutdownErr = fmt.Errorf("reap supervisor before work commit: %w", outcome.waitErr)
	}
	return errors.Join(acceptedErr, finalErr, shutdownErr, outcome.killErr, closeErr)
}

func (launch *clientLaunch) cancelSourceHandoff(accepted *handshakeObservation, cause error) error {
	if failure := launch.abortSourceHandoff(accepted); failure != nil {
		return errors.Join(cause, failure)
	}
	return cause
}

func (launch *clientLaunch) preCommitFinalError(acceptedErr error) error {
	if acceptedErr != nil {
		return nil
	}
	frame, err := readFrame(launch.eventRead)
	if err != nil {
		return fmt.Errorf("read supervisor final before commit: %w", err)
	}
	if frame.kind != frameFinal {
		return fmt.Errorf("supervisor emitted %d instead of final before commit", frame.kind)
	}
	final, err := decodeFinal(frame.payload)
	if err != nil {
		return fmt.Errorf("decode supervisor final before commit: %w", err)
	}
	var failures []error
	if final.InternalError != "" {
		failures = append(failures, errors.New(final.InternalError))
	}
	if final.RunError != "" {
		failures = append(failures, errors.New(final.RunError))
	}
	if final.CleanupError != "" {
		failures = append(failures, errors.New(final.CleanupError))
	}
	if !final.Cancelled {
		failures = append(failures, errors.New("supervisor pre-commit final did not report cancellation"))
	}
	return errors.Join(failures...)
}

func (launch *clientLaunch) stopBeforeStart(observation *handshakeObservation) stoppedLaunch {
	observation.beginAbort()
	exited, observeErr := launch.process.exitedBeforeKill()
	killAfterLiveRead := observeErr == nil && !exited
	var killErr, waitErr error
	if !exited {
		killErr, waitErr = launch.process.terminate()
	} else {
		waitErr = launch.process.wait()
	}
	read := observation.result()
	closeErr := launch.closeDescriptors()
	return stoppedLaunch{ready: read, expected: observation.expected,
		exitedBeforeKill: exited, exitObserveErr: observeErr,
		killAfterLiveRead: killAfterLiveRead, killErr: killErr, waitErr: waitErr, closeErr: closeErr}
}

func (stopped stoppedLaunch) cancellation(cause error) error {
	var independent error
	if err := validateRunSupervisorHandshake(stopped.ready, stopped.expected); err != nil &&
		!(errors.Is(stopped.ready.err, io.EOF) && !stopped.ready.beforeAbort && stopped.expectedKill()) {
		independent = err
	}
	if !stopped.expectedKill() {
		independent = errors.Join(independent, stopped.unexpectedExit())
	}
	if failure := errors.Join(independent, stopped.actionFailure()); failure != nil {
		return errors.Join(cause, failure)
	}
	return cause
}

func (stopped stoppedLaunch) failure() error {
	if stopped.expectedKill() {
		return stopped.actionFailure()
	}
	return errors.Join(stopped.unexpectedExit(), stopped.actionFailure())
}

func (stopped stoppedLaunch) expectedKill() bool {
	if !stopped.killAfterLiveRead || stopped.exitedBeforeKill || stopped.exitObserveErr != nil || stopped.killErr != nil {
		return false
	}
	var exit *exec.ExitError
	if !errors.As(stopped.waitErr, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGKILL
}

func (stopped stoppedLaunch) unexpectedExit() error {
	if stopped.waitErr == nil {
		return errors.New("supervisor exited before startup completed")
	}
	return fmt.Errorf("reap supervisor before startup completed: %w", stopped.waitErr)
}

func (stopped stoppedLaunch) actionFailure() error {
	killErr := stopped.killErr
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	return errors.Join(stopped.exitObserveErr, killErr, stopped.closeErr)
}

func (launch *clientLaunch) closeCancellation() error {
	if launch.cancelWrite == nil {
		return nil
	}
	file := launch.cancelWrite
	launch.cancelWrite = nil
	return file.Close()
}

func (launch *clientLaunch) closeSource() error {
	file := launch.source
	launch.source = nil
	return closeSourceFile(file)
}

func (launch *clientLaunch) closeDescriptors() error {
	err := closeFilesWithErrors(launch.controlRead, launch.controlWrite, launch.eventRead,
		launch.eventWrite, launch.cancelRead, launch.cancelWrite, launch.wake, launch.source)
	launch.controlRead, launch.controlWrite = nil, nil
	launch.eventRead, launch.eventWrite = nil, nil
	launch.cancelRead, launch.cancelWrite = nil, nil
	launch.wake, launch.source = nil, nil
	return err
}

func (launch *clientLaunch) takeClient() *RunSupervisorClient {
	client := &RunSupervisorClient{
		process:  launch.process,
		controls: newControlSender(launch.controlWrite, launch.cancelWrite, launch.wake),
		events:   newEventReceiver(launch.eventRead),
	}
	launch.process = nil
	launch.controlWrite, launch.cancelWrite, launch.eventRead, launch.wake = nil, nil, nil, nil
	return client
}
