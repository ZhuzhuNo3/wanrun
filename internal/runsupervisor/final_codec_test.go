package runsupervisor

import (
	"bytes"
	"context"
	"io"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestFinalResultRoundTripAndIdentityValidation(t *testing.T) {
	transfer1, _ := transfernumber.New(1)
	transfer2, _ := transfernumber.New(2)
	want := Final{Cancelled: true, Reason: CancelLifeline, InternalError: "control pipe closed",
		Source:    &SourceSummary{FollowedSymlinks: 1, IgnoredSymlinks: 2, EmptyDirectories: 3, IgnoredSpecialFiles: 4},
		Transfers: []TransferResult{{Transfer: transfer2, ExitCode: -1, Signal: 9}, {Transfer: transfer1, ExitCode: 7}}}
	encoded, err := encodeFinal(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeFinal(encoded)
	if err != nil {
		t.Fatal(err)
	}
	wantOrdered := []TransferResult{{Transfer: transfer1, ExitCode: 7}, {Transfer: transfer2, ExitCode: -1, Signal: 9}}
	if got.Cancelled != want.Cancelled || got.Reason != want.Reason || got.InternalError != want.InternalError ||
		got.Source == nil || *got.Source != *want.Source || !slices.Equal(got.Transfers, wantOrdered) {
		t.Fatalf("final = %#v, want %#v", got, want)
	}

	for name, invalid := range map[string]Final{
		"duplicate transfer":    {Transfers: []TransferResult{{Transfer: transfer1, ExitCode: 0}, {Transfer: transfer1, ExitCode: 1}}},
		"missing transfer":      {Transfers: []TransferResult{{Transfer: transfer1}, {Transfer: mustTransferNumber(t, 3)}}},
		"exit and signal":       {Transfers: []TransferResult{{Transfer: transfer1, ExitCode: 1, Signal: 9}}},
		"reason without cancel": {Reason: CancelUser},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := encodeFinal(invalid); err == nil {
				t.Fatal("invalid final result was accepted")
			}
		})
	}
}

func mustTransferNumber(t *testing.T, value int) transfernumber.Number {
	t.Helper()
	number, err := transfernumber.New(value)
	if err != nil {
		t.Fatal(err)
	}
	return number
}

func TestFinalDistinguishesMissingSourceSnapshotFromZeroIgnoredEntries(t *testing.T) {
	for _, want := range []Final{{}, {Source: &SourceSummary{}}} {
		encoded, err := encodeFinal(want)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeFinal(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if (got.Source == nil) != (want.Source == nil) {
			t.Fatalf("source presence after round trip = %#v, want %#v", got.Source, want.Source)
		}
	}
	encoded, err := encodeFinal(Final{})
	if err != nil {
		t.Fatal(err)
	}
	encoded[8] = 2
	if _, err := decodeFinal(encoded); err == nil {
		t.Fatal("invalid source summary presence was accepted")
	}
}

func TestFinalCarriesEveryRepresentableTransfer(t *testing.T) {
	final := Final{Transfers: make([]TransferResult, transfernumber.Maximum)}
	for number := 1; number <= transfernumber.Maximum; number++ {
		id, err := transfernumber.New(number)
		if err != nil {
			t.Fatal(err)
		}
		final.Transfers[number-1] = TransferResult{Transfer: id}
	}
	payload, err := encodeFinal(final)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeFinal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded.Transfers) != transfernumber.Maximum ||
		decoded.Transfers[len(decoded.Transfers)-1].Transfer.Value() != uint8(transfernumber.Maximum) {
		t.Fatalf("decoded final transfers=%d last=%d", len(decoded.Transfers),
			decoded.Transfers[len(decoded.Transfers)-1].Transfer.Value())
	}
}

