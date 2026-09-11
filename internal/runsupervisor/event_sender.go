package runsupervisor

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const (
	eventBackpressureLimit = 5 * time.Second
	maximumEventPayload    = pipeAtomicFrameBytes - wireHeaderBytes
	maximumOutputChunk     = maximumEventPayload - 2
)

type eventPipeSender struct {
	mu        sync.Mutex
	writer    io.Writer
	fd        int
	waitLimit time.Duration
	finalSent bool
}

func newEventPipeSender(output io.Writer, waitLimit time.Duration) (*eventPipeSender, error) {
	if output == nil || waitLimit <= 0 {
		return nil, errors.New("supervisor event output is invalid")
	}
	fd, err := prepareEventPipe(output)
	if err != nil {
		return nil, err
	}
	return &eventPipeSender{writer: output, fd: fd, waitLimit: waitLimit}, nil
}

func (sender *eventPipeSender) send(kind frameKind, payload []byte) error {
	encoded, err := encodeEventFrame(kind, payload)
	if err != nil {
		return err
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	return sender.sendLocked(kind, encoded)
}

func (sender *eventPipeSender) sendOutput(id transfernumber.Number, stream OutputStream, contents []byte) error {
	if id.Value() == 0 || !stream.valid() || len(contents) == 0 {
		return fmt.Errorf("supervisor command output is invalid")
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	for len(contents) != 0 {
		count := min(len(contents), maximumOutputChunk)
		frame, err := encodeOutput(id, stream, contents[:count])
		if err != nil {
			return err
		}
		encoded, err := encodeEventFrame(frame.kind, frame.payload)
		if err != nil {
			return err
		}
		if err := sender.sendLocked(frame.kind, encoded); err != nil {
			return err
		}
		contents = contents[count:]
	}
	return nil
}

func encodeEventFrame(kind frameKind, payload []byte) ([]byte, error) {
	if !kind.allowedOnEventPipe() || len(payload) > maximumEventPayload {
		return nil, errors.New("supervisor event frame is not pipe-atomic")
	}
	return encodeFrame(kind, payload)
}

func (sender *eventPipeSender) sendLocked(kind frameKind, encoded []byte) error {
	if sender.finalSent {
		return errors.New("supervisor final event was already sent")
	}
	var err error
	if sender.fd >= 0 {
		err = writeEventPipe(sender.fd, encoded, time.Now().Add(sender.waitLimit))
	} else {
		written, writeErr := sender.writer.Write(encoded)
		if writeErr != nil {
			err = writeErr
		} else if written != len(encoded) {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		return fmt.Errorf("write supervisor event frame: %w", err)
	}
	sender.finalSent = kind == frameFinal
	return nil
}
