package terminal

import (
	"bytes"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestOverviewFrameUsesExactTerminalGeometryAndShowsCommandOutput(t *testing.T) {
	display := newInteractiveFixture(t, 3, MouseScroll, 100, 30)
	defer display.Close()
	writeTransferOutput(t, display, 1, "$ transfer-command /var/tmp/transferlanes/view-1\nupload: sample-001.dat\n")
	runTransfer(t, display, 1)

	frame := display.plainFrame()
	assertFrameGeometry(t, frame, 100, 30)
	joined := strings.Join(frame, "\n")
	for _, want := range []string{
		"transferlanes | OVERVIEW",
		"DIRECT",
		"1-3 focus transfer",
		"[1] transfer 1/3 | network 192.0.2.1 | RUNNING",
		"upload: sample-001.dat",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("frame does not contain %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "AFTER Ctrl-]") {
		t.Fatalf("overview must not imply that direct controls need Leader:\n%s", joined)
	}
	if strings.Contains(joined, "mouse:") {
		t.Fatalf("overview exposes a focused-only mouse mode:\n%s", joined)
	}
	for index, wantRow := range []int{3, 12, 21} {
		prefix := fmt.Sprintf("[%d] transfer", index+1)
		if !strings.HasPrefix(frame[wantRow], prefix) {
			t.Fatalf("transfer pane %d begins on row %d, want %d:\n%s",
				index+1, findRow(frame, prefix), wantRow, joined)
		}
	}
}

func TestOverviewCRProgressStartsAtTop(t *testing.T) {
	display := newInteractiveFixture(t, 1, MouseScroll, 80, 16)
	defer display.Close()
	writeTransferOutput(t, display, 1, "progress 10%\rprogress 90%")

	target := overviewTargetRows(t, display.plainFrame())
	if got := strings.TrimSpace(target[0]); got != "progress 90%" {
		t.Fatalf("first overview target row=%q, want final progress", got)
	}
	for row, value := range target[1:] {
		if strings.TrimSpace(value) != "" {
			t.Fatalf("overview target row %d=%q, want trailing blank", row+1, value)
		}
	}
}

func TestOverviewShortOutputGrowsDownFromTop(t *testing.T) {
	display := newInteractiveFixture(t, 1, MouseScroll, 80, 16)
	defer display.Close()
	writeTransferOutput(t, display, 1, "first row\r\nsecond row\r\nthird row")

	target := overviewTargetRows(t, display.plainFrame())
	for row, want := range []string{"first row", "second row", "third row"} {
		if got := strings.TrimSpace(target[row]); got != want {
			t.Fatalf("overview target row %d=%q, want %q", row, got, want)
		}
	}
	for row, value := range target[3:] {
		if strings.TrimSpace(value) != "" {
			t.Fatalf("overview target row %d=%q, want trailing blank", row+3, value)
		}
	}
}

func TestOverviewLongOutputShowsLastAvailableRows(t *testing.T) {
	display := newInteractiveFixture(t, 1, MouseScroll, 80, 16)
	defer display.Close()
	available := len(overviewTargetRows(t, display.plainFrame()))
	lines := make([]string, available+3)
	for row := range lines {
		lines[row] = fmt.Sprintf("output row %02d", row)
	}
	writeTransferOutput(t, display, 1, strings.Join(lines, "\r\n"))

	target := overviewTargetRows(t, display.plainFrame())
	for row, value := range target {
		want := lines[len(lines)-available+row]
		if got := strings.TrimSpace(value); got != want {
			t.Fatalf("overview tail row %d=%q, want %q", row, got, want)
		}
	}
}

func TestOverviewPaginationAndLetterShortcutsReachEveryTransfer(t *testing.T) {
	display := newInteractiveFixture(t, 12, MouseScroll, 100, 18)
	defer display.Close()
	first := frameText(display.plainFrame())
	for _, want := range []string{"page 1/4", "transfers 1-3/12", "1-9,a-c focus transfer"} {
		if !strings.Contains(first, want) {
			t.Fatalf("first page does not contain %q:\n%s", want, first)
		}
	}

	display.interpretInput([]byte("\x1b[C\x1b[C\x1b[C"), time.Now())
	last := frameText(display.plainFrame())
	for _, want := range []string{"page 4/4", "transfers 10-12/12", "[a] transfer 10/12", "[c] transfer 12/12"} {
		if !strings.Contains(last, want) {
			t.Fatalf("last page does not contain %q:\n%s", want, last)
		}
	}

	display.interpretInput([]byte{'a'}, time.Now())
	if got := frameText(display.plainFrame()); !strings.Contains(got, "transfer 10/12") ||
		!strings.Contains(got, "FOCUSED") {
		t.Fatalf("shortcut a did not focus transfer 10:\n%s", got)
	}
	instructions := display.interpretInput([]byte{leaderByte, 'c'}, time.Now())
	if len(instructions) != 0 {
		t.Fatalf("switch generated target instructions: %#v", instructions)
	}
	if got := frameText(display.plainFrame()); !strings.Contains(got, "transfer 12/12") {
		t.Fatalf("Leader+c did not switch to transfer 12:\n%s", got)
	}
}

func TestOverviewArrowsOnlyChangePageWhenPaginationExists(t *testing.T) {
	display := newInteractiveFixture(t, 3, MouseScroll, 100, 30)
	defer display.Close()
	display.interpretInput([]byte("\x1b[C"), time.Now())
	if got := frameText(display.plainFrame()); !strings.Contains(got, "page 1/1") ||
		strings.Contains(got, "Left/Right change page") {
		t.Fatalf("single-page overview changed or advertised paging:\n%s", got)
	}
}

func TestFocusedFrameKeepsChromeSeparateFromTargetViewport(t *testing.T) {
	display := newInteractiveFixture(t, 3, MouseScroll, 120, 24)
	defer display.Close()
	display.interpretInput([]byte{'2'}, time.Now())
	writeTransferOutput(t, display, 2, "child output\n")

	frame := display.plainFrame()
	assertFrameGeometry(t, frame, 120, 24)
	joined := frameText(frame)
	for _, want := range []string{
		"transferlanes | FOCUSED | transfer 2/3 | network 192.0.2.2 | PREPARING | mouse: scroll",
		"DIRECT         Ctrl-C cancel all transfers",
		"AFTER Ctrl-]   0 overview | 1-3 switch transfer | \\ toggle mouse | Ctrl-] send literal Ctrl-]",
		"child output",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("focused frame does not contain %q:\n%s", want, joined)
		}
	}
	for _, forbidden := range []string{"PgUp", "PgDn", "End", "LIVE", "MOUSE", "NOTICE", "target received"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("focused frame contains removed UI %q:\n%s", forbidden, joined)
		}
	}
	viewport := targetViewport(frame)
	if len(viewport) == 0 || !strings.Contains(frameText(viewport), "child output") {
		t.Fatalf("target viewport is missing below fixed chrome:\n%s", joined)
	}
}

