package childprocesses

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	helperHandoffHeaderBytes  = 4
	maximumHelperFailureBytes = 512
)

type helperHandoffKind uint8

const (
	helperPrepared helperHandoffKind = iota + 1
	helperExecFailed
)

type helperHandoff struct {
	kind       helperHandoffKind
	diagnostic string
}

func writeHelperHandoff(writer io.Writer, handoff helperHandoff) error {
	handoff.diagnostic = normalizeHelperFailure(handoff.diagnostic)
	if handoff.kind == helperPrepared && handoff.diagnostic != "" ||
		handoff.kind == helperExecFailed && handoff.diagnostic == "" ||
		handoff.kind < helperPrepared || handoff.kind > helperExecFailed {
		return fmt.Errorf("child helper handoff is invalid")
	}
	payload := []byte(handoff.diagnostic)
	header := []byte{helperVersion, byte(handoff.kind), 0, 0}
	binary.BigEndian.PutUint16(header[2:], uint16(len(payload)))
	if err := writeHelperBytes(writer, header); err != nil {
		return fmt.Errorf("write child helper handoff header: %w", err)
	}
	if err := writeHelperBytes(writer, payload); err != nil {
		return fmt.Errorf("write child helper handoff payload: %w", err)
	}
	return nil
}

func readHelperHandoff(reader io.Reader) (helperHandoff, error) {
	header := make([]byte, helperHandoffHeaderBytes)
	if _, err := io.ReadFull(reader, header); err != nil {
		return helperHandoff{}, err
	}
	size := int(binary.BigEndian.Uint16(header[2:]))
	kind := helperHandoffKind(header[1])
	if header[0] != helperVersion || size > maximumHelperFailureBytes ||
		kind < helperPrepared || kind > helperExecFailed ||
		kind == helperPrepared && size != 0 || kind == helperExecFailed && size == 0 {
		return helperHandoff{}, fmt.Errorf("child helper handoff header is invalid")
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return helperHandoff{}, fmt.Errorf("read child helper handoff payload: %w", err)
	}
	if !utf8.Valid(payload) {
		return helperHandoff{}, fmt.Errorf("child helper handoff diagnostic is invalid")
	}
	return helperHandoff{kind: kind, diagnostic: string(payload)}, nil
}

func normalizeHelperFailure(value string) string {
	value = strings.ToValidUTF8(value, "\ufffd")
	if len(value) <= maximumHelperFailureBytes {
		return value
	}
	end := maximumHelperFailureBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end]
}

func writeHelperBytes(writer io.Writer, contents []byte) error {
	for len(contents) > 0 {
		written, err := writer.Write(contents)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(contents) {
			return io.ErrShortWrite
		}
		contents = contents[written:]
	}
	return nil
}
