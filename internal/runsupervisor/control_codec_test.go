package runsupervisor

import (
	"bytes"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestControlFramesHaveFixedSemantics(t *testing.T) {
	id, _ := transfernumber.New(3)
	controls := []Control{
		newResizeControl(id, 132, 43),
		newInputControl(id, []byte{'a', 0, 'b'}),
	}
	for _, control := range controls {
		kind, payload, err := encodeControl(control)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeControl(wireFrame{kind: kind, payload: payload})
		if err != nil {
			t.Fatal(err)
		}
		if decoded.kind != control.kind || decoded.transfer != control.transfer ||
			decoded.cols != control.cols || decoded.rows != control.rows ||
			!bytes.Equal(decoded.input, control.input) {
			t.Fatalf("control round trip = %#v, want %#v", decoded, control)
		}
	}
	for name, frame := range map[string]wireFrame{
		"zero resize":                {kind: frameResize, payload: []byte{1, 0, 0, 0, 1}},
		"bad transfer":               {kind: frameInput, payload: []byte{0, 'x'}},
		"cancel on ordinary control": {kind: frameCancel, payload: []byte{byte(CancelUser)}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeControl(frame); err == nil {
				t.Fatal("invalid control frame was accepted")
			}
		})
	}
	var cancellation bytes.Buffer
	if err := writeCancellation(&cancellation, CancelUser); err != nil {
		t.Fatal(err)
	}
	frame, err := readFrame(&cancellation)
	if err != nil {
		t.Fatal(err)
	}
	if reason, err := decodeCancellation(frame); err != nil || reason != CancelUser {
		t.Fatalf("cancellation = %v, %v", reason, err)
	}
	if _, err := decodeCancellation(wireFrame{kind: frameCancel, payload: []byte{0xff}}); err == nil {
		t.Fatal("invalid cancellation was accepted")
	}
}

func TestCancellationUsesOneAtomicSizedWrite(t *testing.T) {
	written := &countingWriter{}
	if err := writeCancellation(written, CancelUser); err != nil {
		t.Fatal(err)
	}
	if written.calls != 1 {
		t.Fatalf("cancellation writes = %d, want one", written.calls)
	}
	if len(written.contents) != wireHeaderBytes+1 {
		t.Fatalf("cancellation frame bytes = %d", len(written.contents))
	}
}

type countingWriter struct {
	calls    int
	contents []byte
}

func (writer *countingWriter) Write(contents []byte) (int, error) {
	writer.calls++
	writer.contents = append(writer.contents, contents...)
	return len(contents), nil
}
