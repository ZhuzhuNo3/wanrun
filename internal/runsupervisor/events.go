package runsupervisor

import (
	"encoding/binary"
	"fmt"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

// EventKind is the closed supervisor-to-parent event vocabulary.
type EventKind uint8

const (
	EventOutput EventKind = iota + 1
	EventStatus
	EventFinal
)

// OutputStream identifies the command descriptor that emitted raw bytes.
type OutputStream uint8

const (
	OutputPTY OutputStream = iota + 1
	OutputStdout
	OutputStderr
)

func (stream OutputStream) valid() bool {
	return stream >= OutputPTY && stream <= OutputStderr
}

// TransferState is the fixed running/exited status vocabulary.
type TransferState uint8

const (
	TransferRunning TransferState = iota + 1
	TransferExited
)

// TransferStatus reports a transfer transition without transferring process ownership.
type TransferStatus struct {
	Transfer transfernumber.Number
	State    TransferState
	ExitCode int
	Signal   int
}

// Event contains exactly one output, status, or final payload.
type Event struct {
	kind     EventKind
	transfer transfernumber.Number
	stream   OutputStream
	bytes    []byte
	status   TransferStatus
	final    Final
}

func (event Event) Kind() EventKind                 { return event.kind }
func (event Event) Transfer() transfernumber.Number { return event.transfer }
func (event Event) Stream() OutputStream            { return event.stream }
func (event Event) Bytes() []byte                   { return append([]byte(nil), event.bytes...) }
func (event Event) Status() TransferStatus          { return event.status }
func (event Event) Final() Final                    { return event.final.Clone() }

// NewOutputEvent validates one byte-exact command chunk for in-process output consumers.
func NewOutputEvent(id transfernumber.Number, stream OutputStream, contents []byte) (Event, error) {
	frame, err := encodeOutput(id, stream, contents)
	if err != nil {
		return Event{}, err
	}
	return decodeOutput(frame.payload)
}

// NewStatusEvent validates one transfer transition for in-process output consumers.
func NewStatusEvent(status TransferStatus) (Event, error) {
	if _, err := encodeStatus(status); err != nil {
		return Event{}, err
	}
	return Event{kind: EventStatus, transfer: status.Transfer, status: status}, nil
}

// NewFinalEvent validates a completed supervisor result for in-process event consumers.
func NewFinalEvent(final Final) (Event, error) {
	ordered, err := orderedFinal(final)
	if err != nil {
		return Event{}, err
	}
	return Event{kind: EventFinal, final: ordered.Clone()}, nil
}

func decodeEvent(frame wireFrame) (Event, error) {
	if !frame.kind.allowedOnEventPipe() || len(frame.payload) > maximumEventPayload {
		return Event{}, fmt.Errorf("supervisor event frame is not pipe-atomic")
	}
	switch frame.kind {
	case frameOutput:
		return decodeOutput(frame.payload)
	case frameStatus:
		status, err := decodeStatus(frame.payload)
		if err != nil {
			return Event{}, err
		}
		return Event{kind: EventStatus, transfer: status.Transfer, status: status}, nil
	case frameFinal:
		final, err := decodeFinal(frame.payload)
		if err != nil {
			return Event{}, err
		}
		return Event{kind: EventFinal, final: final}, nil
	default:
		return Event{}, fmt.Errorf("supervisor emitted a non-event frame")
	}
}

func encodeOutput(id transfernumber.Number, stream OutputStream, contents []byte) (wireFrame, error) {
	if id.Value() == 0 || !stream.valid() || len(contents) == 0 || len(contents) > maximumOutputChunk {
		return wireFrame{}, fmt.Errorf("supervisor command output is invalid")
	}
	payload := make([]byte, len(contents)+2)
	payload[0], payload[1] = id.Value(), byte(stream)
	copy(payload[2:], contents)
	return wireFrame{kind: frameOutput, payload: payload}, nil
}

func decodeOutput(payload []byte) (Event, error) {
	if len(payload) < 3 {
		return Event{}, fmt.Errorf("supervisor command output is invalid")
	}
	id, err := transfernumber.New(int(payload[0]))
	stream := OutputStream(payload[1])
	if err != nil || !stream.valid() {
		return Event{}, fmt.Errorf("supervisor command output identity is invalid")
	}
	return Event{kind: EventOutput, transfer: id, stream: stream,
		bytes: append([]byte(nil), payload[2:]...)}, nil
}

func decodeStatus(payload []byte) (TransferStatus, error) {
	if len(payload) != 8 {
		return TransferStatus{}, fmt.Errorf("supervisor status payload is invalid")
	}
	id, err := transfernumber.New(int(payload[0]))
	if err != nil {
		return TransferStatus{}, fmt.Errorf("supervisor status transfer is invalid")
	}
	status := TransferStatus{Transfer: id, State: TransferState(payload[1]),
		ExitCode: int(int32(binary.BigEndian.Uint32(payload[2:6]))),
		Signal:   int(binary.BigEndian.Uint16(payload[6:8]))}
	if _, err := encodeStatus(status); err != nil {
		return TransferStatus{}, err
	}
	return status, nil
}

func encodeStatus(status TransferStatus) ([]byte, error) {
	if status.Transfer.Value() == 0 || status.State < TransferRunning || status.State > TransferExited {
		return nil, fmt.Errorf("supervisor transfer status is invalid")
	}
	result := TransferResult{Transfer: status.Transfer, ExitCode: status.ExitCode, Signal: status.Signal}
	if status.State == TransferRunning {
		if status.ExitCode != 0 || status.Signal != 0 {
			return nil, fmt.Errorf("supervisor running status has an outcome")
		}
	} else if !validTransferResult(result) {
		return nil, fmt.Errorf("supervisor transfer result is invalid")
	}
	payload := make([]byte, 8)
	payload[0], payload[1] = status.Transfer.Value(), byte(status.State)
	binary.BigEndian.PutUint32(payload[2:6], uint32(int32(status.ExitCode)))
	binary.BigEndian.PutUint16(payload[6:8], uint16(status.Signal))
	return payload, nil
}
