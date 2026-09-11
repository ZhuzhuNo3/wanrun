package terminal

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	term "github.com/charmbracelet/x/term"
)

const (
	enterInteractiveScreen = "\x1b[?1049h\x1b[?25l\x1b[?1000h\x1b[?1006h\x1b[2J\x1b[H"
	leaveInteractiveScreen = "\x1b[?1006l\x1b[?1000l\x1b[?25h\x1b[?1049l"
)

// CommandControl is the only supervisor capability borrowed by the interactive driver.
type CommandControl interface {
	Resize(transfernumber.Number, uint16, uint16) error
	WriteInput(transfernumber.Number, []byte) error
}

// TerminationRequest asks the parent lifecycle owner to act without granting resource authority.
type TerminationRequest struct {
	reason         runsupervisor.CancelReason
	controllerLost bool
}

func (request TerminationRequest) Reason() runsupervisor.CancelReason { return request.reason }
func (request TerminationRequest) ControllerLost() bool               { return request.controllerLost }

type displayRequest struct {
	event   runsupervisor.Event
	begin   bool
	failure error
	result  chan error
}

type interactiveDisplay struct {
	requests chan<- displayRequest
	stopped  <-chan struct{}
	beginErr error
}

func (display interactiveDisplay) Begin() error {
	return display.request(displayRequest{begin: true, failure: display.beginErr,
		result: make(chan error, 1)})
}

func (display interactiveDisplay) Show(event runsupervisor.Event) error {
	return display.request(displayRequest{event: event, result: make(chan error, 1)})
}

func (display interactiveDisplay) request(request displayRequest) error {
	select {
	case display.requests <- request:
	case <-display.stopped:
		select {
		case err := <-request.result:
			return err
		default:
			return errors.New("interactive terminal stopped")
		}
	}
	select {
	case err := <-request.result:
		return err
	case <-display.stopped:
		// A handled request owns its exact result even when terminal shutdown becomes
		// observable at the same boundary.
		select {
		case err := <-request.result:
			return err
		default:
			return errors.New("interactive terminal stopped")
		}
	}
}

type programFinish struct {
	final                 *runsupervisor.Final
	displayEventsComplete bool
}

// Program owns only the outer TTY, input, resize, rendering, and final terminal restoration.
type Program struct {
	display          *InteractiveTerminal
	input            *os.File
	output           *os.File
	requests         chan displayRequest
	finishes         chan programFinish
	stopped          chan struct{}
	commandsStopped  chan struct{}
	stopOnce         sync.Once
	commandsStopOnce sync.Once
}

func NewProgram(display *InteractiveTerminal, input, output *os.File) (*Program, error) {
	if display == nil || input == nil || output == nil {
		if display != nil {
			_ = display.Close()
		}
		return nil, errors.New("interactive terminal inputs are incomplete")
	}
	return &Program{display: display, input: input, output: output,
		requests: make(chan displayRequest), finishes: make(chan programFinish, 1),
		stopped: make(chan struct{}), commandsStopped: make(chan struct{})}, nil
}

// Display returns the endpoint used by the sole output forwarder.
func (program *Program) Display() interface {
	Begin() error
	Show(runsupervisor.Event) error
} {
	if program == nil {
		return interactiveDisplay{stopped: closedChannel()}
	}
	return interactiveDisplay{requests: program.requests, stopped: program.stopped}
}

// TargetSize returns the current authoritative child PTY dimensions.
func (program *Program) TargetSize() (int, int) {
	if program == nil || program.display == nil {
		return 0, 0
	}
	return program.display.TargetSize()
}

// Finish supplies one detached final projection after output completion.
func (program *Program) Finish(final *runsupervisor.Final, displayEventsComplete bool) bool {
	if program == nil {
		return false
	}
	var copyFinal *runsupervisor.Final
	if final != nil {
		cloned := final.Clone()
		copyFinal = &cloned
	}
	select {
	case program.finishes <- programFinish{final: copyFinal,
		displayEventsComplete: displayEventsComplete}:
		return true
	case <-program.stopped:
		return false
	}
}

// StopCommands prevents new input or resize commands while output continues to drain.
func (program *Program) StopCommands() {
	if program == nil {
		return
	}
	program.commandsStopOnce.Do(func() { close(program.commandsStopped) })
}

// Close releases a prepared program that never entered raw terminal mode.
func (program *Program) Close() error {
	if program == nil {
		return nil
	}
	program.StopCommands()
	program.stop()
	return program.display.Close()
}

