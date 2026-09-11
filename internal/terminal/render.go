package terminal

import (
	"fmt"
	"strings"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/rivo/uniseg"
)

const minimumPaneRows = 4

type frameLineKind uint8

const (
	plainFrameLine frameLineKind = iota
	titleFrameLine
	actionFrameLine
	paneTitleFrameLine
	notificationFrameLine
	targetFrameLine
)

type frameLine struct {
	text   string
	styled string
	kind   frameLineKind
}

type overviewLayout struct {
	title       []frameLine
	actions     []frameLine
	contentRows int
	visible     int
	pageCount   int
	start       int
	end         int
}

func (display *InteractiveTerminal) plainFrame() []string {
	frame := display.renderFrame()
	result := make([]string, len(frame))
	for index := range frame {
		result[index] = frame[index].text
	}
	return result
}

func (display *InteractiveTerminal) styledView() string {
	frame := display.renderFrame()
	rows := make([]string, len(frame))
	for index, line := range frame {
		rows[index] = styledFrameLine(line)
	}
	return strings.Join(rows, "\r\n")
}

func (display *InteractiveTerminal) renderFrame() []frameLine {
	var rows []frameLine
	if display.mode == viewFocused {
		rows = display.renderFocused()
	} else {
		rows = display.renderOverview()
	}
	return fitFrame(rows, display.geometry.cols, display.geometry.rows)
}

func (display *InteractiveTerminal) renderOverview() []frameLine {
	layout := display.calculateOverviewLayout()
	display.page = min(display.page, layout.pageCount-1)
	rows := append([]frameLine(nil), layout.title...)
	rows = append(rows, layout.actions...)
	rows = append(rows, frameLine{text: strings.Repeat("-", display.geometry.cols)})
	if layout.contentRows <= 0 || layout.end <= layout.start {
		return rows
	}
	heights := divideRows(layout.contentRows, layout.end-layout.start)
	for offset, height := range heights {
		index := layout.start + offset
		transfer := display.transfers[index]
		screen := display.screens[transfer.number]
		shortcut, _ := transferShortcut(index + 1)
		rows = append(rows, frameLine{kind: paneTitleFrameLine, text: fmt.Sprintf(
			"[%c] transfer %d/%d | network %s | %s", shortcut, index+1,
			len(display.transfers), transfer.network, transferStatusText(screen.status))})
		rows = append(rows, screen.overviewPreview(height-1)...)
	}
	return rows
}

func (display *InteractiveTerminal) calculateOverviewLayout() overviewLayout {
	visible := len(display.transfers)
	pageCount := 1
	var layout overviewLayout
	for range 8 {
		page := min(display.page, pageCount-1)
		start := page * visible
		end := min(len(display.transfers), start+visible)
		title := styledLines(wrapSegments([]string{
			"transferlanes", "OVERVIEW",
			fmt.Sprintf("transfers %d-%d/%d", start+1, end, len(display.transfers)),
			fmt.Sprintf("page %d/%d", page+1, pageCount),
		}, display.geometry.cols), titleFrameLine)
		actionItems := []string{shortcutRange(len(display.transfers)) + " focus transfer",
			"Ctrl-C cancel all transfers"}
		if pageCount > 1 {
			actionItems = append(actionItems, "Left/Right change page")
		}
		actions := styledLines(wrapActions("DIRECT", actionItems, display.geometry.cols), actionFrameLine)
		contentRows := display.geometry.rows - len(title) - len(actions) - 1
		newVisible := min(len(display.transfers), max(1, contentRows/minimumPaneRows))
		newPageCount := max(1, (len(display.transfers)+newVisible-1)/newVisible)
		layout = overviewLayout{title: title, actions: actions, contentRows: contentRows,
			visible: newVisible, pageCount: newPageCount, start: start, end: end}
		if visible == newVisible && pageCount == newPageCount {
			return layout
		}
		visible, pageCount = newVisible, newPageCount
	}
	return layout
}

