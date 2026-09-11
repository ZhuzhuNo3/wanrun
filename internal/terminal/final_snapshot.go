package terminal

import (
	"fmt"
	"strings"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const sgrReset = "\x1b[0m"

type finalSnapshot struct {
	title string
	lines []string
}

func (display *InteractiveTerminal) finalSnapshots(
	results []runsupervisor.TransferResult,
) ([]finalSnapshot, error) {
	byTransfer, err := display.finalResultsByTransfer(results)
	if err != nil {
		return nil, err
	}
	for _, transfer := range display.transfers {
		if display.screens[transfer.number] == nil {
			return nil, fmt.Errorf("final snapshot screen for transfer %d is missing", transfer.number.Value())
		}
	}

	snapshots := make([]finalSnapshot, 0, len(display.transfers))
	for _, transfer := range display.transfers {
		result := byTransfer[transfer.number]
		snapshots = append(snapshots, newFinalSnapshot(
			transfer, result, display.screens[transfer.number].finalActiveViewport(),
		))
	}
	return snapshots, nil
}

func (display *InteractiveTerminal) finalResultsByTransfer(
	results []runsupervisor.TransferResult,
) (map[transfernumber.Number]runsupervisor.TransferResult, error) {
	if len(results) != len(display.transfers) {
		return nil, fmt.Errorf("final snapshot results count is %d, want %d", len(results), len(display.transfers))
	}
	known := make(map[transfernumber.Number]struct{}, len(display.transfers))
	for _, transfer := range display.transfers {
		known[transfer.number] = struct{}{}
	}
	byTransfer := make(map[transfernumber.Number]runsupervisor.TransferResult, len(results))
	for _, result := range results {
		if _, ok := known[result.Transfer]; !ok {
			return nil, fmt.Errorf("final snapshot result identifies unknown transfer %d", result.Transfer.Value())
		}
		if _, duplicate := byTransfer[result.Transfer]; duplicate {
			return nil, fmt.Errorf("final snapshot result for transfer %d is duplicated", result.Transfer.Value())
		}
		byTransfer[result.Transfer] = result
	}
	return byTransfer, nil
}

func newFinalSnapshot(transfer Transfer, result runsupervisor.TransferResult,
	lines []frameLine,
) finalSnapshot {
	body := make([]string, len(lines))
	for index, line := range lines {
		body[index] = line.styled
	}
	return finalSnapshot{
		title: fmt.Sprintf("[transfer %d | network %s | exit=%d signal=%d]",
			transfer.number.Value(), transfer.network, result.ExitCode, result.Signal),
		lines: body,
	}
}

func serializeFinalSnapshots(snapshots []finalSnapshot) []byte {
	var output strings.Builder
	for _, snapshot := range snapshots {
		writeResetLine(&output, snapshot.title)
		for _, line := range snapshot.lines {
			writeResetLine(&output, line)
		}
	}
	return []byte(output.String())
}

func writeResetLine(output *strings.Builder, line string) {
	output.WriteString(sgrReset)
	output.WriteString(line)
	output.WriteString(sgrReset)
	output.WriteString("\r\n")
}
