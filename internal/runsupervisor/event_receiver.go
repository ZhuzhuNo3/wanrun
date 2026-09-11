//go:build linux

package runsupervisor

import (
	"fmt"
	"io"
	"os"
	"sync"
)

type eventReceiver struct {
	mu             sync.Mutex
	events         *os.File
	finalDelivered bool
	closed         bool
	closeErr       error
}

func newEventReceiver(events *os.File) *eventReceiver { return &eventReceiver{events: events} }

func (receiver *eventReceiver) next(controls *controlSender) (Event, error) {
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if receiver.finalDelivered {
		return Event{}, io.EOF
	}
	frame, err := readFrame(receiver.events)
	if err != nil {
		return Event{}, fmt.Errorf("read supervisor event before final: %w", err)
	}
	event, err := decodeEvent(frame)
	if err != nil {
		return Event{}, fmt.Errorf("decode supervisor event before final: %w", err)
	}
	if event.kind == EventFinal {
		controls.stop()
		receiver.finalDelivered = true
	}
	return event, nil
}

func (receiver *eventReceiver) close() error {
	if receiver == nil {
		return nil
	}
	receiver.mu.Lock()
	defer receiver.mu.Unlock()
	if receiver.closed {
		return receiver.closeErr
	}
	receiver.closed = true
	if receiver.events != nil {
		events := receiver.events
		receiver.events = nil
		receiver.closeErr = events.Close()
	}
	return receiver.closeErr
}
