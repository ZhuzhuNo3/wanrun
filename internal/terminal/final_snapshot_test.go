package terminal

import (
	"strings"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestFinalSnapshotsMatchResultsAndUseTransferOrder(t *testing.T) {
	display := newInteractiveFixture(t, 3, MouseScroll, 60, 18)
	defer display.Close()
	writeTransferOutput(t, display, 1, "first")
	writeTransferOutput(t, display, 2, "second")
	writeTransferOutput(t, display, 3, "third")

	snapshots, err := display.finalSnapshots([]runsupervisor.TransferResult{
		finalTransferResult(t, 3, 0, 15),
		finalTransferResult(t, 1, 0, 0),
		finalTransferResult(t, 2, 7, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	plain := stripSGR(string(serializeFinalSnapshots(snapshots)))
	wants := []string{
		"[transfer 1 | network 192.0.2.1 | exit=0 signal=0]\r\nfirst",
		"[transfer 2 | network 192.0.2.2 | exit=7 signal=0]\r\nsecond",
		"[transfer 3 | network 192.0.2.3 | exit=0 signal=15]\r\nthird",
	}
	position := -1
	for _, want := range wants {
		next := strings.Index(plain, want)
		if next <= position {
			t.Fatalf("snapshot %q is missing or out of order:\n%q", want, plain)
		}
		position = next
	}
	if strings.Contains(plain, "\r\n\r\n[transfer") {
		t.Fatalf("snapshot blocks contain an extra separator line: %q", plain)
	}
}

func TestFinalSnapshotsRejectIncompleteOrAmbiguousResults(t *testing.T) {
	display := newInteractiveFixture(t, 2, MouseScroll, 60, 16)
	defer display.Close()
	tests := map[string][]runsupervisor.TransferResult{
		"missing":   {finalTransferResult(t, 1, 0, 0)},
		"duplicate": {finalTransferResult(t, 1, 0, 0), finalTransferResult(t, 1, 1, 0)},
		"extra": {finalTransferResult(t, 1, 0, 0), finalTransferResult(t, 2, 0, 0),
			finalTransferResult(t, 3, 0, 0)},
	}
	for name, results := range tests {
		t.Run(name, func(t *testing.T) {
			if snapshots, err := display.finalSnapshots(results); err == nil || snapshots != nil {
				t.Fatalf("finalSnapshots() = %#v, %v; want nil error result", snapshots, err)
			}
		})
	}
}

func TestFinalViewportUsesActiveBottomScreenInsteadOfLiveViewState(t *testing.T) {
	display := newInteractiveFixture(t, 2, MouseScroll, 60, 16)
	defer display.Close()
	for row := 0; row < 20; row++ {
		writeTransferOutput(t, display, 1, "old line\r\n")
	}
	writeTransferOutput(t, display, 1, "final line")
	display.interpretInput([]byte{'1'}, testNow())
	display.screens[display.transfers[0].number].scrollOffset = 10
	display.page = 1

	snapshots, err := display.finalSnapshots([]runsupervisor.TransferResult{
		finalTransferResult(t, 1, 0, 0), finalTransferResult(t, 2, 0, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	plain := stripSGR(string(serializeFinalSnapshots(snapshots)))
	if !strings.Contains(plain, "final line") {
		t.Fatalf("snapshot did not use the final bottom viewport: %q", plain)
	}
}

func TestFinalViewportInterpretsCursorStylesAndActiveBuffer(t *testing.T) {
	screen := newTransferScreen(16, 5)
	defer screen.close()
	if err := screen.write([]byte("old\rnew\r\nkeep\r\nremove\x1b[2K\r\n\x1b[31mred\x1b[0m")); err != nil {
		t.Fatal(err)
	}
	lines := screen.finalActiveViewport()
	plain := finalLinesPlain(lines)
	if strings.Contains(plain, "old") || strings.Contains(plain, "remove") {
		t.Fatalf("snapshot retained overwritten terminal history: %q", plain)
	}
	for _, want := range []string{"new", "keep", "red"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("snapshot does not contain %q: %q", want, plain)
		}
	}
	if !strings.Contains(lines[len(lines)-1].styled, "\x1b[31mred\x1b[0m") {
		t.Fatalf("snapshot lost terminal cell style: %q", lines[len(lines)-1].styled)
	}

	if err := screen.write([]byte("\x1b[?1049halternate\x1b[2;1Hbottom")); err != nil {
		t.Fatal(err)
	}
	if got := finalLinesPlain(screen.finalActiveViewport()); !strings.Contains(got, "alternate") ||
		!strings.Contains(got, "bottom") || strings.Contains(got, "keep") {
		t.Fatalf("active alternate viewport = %q", got)
	}
	if err := screen.write([]byte("\x1b[?1049l")); err != nil {
		t.Fatal(err)
	}
	if got := finalLinesPlain(screen.finalActiveViewport()); !strings.Contains(got, "keep") ||
		strings.Contains(got, "alternate") {
		t.Fatalf("restored normal viewport = %q", got)
	}
}

func TestFinalViewportTrimsOnlyTrailingVisuallyBlankRows(t *testing.T) {
	screen := newTransferScreen(12, 6)
	defer screen.close()
	content := "\r\nfirst\r\n\r\n\x1b[44m   \x1b[0m\r\n\r\n"
	if err := screen.write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	lines := screen.finalActiveViewport()
	if len(lines) != 4 {
		t.Fatalf("final viewport has %d rows, want 4: %#v", len(lines), lines)
	}
	if strings.TrimSpace(lines[0].text) != "" || strings.TrimSpace(lines[1].text) != "first" ||
		strings.TrimSpace(lines[2].text) != "" {
		t.Fatalf("leading or internal blank rows changed: %#v", lines)
	}
	if !strings.Contains(lines[3].styled, "\x1b[44m") {
		t.Fatalf("visually styled blank row was trimmed: %q", lines[3].styled)
	}
}

func TestFinalSnapshotEmptyScreenHasOnlyResetTitle(t *testing.T) {
	display := newInteractiveFixture(t, 1, MouseScroll, 60, 14)
	defer display.Close()
	snapshots, err := display.finalSnapshots([]runsupervisor.TransferResult{
		finalTransferResult(t, 1, 0, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(serializeFinalSnapshots(snapshots))
	if want := "\x1b[0m[transfer 1 | network 192.0.2.1 | exit=0 signal=0]\x1b[0m\r\n"; serialized != want {
		t.Fatalf("empty snapshot = %q, want %q", serialized, want)
	}
}

func finalTransferResult(t *testing.T, number, exit, signal int) runsupervisor.TransferResult {
	t.Helper()
	identity, err := transfernumber.New(number)
	if err != nil {
		t.Fatal(err)
	}
	return runsupervisor.TransferResult{Transfer: identity, ExitCode: exit, Signal: signal}
}

func finalLinesPlain(lines []frameLine) string {
	plain := make([]string, len(lines))
	for index, line := range lines {
		plain[index] = line.text
	}
	return strings.Join(plain, "\n")
}

func testNow() time.Time { return time.Unix(100, 0) }
