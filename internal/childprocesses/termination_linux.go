//go:build linux

package childprocesses

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type processTerminator struct {
	once    sync.Once
	done    chan struct{}
	outcome terminationOutcome
}

type terminationOutcome struct {
	supervisionErr error
	containmentErr error
}

func newProcessTerminator() *processTerminator {
	return &processTerminator{done: make(chan struct{})}
}

func (terminator *processTerminator) begin(reaper *reapedProcesses) {
	terminator.once.Do(func() {
		go func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), abortTimeout)
			defer cancel()
			terminator.outcome = terminateProcesses(cleanupCtx, reaper)
			close(terminator.done)
		}()
	})
}

func (terminator *processTerminator) wait(ctx context.Context) (terminationOutcome, error) {
	select {
	case <-terminator.done:
		return terminator.outcome, nil
	case <-ctx.Done():
		return terminationOutcome{}, ctx.Err()
	}
}

func terminateProcesses(ctx context.Context, reaper *reapedProcesses) terminationOutcome {
	if containmentObserved(reaper) {
		return terminationOutcome{}
	}
	if os.Getpid() != 1 {
		return waitForReaperCompletion(ctx, reaper)
	}
	var supervisionErr error
	reaperStopped := false
	supervisionErr, reaperStopped = captureReaperFailure(reaper, supervisionErr, reaperStopped)
	for _, signal := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL} {
		if err := unix.Kill(-1, signal); err != nil && err != unix.ESRCH {
			supervisionErr = errors.Join(supervisionErr, fmt.Errorf(
				"signal child process namespace with %s: %w", signal, err))
		}
		contained, waitErr := waitForContainment(ctx, reaper, signalGrace)
		supervisionErr, reaperStopped = captureReaperFailure(reaper, supervisionErr, reaperStopped)
		if contained {
			return terminationOutcome{supervisionErr: supervisionErr}
		}
		if waitErr != nil {
			return terminationOutcome{supervisionErr: supervisionErr, containmentErr: waitErr}
		}
	}
	if supervisionErr != nil {
		return terminationOutcome{supervisionErr: supervisionErr}
	}
	return waitForReaperCompletion(ctx, reaper)
}

func waitForReaperCompletion(ctx context.Context, reaper *reapedProcesses) terminationOutcome {
	select {
	case <-reaper.stopped:
		return terminationOutcome{supervisionErr: reaper.wait(context.Background())}
	case <-ctx.Done():
		return terminationOutcome{containmentErr: ctx.Err()}
	}
}

func waitForContainment(ctx context.Context, reaper *reapedProcesses,
	duration time.Duration,
) (bool, error) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-reaper.containment:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, nil
	}
}

func captureReaperFailure(reaper *reapedProcesses, current error, observed bool) (error, bool) {
	if observed {
		return current, true
	}
	select {
	case <-reaper.stopped:
		err := reaper.wait(context.Background())
		return errors.Join(current, err), true
	default:
		return current, false
	}
}

func containmentObserved(reaper *reapedProcesses) bool {
	select {
	case <-reaper.containment:
		return true
	default:
		return false
	}
}
