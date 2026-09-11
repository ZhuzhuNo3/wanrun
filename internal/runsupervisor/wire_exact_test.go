package runsupervisor

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestCarrierV5CanonicalFrames(t *testing.T) {
	tests := []struct {
		name    string
		kind    frameKind
		payload []byte
		hex     string
	}{
		{name: "ready", kind: frameReady, hex: "5753050100000000"},
		{name: "accepted", kind: frameAccepted, hex: "5753050200000000"},
		{name: "start", kind: frameStart, payload: []byte("job"), hex: "57530503000000036a6f62"},
		{name: "commit", kind: frameCommit, hex: "5753050400000000"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertCanonicalFrame(t, test.kind, test.payload, test.hex)
		})
	}
}

func TestCarrierV5CanonicalControlFrames(t *testing.T) {
	transfer, _ := transfernumber.New(3)
	resizeKind, resizePayload, err := encodeControl(newResizeControl(transfer, 132, 43))
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalFrame(t, resizeKind, resizePayload, "5753050600000005030084002b")

	inputKind, inputPayload, err := encodeControl(newInputControl(transfer, []byte{'a', 0, 'b'}))
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalFrame(t, inputKind, inputPayload, "575305070000000403610062")

	var cancellation bytes.Buffer
	if err := writeCancellation(&cancellation, CancelTerminate); err != nil {
		t.Fatal(err)
	}
	assertCanonicalBytes(t, cancellation.Bytes(), "575305050000000102")
}

func TestCarrierV5CanonicalEventFrames(t *testing.T) {
	transfer, _ := transfernumber.New(1)
	output, err := encodeOutput(transfer, OutputStderr, []byte("err"))
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalFrame(t, output.kind, output.payload, "57530508000000050103657272")

	status, err := encodeStatus(TransferStatus{Transfer: transfer, State: TransferExited, ExitCode: 7})
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalFrame(t, frameStatus, status, "57530509000000080102000000070000")

	final, err := encodeFinal(Final{
		Cancelled: true,
		Reason:    CancelUser,
		Source: &SourceSummary{
			FollowedSymlinks:    1,
			IgnoredSymlinks:     2,
			EmptyDirectories:    3,
			IgnoredSpecialFiles: 4,
		},
		InternalError: "i",
		RunError:      "r",
		CleanupError:  "c",
		Transfers:     []TransferResult{{Transfer: transfer, ExitCode: -1, Signal: 9}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalFrame(t, frameFinal, final,
		"5753050a0000003401010001000100010100000000000000010000000000000002"+
			"000000000000000300000000000000046972630101ffffffff0009")
}

func assertCanonicalFrame(t *testing.T, kind frameKind, payload []byte, wantHex string) {
	t.Helper()
	encoded, err := encodeFrame(kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalBytes(t, encoded, wantHex)
}

func assertCanonicalBytes(t *testing.T, got []byte, wantHex string) {
	t.Helper()
	want, err := hex.DecodeString(wantHex)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical bytes = %x, want %x", got, want)
	}
}