func TestFocusedChromeWrapsWithoutHidingContextOrControls(t *testing.T) {
	display := newInteractiveFixture(t, 3, MouseScroll, 60, 14)
	defer display.Close()
	display.interpretInput([]byte{'2'}, time.Now())
	frame := display.plainFrame()
	assertFrameGeometry(t, frame, 60, 14)
	joined := frameText(frame)
	for _, want := range []string{
		"transferlanes | FOCUSED", "transfer 2/3", "network 192.0.2.2", "PREPARING",
		"mouse: scroll", "Ctrl-C cancel all transfers", "Ctrl-] send literal Ctrl-]",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("wrapped chrome does not contain %q:\n%s", want, joined)
		}
	}
}

func TestFocusedInputReservesOnlyControlCAndTimedLeaderCommands(t *testing.T) {
	display := newInteractiveFixture(t, 3, MouseScroll, 120, 24)
	defer display.Close()
	now := time.Unix(100, 0)
	display.interpretInput([]byte{'2'}, now)

	instructions := display.interpretInput([]byte("1hello"), now)
	assertSingleInput(t, instructions, 2, "1hello")
	display.interpretInput([]byte{leaderByte, '1'}, now)
	if got := frameText(display.plainFrame()); !strings.Contains(got, "transfer 1/3") {
		t.Fatalf("Leader+1 did not switch transfer:\n%s", got)
	}

	display.interpretInput([]byte{leaderByte}, now)
	if instructions := display.expire(now.Add(leaderTimeout + time.Nanosecond)); len(instructions) != 0 {
		t.Fatalf("Leader expiration generated target input: %#v", instructions)
	}
	instructions = display.interpretInput([]byte{'2'}, now.Add(leaderTimeout+time.Millisecond))
	assertSingleInput(t, instructions, 1, "2")

	instructions = display.interpretInput([]byte{leaderByte, leaderByte}, now.Add(3*time.Second))
	assertSingleInput(t, instructions, 1, string([]byte{leaderByte}))

	instructions = display.interpretInput([]byte{0x03, 'x'}, now)
	if len(instructions) != 1 || instructions[0].cancel != runsupervisor.CancelUser {
		t.Fatalf("Ctrl-C instructions = %#v", instructions)
	}
}