func (display *InteractiveTerminal) overviewPageCount() int {
	return display.calculateOverviewLayout().pageCount
}

func (display *InteractiveTerminal) renderFocused() []frameLine {
	transfer := display.transfers[display.focused-1]
	screen := display.screens[transfer.number]
	rows := focusedChrome(display, transfer, screen.status)
	return append(rows, screen.focusedViewport(display.geometry.targetRows)...)
}

func focusedChrome(display *InteractiveTerminal, transfer Transfer,
	status runsupervisor.TransferStatus,
) []frameLine {
	rows := fitSectionRows(styledLines(focusedTitle(transfer, len(display.transfers), status,
		display.mouse, display.geometry.cols), titleFrameLine), display.geometry.focusedTitleRows)
	rows = append(rows, fitSectionRows(styledLines(focusedDirectActions(len(display.transfers),
		display.geometry.cols), actionFrameLine), display.geometry.directActionRows)...)
	rows = append(rows, fitSectionRows(styledLines(focusedLeaderActions(len(display.transfers),
		display.geometry.cols), actionFrameLine), display.geometry.leaderActionRows)...)
	notice := frameLine{text: display.notice}
	if display.notice != "" {
		notice.kind = notificationFrameLine
	}
	rows = append(rows, notice)
	return append(rows, frameLine{text: strings.Repeat("-", display.geometry.cols)})
}

func focusedTitle(transfer Transfer, count int, status runsupervisor.TransferStatus,
	mouse MouseMode, cols int,
) []string {
	return wrapSegments([]string{
		"transferlanes", "FOCUSED", fmt.Sprintf("transfer %d/%d", transfer.number.Value(), count),
		"network " + transfer.network.String(), transferStatusText(status), "mouse: " + mouse.String(),
	}, cols)
}

func focusedDirectActions(_ int, cols int) []string {
	item := "Ctrl-C cancel all transfers"
	if cols < 60 {
		item = "Ctrl-C cancel all"
	}
	return wrapActions("DIRECT", []string{item}, cols)
}

func focusedLeaderActions(count, cols int) []string {
	items := []string{"0 overview", shortcutRange(count) + " switch transfer", "\\ toggle mouse",
		"Ctrl-] send literal Ctrl-]"}
	if cols < 60 {
		items = []string{"0 overview", shortcutRange(count) + " transfer", "\\ mouse", "Ctrl-] literal"}
	}
	return wrapActions("AFTER Ctrl-]", items, cols)
}

func fitSectionRows(rows []frameLine, height int) []frameLine {
	result := make([]frameLine, height)
	copy(result, rows)
	return result
}

func styledLines(lines []string, kind frameLineKind) []frameLine {
	result := make([]frameLine, len(lines))
	for index, line := range lines {
		result[index] = frameLine{text: line, kind: kind}
	}
	return result
}

func shortcutRange(count int) string {
	if count <= 9 {
		return fmt.Sprintf("1-%d", count)
	}
	last, _ := transferShortcut(count)
	return fmt.Sprintf("1-9,a-%c", last)
}

func wrapSegments(segments []string, width int) []string {
	if len(segments) == 0 {
		return nil
	}
	var rows []string
	current := segments[0]
	for _, segment := range segments[1:] {
		candidate := current + " | " + segment
		if displayWidth(candidate) <= width {
			current = candidate
			continue
		}
		rows = append(rows, splitDisplayText(current, width)...)
		current = segment
	}
	return append(rows, splitDisplayText(current, width)...)
}

func wrapActions(label string, items []string, width int) []string {
	gap := max(1, 15-displayWidth(label))
	if width < 60 {
		gap = 1
	}
	prefix := label + strings.Repeat(" ", gap)
	if displayWidth(prefix) >= width {
		rows := splitDisplayText(prefix, width)
		return append(rows, wrapWords(strings.Fields(strings.Join(items, " | ")), width)...)
	}
	continuation := strings.Repeat(" ", displayWidth(prefix))
	contents := wrapWords(strings.Fields(strings.Join(items, " | ")), width-displayWidth(prefix))
	rows := make([]string, len(contents))
	for index, content := range contents {
		if index == 0 {
			rows[index] = prefix + content
		} else {
			rows[index] = continuation + content
		}
	}
	return rows
}

