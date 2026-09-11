//go:build linux

package childprocesses

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

func TestReaperErrorStopsSupervisionWithoutConfirmingContainment(t *testing.T) {
	child, closeWriter := launchedChildForReaperTest(t, 101)
	defer closeWriter()
	waitErr := errors.New("wait4 supervision failed")
	reaper := startReaperWith([]*launchedChild{child}, scriptedWait4(
		wait4Result{pid: child.pid, status: unix.WaitStatus(7 << 8)},
		wait4Result{err: waitErr},
	))
	outputWake, controlWake := testProcessWakes(t)
	session := newProcessSet(context.Background(), []*launchedChild{child}, reaper, outputWake, controlWake)
	drainSessionEvidence(session)

	waited := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := session.Wait()
		waited <- struct {
			result Result
			err    error
		}{result: result, err: err}
	}()
	var result Result
	var err error
	select {
	case outcome := <-waited:
		result, err = outcome.result, outcome.err
	case <-time.After(time.Second):
		t.Fatal("reaper failure left output reader waiting for EOF")
	}
	if !errors.Is(err, waitErr) || !result.Cancelled || len(result.Transfers) != 1 ||
		result.Transfers[0].ExitCode != 7 {
		t.Fatalf("reaper failure result=%#v err=%v", result, err)
	}
	select {
	case <-session.ContainmentDone():
		t.Fatal("non-ECHILD reaper failure confirmed containment")
	default:
	}
}

func TestStartupAbortReturnsOwnerWhenReaperCannotConfirmContainment(t *testing.T) {
	child, closeWriter := launchedChildForReaperTest(t, 102)
	defer closeWriter()
	waitErr := errors.New("wait4 supervision failed")
	reaper := startReaperWith([]*launchedChild{child}, scriptedWait4(wait4Result{err: waitErr}))
	startupErr := errors.New("prepare failed")

	wakes, err := newProcessWakeOwner()
	if err != nil {
		t.Fatal(err)
	}
	launch := &processSetLaunch{wakes: wakes, children: []*launchedChild{child}, reaper: reaper}
	owner, err := launch.abortBeforeBarrier(startupErr)
	if owner == nil || !errors.Is(err, startupErr) || errors.Is(err, waitErr) {
		t.Fatalf("abort owner=%v err=%v", owner, err)
	}
	drainSessionEvidence(owner)
	<-owner.completion.done
	stopped := owner.FinishStartFailure(context.Background())
	if !errors.Is(stopped.SupervisionError, waitErr) || stopped.ContainmentError == nil ||
		errors.Is(stopped.ContainmentError, waitErr) {
		t.Fatalf("aborted owner failed-start result = %#v", stopped)
	}
	select {
	case <-owner.ContainmentDone():
		t.Fatal("aborted owner falsely confirmed containment")
	default:
	}
}

func TestReaperKeepsDirectResultsBoundedAcrossManyAdoptedChildren(t *testing.T) {
	child, closeWriter := launchedChildForReaperTest(t, 103)
	const adopted = 100_000
	call := 0
	reaper := startReaperWith([]*launchedChild{child}, func(status *unix.WaitStatus) (int, error) {
		call++
		if call <= adopted {
			*status = 0
			return 10_000 + call, nil
		}
		if call == adopted+1 {
			*status = unix.WaitStatus(3 << 8)
			return child.pid, nil
		}
		return -1, unix.ECHILD
	})
	outputWake, controlWake := testProcessWakes(t)
	session := newProcessSet(context.Background(), []*launchedChild{child}, reaper, outputWake, controlWake)
	drainSessionEvidence(session)
	// ECHILD means the real child side of every PTY has disappeared. Model that
	// kernel fact before waiting for the owner to drain its master.
	closeWriter()

	result, err := session.Wait()
	if err != nil || len(result.Transfers) != 1 || result.Transfers[0].ExitCode != 3 {
		t.Fatalf("adopted-child stress result=%#v err=%v", result, err)
	}
	select {
	case <-session.ContainmentDone():
	case <-time.After(time.Second):
		t.Fatal("ECHILD did not confirm containment")
	}
}

type wait4Result struct {
	pid    int
	status unix.WaitStatus
	err    error
}

func scriptedWait4(results ...wait4Result) childWait4 {
	index := 0
	return func(status *unix.WaitStatus) (int, error) {
		if index >= len(results) {
			return -1, unix.ECHILD
		}
		result := results[index]
		index++
		*status = result.status
		return result.pid, result.err
	}
}

func launchedChildForReaperTest(t *testing.T, pid int) (*launchedChild, func()) {
	t.Helper()
	id, _ := transfernumber.New(1)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	return &launchedChild{transfer: id, pid: pid,
		outputs: []preparedOutput{{stream: StreamPTY, file: reader}}}, func() { _ = writer.Close() }
}

func drainSessionEvidence(session *ProcessSet) {
	go func() {
		for range session.Output() {
		}
	}()
	go func() {
		for range session.Statuses() {
		}
	}()
}