func TestOverviewShortcutDoesNotDropFollowingFocusedInputFromSameRead(t *testing.T) {
	display := newInteractiveFixture(t, 2, MouseScroll, 80, 24)
	defer display.Close()
	assertSingleInput(t, display.interpretInput([]byte("1hello"), time.Now()), 1, "hello")
}

func TestEscapeDisambiguationPreservesStandaloneAndCrossReadSequences(t *testing.T) {
	display := newInteractiveFixture(t, 2, MouseScroll, 80, 24)
	defer display.Close()
	now := time.Unix(200, 0)
	display.interpretInput([]byte{'1'}, now)

	if instructions := display.interpretInput([]byte{0x1b}, now); len(instructions) != 0 {
		t.Fatalf("standalone Esc forwarded before ambiguity window: %#v", instructions)
	}
	assertSingleInput(t, display.expire(now.Add(escapeTimeout+time.Nanosecond)), 1, "\x1b")

	if instructions := display.interpretInput([]byte("\x1b["), now); len(instructions) != 0 {
		t.Fatalf("partial CSI forwarded early: %#v", instructions)
	}
	assertSingleInput(t, display.interpretInput([]byte("A"), now.Add(time.Millisecond)), 1, "\x1b[A")

	display.interpretInput([]byte{leaderByte}, now)
	if instructions := display.interpretInput([]byte{0x1b}, now); len(instructions) != 0 {
		t.Fatalf("Leader+Esc reached target: %#v", instructions)
	}
	assertSingleInput(t, display.interpretInput([]byte{'q'}, now), 1, "q")
}