func wrapWords(words []string, width int) []string {
	if len(words) == 0 {
		return []string{""}
	}
	var rows []string
	current := ""
	for _, word := range words {
		candidate := word
		if current != "" {
			candidate = current + " " + word
		}
		if displayWidth(candidate) <= width {
			current = candidate
			continue
		}
		if current != "" {
			rows = append(rows, current)
			current = ""
		}
		parts := splitDisplayText(word, width)
		if len(parts) > 1 {
			rows = append(rows, parts[:len(parts)-1]...)
		}
		current = parts[len(parts)-1]
	}
	return append(rows, current)
}

func splitDisplayText(value string, width int) []string {
	if width <= 0 {
		return nil
	}
	var rows []string
	var current strings.Builder
	currentWidth := 0
	graphemes := uniseg.NewGraphemes(value)
	for graphemes.Next() {
		cluster := graphemes.Str()
		clusterWidth := uniseg.StringWidth(cluster)
		if currentWidth > 0 && currentWidth+clusterWidth > width {
			rows = append(rows, current.String())
			current.Reset()
			currentWidth = 0
		}
		if clusterWidth > width {
			continue
		}
		current.WriteString(cluster)
		currentWidth += clusterWidth
	}
	if current.Len() != 0 || len(rows) == 0 {
		rows = append(rows, current.String())
	}
	return rows
}

func divideRows(total, count int) []int {
	if count < 1 {
		return nil
	}
	result := make([]int, count)
	for index := range result {
		result[index] = total / count
		if index < total%count {
			result[index]++
		}
	}
	return result
}

func fitFrame(rows []frameLine, width, height int) []frameLine {
	frame := make([]frameLine, height)
	for row := range frame {
		if row < len(rows) {
			frame[row] = rows[row]
		}
		frame[row].text = fitText(frame[row].text, width)
		if frame[row].styled != "" && displayWidth(stripSGR(frame[row].styled)) != width {
			frame[row].styled = ""
		}
	}
	return frame
}

func fitText(value string, width int) string {
	if width <= 0 {
		return ""
	}
	valueWidth := displayWidth(value)
	if valueWidth <= width {
		return value + strings.Repeat(" ", width-valueWidth)
	}
	limit := width - 1
	var output strings.Builder
	used := 0
	graphemes := uniseg.NewGraphemes(value)
	for graphemes.Next() {
		cluster := graphemes.Str()
		clusterWidth := uniseg.StringWidth(cluster)
		if used+clusterWidth > limit {
			break
		}
		output.WriteString(cluster)
		used += clusterWidth
	}
	output.WriteRune('…')
	used++
	return output.String() + strings.Repeat(" ", width-used)
}

func displayWidth(value string) int { return uniseg.StringWidth(value) }

func styledFrameLine(line frameLine) string {
	if line.kind == targetFrameLine && line.styled != "" {
		return line.styled
	}
	style := ""
	switch line.kind {
	case titleFrameLine:
		style = "\x1b[1;36m"
	case actionFrameLine:
		style = "\x1b[1m"
	case paneTitleFrameLine:
		style = "\x1b[1;34m"
	case notificationFrameLine:
		style = "\x1b[33m"
	}
	if style == "" {
		return line.text
	}
	return style + line.text + "\x1b[0m"
}

func stripSGR(value string) string {
	var output strings.Builder
	for index := 0; index < len(value); {
		if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '[' {
			end := index + 2
			for end < len(value) && value[end] != 'm' {
				end++
			}
			if end < len(value) {
				index = end + 1
				continue
			}
		}
		output.WriteByte(value[index])
		index++
	}
	return output.String()
}
