package terminal

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	xterm "github.com/gitpod-io/xterm-go"
)

const terminalScrollbackRows = 4096

type transferScreen struct {
	terminal     *xterm.Terminal
	mouseEncoder *xterm.MouseStateService
	status       runsupervisor.TransferStatus
	scrollOffset int
}

func newTransferScreen(cols, rows int) *transferScreen {
	return &transferScreen{terminal: xterm.New(xterm.WithCols(cols), xterm.WithRows(rows),
		xterm.WithScrollback(terminalScrollbackRows)), mouseEncoder: xterm.NewMouseStateService()}
}

func (screen *transferScreen) write(content []byte) error {
	if len(content) == 0 {
		return errors.New("interactive terminal received empty PTY output")
	}
	before := screen.terminal.Buffer().YBase
	if _, err := screen.terminal.Write(content); err != nil {
		return err
	}
	after := screen.terminal.Buffer().YBase
	if screen.scrollOffset > 0 && after > before {
		screen.scrollOffset += after - before
	}
	screen.limitScroll(screen.terminal.Rows())
	return nil
}

func (screen *transferScreen) resize(cols, rows int) {
	screen.terminal.Resize(cols, rows)
	screen.limitScroll(rows)
}

func (screen *transferScreen) scrollMouse(event xterm.CoreMouseEvent, viewportRows int) {
	if event.Button != xterm.MouseButtonWheel {
		return
	}
	const rowsPerStep = 3
	if event.Action == xterm.MouseActionUp {
		screen.scrollOffset += rowsPerStep
	} else if event.Action == xterm.MouseActionDown {
		screen.scrollOffset -= rowsPerStep
	} else {
		return
	}
	screen.limitScroll(viewportRows)
}

func (screen *transferScreen) followLatest() { screen.scrollOffset = 0 }

func (screen *transferScreen) limitScroll(viewportRows int) {
	if screen.scrollOffset < 0 {
		screen.scrollOffset = 0
	}
	buffer := screen.terminal.Buffer()
	maximum := max(0, buffer.YBase+screen.terminal.Rows()-max(1, viewportRows))
	if screen.scrollOffset > maximum {
		screen.scrollOffset = maximum
	}
}

func (screen *transferScreen) encodeMouse(event xterm.CoreMouseEvent) ([]byte, bool) {
	modes := screen.terminal.DecPrivateModes()
	if modes.MouseEncoding == "SGR_PIXELS" {
		return nil, false
	}
	encoding := modes.MouseEncoding
	if encoding == "" {
		encoding = "DEFAULT"
	}
	if encoding != "DEFAULT" && encoding != "SGR" {
		return nil, false
	}
	screen.mouseEncoder.SetActiveProtocol(modes.MouseTrackingMode)
	screen.mouseEncoder.SetActiveEncoding(encoding)
	encoded, ok := screen.mouseEncoder.TriggerMouseEvent(event)
	if !ok {
		return nil, false
	}
	return []byte(encoded), true
}

func (screen *transferScreen) overviewPreview(count int) []frameLine {
	if count <= 0 {
		return nil
	}
	buffer := screen.terminal.Buffer()
	end := screen.contentEnd()
	start := max(0, end-count)
	return screen.renderRows(buffer, start, end, count)
}

func (screen *transferScreen) focusedViewport(count int) []frameLine {
	if count <= 0 {
		return nil
	}
	screen.limitScroll(count)
	buffer := screen.terminal.Buffer()
	end := buffer.YBase + screen.terminal.Rows() - screen.scrollOffset
	start := max(0, end-count)
	return screen.renderRows(buffer, start, end, count)
}

func (screen *transferScreen) finalActiveViewport() []frameLine {
	buffer := screen.terminal.Buffer()
	start := buffer.YBase
	end := min(buffer.Lines.Length(), start+screen.terminal.Rows())
	lines := make([]frameLine, 0, max(0, end-start))
	lastVisible := 0
	for row := start; row < end; row++ {
		line := buffer.Lines.Get(row)
		lines = append(lines, renderTerminalRow(line, screen.terminal.Cols()))
		if !terminalLineVisuallyBlank(line, screen.terminal.Cols()) {
			lastVisible = len(lines)
		}
	}
	return lines[:lastVisible]
}

func terminalLineVisuallyBlank(line *xterm.BufferLine, cols int) bool {
	if line == nil {
		return true
	}
	cell := xterm.NewCellData()
	for column := 0; column < min(cols, line.Len); column++ {
		line.LoadCell(column, cell)
		if terminalCellVisible(cell) {
			return false
		}
	}
	return true
}

func terminalCellVisible(cell *xterm.CellData) bool {
	if cell.GetBgColorMode() != xterm.AttrCMDefault || cell.IsInverse() != 0 ||
		cell.IsUnderline() != 0 || cell.IsStrikethrough() != 0 || cell.IsOverline() != 0 {
		return true
	}
	if cell.IsInvisible() != 0 {
		return false
	}
	for _, value := range cell.GetChars() {
		if !unicode.IsSpace(value) {
			return true
		}
	}
	return false
}

