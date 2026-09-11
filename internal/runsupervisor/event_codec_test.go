package runsupervisor

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestEveryCommandOutputStreamRoundTripsWithoutChangingBytes(t *testing.T) {
	id, _ := transfernumber.New(2)
	contents := []byte{'a', '\r', 0, 0xff, 'z'}
	for _, stream := range []OutputStream{OutputPTY, OutputStdout, OutputStderr} {
		frame, err := encodeOutput(id, stream, contents)
		if err != nil {
			t.Fatalf("encode %v output: %v", stream, err)
		}
		event, err := decodeEvent(frame)
		if err != nil {
			t.Fatalf("decode %v output: %v", stream, err)
		}
		if event.Kind() != EventOutput || event.Transfer() != id || event.Stream() != stream ||
			!bytes.Equal(event.Bytes(), contents) {
			t.Fatalf("%v output event = %#v", stream, event)
		}
	}
}

func TestLargeCommandOutputUsesCompletePipeAtomicFramesWithoutChangingBytes(t *testing.T) {
	id, _ := transfernumber.New(2)
	stdout := bytes.Repeat([]byte("stdout-"), 5000)
	stderr := bytes.Repeat([]byte("stderr-"), 3000)
	var target bytes.Buffer
	writer, err := newEventPipeSender(&target, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	run := &RunSupervisor{events: writer}
	if err := run.SendOutput(id, OutputStdout, stdout); err != nil {
		t.Fatal(err)
	}
	if err := run.SendOutput(id, OutputStderr, stderr); err != nil {
		t.Fatal(err)
	}

	var gotStdout, gotStderr []byte
	frames := 0
	for target.Len() != 0 {
		before := target.Len()
		frame, err := readFrame(&target)
		if err != nil {
			t.Fatal(err)
		}
		if encodedBytes := before - target.Len(); encodedBytes > 4096 {
			t.Fatalf("event frame bytes = %d, exceeds Linux pipe atomic write size %d",
				encodedBytes, 4096)
		}
		event, err := decodeEvent(frame)
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind() != EventOutput || event.Transfer() != id {
			t.Fatalf("large output event = %#v", event)
		}
		switch event.Stream() {
		case OutputStdout:
			if len(gotStderr) != 0 {
				t.Fatal("stdout frame crossed the later stderr send")
			}
			gotStdout = append(gotStdout, event.Bytes()...)
		case OutputStderr:
			gotStderr = append(gotStderr, event.Bytes()...)
		default:
			t.Fatalf("unexpected stream %v", event.Stream())
		}
		frames++
	}
	if frames < 2 || !bytes.Equal(gotStdout, stdout) || !bytes.Equal(gotStderr, stderr) {
		t.Fatalf("large output frames=%d stdout=%d/%d stderr=%d/%d",
			frames, len(gotStdout), len(stdout), len(gotStderr), len(stderr))
	}
}

func TestCommandOutputRejectsInvalidIdentityStreamAndBounds(t *testing.T) {
	id, _ := transfernumber.New(1)
	for name, frame := range map[string]wireFrame{
		"zero transfer":  {kind: frameOutput, payload: []byte{0, byte(OutputPTY), 'x'}},
		"unknown stream": {kind: frameOutput, payload: []byte{id.Value(), 0xff, 'x'}},
		"empty output":   {kind: frameOutput, payload: []byte{id.Value(), byte(OutputPTY)}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeEvent(frame); err == nil {
				t.Fatal("invalid output event was accepted")
			}
		})
	}
	if _, err := encodeOutput(id, OutputStdout,
		bytes.Repeat([]byte("x"), maximumWirePayload)); err == nil {
		t.Fatal("oversized output event was accepted")
	}
}

