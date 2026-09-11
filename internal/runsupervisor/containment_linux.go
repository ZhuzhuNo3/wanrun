//go:build linux

package runsupervisor

import (
	"context"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

func callContainedWork(ctx context.Context, run *RunSupervisor, work Work) (final Final) {
	defer func() {
		if recovered := recover(); recovered != nil {
			final = Final{Cancelled: true, Reason: CancelInternal,
				InternalError: boundedDiagnostic(fmt.Sprintf("supervisor work panic: %v", recovered))}
		}
		if err := containDescendants(); err != nil {
			final.Cancelled, final.Reason = true, CancelInternal
			final.InternalError = joinDiagnostic(final.InternalError, err.Error())
		}
	}()
	return work(ctx, run)
}

func containDescendants() error {
	foundLive := false
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		switch {
		case err == unix.ECHILD:
			if foundLive {
				return fmt.Errorf("supervisor work left live descendants")
			}
			return nil
		case err != nil && err != unix.EINTR:
			return fmt.Errorf("reap supervisor descendant: %w", err)
		case err == unix.EINTR || pid > 0:
			continue
		case pid == 0:
			foundLive = true
			if err := unix.Kill(-1, syscall.SIGKILL); err != nil && err != unix.ESRCH {
				return fmt.Errorf("contain supervisor descendants: %w", err)
			}
			if err := reapKilledDescendants(); err != nil {
				return err
			}
		}
	}
}

func reapKilledDescendants() error {
	for {
		var status unix.WaitStatus
		_, err := unix.Wait4(-1, &status, 0, nil)
		if err == unix.ECHILD {
			return nil
		}
		if err != nil && err != unix.EINTR {
			return fmt.Errorf("reap contained supervisor descendant: %w", err)
		}
	}
}