func TestFinalDiagnosticIsNormalizedAtTheWireBoundary(t *testing.T) {
	transfer1, _ := transfernumber.New(1)
	for name, diagnostic := range map[string]string{
		"long ASCII":         strings.Repeat("x", maximumDiagnosticBytes+37),
		"multibyte edge":     strings.Repeat("界", maximumDiagnosticBytes/3+2),
		"invalid UTF-8":      string([]byte{'b', 'a', 'd', 0xff, 'z'}),
		"invalid near limit": string(append(bytes.Repeat([]byte("x"), maximumDiagnosticBytes-2), 0xff, 0xfe, 'z')),
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := encodeFinal(Final{Cancelled: true, Reason: CancelInternal,
				InternalError: diagnostic, Transfers: []TransferResult{{Transfer: transfer1, ExitCode: 7}}})
			if err != nil {
				t.Fatalf("encode normalized final: %v", err)
			}
			final, err := decodeFinal(encoded)
			if err != nil {
				t.Fatalf("decode normalized final: %v", err)
			}
			if !utf8.ValidString(final.InternalError) || len(final.InternalError) > maximumDiagnosticBytes {
				t.Fatalf("diagnostic bytes=%d valid=%v: %q", len(final.InternalError),
					utf8.ValidString(final.InternalError), final.InternalError)
			}
			if name == "invalid UTF-8" && final.InternalError != "bad\ufffdz" {
				t.Fatalf("invalid diagnostic normalization = %q", final.InternalError)
			}
			if !final.Cancelled || final.Reason != CancelInternal ||
				!slices.Equal(final.Transfers, []TransferResult{{Transfer: transfer1, ExitCode: 7}}) {
				t.Fatalf("normalized final lost evidence: %#v", final)
			}
		})
	}
}

func TestFinalEncodingFallbackKeepsOnlyTheCompleteValidTransferPrefix(t *testing.T) {
	transfer1, _ := transfernumber.New(1)
	transfer2, _ := transfernumber.New(2)
	transfer3, _ := transfernumber.New(3)
	controlReader, controlWriter := io.Pipe()
	eventReader, eventWriter := io.Pipe()
	served := make(chan error, 1)
	go func() {
		served <- serveWireSession(controlReader, heldCancellationPipe(t), eventWriter, func(context.Context, *RunSupervisor) Final {
			return Final{Cancelled: true, Reason: CancelUser,
				Source: &SourceSummary{FollowedSymlinks: 4, IgnoredSymlinks: 5, EmptyDirectories: 6, IgnoredSpecialFiles: 7},
				InternalError: strings.Repeat("original failure; ", maximumDiagnosticBytes/4) +
					string([]byte{0xff}),
				Transfers: []TransferResult{
					{Transfer: transfer1, ExitCode: 0},
					{Transfer: transfer2, ExitCode: 1, Signal: 9},
					{Transfer: transfer3, ExitCode: -1, Signal: 15},
				}}
		})
	}()
	if frame, err := readFrame(eventReader); err != nil || frame.kind != frameReady {
		t.Fatalf("ready frame = %#v, %v", frame, err)
	}
	if err := writeFrame(controlWriter, frameStart, nil); err != nil {
		t.Fatal(err)
	}
	expectFrameKind(t, eventReader, frameAccepted)
	if err := writeFrame(controlWriter, frameCommit, nil); err != nil {
		t.Fatal(err)
	}
	frame, err := readFrame(eventReader)
	if err != nil || frame.kind != frameFinal {
		t.Fatalf("final frame = %#v, %v", frame, err)
	}
	final, err := decodeFinal(frame.payload)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Cancelled || final.Reason != CancelUser ||
		final.Source == nil || *final.Source != (SourceSummary{FollowedSymlinks: 4,
		IgnoredSymlinks: 5, EmptyDirectories: 6, IgnoredSpecialFiles: 7}) ||
		!slices.Equal(final.Transfers, []TransferResult{{Transfer: transfer1, ExitCode: 0}}) ||
		!strings.Contains(final.InternalError, "encode supervisor final") ||
		!utf8.ValidString(final.InternalError) {
		t.Fatalf("fallback final lost evidence: %#v", final)
	}
	_ = controlWriter.Close()
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}
