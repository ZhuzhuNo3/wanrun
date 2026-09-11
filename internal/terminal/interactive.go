package terminal

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	xterm "github.com/gitpod-io/xterm-go"
)

const (
	controlC      byte = 0x03
	leaderByte    byte = 0x1d
	leaderTimeout      = 2 * time.Second

	notificationTimeout = 1500 * time.Millisecond
)

type viewMode uint8

const (
	viewOverview viewMode = iota
	viewFocused
)

type instructionKind uint8

const (
	writeTargetInput instructionKind = iota + 1
	resizeTarget
	cancelRun
)

type terminalInstruction struct {
	kind     instructionKind
	transfer transfernumber.Number
	content  []byte
	cols     uint16
	rows     uint16
	cancel   runsupervisor.CancelReason
}

// InteractiveTerminal owns presentation state only. Live process and supervisor ownership stay outside it.
type InteractiveTerminal struct {
	transfers   []Transfer
	screens     map[transfernumber.Number]*transferScreen
	mode        viewMode
	focused     int
	page        int
	mouse       MouseMode
	geometry    interactiveGeometry
	input       terminalInput
	leaderUntil time.Time
	notice      string
	noticeUntil time.Time
	cancelled   bool
	closed      bool
}

func NewInteractiveTerminal(transfers []Transfer, cols, rows int,
	mouse MouseMode,
) (*InteractiveTerminal, error) {
	if len(transfers) == 0 || len(transfers) > transfernumber.Maximum ||
		(mouse != MouseScroll && mouse != MouseTarget) {
		return nil, errors.New("interactive terminal inputs are invalid")
	}
	ordered := append([]Transfer(nil), transfers...)
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].number.Value() < ordered[right].number.Value()
	})
	geometry, err := calculateInteractiveGeometry(ordered, cols, rows)
	if err != nil {
		return nil, err
	}
	screens := make(map[transfernumber.Number]*transferScreen, len(ordered))
	display := &InteractiveTerminal{transfers: ordered, screens: screens, focused: 1,
		mouse: mouse, geometry: geometry}
	for index, transfer := range ordered {
		if transfer.number.Value() != uint8(index+1) || !transfer.network.Is4() {
			display.Close()
			return nil, errors.New("interactive transfers must have contiguous identities")
		}
		screens[transfer.number] = newTransferScreen(geometry.cols, geometry.targetRows)
	}
	return display, nil
}

func (display *InteractiveTerminal) apply(event runsupervisor.Event) error {
	if event.Kind() == runsupervisor.EventFinal {
		return errors.New("interactive terminal does not consume final as a transfer event")
	}
	screen := display.screens[event.Transfer()]
	if screen == nil {
		return fmt.Errorf("interactive terminal received event for unknown transfer %d", event.Transfer().Value())
	}
	switch event.Kind() {
	case runsupervisor.EventOutput:
		if event.Stream() != runsupervisor.OutputPTY {
			return errors.New("interactive terminal received non-PTY command output")
		}
		if err := screen.write(event.Bytes()); err != nil {
			return fmt.Errorf("render transfer %d PTY: %w", event.Transfer().Value(), err)
		}
		return nil
	case runsupervisor.EventStatus:
		return display.updateStatus(event.Status())
	default:
		return errors.New("interactive terminal received unknown event")
	}
}

func (display *InteractiveTerminal) updateStatus(status runsupervisor.TransferStatus) error {
	screen := display.screens[status.Transfer]
	if screen == nil || status.State < runsupervisor.TransferRunning || status.State > runsupervisor.TransferExited {
		return errors.New("interactive terminal received invalid transfer status")
	}
	if screen.status.State == 0 && status.State != runsupervisor.TransferRunning ||
		screen.status.State == runsupervisor.TransferExited ||
		screen.status.State == runsupervisor.TransferRunning && status.State != runsupervisor.TransferExited {
		return errors.New("interactive terminal received invalid transfer status transition")
	}
	screen.status = status
	return nil
}

func (display *InteractiveTerminal) interpretInput(content []byte, now time.Time) []terminalInstruction {
	var instructions []terminalInstruction
	instructions = append(instructions, display.expire(now)...)
	if display.cancelled || len(content) == 0 {
		return instructions
	}
	if interrupt := bytes.IndexByte(content, controlC); interrupt >= 0 {
		instructions = append(instructions, display.interpretWithoutInterrupt(content[:interrupt], now)...)
		display.input.clear()
		display.cancelled = true
		return append(instructions, terminalInstruction{kind: cancelRun, cancel: runsupervisor.CancelUser})
	}
	return append(instructions, display.interpretWithoutInterrupt(content, now)...)
}

