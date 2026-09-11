//go:build linux

package childprocesses

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

// ProcessSet is the sole runtime facade for one complete set of direct children and descendants.
type ProcessSet struct {
	io         *processIO
	directPIDs map[int]transfernumber.Number
	reaper     *reapedProcesses
	terminator *processTerminator
	completion *processCompletion
	statuses   chan Status
}

type processCompletion struct {
	mu                  sync.Mutex
	cancelled           bool
	supervisionFailures []error
	result              Result
	waitErr             error
	published           bool
	done                chan struct{}
}

func newProcessSet(ctx context.Context, children []*launchedChild, reaper *reapedProcesses,
	outputWake, controlWake int,
) *ProcessSet {
	processIO, ioFailures := newProcessIO(children, 128, outputWake, controlWake)
	processes := &ProcessSet{
		io:         processIO,
		directPIDs: make(map[int]transfernumber.Number, len(children)),
		reaper:     reaper,
		terminator: newProcessTerminator(),
		completion: newProcessCompletion(),
		statuses:   make(chan Status, 2*len(children)),
	}
	for _, failure := range ioFailures {
		processes.completion.recordSupervisionFailure(failure)
	}
	for _, child := range children {
		processes.directPIDs[child.pid] = child.transfer
		processes.statuses <- Status{Transfer: child.transfer, Kind: StatusRunning}
	}
	go processes.supervise()
	go processes.cancelWithContext(ctx)
	if processes.completion.hasSupervisionFailure() {
		processes.beginCancellation()
	}
	return processes
}

func newProcessCompletion() *processCompletion {
	return &processCompletion{done: make(chan struct{})}
}

// Output returns byte-exact chunks with their real PTY/stdout/stderr identity.
func (processes *ProcessSet) Output() <-chan Output { return processes.io.output }

// Statuses returns one running and one terminal status for every direct command.
func (processes *ProcessSet) Statuses() <-chan Status { return processes.statuses }

func (processes *ProcessSet) supervise() {
	processes.io.startReaders(processes.failSupervision)
	results, directErr := processes.collectDirectResults()
	processes.io.stopControls()
	termination := processes.finishRemainingDescendants()
	if termination.supervisionErr != nil || termination.containmentErr != nil {
		processes.completion.markCancelled()
	}
	if !processes.reaper.containmentConfirmed() {
		processes.io.stopReaders()
	}
	processes.io.waitReaders()
	processes.io.stopReaders()
	processes.io.waitInputWriters()
	processes.io.close()
	processes.io.closeOutput()
	close(processes.statuses)
	processes.completion.publish(results, len(processes.directPIDs),
		errors.Join(directErr, termination.supervisionErr, termination.containmentErr))
}

func (processes *ProcessSet) collectDirectResults() ([]TransferResult, error) {
	results := make([]TransferResult, 0, len(processes.directPIDs))
	seen := make(map[int]bool, len(processes.directPIDs))
	for len(results) < len(processes.directPIDs) {
		exit, ok, err := processes.reaper.nextDirect(seen)
		if !ok {
			return results, err
		}
		seen[exit.pid] = true
		id, direct := processes.directPIDs[exit.pid]
		if !direct {
			continue
		}
		exitCode, signal := waitStatusResult(exit.status)
		results = append(results, TransferResult{Transfer: id, ExitCode: exitCode, Signal: signal})
		processes.statuses <- Status{Transfer: id, Kind: StatusExited,
			ExitCode: exitCode, Signal: signal}
	}
	return results, nil
}

func (processes *ProcessSet) finishRemainingDescendants() terminationOutcome {
	processes.beginTermination()
	<-processes.terminator.done
	return processes.terminator.outcome
}

func (processes *ProcessSet) failSupervision(err error) {
	processes.completion.recordSupervisionFailure(err)
	processes.beginCancellation()
}

func (processes *ProcessSet) cancelWithContext(ctx context.Context) {
	select {
	case <-ctx.Done():
		processes.cancelBackground()
	case <-processes.completion.done:
	}
}

func (processes *ProcessSet) cancelBackground() {
	ctx, cancel := context.WithTimeout(context.Background(), abortTimeout)
	defer cancel()
	_ = processes.Cancel(ctx)
}

// WriteInput writes bytes only to a currently controlled interactive transfer PTY.
func (processes *ProcessSet) WriteInput(id transfernumber.Number, contents []byte) error {
	return processes.io.writeInput(id, contents, processes.failSupervision)
}

