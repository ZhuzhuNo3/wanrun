package terminal

import (
	"errors"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	xterm "github.com/gitpod-io/xterm-go"
)

type interactiveGeometry struct {
	cols             int
	rows             int
	focusedTitleRows int
	directActionRows int
	leaderActionRows int
	chromeRows       int
	targetRows       int
}

func calculateInteractiveGeometry(transfers []Transfer, cols, rows int) (interactiveGeometry, error) {
	if cols < xterm.MinimumCols || rows < xterm.MinimumRows || cols > 65535 || rows > 65535 {
		return interactiveGeometry{}, errors.New("interactive terminal size is invalid")
	}
	titleRows := maximumFocusedTitleRows(transfers, cols)
	directRows := len(focusedDirectActions(len(transfers), cols))
	leaderRows := len(focusedLeaderActions(len(transfers), cols))
	chromeRows := titleRows + directRows + leaderRows + 2
	if chromeRows >= rows {
		return interactiveGeometry{}, errors.New("interactive terminal is too short for its controls")
	}
	return interactiveGeometry{cols: cols, rows: rows, focusedTitleRows: titleRows,
		directActionRows: directRows, leaderActionRows: leaderRows, chromeRows: chromeRows,
		targetRows: rows - chromeRows}, nil
}

func maximumFocusedTitleRows(transfers []Transfer, cols int) int {
	statuses := []runsupervisor.TransferStatus{
		{},
		{State: runsupervisor.TransferRunning},
		{State: runsupervisor.TransferExited, ExitCode: 255},
		{State: runsupervisor.TransferExited, Signal: 255},
	}
	maximum := 0
	for _, transfer := range transfers {
		for _, status := range statuses {
			for _, mouse := range []MouseMode{MouseScroll, MouseTarget} {
				rows := focusedTitle(transfer, len(transfers), status, mouse, cols)
				maximum = max(maximum, len(rows))
			}
		}
	}
	return maximum
}