func (display *InteractiveTerminal) interpretWithoutInterrupt(content []byte,
	now time.Time,
) []terminalInstruction {
	var instructions []terminalInstruction
	for len(content) != 0 {
		if display.leaderPending() {
			value := content[0]
			content = content[1:]
			instructions = append(instructions, display.handleLeader(value, now)...)
			continue
		}
		decoded := display.input.decode(content, now)
		content = nil
		for _, event := range decoded {
			instructions = append(instructions, display.handleDecoded(event, now)...)
		}
	}
	return instructions
}

func (display *InteractiveTerminal) handleDecoded(event decodedInput,
	now time.Time,
) []terminalInstruction {
	switch event.kind {
	case decodedMouse:
		return display.handleMouse(event)
	case decodedPagePrevious:
		if display.mode == viewOverview {
			display.changePage(-1)
			return nil
		}
		return display.targetInput(event.raw)
	case decodedPageNext:
		if display.mode == viewOverview {
			display.changePage(1)
			return nil
		}
		return display.targetInput(event.raw)
	default:
		return display.handleBytes(event.raw, now)
	}
}

func (display *InteractiveTerminal) handleBytes(content []byte, now time.Time) []terminalInstruction {
	var instructions []terminalInstruction
	for len(content) != 0 {
		if display.mode == viewOverview {
			value := content[0]
			content = content[1:]
			if number, ok := transferNumberForShortcut(value); ok {
				display.focus(number, now)
			}
			continue
		}
		if display.leaderPending() {
			instructions = append(instructions, display.handleLeader(content[0], now)...)
			content = content[1:]
			continue
		}
		leader := bytes.IndexByte(content, leaderByte)
		if leader < 0 {
			return append(instructions, display.targetInput(content)...)
		}
		if leader > 0 {
			instructions = append(instructions, display.targetInput(content[:leader])...)
		}
		display.leaderUntil = now.Add(leaderTimeout)
		display.setNoticeUntil("Leader active for 2s: press a Transfer Lanes command", display.leaderUntil)
		content = content[leader+1:]
	}
	return instructions
}

func (display *InteractiveTerminal) handleLeader(value byte, now time.Time) []terminalInstruction {
	display.clearLeader()
	display.clearNotice()
	switch {
	case value == '0':
		display.mode = viewOverview
	case value == '\\':
		if display.mouse == MouseScroll {
			display.mouse = MouseTarget
			display.focusedScreen().followLatest()
		} else {
			display.mouse = MouseScroll
		}
		display.setNoticeUntil("mouse mode changed to "+display.mouse.String(), now.Add(notificationTimeout))
	case value == leaderByte:
		return display.targetInput([]byte{leaderByte})
	case value == 0x1b:
		// Escape only cancels a pending Leader.
	default:
		if number, ok := transferNumberForShortcut(value); ok {
			display.focus(number, now)
		} else {
			display.setNoticeUntil(fmt.Sprintf("unknown Leader command: %s", printableKey(value)),
				now.Add(notificationTimeout))
		}
	}
	return nil
}

func (display *InteractiveTerminal) handleMouse(event decodedInput) []terminalInstruction {
	if display.mode != viewFocused || !event.mouseParsed {
		return nil
	}
	mouse, ok := display.targetMouse(event.mouse)
	if !ok {
		return nil
	}
	screen := display.focusedScreen()
	if display.mouse == MouseScroll {
		screen.scrollMouse(mouse, display.geometry.targetRows)
		return nil
	}
	encoded, ok := screen.encodeMouse(mouse)
	if !ok {
		return nil
	}
	return display.targetInput(encoded)
}

func (display *InteractiveTerminal) targetMouse(mouse xterm.CoreMouseEvent) (xterm.CoreMouseEvent, bool) {
	row := mouse.Row - display.geometry.chromeRows
	if mouse.Col < 1 || mouse.Col > display.geometry.cols || row < 1 || row > display.geometry.targetRows {
		return xterm.CoreMouseEvent{}, false
	}
	mouse.Row = row
	mouse.X, mouse.Y = 0, 0
	return mouse, true
}

