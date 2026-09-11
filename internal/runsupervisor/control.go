package runsupervisor

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

// CancelReason identifies the small fixed set of whole-run cancellation causes.
type CancelReason uint8

const (
	CancelNone CancelReason = iota
	CancelUser
	CancelTerminate
	CancelHangup
	CancelLifeline
	CancelInternal
)

func (reason CancelReason) validCancellation() bool {
	return reason >= CancelUser && reason <= CancelInternal
}

type controlKind uint8

const (
	controlResize controlKind = iota + 1
	controlInput
)

// Control is one validated parent-to-supervisor runtime instruction.
type Control struct {
	kind       controlKind
	transfer   transfernumber.Number
	cols, rows uint16
	input      []byte
}

func newResizeControl(id transfernumber.Number, cols, rows uint16) Control {
	return Control{kind: controlResize, transfer: id, cols: cols, rows: rows}
}

func newInputControl(id transfernumber.Number, input []byte) Control {
	return Control{kind: controlInput, transfer: id, input: append([]byte(nil), input...)}
}

// Kind reports resize or input without exposing wire values.
func (control Control) Kind() string {
	switch control.kind {
	case controlResize:
		return "resize"
	case controlInput:
		return "input"
	default:
		return "invalid"
	}
}

func (control Control) Transfer() transfernumber.Number { return control.transfer }
func (control Control) Size() (uint16, uint16)          { return control.cols, control.rows }
func (control Control) Input() []byte                   { return append([]byte(nil), control.input...) }

func encodeControl(control Control) (frameKind, []byte, error) {
	switch control.kind {
	case controlResize:
		if control.transfer.Value() == 0 || control.cols == 0 || control.rows == 0 {
			return 0, nil, fmt.Errorf("supervisor resize is invalid")
		}
		payload := make([]byte, 5)
		payload[0] = control.transfer.Value()
		binary.BigEndian.PutUint16(payload[1:3], control.cols)
		binary.BigEndian.PutUint16(payload[3:5], control.rows)
		return frameResize, payload, nil
	case controlInput:
		if control.transfer.Value() == 0 || len(control.input) == 0 || len(control.input)+1 > maximumWirePayload {
			return 0, nil, fmt.Errorf("supervisor input is invalid")
		}
		return frameInput, append([]byte{control.transfer.Value()}, control.input...), nil
	default:
		return 0, nil, fmt.Errorf("supervisor control kind is invalid")
	}
}

func decodeControl(frame wireFrame) (Control, error) {
	switch frame.kind {
	case frameResize:
		if len(frame.payload) != 5 {
			return Control{}, fmt.Errorf("supervisor resize payload is invalid")
		}
		id, err := transfernumber.New(int(frame.payload[0]))
		cols, rows := binary.BigEndian.Uint16(frame.payload[1:3]), binary.BigEndian.Uint16(frame.payload[3:5])
		if err != nil || cols == 0 || rows == 0 {
			return Control{}, fmt.Errorf("supervisor resize payload is invalid")
		}
		return newResizeControl(id, cols, rows), nil
	case frameInput:
		if len(frame.payload) < 2 {
			return Control{}, fmt.Errorf("supervisor input payload is invalid")
		}
		id, err := transfernumber.New(int(frame.payload[0]))
		if err != nil {
			return Control{}, fmt.Errorf("supervisor input payload is invalid")
		}
		return newInputControl(id, frame.payload[1:]), nil
	default:
		return Control{}, fmt.Errorf("supervisor frame is not a control")
	}
}

func writeCancellation(writer io.Writer, reason CancelReason) error {
	if !reason.validCancellation() {
		return fmt.Errorf("supervisor cancel reason is invalid")
	}
	encoded, err := encodeFrame(frameCancel, []byte{byte(reason)})
	if err != nil {
		return err
	}
	written, err := writer.Write(encoded)
	if err != nil {
		return fmt.Errorf("write supervisor cancellation frame: %w", err)
	}
	if written != len(encoded) {
		return fmt.Errorf("write supervisor cancellation frame: %w", io.ErrShortWrite)
	}
	return nil
}

func decodeCancellation(frame wireFrame) (CancelReason, error) {
	if frame.kind != frameCancel || len(frame.payload) != 1 {
		return CancelNone, fmt.Errorf("supervisor cancellation frame is invalid")
	}
	reason := CancelReason(frame.payload[0])
	if !reason.validCancellation() {
		return CancelNone, fmt.Errorf("supervisor cancel reason is invalid")
	}
	return reason, nil
}