func TestMouseWheelScrollsOrSilentlyReachesOnlyMouseAwareTarget(t *testing.T) {
	display := newInteractiveFixture(t, 1, MouseScroll, 100, 16)
	defer display.Close()
	display.interpretInput([]byte{'1'}, time.Now())
	for index := 0; index < 30; index++ {
		writeTransferOutput(t, display, 1, fmt.Sprintf("completed object %02d\n", index))
	}

	firstTargetRow := display.geometry.chromeRows + 1
	wheelUp := []byte(fmt.Sprintf("\x1b[<64;12;%dM", firstTargetRow))
	if instructions := display.interpretInput(wheelUp, time.Now()); len(instructions) != 0 {
		t.Fatalf("scroll wheel reached target in scroll mode: %#v", instructions)
	}
	before := frameText(targetViewport(display.plainFrame()))
	display.interpretInput(wheelUp, time.Now())
	after := frameText(targetViewport(display.plainFrame()))
	if before == after {
		t.Fatal("wheel did not move the target viewport in scroll mode")
	}
	before = after
	display.interpretInput([]byte(fmt.Sprintf("\x1b[<64;12;%dM", display.geometry.chromeRows)),
		time.Now())
	if after = frameText(targetViewport(display.plainFrame())); after != before {
		t.Fatal("wheel over focused chrome moved the target viewport")
	}

	display.interpretInput([]byte{leaderByte, '\\'}, time.Now())
	if got := frameText(display.plainFrame()); !strings.Contains(got, "mouse: target") ||
		strings.Contains(got, "MOUSE") {
		t.Fatalf("mouse mode was not shown only in focused title:\n%s", got)
	}
	if instructions := display.interpretInput(wheelUp, time.Now()); len(instructions) != 0 {
		t.Fatalf("mouse-unaware target received mouse: %#v", instructions)
	}
	writeTransferOutput(t, display, 1, "\x1b[?1000h\x1b[?1006h")
	assertSingleInput(t, display.interpretInput(wheelUp, time.Now()), 1, "\x1b[<64;12;1M")
	if got := frameText(display.plainFrame()); strings.Contains(got, "mouse event") ||
		strings.Contains(got, "target received") {
		t.Fatalf("forwarded mouse generated UI feedback:\n%s", got)
	}
}

func TestTargetMouseUsesViewportCoordinatesDeclaredEncodingAndTracking(t *testing.T) {
	for _, test := range []struct {
		name        string
		declaration string
		code        int
		final       byte
		outerColumn int
		targetRow   int
		want        string
	}{
		{name: "default", declaration: "\x1b[?1000h", outerColumn: 12, targetRow: 1,
			want: "\x1b[M ,!"},
		{name: "sgr", declaration: "\x1b[?1000h\x1b[?1006h", code: 16,
			outerColumn: 12, targetRow: 2, want: "\x1b[<16;12;2M"},
		{name: "horizontal-wheel", declaration: "\x1b[?1000h\x1b[?1006h", code: 66,
			outerColumn: 12, targetRow: 2, want: "\x1b[<66;12;2M"},
		{name: "auxiliary-eight", declaration: "\x1b[?1000h\x1b[?1006h", code: 195,
			outerColumn: 12, targetRow: 2, want: "\x1b[<195;12;2M"},
		{name: "chrome", declaration: "\x1b[?1000h\x1b[?1006h", outerColumn: 12,
			targetRow: 0},
		{name: "outside-column", declaration: "\x1b[?1000h\x1b[?1006h", outerColumn: 81,
			targetRow: 1},
		{name: "no-protocol", outerColumn: 12, targetRow: 1},
		{name: "pixel-encoding", declaration: "\x1b[?1000h\x1b[?1016h", outerColumn: 12,
			targetRow: 1},
		{name: "x10-filters-wheel", declaration: "\x1b[?9h", code: 64,
			outerColumn: 12, targetRow: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			display := newInteractiveFixture(t, 1, MouseTarget, 80, 24)
			defer display.Close()
			display.interpretInput([]byte{'1'}, time.Now())
			if test.declaration != "" {
				writeTransferOutput(t, display, 1, test.declaration)
			}
			outerRow := display.geometry.chromeRows + test.targetRow
			final := test.final
			if final == 0 {
				final = 'M'
			}
			raw := []byte(fmt.Sprintf("\x1b[<%d;%d;%d%c", test.code, test.outerColumn,
				outerRow, final))
			instructions := display.interpretInput(raw, time.Now())
			if test.want == "" {
				if len(instructions) != 0 {
					t.Fatalf("mouse event leaked to target: %#v", instructions)
				}
				return
			}
			assertSingleInput(t, instructions, 1, test.want)
		})
	}
}

