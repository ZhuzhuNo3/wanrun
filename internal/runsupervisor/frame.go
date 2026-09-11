package runsupervisor

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	wireVersion          = uint8(5)
	wireHeaderBytes      = 8
	maximumWirePayload   = 64 * 1024
	pipeAtomicFrameBytes = 4096
)

var (
	wireMagic    = [2]byte{'W', 'S'}
	errHandshake = errors.New("supervisor start handshake is invalid")
)

type frameKind uint8

const (
	frameReady frameKind = iota + 1
	frameAccepted
	frameStart
	frameCommit
	frameCancel
	frameResize
	frameInput
	frameOutput
	frameStatus
	frameFinal
)

type wireFrame struct {
	kind    frameKind
	payload []byte
}

func writeFrame(writer io.Writer, kind frameKind, payload []byte) error {
	encoded, err := encodeFrame(kind, payload)
	if err != nil {
		return err
	}
	if err := writeFull(writer, encoded); err != nil {
		return fmt.Errorf("write supervisor frame: %w", err)
	}
	return nil
}

func encodeFrame(kind frameKind, payload []byte) ([]byte, error) {
	if !kind.valid() || len(payload) > maximumWirePayload {
		return nil, fmt.Errorf("supervisor wire frame is invalid")
	}
	encoded := make([]byte, wireHeaderBytes+len(payload))
	copy(encoded[:2], wireMagic[:])
	encoded[2], encoded[3] = wireVersion, byte(kind)
	binary.BigEndian.PutUint32(encoded[4:wireHeaderBytes], uint32(len(payload)))
	copy(encoded[wireHeaderBytes:], payload)
	return encoded, nil
}

func readFrame(reader io.Reader) (wireFrame, error) {
	header := make([]byte, wireHeaderBytes)
	if _, err := io.ReadFull(reader, header); err != nil {
		return wireFrame{}, err
	}
	kind := frameKind(header[3])
	size := binary.BigEndian.Uint32(header[4:])
	if header[0] != wireMagic[0] || header[1] != wireMagic[1] || header[2] != wireVersion ||
		!kind.valid() || size > maximumWirePayload {
		return wireFrame{}, fmt.Errorf("supervisor wire header is invalid")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return wireFrame{}, fmt.Errorf("read supervisor frame payload: %w", err)
	}
	return wireFrame{kind: kind, payload: payload}, nil
}

func writeFull(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func (kind frameKind) valid() bool { return kind >= frameReady && kind <= frameFinal }

func (kind frameKind) allowedOnEventPipe() bool {
	return kind == frameReady || kind == frameAccepted || kind == frameOutput ||
		kind == frameStatus || kind == frameFinal
}