// Run drives terminal presentation until output completion and a matching Finish projection.
func (program *Program) Run(control CommandControl, outputDone <-chan struct{}, resize <-chan struct{},
	requestTermination func(TerminationRequest),
) (result error) {
	if program == nil || control == nil || outputDone == nil || requestTermination == nil {
		return errors.New("interactive terminal driver inputs are incomplete")
	}
	previous, err := term.MakeRaw(program.input.Fd())
	if err != nil {
		program.stop()
		return errors.Join(fmt.Errorf("enter raw terminal mode: %w", err), program.display.Close())
	}
	_, enterErr := io.WriteString(program.output, enterInteractiveScreen)
	inputEvents, stopInput, inputErr := startOwnedInput(program.input)
	loop := programLoop{program: program, control: control, outputDone: outputDone,
		resize: resize, inputEvents: inputEvents, requestTermination: requestTermination,
		commandsStopped: program.commandsStopped}
	loop.startInstructionSender()
	if inputErr != nil {
		loop.failure = errors.Join(loop.failure, inputErr)
		loop.publish(TerminationRequest{reason: runsupervisor.CancelLifeline, controllerLost: true})
		loop.inputEvents = nil
	}
	finish, driveErr := loop.wait(wrapTerminalError("enter alternate screen", enterErr))
	program.stop()
	if stopInput != nil {
		stopInput()
	}
	result = errors.Join(loop.failure, driveErr, loop.stopInstructionSender())
	return errors.Join(result, program.restore(previous, finish))
}

func (program *Program) restore(previous *term.State, finish programFinish) error {
	var snapshots []byte
	var snapshotErr error
	if finish.final != nil && finish.displayEventsComplete {
		values, err := program.display.finalSnapshots(finish.final.Transfers)
		if err != nil {
			snapshotErr = fmt.Errorf("compose final command snapshots: %w", err)
		} else {
			snapshots = serializeFinalSnapshots(values)
		}
	}
	displayCloseErr := program.display.Close()
	leaveErr := writeTerminalBytes(program.output, []byte(leaveInteractiveScreen), "leave alternate screen")
	restoreErr := wrapTerminalError("restore terminal mode", term.Restore(program.input.Fd(), previous))
	result := errors.Join(snapshotErr, displayCloseErr, leaveErr, restoreErr)
	if snapshotErr == nil && leaveErr == nil && restoreErr == nil {
		result = errors.Join(result,
			writeTerminalBytes(program.output, snapshots, "write final command snapshots"))
	}
	return result
}

func (program *Program) stop() {
	program.stopOnce.Do(func() { close(program.stopped) })
}

type programLoop struct {
	program            *Program
	control            CommandControl
	outputDone         <-chan struct{}
	resize             <-chan struct{}
	inputEvents        <-chan inputEvent
	requestTermination func(TerminationRequest)
	instructions       chan terminalInstruction
	instructionErr     chan error
	instructionStop    chan struct{}
	instructionDone    chan struct{}
	stopOnce           sync.Once
	commandsStopped    <-chan struct{}
	pending            []terminalInstruction
	inFlight           bool
	cancelled          bool
	failure            error
	beginErr           error
}

func (loop *programLoop) wait(beginErr error) (programFinish, error) {
	loop.beginErr = beginErr
	for {
		expiration, stopTimer := expirationChannel(loop.program.display.nextDeadline())
		finish, done, err := loop.waitOnce(expiration)
		stopTimer()
		if done {
			return finish, err
		}
	}
}

func (loop *programLoop) waitOnce(expiration <-chan time.Time) (programFinish, bool, error) {
	inputEvents, instructionTarget, next := loop.readyChannels()
	select {
	case instructionTarget <- next:
		loop.pending = loop.pending[1:]
		loop.inFlight = true
	case err := <-loop.instructionErr:
		loop.inFlight = false
		if err != nil {
			loop.stopSending()
			return programFinish{}, true, err
		}
	case received, open := <-inputEvents:
		if err := loop.handleInput(received, open); err != nil {
			loop.stopSending()
			return programFinish{}, true, err
		}
	case <-loop.resize:
		if err := loop.handleResize(); err != nil {
			loop.stopSending()
			return programFinish{}, true, err
		}
	case request := <-loop.program.requests:
		err := loop.handleDisplay(request)
		request.result <- err
		if err != nil {
			loop.stopSending()
			// The requesting output forwarder now owns this failure. Program.Run returns
			// only independent driver and restoration failures.
			return programFinish{}, true, nil
		}
	case now := <-expiration:
		loop.acceptInstructions(loop.program.display.expire(now))
		if err := loop.render(); err != nil {
			loop.stopSending()
			return programFinish{}, true, err
		}
	case <-loop.commandsStopped:
		loop.commandsStopped = nil
		loop.pending = nil
		loop.stopSending()
	case <-loop.outputDone:
		loop.stopSending()
		finish := <-loop.program.finishes
		return finish, true, nil
	}
	return programFinish{}, false, nil
}