func (screen *transferScreen) renderRows(buffer *xterm.Buffer, start, end, count int) []frameLine {
	result := make([]frameLine, 0, count)
	for row := start; row < end && len(result) < count; row++ {
		result = append(result, renderTerminalRow(buffer.Lines.Get(row), screen.terminal.Cols()))
	}
	for len(result) < count {
		result = append(result, frameLine{kind: targetFrameLine})
	}
	return result
}

func (screen *transferScreen) contentEnd() int {
	buffer := screen.terminal.Buffer()
	end := min(buffer.Lines.Length(), buffer.YBase+buffer.Y+1)
	cell := xterm.NewCellData()
	for row := buffer.Lines.Length() - 1; row >= 0; row-- {
		line := buffer.Lines.Get(row)
		if line == nil {
			continue
		}
		for column := 0; column < line.Len; column++ {
			line.LoadCell(column, cell)
			if cell.GetChars() != "" {
				return max(end, row+1)
			}
		}
	}
	return end
}

func (screen *transferScreen) close() {
	screen.mouseEncoder.Dispose()
	screen.terminal.Dispose()
}

func renderTerminalRow(line *xterm.BufferLine, cols int) frameLine {
	if line == nil {
		return frameLine{kind: targetFrameLine}
	}
	plain, styled := terminalCellText(line, cols)
	plain = fitText(plain, cols)
	if displayWidth(stripSGR(styled)) != cols {
		styled = plain
	}
	return frameLine{text: plain, styled: styled, kind: targetFrameLine}
}

func terminalCellText(line *xterm.BufferLine, cols int) (string, string) {
	var plain, styled strings.Builder
	currentStyle := ""
	cell := xterm.NewCellData()
	for column := 0; column < min(cols, line.Len); column++ {
		line.LoadCell(column, cell)
		width := cell.GetWidth()
		if width == 0 {
			continue
		}
		value := cell.GetChars()
		if value == "" {
			value = strings.Repeat(" ", width)
		}
		plain.WriteString(value)
		style := cellSGR(cell)
		if style != currentStyle {
			if currentStyle != "" {
				styled.WriteString("\x1b[0m")
			}
			if style != "" {
				styled.WriteString(style)
			}
			currentStyle = style
		}
		styled.WriteString(value)
	}
	if currentStyle != "" {
		styled.WriteString("\x1b[0m")
	}
	return plain.String(), styled.String()
}

func cellSGR(cell *xterm.CellData) string {
	var codes []string
	appendFlag := func(set uint32, code string) {
		if set != 0 {
			codes = append(codes, code)
		}
	}
	appendFlag(cell.IsBold(), "1")
	appendFlag(cell.IsDim(), "2")
	appendFlag(cell.IsItalic(), "3")
	appendFlag(cell.IsUnderline(), "4")
	appendFlag(cell.IsBlink(), "5")
	appendFlag(cell.IsInverse(), "7")
	appendFlag(cell.IsInvisible(), "8")
	appendFlag(cell.IsStrikethrough(), "9")
	appendFlag(cell.IsOverline(), "53")
	codes = append(codes, colorSGR(cell.GetFgColorMode(), cell.GetFgColor(), false)...)
	codes = append(codes, colorSGR(cell.GetBgColorMode(), cell.GetBgColor(), true)...)
	if len(codes) == 0 {
		return ""
	}
	return "\x1b[" + strings.Join(codes, ";") + "m"
}

func colorSGR(mode uint32, color int, background bool) []string {
	if color < 0 {
		return nil
	}
	switch mode {
	case xterm.AttrCMP16:
		base := 30
		if background {
			base = 40
		}
		if color >= 8 {
			base += 60
			color -= 8
		}
		return []string{strconv.Itoa(base + color)}
	case xterm.AttrCMP256:
		prefix := "38"
		if background {
			prefix = "48"
		}
		return []string{prefix, "5", strconv.Itoa(color)}
	case xterm.AttrCMRGB:
		prefix := "38"
		if background {
			prefix = "48"
		}
		return []string{prefix, "2", strconv.Itoa((color >> 16) & 0xff),
			strconv.Itoa((color >> 8) & 0xff), strconv.Itoa(color & 0xff)}
	default:
		return nil
	}
}

func transferStatusText(status runsupervisor.TransferStatus) string {
	switch status.State {
	case runsupervisor.TransferRunning:
		return "RUNNING"
	case runsupervisor.TransferExited:
		if status.Signal != 0 {
			return fmt.Sprintf("EXIT signal=%d", status.Signal)
		}
		return fmt.Sprintf("EXIT %d", status.ExitCode)
	default:
		return "PREPARING"
	}
}
