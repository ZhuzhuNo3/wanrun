//go:build linux

package childprocesses

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/sys/unix"
)

type processExit struct {
	pid    int
	status unix.WaitStatus
}

type childWait4 func(*unix.WaitStatus) (int, error)

// reapedProcesses retains only direct-child results. Adopted descendants are reaped and wake
// observers, but their histories are never accumulated.
type reapedProcesses struct {
	stopped     chan struct{}
	containment chan struct{}
	changed     chan struct{}

	mu          sync.Mutex
	directOrder []int
	direct      map[int]processExit
	stopErr     error
	complete    bool
	confirmed   bool
}

func startReaper(children []*launchedChild) *reapedProcesses {
	return startReaperWith(children, systemWait4)
}

func startReaperWith(children []*launchedChild, wait4 childWait4) *reapedProcesses {
	directOrder := make([]int, len(children))
	direct := make(map[int]processExit, len(children))
	for index, child := range children {
		directOrder[index] = child.pid
		direct[child.pid] = processExit{}
	}
	result := &reapedProcesses{
		stopped:     make(chan struct{}),
		containment: make(chan struct{}),
		changed:     make(chan struct{}, 1),
		directOrder: directOrder,
		direct:      direct,
	}
	go func() {
		confirmed, err := reapAll(wait4, result.recordExit)
		result.stop(confirmed, err)
	}()
	return result
}

func systemWait4(status *unix.WaitStatus) (int, error) {
	return unix.Wait4(-1, status, 0, nil)
}

func reapAll(wait4 childWait4, observed func(processExit)) (bool, error) {
	for {
		var status unix.WaitStatus
		pid, err := wait4(&status)
		if err == unix.EINTR {
			continue
		}
		if err == unix.ECHILD {
			return true, nil
		}
		if err != nil {
			return false, fmt.Errorf("reap child process: %w", err)
		}
		observed(processExit{pid: pid, status: status})
	}
}

func (processes *reapedProcesses) recordExit(exit processExit) {
	processes.mu.Lock()
	if _, direct := processes.direct[exit.pid]; direct {
		processes.direct[exit.pid] = exit
	}
	processes.mu.Unlock()
	processes.notify()
}

func (processes *reapedProcesses) stop(confirmed bool, err error) {
	processes.mu.Lock()
	processes.stopErr = err
	processes.complete = true
	processes.confirmed = confirmed
	processes.mu.Unlock()
	processes.notify()
	if confirmed {
		close(processes.containment)
	}
	// Publish the terminal supervision result only after publishing containment.
	// A caller that observes stopped may therefore decide cleanup immediately
	// without racing the ECHILD evidence.
	close(processes.stopped)
}

func (processes *reapedProcesses) notify() {
	select {
	case processes.changed <- struct{}{}:
	default:
	}
}

func (processes *reapedProcesses) nextDirect(seen map[int]bool) (processExit, bool, error) {
	for {
		processes.mu.Lock()
		for _, pid := range processes.directOrder {
			exit := processes.direct[pid]
			if !seen[pid] && exit.pid != 0 {
				processes.mu.Unlock()
				return exit, true, nil
			}
		}
		if processes.complete {
			err := processes.stopErr
			processes.mu.Unlock()
			return processExit{}, false, err
		}
		processes.mu.Unlock()
		<-processes.changed
	}
}

func (processes *reapedProcesses) wait(ctx context.Context) error {
	select {
	case <-processes.stopped:
		processes.mu.Lock()
		defer processes.mu.Unlock()
		if processes.confirmed {
			return nil
		}
		if processes.stopErr != nil {
			return processes.stopErr
		}
		return fmt.Errorf("process reaper stopped without containment evidence")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (processes *reapedProcesses) containmentConfirmed() bool {
	processes.mu.Lock()
	defer processes.mu.Unlock()
	return processes.confirmed
}
