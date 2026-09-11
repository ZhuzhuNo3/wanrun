//go:build linux

package childprocesses

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

func TestProcessCompletionPublishesOnceAndFreezes(t *testing.T) {
	completion := newProcessCompletion()
	id, _ := transfernumber.New(1)
	completion.markCancelled()
	completion.recordSupervisionFailure(errors.New("failure-before-publish"))

	const waiters = 16
	type waitOutcome struct {
		result Result
		err    error
	}
	waited := make(chan waitOutcome, waiters)
	var waitersReady sync.WaitGroup
	waitersReady.Add(waiters)
	for range waiters {
		go func() {
			waitersReady.Done()
			result, err := completion.wait()
			waited <- waitOutcome{result: result, err: err}
		}()
	}
	waitersReady.Wait()
	select {
	case <-waited:
		t.Fatal("completion waiter returned before publication")
	default:
	}

	start := make(chan struct{})
	var competing sync.WaitGroup
	for _, exitCode := range []int{7, 9} {
		exitCode := exitCode
		competing.Add(1)
		go func() {
			defer competing.Done()
			<-start
			completion.publish([]TransferResult{{Transfer: id, ExitCode: exitCode}}, 1,
				fmt.Errorf("publish-%d", exitCode))
		}()
	}
	const failures = 32
	for index := range failures {
		index := index
		competing.Add(1)
		go func() {
			defer competing.Done()
			<-start
			completion.markCancelled()
			completion.recordSupervisionFailure(fmt.Errorf("failure-%d", index))
		}()
	}
	close(start)
	competing.Wait()

	first := <-waited
	if !first.result.Cancelled || len(first.result.Transfers) != 1 || first.err == nil ||
		!strings.Contains(first.err.Error(), "failure-before-publish") {
		t.Fatalf("published completion=%#v error=%v", first.result, first.err)
	}
	exitCode := first.result.Transfers[0].ExitCode
	if exitCode != 7 && exitCode != 9 ||
		!strings.Contains(first.err.Error(), fmt.Sprintf("publish-%d", exitCode)) {
		t.Fatalf("completion mixed competing publications: result=%#v error=%v", first.result,
			first.err)
	}
	for range waiters - 1 {
		outcome := <-waited
		if !outcome.result.Cancelled || len(outcome.result.Transfers) != 1 ||
			outcome.result.Transfers[0].ExitCode != exitCode || outcome.err == nil ||
			outcome.err.Error() != first.err.Error() {
			t.Fatalf("completion waiters disagreed: first=%#v/%v next=%#v/%v",
				first.result, first.err, outcome.result, outcome.err)
		}
	}

	completion.markCancelled()
	completion.recordSupervisionFailure(errors.New("late failure"))
	completion.publish([]TransferResult{{Transfer: id, ExitCode: 9}}, 1,
		errors.New("late publish"))
	first.result.Transfers[0].ExitCode = 99
	second, secondErr := completion.wait()
	if second.Transfers[0].ExitCode != exitCode || secondErr == nil ||
		secondErr.Error() != first.err.Error() || strings.Contains(secondErr.Error(), "late failure") ||
		strings.Contains(secondErr.Error(), "late publish") {
		t.Fatalf("completion was not frozen or defensively copied: first=%#v/%v second=%#v/%v",
			first.result, first.err, second, secondErr)
	}
}

func TestConcurrentCancelSharesTerminationButKeepsCallerContext(t *testing.T) {
	id, _ := transfernumber.New(1)
	releaseReaper := make(chan struct{})
	waitCalls := 0
	reaper := startReaperWith([]*launchedChild{{transfer: id, pid: 301}},
		func(status *unix.WaitStatus) (int, error) {
			if waitCalls == 0 {
				<-releaseReaper
				waitCalls++
				*status = 0
				return 301, nil
			}
			return -1, unix.ECHILD
		})
	outputWake, controlWake := testProcessWakes(t)
	processes := newProcessSet(context.Background(), []*launchedChild{{transfer: id, pid: 301}},
		reaper, outputWake, controlWake)

	expired, expire := context.WithCancel(context.Background())
	expire()
	if err := processes.Cancel(expired); !errors.Is(err, context.Canceled) {
		t.Fatalf("expired cancellation error=%v", err)
	}
	const callers = 16
	errorsByCaller := make(chan error, callers)
	for range callers {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			errorsByCaller <- processes.Cancel(ctx)
		}()
	}
	close(releaseReaper)
	for range callers {
		if err := <-errorsByCaller; err != nil {
			t.Fatalf("shared cancellation error=%v", err)
		}
	}
	drainSessionEvidence(processes)
	result, err := processes.Wait()
	if err != nil || !result.Cancelled {
		t.Fatalf("cancelled process result=%#v error=%v", result, err)
	}
}

func TestWaitClosesRuntimeResourcesAndReturnsDefensiveResult(t *testing.T) {
	id, _ := transfernumber.New(1)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	readerFD := int(reader.Fd())
	_ = writer.Close()
	child := &launchedChild{transfer: id, pid: 401,
		outputs: []preparedOutput{{stream: StreamStdout, file: reader}}}
	reaper := startReaperWith([]*launchedChild{child}, scriptedWait4(
		wait4Result{pid: child.pid, status: unix.WaitStatus(3 << 8)},
		wait4Result{err: unix.ECHILD},
	))
	outputWake, controlWake := testProcessWakes(t)
	processes := newProcessSet(context.Background(), []*launchedChild{child}, reaper,
		outputWake, controlWake)
	first, waitErr := processes.Wait()
	if waitErr != nil || len(first.Transfers) != 1 || first.Transfers[0].ExitCode != 3 {
		t.Fatalf("wait result=%#v error=%v", first, waitErr)
	}
	for range processes.Output() {
	}
	for range processes.Statuses() {
	}
	assertRawDescriptorClosed(t, readerFD)
	assertRawDescriptorClosed(t, outputWake)
	assertRawDescriptorClosed(t, controlWake)
	if processes.io.streams != nil {
		t.Fatalf("wait retained process streams: %#v", processes.io.streams)
	}
	first.Transfers[0].ExitCode = 99
	second, secondErr := processes.Wait()
	if secondErr != nil || second.Transfers[0].ExitCode != 3 {
		t.Fatalf("second wait result=%#v error=%v", second, secondErr)
	}
}
