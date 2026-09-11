package plainoutput

import (
	"bytes"
	"errors"
	"io"
	"net/netip"
	"strings"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestPlainTextOutputKeepsLogicalLinesAndOuterStreamsSeparate(t *testing.T) {
	first, _ := transfernumber.New(1)
	second, _ := transfernumber.New(2)
	var stdout, stderr bytes.Buffer
	output, err := New(&stdout, &stderr, []Transfer{
		mustPlainTransfer(t, first, "192.0.2.10"),
		mustPlainTransfer(t, second, "192.0.2.11"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := output.Begin(); err != nil {
		t.Fatal(err)
	}
	showOutput(t, output, first, runsupervisor.OutputStdout, []byte("part"))
	showOutput(t, output, second, runsupervisor.OutputStdout, []byte("sibling\n"))
	showOutput(t, output, first, runsupervisor.OutputStderr, []byte("failure\n"))
	showOutput(t, output, first, runsupervisor.OutputStdout, []byte("ial\nnext\n"))
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "transferlanes: starting 2 transfers\n"+
		"[2 192.0.2.11] sibling\n[1 192.0.2.10] partial\n[1 192.0.2.10] next\n"; got != want {
		t.Fatalf("stdout=%q, want %q", got, want)
	}
	if got, want := stderr.String(), "[1 192.0.2.10] failure\n"; got != want {
		t.Fatalf("stderr=%q, want %q", got, want)
	}
}

func TestPlainTextOutputCoalescesCarriageReturnAndRemovesTerminalControls(t *testing.T) {
	id, _ := transfernumber.New(1)
	var stdout, stderr bytes.Buffer
	output, err := New(&stdout, &stderr, []Transfer{mustPlainTransfer(t, id, "192.0.2.10")})
	if err != nil {
		t.Fatal(err)
	}
	showOutput(t, output, id, runsupervisor.OutputStdout, []byte("\x1b[31m10%\x1b["))
	showOutput(t, output, id, runsupervisor.OutputStdout, []byte("0m\r\x1b[32m20%\x1b[0m\r\x1b[2J30%"))
	exited, _ := runsupervisor.NewStatusEvent(runsupervisor.TransferStatus{Transfer: id,
		State: runsupervisor.TransferExited, ExitCode: 0})
	if err := output.Show(exited); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	got := stdout.String()
	if strings.Contains(got, "\x1b") || strings.Contains(got, "20%") ||
		got != "[1 192.0.2.10] 10%\n[1 192.0.2.10] 30%\n" {
		t.Fatalf("coalesced stdout=%q", got)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestPlainTextOutputDoesNotRepeatAnAlreadyAppendedProgressAtNewline(t *testing.T) {
	id, _ := transfernumber.New(1)
	var stdout bytes.Buffer
	output, err := New(&stdout, io.Discard, []Transfer{mustPlainTransfer(t, id, "192.0.2.10")})
	if err != nil {
		t.Fatal(err)
	}
	showOutput(t, output, id, runsupervisor.OutputStdout, []byte("10%\r\n"))
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "[1 192.0.2.10] 10%\n"; got != want {
		t.Fatalf("progress newline output=%q, want %q", got, want)
	}
}

func TestPlainTextOutputKeepsLatestPendingProgressAcrossEmptyCarriageReturns(t *testing.T) {
	id, _ := transfernumber.New(1)
	want := "[1 192.0.2.10] 10%\n[1 192.0.2.10] 20%\n"
	for _, test := range []struct {
		name    string
		content string
	}{
		{name: "close", content: "10%\r20%\r\r"},
		{name: "newline", content: "10%\r20%\r\r\n"},
		{name: "CRLF", content: "10%\r20%\r\r\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			output, err := New(&stdout, io.Discard,
				[]Transfer{mustPlainTransfer(t, id, "192.0.2.10")})
			if err != nil {
				t.Fatal(err)
			}
			for _, frame := range []byte(test.content) {
				showOutput(t, output, id, runsupervisor.OutputStdout, []byte{frame})
			}
			if err := output.Close(); err != nil {
				t.Fatal(err)
			}
			if got := stdout.String(); got != want {
				t.Fatalf("progress output=%q, want %q", got, want)
			}
		})
	}
}

func TestPlainTextSanitizerConsumesECMA48EscapesAndC1AcrossFrames(t *testing.T) {
	id, _ := transfernumber.New(1)
	var stdout bytes.Buffer
	output, err := New(&stdout, io.Discard, []Transfer{mustPlainTransfer(t, id, "192.0.2.10")})
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("中文🙂“ " +
		"\x1b(0A\x1b(BB\x1b#8C" +
		"\x1b[31mRED\x1b[0m" +
		"\x1b]ignored\x1b\\" +
		"\x1bPignored\x1b\\\x1bXignored\x1b\\\x1b^ignored\x1b\\\x1b_ignored\x1b\\" +
		"\xc2\x9b32mGREEN\xc2\x9b0m" +
		"\x9b33mRAW\x9b0m" +
		"\xc2\x9dignored\xe2\x80\x9cinside\xc2\x9c" +
		"\x90ignored\x9c tail\n")
	for _, value := range content {
		showOutput(t, output, id, runsupervisor.OutputStdout, []byte{value})
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "[1 192.0.2.10] 中文🙂“ ABCREDGREENRAW tail\n"; got != want {
		t.Fatalf("sanitized output=%q, want %q", got, want)
	}
}

func TestPlainTextSanitizerBoundsUnfinishedControlsAndReplacesIncompleteUTF8AtClose(t *testing.T) {
	id, _ := transfernumber.New(1)
	var stdout bytes.Buffer
	output, err := New(&stdout, io.Discard, []Transfer{mustPlainTransfer(t, id, "192.0.2.10")})
	if err != nil {
		t.Fatal(err)
	}
	showOutput(t, output, id, runsupervisor.OutputStdout, []byte("before\xe4\xb8\x1b]"))
	for range maximumLogicalLineBytes * 2 {
		showOutput(t, output, id, runsupervisor.OutputStdout, []byte{'x'})
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "[1 192.0.2.10] before�\n"; got != want {
		t.Fatalf("unfinished sequence output=%q, want %q", got, want)
	}
}

func TestPlainTextOutputUsesFullWritesAndBoundsUnfinishedLinesWithoutDroppingBytes(t *testing.T) {
	id, _ := transfernumber.New(1)
	stdout := &oneByteWriter{}
	output, err := New(stdout, io.Discard, []Transfer{mustPlainTransfer(t, id, "192.0.2.10")})
	if err != nil {
		t.Fatal(err)
	}
	content := bytes.Repeat([]byte{'x'}, maximumLogicalLineBytes+37)
	for offset := 0; offset < len(content); offset += 2048 {
		end := min(offset+2048, len(content))
		showOutput(t, output, id, runsupervisor.OutputStdout, content[offset:end])
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	got := strings.ReplaceAll(string(stdout.content), "[1 192.0.2.10] ", "")
	got = strings.ReplaceAll(got, "\n", "")
	if got != string(content) {
		t.Fatalf("bounded logical output retained %d bytes, want %d", len(got), len(content))
	}
}

func TestPlainTextOutputDoesNotSplitUTF8AtTheLogicalLineBound(t *testing.T) {
	id, _ := transfernumber.New(1)
	var stdout bytes.Buffer
	output, err := New(&stdout, io.Discard, []Transfer{mustPlainTransfer(t, id, "192.0.2.10")})
	if err != nil {
		t.Fatal(err)
	}
	content := append(bytes.Repeat([]byte{'x'}, maximumLogicalLineBytes-1), []byte("🙂中文")...)
	for offset := 0; offset < len(content); offset += 2048 {
		end := min(offset+2048, len(content))
		showOutput(t, output, id, runsupervisor.OutputStdout, content[offset:end])
	}
	showOutput(t, output, id, runsupervisor.OutputStdout, []byte{'\n'})
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	got := strings.ReplaceAll(stdout.String(), "[1 192.0.2.10] ", "")
	got = strings.ReplaceAll(got, "\n", "")
	if got != string(content) {
		t.Fatalf("bounded UTF-8 output suffix=%q, want %q", tail(got, 24), tail(string(content), 24))
	}
}

func TestPlainTextOutputDoesNotInventALineAfterAnExactBoundedSegment(t *testing.T) {
	id, _ := transfernumber.New(1)
	prefix := "[1 192.0.2.10] "
	for _, test := range []struct {
		name    string
		content []byte
		want    string
	}{
		{name: "one segment then newline", content: append(bytes.Repeat([]byte{'x'}, maximumLogicalLineBytes), '\n'),
			want: prefix + strings.Repeat("x", maximumLogicalLineBytes) + "\n"},
		{name: "two segments then newline", content: append(bytes.Repeat([]byte{'x'}, 2*maximumLogicalLineBytes), '\n'),
			want: prefix + strings.Repeat("x", maximumLogicalLineBytes) + "\n" +
				prefix + strings.Repeat("x", maximumLogicalLineBytes) + "\n"},
		{name: "one segment then carriage return", content: append(bytes.Repeat([]byte{'x'}, maximumLogicalLineBytes), '\r'),
			want: prefix + strings.Repeat("x", maximumLogicalLineBytes) + "\n"},
		{name: "one segment then CRLF", content: append(bytes.Repeat([]byte{'x'}, maximumLogicalLineBytes), '\r', '\n'),
			want: prefix + strings.Repeat("x", maximumLogicalLineBytes) + "\n"},
		{name: "one segment then repeated CRLF", content: append(bytes.Repeat([]byte{'x'}, maximumLogicalLineBytes), '\r', '\r', '\n'),
			want: prefix + strings.Repeat("x", maximumLogicalLineBytes) + "\n"},
		{name: "UTF-8 suffix crosses next segment", content: append(append(
			bytes.Repeat([]byte{'x'}, maximumLogicalLineBytes-1), []byte("🙂")...), '\n'),
			want: prefix + strings.Repeat("x", maximumLogicalLineBytes-1) + "\n" + prefix + "🙂\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			output, err := New(&stdout, io.Discard, []Transfer{mustPlainTransfer(t, id, "192.0.2.10")})
			if err != nil {
				t.Fatal(err)
			}
			for offset := 0; offset < len(test.content); offset += 2048 {
				end := min(offset+2048, len(test.content))
				showOutput(t, output, id, runsupervisor.OutputStdout, test.content[offset:end])
			}
			if err := output.Close(); err != nil {
				t.Fatal(err)
			}
			if got := stdout.String(); got != test.want {
				t.Fatalf("output bytes=%d suffix=%q, want bytes=%d suffix=%q",
					len(got), tail(got, 40), len(test.want), tail(test.want, 40))
			}
		})
	}
}

func TestPlainTextOutputPreservesTrueEmptyLogicalLines(t *testing.T) {
	id, _ := transfernumber.New(1)
	var stdout bytes.Buffer
	output, err := New(&stdout, io.Discard, []Transfer{mustPlainTransfer(t, id, "192.0.2.10")})
	if err != nil {
		t.Fatal(err)
	}
	showOutput(t, output, id, runsupervisor.OutputStdout, []byte("\n\n"))
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "[1 192.0.2.10] \n[1 192.0.2.10] \n"; got != want {
		t.Fatalf("empty-line output=%q, want %q", got, want)
	}
}

func TestPlainTextOutputTreatsEmptyCRLFAsOneEmptyLogicalLine(t *testing.T) {
	id, _ := transfernumber.New(1)
	var stdout bytes.Buffer
	output, err := New(&stdout, io.Discard, []Transfer{mustPlainTransfer(t, id, "192.0.2.10")})
	if err != nil {
		t.Fatal(err)
	}
	showOutput(t, output, id, runsupervisor.OutputStdout, []byte("\r\n"))
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(), "[1 192.0.2.10] \n"; got != want {
		t.Fatalf("empty CRLF output=%q, want %q", got, want)
	}
}

func tail(value string, length int) string {
	if len(value) <= length {
		return value
	}
	return value[len(value)-length:]
}

func TestPlainTextOutputReturnsOuterWriterFailure(t *testing.T) {
	id, _ := transfernumber.New(1)
	failure := errors.New("stdout unavailable")
	output, err := New(&faultWriter{remaining: 4, err: failure}, io.Discard,
		[]Transfer{mustPlainTransfer(t, id, "192.0.2.10")})
	if err != nil {
		t.Fatal(err)
	}
	event, err := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStdout, []byte("complete line\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := output.Show(event); !errors.Is(err, failure) {
		t.Fatalf("writer failure=%v, want %v", err, failure)
	}
}

func mustPlainTransfer(t *testing.T, number transfernumber.Number, address string) Transfer {
	t.Helper()
	transfer, err := NewTransfer(number, netip.MustParseAddr(address))
	if err != nil {
		t.Fatal(err)
	}
	return transfer
}

func showOutput(t *testing.T, output *PlainTextOutput, id transfernumber.Number,
	stream runsupervisor.OutputStream, content []byte,
) {
	t.Helper()
	event, err := runsupervisor.NewOutputEvent(id, stream, content)
	if err != nil {
		t.Fatal(err)
	}
	if err := output.Show(event); err != nil {
		t.Fatal(err)
	}
}

type oneByteWriter struct{ content []byte }

func (writer *oneByteWriter) Write(content []byte) (int, error) {
	if len(content) == 0 {
		return 0, nil
	}
	writer.content = append(writer.content, content[0])
	return 1, nil
}

type faultWriter struct {
	remaining int
	err       error
}

func (writer *faultWriter) Write(content []byte) (int, error) {
	if writer.remaining == 0 {
		return 0, writer.err
	}
	written := len(content)
	if written > writer.remaining {
		written = writer.remaining
	}
	writer.remaining -= written
	return written, nil
}