func TestOwnedMouseSequencesNeverFallThroughAsFocusedKeyboardInput(t *testing.T) {
	for _, mouseMode := range []MouseMode{MouseScroll, MouseTarget} {
		for _, test := range []struct {
			name        string
			declaration string
			raw         func(*InteractiveTerminal) string
		}{
			{name: "unsupported-code", declaration: "\x1b[?1000h\x1b[?1006h",
				raw: func(display *InteractiveTerminal) string {
					return fmt.Sprintf("\x1b[<256;12;%dM", display.geometry.chromeRows+1)
				}},
			{name: "malformed-fields", declaration: "\x1b[?1000h\x1b[?1006h",
				raw: func(display *InteractiveTerminal) string {
					return fmt.Sprintf("\x1b[<0;;%dM", display.geometry.chromeRows+1)
				}},
			{name: "chrome", declaration: "\x1b[?1000h\x1b[?1006h",
				raw: func(display *InteractiveTerminal) string {
					return fmt.Sprintf("\x1b[<0;12;%dM", display.geometry.chromeRows)
				}},
			{name: "undeclared", raw: func(display *InteractiveTerminal) string {
				return fmt.Sprintf("\x1b[<0;12;%dM", display.geometry.chromeRows+1)
			}},
			{name: "sgr-pixels", declaration: "\x1b[?1000h\x1b[?1016h",
				raw: func(display *InteractiveTerminal) string {
					return fmt.Sprintf("\x1b[<0;12;%dM", display.geometry.chromeRows+1)
				}},
		} {
			t.Run(mouseMode.String()+"/"+test.name, func(t *testing.T) {
				display := newInteractiveFixture(t, 1, mouseMode, 80, 24)
				defer display.Close()
				display.interpretInput([]byte{'1'}, time.Now())
				if test.declaration != "" {
					writeTransferOutput(t, display, 1, test.declaration)
				}
				if instructions := display.interpretInput([]byte(test.raw(display)), time.Now()); len(instructions) != 0 {
					t.Fatalf("owned mouse sequence reached target input: %#v", instructions)
				}
			})
		}
	}
}

func TestHorizontalWheelIsSilentlyConsumedInScrollMode(t *testing.T) {
	display := newInteractiveFixture(t, 1, MouseScroll, 80, 24)
	defer display.Close()
	display.interpretInput([]byte{'1'}, time.Now())
	for index := 0; index < 30; index++ {
		writeTransferOutput(t, display, 1, fmt.Sprintf("completed object %02d\n", index))
	}
	before := frameText(targetViewport(display.plainFrame()))
	raw := fmt.Sprintf("\x1b[<66;12;%dM", display.geometry.chromeRows+1)
	if instructions := display.interpretInput([]byte(raw), time.Now()); len(instructions) != 0 {
		t.Fatalf("horizontal wheel reached target in scroll mode: %#v", instructions)
	}
	if after := frameText(targetViewport(display.plainFrame())); after != before {
		t.Fatal("horizontal wheel moved a vertical-only scroll viewport")
	}
}

func TestMouseSequenceWaitsAcrossReadsBeforeForwardingOneCompleteEvent(t *testing.T) {
	display := newInteractiveFixture(t, 1, MouseTarget, 100, 16)
	defer display.Close()
	display.interpretInput([]byte{'1'}, time.Now())
	writeTransferOutput(t, display, 1, "\x1b[?1000h\x1b[?1006h")
	now := time.Unix(250, 0)
	raw := []byte(fmt.Sprintf("\x1b[<0;12;%dM", display.geometry.chromeRows+2))
	if instructions := display.interpretInput(raw[:len(raw)-2], now); len(instructions) != 0 {
		t.Fatalf("partial mouse event reached target: %#v", instructions)
	}
	assertSingleInput(t, display.interpretInput(raw[len(raw)-2:], now.Add(time.Millisecond)),
		1, "\x1b[<0;12;2M")
}