func TestRunSupervisorOutputBackpressureAndDisconnectAreBounded(t *testing.T) {
	reader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer output.Close()
	writer, err := newEventPipeSender(output, 25*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// Keep this cross-platform unit below the smallest supported Unix PIPE_BUF.
	// Linux's exact 4096-byte boundary is exercised separately.
	payload := bytes.Repeat([]byte("x"), 128)
	started := time.Now()
	for {
		err = writer.send(frameOutput, payload)
		if err != nil {
			break
		}
	}
	if elapsed := time.Since(started); elapsed > time.Second ||
		!strings.Contains(err.Error(), "backpressure") {
		t.Fatalf("bounded backpressure error after %s = %v", elapsed, err)
	}

	disconnectedRead, disconnectedWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := disconnectedRead.Close(); err != nil {
		t.Fatal(err)
	}
	defer disconnectedWrite.Close()
	disconnected, err := newEventPipeSender(disconnectedWrite, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := disconnected.send(frameStatus, make([]byte, 8)); err == nil {
		t.Fatal("disconnected supervisor output was accepted")
	}
}

func TestRunSupervisorOutputAcceptsOnlyOneFinalFrame(t *testing.T) {
	var target bytes.Buffer
	writer, err := newEventPipeSender(&target, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.send(frameFinal, make([]byte, 34)); err != nil {
		t.Fatal(err)
	}
	if err := writer.send(frameFinal, make([]byte, 34)); err == nil {
		t.Fatal("supervisor output accepted a second final frame")
	}
	if err := writer.send(frameStatus, make([]byte, 8)); err == nil {
		t.Fatal("supervisor output accepted an event after final")
	}
}

func TestRunSupervisorPreservesTheSenderOrderOfOutputAndStatusFrames(t *testing.T) {
	first, _ := transfernumber.New(1)
	second, _ := transfernumber.New(2)
	var target bytes.Buffer
	writer, err := newEventPipeSender(&target, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	frames := []wireFrame{
		mustOutputFrame(t, first, OutputStdout, []byte("first-a")),
		mustOutputFrame(t, second, OutputStderr, []byte("second-a")),
		mustOutputFrame(t, first, OutputStdout, []byte("first-b")),
	}
	for _, frame := range frames {
		if err := writer.send(frame.kind, frame.payload); err != nil {
			t.Fatal(err)
		}
	}
	for index, want := range frames {
		got, err := readFrame(&target)
		if err != nil || got.kind != want.kind || !bytes.Equal(got.payload, want.payload) {
			t.Fatalf("frame %d = %#v, %v; want %#v", index, got, err, want)
		}
	}
}

func mustOutputFrame(t *testing.T, id transfernumber.Number, stream OutputStream,
	contents []byte,
) wireFrame {
	t.Helper()
	frame, err := encodeOutput(id, stream, contents)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func TestInProcessEventsValidateRawOutputAndStatus(t *testing.T) {
	id, _ := transfernumber.New(1)
	raw := []byte("progress\x00\r\x1b[31m")
	output, err := NewOutputEvent(id, OutputStdout, raw)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] = 'X'
	if output.Kind() != EventOutput || output.Transfer() != id || output.Stream() != OutputStdout ||
		string(output.Bytes()) != "progress\x00\r\x1b[31m" {
		t.Fatalf("output event = %#v bytes=%q", output, output.Bytes())
	}
	status := TransferStatus{Transfer: id, State: TransferExited, ExitCode: 7}
	statusEvent, err := NewStatusEvent(status)
	if err != nil {
		t.Fatal(err)
	}
	if statusEvent.Kind() != EventStatus || statusEvent.Status() != status {
		t.Fatalf("status event = %#v", statusEvent)
	}
	if _, err := NewOutputEvent(transfernumber.Number{}, OutputStdout, []byte("x")); err == nil {
		t.Fatal("invalid output identity was accepted")
	}
	if _, err := NewStatusEvent(TransferStatus{Transfer: id, State: TransferExited, ExitCode: -1}); err == nil {
		t.Fatal("invalid exited status was accepted")
	}
}
