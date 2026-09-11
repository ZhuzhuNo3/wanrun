package terminal

import (
	"bytes"
	"strconv"
	"strings"
	"time"

	xterm "github.com/gitpod-io/xterm-go"
)

const escapeTimeout = 35 * time.Millisecond

type decodedInputKind uint8

const (
	decodedBytes decodedInputKind = iota + 1
	decodedMouse
	decodedPagePrevious
	decodedPageNext
)

type decodedInput struct {
	kind        decodedInputKind
	raw         []byte
	mouse       xterm.CoreMouseEvent
	mouseParsed bool
}

type terminalInput struct {
	pending  []byte
	deadline time.Time
}

func (input *terminalInput) decode(content []byte, now time.Time) []decodedInput {
	if len(content) != 0 {
		input.pending = append(input.pending, content...)
	}
	var decoded []decodedInput
	for len(input.pending) != 0 {
		event, consumed, complete := decodeOneInput(input.pending)
		if !complete {
			if input.deadline.IsZero() {
				input.deadline = now.Add(escapeTimeout)
			}
			break
		}
		input.deadline = time.Time{}
		decoded = append(decoded, event)
		input.pending = input.pending[consumed:]
	}
	if len(input.pending) == 0 {
		input.deadline = time.Time{}
	}
	return decoded
}

func (input *terminalInput) expire(now time.Time) []decodedInput {
	if len(input.pending) == 0 || input.deadline.IsZero() || now.Before(input.deadline) {
		return nil
	}
	raw := append([]byte(nil), input.pending...)
	input.pending = nil
	input.deadline = time.Time{}
	return []decodedInput{{kind: decodedBytes, raw: raw}}
}

func (input *terminalInput) clear() {
	input.pending = nil
	input.deadline = time.Time{}
}

func (input *terminalInput) nextDeadline() time.Time { return input.deadline }

func decodeOneInput(content []byte) (decodedInput, int, bool) {
	if content[0] != 0x1b {
		end := bytes.IndexByte(content, 0x1b)
		if end < 0 {
			end = len(content)
		}
		return decodedInput{kind: decodedBytes, raw: append([]byte(nil), content[:end]...)}, end, true
	}
	if len(content) == 1 {
		return decodedInput{}, 0, false
	}
	if content[1] != '[' && content[1] != 'O' {
		return decodedInput{kind: decodedBytes, raw: append([]byte(nil), content[:2]...)}, 2, true
	}
	if content[1] == 'O' {
		if len(content) < 3 {
			return decodedInput{}, 0, false
		}
		return decodedInput{kind: decodedBytes, raw: append([]byte(nil), content[:3]...)}, 3, true
	}
	end := csiSequenceEnd(content)
	if end < 0 {
		return decodedInput{}, 0, false
	}
	raw := append([]byte(nil), content[:end+1]...)
	if isSGRMouseSequence(raw) {
		mouse, parsed := decodeMouse(raw)
		return decodedInput{kind: decodedMouse, raw: raw, mouse: mouse,
			mouseParsed: parsed}, len(raw), true
	}
	switch string(raw) {
	case "\x1b[D":
		return decodedInput{kind: decodedPagePrevious, raw: raw}, len(raw), true
	case "\x1b[C":
		return decodedInput{kind: decodedPageNext, raw: raw}, len(raw), true
	default:
		return decodedInput{kind: decodedBytes, raw: raw}, len(raw), true
	}
}

func isSGRMouseSequence(raw []byte) bool {
	return len(raw) >= 4 && bytes.HasPrefix(raw, []byte("\x1b[<")) &&
		(raw[len(raw)-1] == 'M' || raw[len(raw)-1] == 'm')
}

func csiSequenceEnd(content []byte) int {
	for index := 2; index < len(content); index++ {
		if content[index] >= 0x40 && content[index] <= 0x7e {
			return index
		}
	}
	return -1
}

func decodeMouse(raw []byte) (xterm.CoreMouseEvent, bool) {
	if len(raw) < 6 || raw[len(raw)-1] != 'M' && raw[len(raw)-1] != 'm' {
		return xterm.CoreMouseEvent{}, false
	}
	parts := strings.Split(string(raw[3:len(raw)-1]), ";")
	if len(parts) != 3 {
		return xterm.CoreMouseEvent{}, false
	}
	values := make([]int, len(parts))
	for index, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || index > 0 && value < 1 {
			return xterm.CoreMouseEvent{}, false
		}
		values[index] = value
	}
	code := values[0]
	event := xterm.CoreMouseEvent{Col: values[1], Row: values[2],
		Shift: code&4 != 0, Alt: code&8 != 0, Ctrl: code&16 != 0}
	button, action, ok := decodeMouseIdentity(code)
	if !ok {
		return xterm.CoreMouseEvent{}, false
	}
	event.Button, event.Action = button, action
	if raw[len(raw)-1] == 'm' {
		if event.Button == xterm.MouseButtonWheel || event.Action == xterm.MouseActionMove {
			return xterm.CoreMouseEvent{}, false
		}
		event.Action = xterm.MouseActionUp
	}
	return event, true
}

func decodeMouseIdentity(code int) (xterm.CoreMouseButton, xterm.CoreMouseAction, bool) {
	code &^= 4 | 8 | 16
	if code >= 64 && code <= 67 {
		return xterm.MouseButtonWheel, xterm.CoreMouseAction(code - 64), true
	}
	action := xterm.MouseActionDown
	if code&32 != 0 {
		action = xterm.MouseActionMove
		code &^= 32
	}
	switch {
	case code >= 0 && code <= 3:
		return xterm.CoreMouseButton(code), action, true
	case code >= 128 && code <= 131:
		return xterm.CoreMouseButton(int(xterm.MouseButtonAux1) + code - 128), action, true
	case code >= 192 && code <= 195:
		return xterm.CoreMouseButton(int(xterm.MouseButtonAux5) + code - 192), action, true
	default:
		return 0, 0, false
	}
}