// Resize changes exactly one interactive transfer's PTY dimensions.
func (processes *ProcessSet) Resize(id transfernumber.Number, cols, rows int) error {
	return processes.io.resize(id, cols, rows, processes.failSupervision)
}

// Cancel stops controls, escalates signals, and waits for every descendant to be reaped.
func (processes *ProcessSet) Cancel(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("child process cancellation context is required")
	}
	processes.beginCancellation()
	outcome, err := processes.terminator.wait(ctx)
	if err != nil {
		return err
	}
	return errors.Join(outcome.supervisionErr, outcome.containmentErr)
}

func (processes *ProcessSet) beginCancellation() {
	processes.completion.markCancelled()
	processes.io.stopControls()
	processes.beginTermination()
}

func (processes *ProcessSet) beginTermination() {
	processes.terminator.begin(processes.reaper)
}

// Wait returns only after direct results, output draining, and containment observation finish.
func (processes *ProcessSet) Wait() (Result, error) {
	return processes.completion.wait()
}

// ContainmentDone closes only after every process in the supervisor PID namespace has been reaped.
func (processes *ProcessSet) ContainmentDone() <-chan struct{} {
	return processes.reaper.containment
}

// ConfirmContainment reports only process-absence evidence after Wait has delivered supervision.
func (processes *ProcessSet) ConfirmContainment(ctx context.Context) error {
	if ctx == nil {
		return errors.New("process containment context is required")
	}
	select {
	case <-processes.reaper.containment:
		return nil
	default:
	}
	select {
	case <-processes.reaper.containment:
		return nil
	case <-processes.completion.done:
		select {
		case <-processes.reaper.containment:
			return nil
		default:
			return errors.New("process supervision stopped without containment evidence")
		}
	case <-ctx.Done():
		select {
		case <-processes.reaper.containment:
			return nil
		default:
			return ctx.Err()
		}
	}
}

// FinishStartFailure obtains supervision and containment evidence still owned after failed start.
func (processes *ProcessSet) FinishStartFailure(ctx context.Context) StartFailureResult {
	if ctx == nil {
		return StartFailureResult{ContainmentError: errors.New("failed-start context is required")}
	}
	var termination terminationOutcome
	select {
	case <-processes.completion.done:
	default:
		processes.beginCancellation()
		select {
		case <-processes.terminator.done:
			termination = processes.terminator.outcome
		case <-ctx.Done():
		}
	}
	_, waitErr := processes.waitForStop(ctx)
	return StartFailureResult{SupervisionError: errors.Join(termination.supervisionErr, waitErr),
		ContainmentError: processes.ConfirmContainment(ctx)}
}

func (processes *ProcessSet) waitForStop(ctx context.Context) (Result, error) {
	select {
	case <-processes.completion.done:
		return processes.Wait()
	default:
	}
	select {
	case <-processes.completion.done:
		return processes.Wait()
	case <-ctx.Done():
		select {
		case <-processes.completion.done:
			return processes.Wait()
		default:
			return Result{}, nil
		}
	}
}

func (completion *processCompletion) markCancelled() {
	completion.mu.Lock()
	defer completion.mu.Unlock()
	if !completion.published {
		completion.cancelled = true
	}
}

func (completion *processCompletion) recordSupervisionFailure(err error) {
	if err == nil {
		return
	}
	completion.mu.Lock()
	defer completion.mu.Unlock()
	if !completion.published {
		completion.supervisionFailures = append(completion.supervisionFailures, err)
	}
}

func (completion *processCompletion) hasSupervisionFailure() bool {
	completion.mu.Lock()
	defer completion.mu.Unlock()
	return len(completion.supervisionFailures) != 0
}

func (completion *processCompletion) publish(results []TransferResult, expected int, supervisionErr error) {
	completion.mu.Lock()
	defer completion.mu.Unlock()
	if completion.published {
		return
	}
	result, resultErr := newResult(results, completion.cancelled)
	if len(results) != expected {
		resultErr = errors.Join(resultErr, errors.New("direct child result set is incomplete"))
	}
	completion.result = result
	completion.waitErr = errors.Join(supervisionErr, resultErr,
		errors.Join(completion.supervisionFailures...))
	completion.published = true
	close(completion.done)
}

func (completion *processCompletion) wait() (Result, error) {
	<-completion.done
	completion.mu.Lock()
	defer completion.mu.Unlock()
	return cloneResult(completion.result), completion.waitErr
}