func TestScrollingStopsAtAFullOldestViewport(t *testing.T) {
	display := newInteractiveFixture(t, 1, MouseScroll, 100, 16)
	defer display.Close()
	display.interpretInput([]byte{'1'}, time.Now())
	writeTransferOutput(t, display, 1, "$ transfer-command\n")
	for index := 0; index < 30; index++ {
		writeTransferOutput(t, display, 1, fmt.Sprintf("completed object %02d\n", index))
	}
	for range 100 {
		display.interpretInput([]byte("\x1b[<64;12;7M"), time.Now())
	}
	viewport := targetViewport(display.plainFrame())
	if !strings.HasPrefix(strings.TrimSpace(viewport[0]), "$ transfer-command") {
		t.Fatalf("oldest viewport does not start with earliest output:\n%s", frameText(display.plainFrame()))
	}
	for index, row := range viewport {
		if strings.TrimSpace(row) == "" {
			t.Fatalf("oldest viewport lost output at row %d:\n%s", index, frameText(display.plainFrame()))
		}
	}
}

func TestFocusedScrollbackContinuesReceivingLiveOutput(t *testing.T) {
	display := newInteractiveFixture(t, 1, MouseScroll, 100, 16)
	defer display.Close()
	display.interpretInput([]byte{'1'}, time.Now())
	for row := 0; row < 30; row++ {
		writeTransferOutput(t, display, 1, fmt.Sprintf("initial row %02d\r\n", row))
	}
	wheelUp := []byte(fmt.Sprintf("\x1b[<64;12;%dM", display.geometry.chromeRows+1))
	display.interpretInput(bytes.Repeat(wheelUp, 5), time.Now())

	for row := 0; row < 5; row++ {
		writeTransferOutput(t, display, 1, fmt.Sprintf("live row %02d\r\n", row))
	}
	if got := frameText(targetViewport(display.plainFrame())); strings.Contains(got, "live row 04") {
		t.Fatalf("scrolled viewport unexpectedly followed latest output:\n%s", got)
	}

	wheelDown := []byte(fmt.Sprintf("\x1b[<65;12;%dM", display.geometry.chromeRows+1))
	display.interpretInput(bytes.Repeat(wheelDown, 100), time.Now())
	if got := frameText(targetViewport(display.plainFrame())); !strings.Contains(got, "live row 04") {
		t.Fatalf("latest output was not retained while viewport was scrolled:\n%s", got)
	}
}

func TestNotificationIsTemporaryColoredAndUnlabelled(t *testing.T) {
	display := newInteractiveFixture(t, 1, MouseScroll, 100, 16)
	defer display.Close()
	now := time.Unix(300, 0)
	display.interpretInput([]byte{'1'}, now)
	separatorBefore := findRow(display.plainFrame(), strings.Repeat("-", 100))
	display.interpretInput([]byte{leaderByte, '\\'}, now)
	plain := frameText(display.plainFrame())
	styled := display.styledView()
	if !strings.Contains(plain, "mouse mode changed to target") || strings.Contains(plain, "NOTICE") {
		t.Fatalf("temporary notification is wrong:\n%s", plain)
	}
	if !strings.Contains(styled, "\x1b[33m") {
		t.Fatalf("notification does not have distinct color: %q", styled)
	}
	if separatorWithNotice := findRow(display.plainFrame(), strings.Repeat("-", 100)); separatorWithNotice != separatorBefore {
		t.Fatalf("temporary notification changed fixed chrome from row %d to %d",
			separatorBefore, separatorWithNotice)
	}
	display.expire(now.Add(notificationTimeout + time.Nanosecond))
	if got := frameText(display.plainFrame()); strings.Contains(got, "mouse mode changed") {
		t.Fatalf("expired notification remains visible:\n%s", got)
	}
}

