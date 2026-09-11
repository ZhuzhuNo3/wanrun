//go:build linux

package runsupervisor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

func TestLargeOutputBackpressureStillCompletesCleanupAndOneFinal(t *testing.T) {
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cancelReader, cancelWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	eventReader, eventWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeFiles(controlReader, controlWriter, cancelReader, cancelWriter, eventReader, eventWriter)
	})
	capacity, err := unix.FcntlInt(eventWriter.Fd(), unix.F_SETPIPE_SZ, linuxPipeAtomicWriteBytes)
	if err != nil || capacity != linuxPipeAtomicWriteBytes {
		t.Fatalf("event pipe capacity = %d, %v", capacity, err)
	}
	id, _ := transfernumber.New(1)
	contents := bytes.Repeat([]byte("raw-child-output-"), 4096)
	cleaned := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		served <- serveWireSession(controlReader, cancelReader, eventWriter,
			func(_ context.Context, run *RunSupervisor) Final {
				sendErr := run.SendOutput(id, OutputStdout, contents)
				close(cleaned)
				if sendErr == nil {
					return Final{Cancelled: true, Reason: CancelInternal,
						InternalError: "large output unexpectedly escaped backpressure"}
				}
				return Final{Cancelled: true, Reason: CancelInternal,
					InternalError: sendErr.Error()}
			})
	}()

	expectFrameKind(t, eventReader, frameReady)
	if err := writeFrame(controlWriter, frameStart, nil); err != nil {
		t.Fatal(err)
	}
	expectFrameKind(t, eventReader, frameAccepted)
	if err := writeFrame(controlWriter, frameCommit, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cleaned:
	case <-time.After(eventBackpressureLimit + time.Second):
		t.Fatal("event backpressure did not return control for owner cleanup")
	}
	outputFrame := expectFrameKind(t, eventReader, frameOutput)
	outputEvent, err := decodeEvent(outputFrame)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(outputEvent.Bytes(), contents[:maximumOutputChunk]) {
		t.Fatal("complete output prefix changed before backpressure")
	}
	finalFrame := expectFrameKind(t, eventReader, frameFinal)
	final, err := decodeFinal(finalFrame.payload)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Cancelled || final.Reason != CancelInternal ||
		!strings.Contains(final.InternalError, "backpressure") {
		t.Fatalf("event backpressure final = %#v", final)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if err := eventWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if frame, err := readFrame(eventReader); !errors.Is(err, io.EOF) {
		t.Fatalf("event stream continued after unique final: %#v, %v", frame, err)
	}
}

func TestBackpressuredLargeOutputLeavesCompleteFramesBeforeFinal(t *testing.T) {
	reader, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFiles(reader, output) })
	capacity, err := unix.FcntlInt(output.Fd(), unix.F_SETPIPE_SZ, linuxPipeAtomicWriteBytes)
	if err != nil {
		t.Fatal(err)
	}
	if capacity != linuxPipeAtomicWriteBytes {
		t.Fatalf("event pipe capacity = %d, want %d", capacity, linuxPipeAtomicWriteBytes)
	}
	writer, err := newEventPipeSender(output, 25*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := transfernumber.New(1)
	run := &RunSupervisor{events: writer}
	if err := run.SendOutput(id, OutputStdout,
		bytes.Repeat([]byte("production-output-"), 2048)); err == nil ||
		!strings.Contains(err.Error(), "backpressure") {
		t.Fatalf("large backpressured output error = %v", err)
	}

	prefix := make([]byte, capacity)
	if _, err := io.ReadFull(reader, prefix); err != nil {
		t.Fatal(err)
	}
	wantFinal := Final{Cancelled: true, Reason: CancelInternal,
		InternalError: "event output backpressure", CleanupError: "network cleanup failed"}
	finalPayload, err := encodeFinal(wantFinal)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.send(frameFinal, finalPayload); err != nil {
		t.Fatalf("send final after output backpressure: %v", err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	tail, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	stream := bytes.NewReader(append(prefix, tail...))
	finals := 0
	for stream.Len() != 0 {
		frame, err := readFrame(stream)
		if err != nil {
			t.Fatalf("event stream was corrupted before final: %v", err)
		}
		if frame.kind != frameFinal {
			if _, err := decodeEvent(frame); err != nil {
				t.Fatal(err)
			}
			continue
		}
		final, err := decodeFinal(frame.payload)
		if err != nil {
			t.Fatal(err)
		}
		if final.InternalError != wantFinal.InternalError || final.CleanupError != wantFinal.CleanupError {
			t.Fatalf("final lost run/cleanup evidence: %#v", final)
		}
		finals++
	}
	if finals != 1 {
		t.Fatalf("decoded final frames = %d, want one", finals)
	}
}
