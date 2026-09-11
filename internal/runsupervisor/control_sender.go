//go:build linux

package runsupervisor

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const maximumAtomicInputBytes = pipeAtomicFrameBytes - wireHeaderBytes - 1

type controlState uint8

const (
	controlOpen controlState = iota
	controlCancelling
	controlStopped
	controlClosed
)

type controlSender struct {
	sendMu  sync.Mutex
	stateMu sync.Mutex
	state   controlState

	control      *os.File
	cancellation *os.File
	wake         *os.File

	cancelErr   error
	lifelineErr error
	teardownErr error
}

func newControlSender(control, cancellation, wake *os.File) *controlSender {
	return &controlSender{control: control, cancellation: cancellation, wake: wake}
}

func (sender *controlSender) resize(id transfernumber.Number, cols, rows uint16) error {
	kind, payload, err := encodeControl(newResizeControl(id, cols, rows))
	if err != nil {
		return err
	}
	return sender.sendFrame(kind, payload)
}

func (sender *controlSender) writeInput(id transfernumber.Number, input []byte) error {
	if id.Value() == 0 || len(input) == 0 || len(input)+1 > maximumWirePayload {
		return fmt.Errorf("supervisor input is invalid")
	}
	input = append([]byte(nil), input...)
	sender.sendMu.Lock()
	defer sender.sendMu.Unlock()
	for len(input) != 0 {
		if !sender.openForSend() {
			return nil
		}
		count := min(len(input), maximumAtomicInputBytes)
		payload := make([]byte, count+1)
		payload[0] = id.Value()
		copy(payload[1:], input[:count])
		if err := sender.writeOpenFrame(frameInput, payload); err != nil {
			return err
		}
		input = input[count:]
	}
	return nil
}

func (sender *controlSender) sendFrame(kind frameKind, payload []byte) error {
	sender.sendMu.Lock()
	defer sender.sendMu.Unlock()
	if !sender.openForSend() {
		return nil
	}
	return sender.writeOpenFrame(kind, payload)
}

func (sender *controlSender) writeOpenFrame(kind frameKind, payload []byte) error {
	sender.stateMu.Lock()
	if sender.state != controlOpen {
		sender.stateMu.Unlock()
		return nil
	}
	control, wake := sender.control, sender.wake
	sender.stateMu.Unlock()
	err := writeAtomicControlFile(control, wake, kind, payload)
	if err == nil {
		return nil
	}
	sender.stateMu.Lock()
	open := sender.state == controlOpen
	sender.stateMu.Unlock()
	if !open {
		return nil
	}
	return err
}

func (sender *controlSender) openForSend() bool {
	sender.stateMu.Lock()
	defer sender.stateMu.Unlock()
	return sender.state == controlOpen
}

func (sender *controlSender) cancel(reason CancelReason) error {
	if !reason.validCancellation() {
		return fmt.Errorf("supervisor cancel reason is invalid")
	}
	sender.stateMu.Lock()
	defer sender.stateMu.Unlock()
	switch sender.state {
	case controlOpen:
		sender.state = controlCancelling
		sender.cancelErr = writeCancellation(sender.cancellation, reason)
		signalControlWriteWake(sender.wake)
		return sender.cancelErr
	case controlCancelling:
		return sender.cancelErr
	case controlStopped, controlClosed:
		return nil
	default:
		return errors.New("supervisor control state is invalid")
	}
}

func (sender *controlSender) stop() {
	if sender == nil {
		return
	}
	sender.stateMu.Lock()
	if sender.state == controlOpen || sender.state == controlCancelling {
		sender.state = controlStopped
		signalControlWriteWake(sender.wake)
	}
	sender.stateMu.Unlock()
}

func (sender *controlSender) closeLifeline() error {
	sender.stop()
	sender.stateMu.Lock()
	defer sender.stateMu.Unlock()
	if sender.cancellation == nil {
		return sender.lifelineErr
	}
	cancellation := sender.cancellation
	sender.cancellation = nil
	sender.lifelineErr = cancellation.Close()
	return sender.lifelineErr
}

func (sender *controlSender) teardown() error {
	if sender == nil {
		return nil
	}
	sender.stop()
	sender.sendMu.Lock()
	defer sender.sendMu.Unlock()
	sender.stateMu.Lock()
	if sender.state == controlClosed {
		err := sender.teardownErr
		sender.stateMu.Unlock()
		return err
	}
	control, cancellation, wake := sender.control, sender.cancellation, sender.wake
	sender.control, sender.cancellation, sender.wake = nil, nil, nil
	sender.state = controlClosed
	lifelineErr := sender.lifelineErr
	sender.stateMu.Unlock()
	err := errors.Join(lifelineErr, closeFilesWithErrors(control, cancellation, wake))
	sender.stateMu.Lock()
	sender.teardownErr = err
	sender.stateMu.Unlock()
	return err
}