func (loop *programLoop) readyChannels() (<-chan inputEvent, chan terminalInstruction,
	terminalInstruction,
) {
	inputEvents := loop.inputEvents
	var target chan terminalInstruction
	var next terminalInstruction
	if len(loop.pending) != 0 && !loop.inFlight {
		inputEvents = nil
		target, next = loop.instructions, loop.pending[0]
	} else if loop.inFlight && len(loop.pending) != 0 {
		inputEvents = nil
	}
	return inputEvents, target, next
}

func (loop *programLoop) handleInput(received inputEvent, open bool) error {
	if !open {
		loop.inputEvents = nil
		return nil
	}
	if received.err != nil {
		loop.failure = errors.Join(loop.failure, received.err)
		loop.publish(TerminationRequest{reason: runsupervisor.CancelLifeline, controllerLost: true})
		loop.inputEvents = nil
		return nil
	}
	if !loop.cancelled {
		loop.acceptInstructions(loop.program.display.interpretInput(received.content, time.Now()))
		if err := loop.render(); err != nil {
			loop.publish(TerminationRequest{reason: runsupervisor.CancelInternal})
			return err
		}
	}
	return nil
}

func (loop *programLoop) handleResize() error {
	columns, rows, err := term.GetSize(loop.program.output.Fd())
	if err != nil {
		return fmt.Errorf("read terminal resize: %w", err)
	}
	instructions, err := loop.program.display.resize(columns, rows)
	if err != nil {
		return err
	}
	loop.acceptInstructions(instructions)
	return loop.render()
}

func (loop *programLoop) handleDisplay(request displayRequest) error {
	if request.failure != nil {
		return request.failure
	}
	if request.begin && loop.beginErr != nil {
		err := loop.beginErr
		loop.beginErr = nil
		return err
	}
	if !request.begin {
		if err := loop.program.display.apply(request.event); err != nil {
			return err
		}
	}
	return loop.render()
}

func (loop *programLoop) acceptInstructions(instructions []terminalInstruction) {
	for _, instruction := range instructions {
		if instruction.kind == cancelRun {
			if loop.cancelled {
				continue
			}
			loop.cancelled = true
			loop.pending = nil
			loop.publish(TerminationRequest{reason: instruction.cancel})
			loop.stopSending()
			continue
		}
		if !loop.cancelled {
			loop.pending = append(loop.pending, instruction)
		}
	}
}

func (loop *programLoop) render() error {
	return redraw(loop.program.output, loop.program.display.styledView())
}

func (loop *programLoop) publish(request TerminationRequest) {
	loop.requestTermination(request)
}

func (loop *programLoop) startInstructionSender() {
	loop.instructions = make(chan terminalInstruction)
	loop.instructionErr = make(chan error)
	loop.instructionStop = make(chan struct{})
	loop.instructionDone = make(chan struct{})
	go loop.sendInstructions()
}

func (loop *programLoop) sendInstructions() {
	defer close(loop.instructionDone)
	for {
		select {
		case <-loop.instructionStop:
			return
		case instruction := <-loop.instructions:
			err := sendTerminalInstruction(loop.control, instruction)
			select {
			case loop.instructionErr <- err:
			case <-loop.instructionStop:
				return
			}
		}
	}
}

func (loop *programLoop) stopInstructionSender() error {
	loop.stopSending()
	<-loop.instructionDone
	return nil
}

func (loop *programLoop) stopSending() {
	loop.inputEvents = nil
	loop.stopOnce.Do(func() { close(loop.instructionStop) })
}

func sendTerminalInstruction(control CommandControl, instruction terminalInstruction) error {
	switch instruction.kind {
	case writeTargetInput:
		if len(instruction.content) == 0 {
			return errors.New("terminal produced empty target input")
		}
		return control.WriteInput(instruction.transfer, instruction.content)
	case resizeTarget:
		return control.Resize(instruction.transfer, instruction.cols, instruction.rows)
	default:
		return errors.New("terminal produced an invalid asynchronous instruction")
	}
}

func writeTerminalBytes(output io.Writer, content []byte, action string) error {
	if len(content) == 0 {
		return nil
	}
	written, err := output.Write(content)
	if err == nil && written != len(content) {
		err = io.ErrShortWrite
	}
	return wrapTerminalError(action, err)
}

func expirationChannel(deadline time.Time) (<-chan time.Time, func()) {
	if deadline.IsZero() {
		return nil, func() {}
	}
	timer := time.NewTimer(max(time.Until(deadline), 0))
	return timer.C, func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}

func redraw(output io.Writer, view string) error {
	_, err := io.WriteString(output, "\x1b[H"+view)
	return wrapTerminalError("render terminal", err)
}

func wrapTerminalError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", action, err)
}

func closedChannel() <-chan struct{} {
	closed := make(chan struct{})
	close(closed)
	return closed
}
