package transfer

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

// CommandControl is one terminal intent for an already-owned command group.
type CommandControl struct {
	transfer   transfernumber.Number
	input      []byte
	cols, rows uint16
}

func InputControl(id transfernumber.Number, input []byte) (CommandControl, error) {
	if id.Value() == 0 || len(input) == 0 {
		return CommandControl{}, fmt.Errorf("command input control is invalid")
	}
	return CommandControl{transfer: id, input: append([]byte(nil), input...)}, nil
}

func ResizeControl(id transfernumber.Number, cols, rows uint16) (CommandControl, error) {
	if id.Value() == 0 || cols == 0 || rows == 0 {
		return CommandControl{}, fmt.Errorf("command resize control is invalid")
	}
	return CommandControl{transfer: id, cols: cols, rows: rows}, nil
}

// CommandEvents is the narrow outward boundary for byte-exact command output, status, and input.
type CommandEvents interface {
	SendOutput(childprocesses.Output) error
	SendStatus(childprocesses.Status) error
	Controls() <-chan CommandControl
}

type discardCommandEvents struct{}

func (discardCommandEvents) SendOutput(childprocesses.Output) error { return nil }
func (discardCommandEvents) SendStatus(childprocesses.Status) error { return nil }
func (discardCommandEvents) Controls() <-chan CommandControl        { return nil }

// commandProcessOwner supplies the user-command event surface in addition to shared supervision.
type commandProcessOwner interface {
	supervisedProcesses
	Output() <-chan childprocesses.Output
	Statuses() <-chan childprocesses.Status
	WriteInput(transfernumber.Number, []byte) error
	Resize(transfernumber.Number, int, int) error
}

type commandProcessGroup struct {
	processes         commandProcessOwner
	cancelProcesses   context.CancelFunc
	cancelProcessOnce sync.Once
	done              chan struct{}
	errMu             sync.Mutex
	err               error
}

func newCommandProcessGroup(processes commandProcessOwner, events CommandEvents,
	cancel context.CancelFunc,
) *commandProcessGroup {
	group := &commandProcessGroup{processes: processes, cancelProcesses: cancel, done: make(chan struct{})}
	go group.forward(events)
	return group
}

func (group *commandProcessGroup) forward(events CommandEvents) {
	defer func() {
		group.cancelProcessContext()
		close(group.done)
	}()
	if events == nil {
		events = discardCommandEvents{}
	}
	controlStop, controlsDone := make(chan struct{}), make(chan struct{})
	go group.forwardControls(events.Controls(), controlStop, controlsDone)
	output, statuses := group.processes.Output(), group.processes.Statuses()
	for output != nil || statuses != nil {
		select {
		case value, open := <-output:
			if !open {
				output = nil
			} else if err := events.SendOutput(value); err != nil {
				group.fail(fmt.Errorf("publish child output: %w", err))
				events = discardCommandEvents{}
			}
		case value, open := <-statuses:
			if !open {
				statuses = nil
			} else if err := events.SendStatus(value); err != nil {
				group.fail(fmt.Errorf("publish child status: %w", err))
				events = discardCommandEvents{}
			}
		}
	}
	close(controlStop)
	<-controlsDone
}

func (group *commandProcessGroup) forwardControls(controls <-chan CommandControl,
	stop <-chan struct{}, done chan<- struct{},
) {
	defer close(done)
	for {
		select {
		case <-stop:
			return
		case control, open := <-controls:
			if !open {
				return
			}
			if err := group.apply(control); err != nil {
				group.fail(err)
				return
			}
		}
	}
}

func (group *commandProcessGroup) apply(control CommandControl) error {
	if len(control.input) != 0 {
		return group.processes.WriteInput(control.transfer, control.input)
	}
	if control.cols != 0 && control.rows != 0 {
		return group.processes.Resize(control.transfer, int(control.cols), int(control.rows))
	}
	return errors.New("terminal delivered an invalid command control")
}

func (group *commandProcessGroup) fail(err error) {
	group.errMu.Lock()
	group.err = errors.Join(group.err, err)
	group.errMu.Unlock()
	group.cancelProcessContext()
}

func (group *commandProcessGroup) cancelProcessContext() {
	group.cancelProcessOnce.Do(group.cancelProcesses)
}

func (group *commandProcessGroup) forwardingError() error {
	group.errMu.Lock()
	defer group.errMu.Unlock()
	return group.err
}

func (group *commandProcessGroup) Wait() (childprocesses.Result, error) {
	result, err := group.processes.Wait()
	<-group.done
	return result, errors.Join(err, group.forwardingError())
}

func (group *commandProcessGroup) FinishStartFailure(ctx context.Context) childprocesses.StartFailureResult {
	if ctx == nil {
		group.cancelProcessContext()
		return childprocesses.StartFailureResult{
			SupervisionError: group.forwardingError(),
			ContainmentError: errors.New("failed-start command process context is required"),
		}
	}
	result := group.processes.FinishStartFailure(ctx)
	result.SupervisionError = errors.Join(result.SupervisionError,
		group.waitForForwarding(ctx, "finish failed-start command event forwarding"))
	if ctx.Err() != nil && !group.processContainmentConfirmed() {
		result.ContainmentError = errors.Join(result.ContainmentError,
			fmt.Errorf("failed-start process containment remains unconfirmed: %w", ctx.Err()))
	}
	return result
}

func (group *commandProcessGroup) ConfirmContainment(ctx context.Context) error {
	if ctx == nil {
		group.cancelProcessContext()
		return errors.New("command process containment context is required")
	}
	err := group.processes.ConfirmContainment(ctx)
	return errors.Join(err, group.waitForForwarding(ctx, "finish command event forwarding"))
}

func (group *commandProcessGroup) waitForForwarding(ctx context.Context, operation string) error {
	select {
	case <-group.done:
		return group.forwardingError()
	default:
	}
	select {
	case <-group.done:
		return group.forwardingError()
	case <-ctx.Done():
		select {
		case <-group.done:
			return group.forwardingError()
		default:
		}
		group.cancelProcessContext()
		return errors.Join(group.forwardingError(), fmt.Errorf("%s: %w", operation, ctx.Err()))
	}
}

func (group *commandProcessGroup) ContainmentDone() <-chan struct{} {
	return group.processes.ContainmentDone()
}

func (group *commandProcessGroup) processContainmentConfirmed() bool {
	select {
	case <-group.processes.ContainmentDone():
		return true
	default:
		return false
	}
}