func TestTerminalScreensPreserveCRAlternateBufferAndStyle(t *testing.T) {
	display := newInteractiveFixture(t, 1, MouseScroll, 80, 24)
	defer display.Close()
	display.interpretInput([]byte{'1'}, time.Now())
	writeTransferOutput(t, display, 1, "progress 10%\r\x1b[31mprogress 90%\x1b[0m")
	plain := frameText(display.plainFrame())
	if !strings.Contains(plain, "progress 90%") || strings.Contains(plain, "progress 10%") {
		t.Fatalf("CR semantics were not preserved:\n%s", plain)
	}
	if !strings.Contains(display.styledView(), "\x1b[31m") {
		t.Fatalf("target foreground style was lost: %q", display.styledView())
	}

	writeTransferOutput(t, display, 1, "\nnormal buffer\x1b[?1049h\x1b[Halternate buffer")
	if got := frameText(display.plainFrame()); !strings.Contains(got, "alternate buffer") ||
		strings.Contains(got, "normal buffer") {
		t.Fatalf("alternate screen was not active:\n%s", got)
	}
	writeTransferOutput(t, display, 1, "\x1b[?1049l")
	if got := frameText(display.plainFrame()); !strings.Contains(got, "normal buffer") ||
		strings.Contains(got, "alternate buffer") {
		t.Fatalf("normal screen was not restored:\n%s", got)
	}
}

func TestFramesUseDisplayCellsWithoutSplittingGraphemes(t *testing.T) {
	for _, size := range []struct{ cols, rows int }{{40, 10}, {60, 14}, {80, 24}, {120, 40}} {
		for _, focused := range []bool{false, true} {
			display := newInteractiveFixture(t, 3, MouseScroll, size.cols, size.rows)
			if focused {
				display.interpretInput([]byte{'2'}, time.Now())
			}
			writeTransferOutput(t, display, 2, "CJK 界 | combining e\u0301 | zero\u200bwidth | family 👨‍👩‍👧‍👦\n")
			assertFrameGeometry(t, display.plainFrame(), size.cols, size.rows)
			display.Close()
		}
	}

	const family = "👨‍👩‍👧‍👦"
	for value, want := range map[string]int{"界": 2, "e\u0301": 1, "\u200b": 0, family: 2} {
		if got := displayWidth(value); got != want {
			t.Fatalf("displayWidth(%q)=%d, want %d", value, got, want)
		}
	}
	got := fitText("ab"+family+"cd", 4)
	if got != "ab… " || strings.Contains(got, "👨") {
		t.Fatalf("fitText split a grapheme or used wrong display width: %q", got)
	}
}

func TestResizeRecalculatesFrameAndResizesEveryTargetViewport(t *testing.T) {
	display := newInteractiveFixture(t, 3, MouseScroll, 80, 24)
	defer display.Close()
	instructions, err := display.resize(40, 10)
	if err != nil {
		t.Fatal(err)
	}
	assertFrameGeometry(t, display.plainFrame(), 40, 10)
	if len(instructions) != 3 {
		t.Fatalf("resize instructions=%d, want 3", len(instructions))
	}
	for index, instruction := range instructions {
		if instruction.transfer.Value() != uint8(index+1) || instruction.cols != 40 ||
			instruction.rows != uint16(display.focusedViewportRows()) {
			t.Fatalf("resize instruction %d = %#v", index, instruction)
		}
	}
}

func TestFocusedViewportHeightDoesNotChangeWithTransferStatus(t *testing.T) {
	for _, cols := range []int{77, 80} {
		display := newInteractiveFixture(t, 1, MouseScroll, cols, 24)
		display.interpretInput([]byte{'1'}, time.Now())
		initialRows := display.focusedViewportRows()
		initialSeparator := findRow(display.plainFrame(), strings.Repeat("-", cols))
		runTransfer(t, display, 1)
		if rows := display.focusedViewportRows(); rows != initialRows {
			t.Fatalf("%d columns: RUNNING changed viewport rows from %d to %d",
				cols, initialRows, rows)
		}
		exitTransfer(t, display, 1, -1, 255)
		if rows := display.focusedViewportRows(); rows != initialRows {
			t.Fatalf("%d columns: EXIT signal changed viewport rows from %d to %d",
				cols, initialRows, rows)
		}
		if separator := findRow(display.plainFrame(), strings.Repeat("-", cols)); separator != initialSeparator {
			t.Fatalf("%d columns: status moved separator from row %d to %d",
				cols, initialSeparator, separator)
		}
		display.Close()
	}
}