func (display *InteractiveTerminal) targetInput(content []byte) []terminalInstruction {
	if display.mode != viewFocused || len(content) == 0 {
		return nil
	}
	return []terminalInstruction{{kind: writeTargetInput,
		transfer: display.transfers[display.focused-1].number, content: append([]byte(nil), content...)}}
}

func (display *InteractiveTerminal) focus(number int, now time.Time) {
	if number < 1 || number > len(display.transfers) {
		display.setNoticeUntil(fmt.Sprintf("transfer %d does not exist", number),
			now.Add(notificationTimeout))
		return
	}
	display.mode = viewFocused
	display.focused = number
	display.clearLeader()
	display.clearNotice()
}

func (display *InteractiveTerminal) expire(now time.Time) []terminalInstruction {
	var instructions []terminalInstruction
	for _, event := range display.input.expire(now) {
		instructions = append(instructions, display.handleDecoded(event, now)...)
	}
	if display.leaderPending() && !now.Before(display.leaderUntil) {
		display.clearLeader()
	}
	if display.notice != "" && !now.Before(display.noticeUntil) {
		display.clearNotice()
	}
	return instructions
}

func (display *InteractiveTerminal) nextDeadline() time.Time {
	result := display.input.nextDeadline()
	for _, deadline := range []time.Time{display.leaderUntil, display.noticeUntil} {
		if !deadline.IsZero() && (result.IsZero() || deadline.Before(result)) {
			result = deadline
		}
	}
	return result
}

func (display *InteractiveTerminal) leaderPending() bool { return !display.leaderUntil.IsZero() }
func (display *InteractiveTerminal) clearLeader()        { display.leaderUntil = time.Time{} }
func (display *InteractiveTerminal) clearNotice() {
	display.notice = ""
	display.noticeUntil = time.Time{}
}
func (display *InteractiveTerminal) setNoticeUntil(message string, until time.Time) {
	display.notice, display.noticeUntil = message, until
}

func (display *InteractiveTerminal) changePage(delta int) {
	pages := display.overviewPageCount()
	if display.mode != viewOverview || pages < 2 {
		return
	}
	display.page = (display.page + delta + pages) % pages
}

func (display *InteractiveTerminal) resize(cols, rows int) ([]terminalInstruction, error) {
	geometry, err := calculateInteractiveGeometry(display.transfers, cols, rows)
	if err != nil {
		return nil, err
	}
	display.geometry = geometry
	var instructions []terminalInstruction
	for _, transfer := range display.transfers {
		screen := display.screens[transfer.number]
		screen.resize(geometry.cols, geometry.targetRows)
		instructions = append(instructions, terminalInstruction{kind: resizeTarget,
			transfer: transfer.number, cols: uint16(geometry.cols), rows: uint16(geometry.targetRows)})
	}
	return instructions, nil
}

func (display *InteractiveTerminal) focusedScreen() *transferScreen {
	return display.screens[display.transfers[display.focused-1].number]
}

func (display *InteractiveTerminal) focusedViewportRows() int {
	return display.geometry.targetRows
}

// TargetSize is the authoritative initial and runtime size of every child PTY.
func (display *InteractiveTerminal) TargetSize() (int, int) {
	return display.geometry.cols, display.geometry.targetRows
}

// Close releases every terminal screen still owned by this display.
func (display *InteractiveTerminal) Close() error {
	if display == nil || display.closed {
		return nil
	}
	display.closed = true
	for _, screen := range display.screens {
		screen.close()
	}
	return nil
}

func transferShortcut(number int) (byte, bool) {
	switch {
	case number >= 1 && number <= 9:
		return byte('0' + number), true
	case number >= 10 && number <= transfernumber.Maximum:
		return byte('a' + number - 10), true
	default:
		return 0, false
	}
}

func transferNumberForShortcut(value byte) (int, bool) {
	switch {
	case value >= '1' && value <= '9':
		return int(value - '0'), true
	case value >= 'a' && value <= 'z':
		return int(value-'a') + 10, true
	default:
		return 0, false
	}
}

func printableKey(value byte) string {
	if value >= 0x20 && value <= 0x7e {
		return fmt.Sprintf("%q", value)
	}
	return fmt.Sprintf("0x%02x", value)
}