func newInteractiveFixture(t *testing.T, count int, mouse MouseMode, cols, rows int) *InteractiveTerminal {
	t.Helper()
	transfers := make([]Transfer, count)
	for index := range transfers {
		number, _ := transfernumber.New(index + 1)
		value, err := NewTransfer(number, netip.AddrFrom4([4]byte{192, 0, 2, byte(index + 1)}))
		if err != nil {
			t.Fatal(err)
		}
		transfers[index] = value
	}
	display, err := NewInteractiveTerminal(transfers, cols, rows, mouse)
	if err != nil {
		t.Fatal(err)
	}
	return display
}

func writeTransferOutput(t *testing.T, display *InteractiveTerminal, number int, content string) {
	t.Helper()
	id, _ := transfernumber.New(number)
	event, err := runsupervisor.NewOutputEvent(id, runsupervisor.OutputPTY, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	if err := display.apply(event); err != nil {
		t.Fatal(err)
	}
}

func runTransfer(t *testing.T, display *InteractiveTerminal, number int) {
	t.Helper()
	id, _ := transfernumber.New(number)
	event, err := runsupervisor.NewStatusEvent(runsupervisor.TransferStatus{Transfer: id, State: runsupervisor.TransferRunning})
	if err != nil {
		t.Fatal(err)
	}
	if err := display.apply(event); err != nil {
		t.Fatal(err)
	}
}

func exitTransfer(t *testing.T, display *InteractiveTerminal, number, exitCode, signal int) {
	t.Helper()
	id, _ := transfernumber.New(number)
	event, err := runsupervisor.NewStatusEvent(runsupervisor.TransferStatus{Transfer: id,
		State: runsupervisor.TransferExited, ExitCode: exitCode, Signal: signal})
	if err != nil {
		t.Fatal(err)
	}
	if err := display.apply(event); err != nil {
		t.Fatal(err)
	}
}

func assertSingleInput(t *testing.T, instructions []terminalInstruction, transfer int, content string) {
	t.Helper()
	if len(instructions) != 1 || instructions[0].transfer.Value() != uint8(transfer) ||
		!bytes.Equal(instructions[0].content, []byte(content)) {
		t.Fatalf("instructions=%#v, want transfer %d input %q", instructions, transfer, content)
	}
}

func assertFrameGeometry(t *testing.T, frame []string, cols, rows int) {
	t.Helper()
	if len(frame) != rows {
		t.Fatalf("frame has %d rows, want %d", len(frame), rows)
	}
	for row, line := range frame {
		if got := displayWidth(line); got != cols {
			t.Fatalf("row %d has display width %d, want %d: %q", row, got, cols, line)
		}
	}
}

func findRow(frame []string, prefix string) int {
	for index, row := range frame {
		if strings.HasPrefix(row, prefix) {
			return index
		}
	}
	return -1
}

func frameText(frame []string) string { return strings.Join(frame, "\n") }

func targetViewport(frame []string) []string {
	for index, row := range frame {
		if strings.Trim(row, " -") == "" && strings.Contains(row, "-") {
			return frame[index+1:]
		}
	}
	return nil
}

func overviewTargetRows(t *testing.T, frame []string) []string {
	t.Helper()
	title := findRow(frame, "[1] transfer")
	if title < 0 || title+1 >= len(frame) {
		t.Fatalf("overview transfer pane is missing:\n%s", frameText(frame))
	}
	return frame[title+1:]
}
