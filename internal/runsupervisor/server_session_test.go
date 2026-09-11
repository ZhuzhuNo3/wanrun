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
)

func TestHandshakeReportsLifelineEOFAsFinalCancellation(t *testing.T) {
	controlReader, controlWriter := io.Pipe()
	cancelReader, cancelWriter := io.Pipe()
	eventReader, eventWriter := io.Pipe()
	served := make(chan error, 1)
	go func() {
		served <- serveWireSession(controlReader, cancelReader, eventWriter, func(ctx context.Context, _ *RunSupervisor) Final {
			<-ctx.Done()
			return Final{}
		})
	}()
	ready, err := readFrame(eventReader)
	if err != nil || ready.kind != frameReady || len(ready.payload) != 0 {
		t.Fatalf("ready frame = %#v, %v", ready, err)
	}
	if err := writeFrame(controlWriter, frameStart, nil); err != nil {
		t.Fatal(err)
	}
	expectFrameKind(t, eventReader, frameAccepted)
	if err := writeFrame(controlWriter, frameCommit, nil); err != nil {
		t.Fatal(err)
	}
	if err := cancelWriter.Close(); err != nil {
		t.Fatal(err)
	}
	finalFrame, err := readFrame(eventReader)
	if err != nil || finalFrame.kind != frameFinal {
		t.Fatalf("final frame = %#v, %v", finalFrame, err)
	}
	final, err := decodeFinal(finalFrame.payload)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Cancelled || final.Reason != CancelLifeline {
		t.Fatalf("lifeline final = %#v", final)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	_ = controlWriter.Close()
}

func TestOrdinaryControlEOFBeforeFinalIsAnInternalFailure(t *testing.T) {
	controlReader, controlWriter := io.Pipe()
	eventReader, eventWriter := io.Pipe()
	served := make(chan error, 1)
	go func() {
		served <- serveWireSession(controlReader, heldCancellationPipe(t), eventWriter,
			func(ctx context.Context, _ *RunSupervisor) Final {
				select {
				case <-ctx.Done():
				case <-time.After(250 * time.Millisecond):
				}
				return Final{}
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
	if err := controlWriter.Close(); err != nil {
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
	if !final.Cancelled || final.Reason != CancelInternal ||
		!strings.Contains(final.InternalError, "control") {
		t.Fatalf("ordinary EOF final = %#v", final)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestCancellationBypassesBackpressuredOrdinaryControl(t *testing.T) {
	id, _ := transfernumber.New(1)
	controlReader, controlWriter := io.Pipe()
	defer controlReader.Close()
	kind, payload, err := encodeControl(newResizeControl(id, 80, 24))
	if err != nil {
		t.Fatal(err)
	}
	eventReader, eventWriter := io.Pipe()
	cancelReader, cancelWriter := io.Pipe()
	defer cancelReader.Close()
	defer cancelWriter.Close()
	served := make(chan error, 1)
	delivered := make(chan int, 1)
	go func() {
		served <- serveWireSession(controlReader, cancelReader, eventWriter, func(ctx context.Context, run *RunSupervisor) Final {
			<-ctx.Done()
			for count := 0; count <= maximumPendingControls; count++ {
				<-run.Controls()
			}
			delivered <- maximumPendingControls + 1
			return Final{}
		})
	}()
	ready, err := readFrame(eventReader)
	if err != nil || ready.kind != frameReady {
		t.Fatalf("ready frame = %#v, %v", ready, err)
	}
	if err := writeFrame(controlWriter, frameStart, nil); err != nil {
		t.Fatal(err)
	}
	expectFrameKind(t, eventReader, frameAccepted)
	if err := writeFrame(controlWriter, frameCommit, nil); err != nil {
		t.Fatal(err)
	}
	for range maximumPendingControls + 1 {
		if err := writeFrame(controlWriter, kind, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeCancellation(cancelWriter, CancelUser); err != nil {
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
	if !final.Cancelled || final.Reason != CancelUser || final.InternalError != "" {
		t.Fatalf("backpressure final = %#v", final)
	}
	if count := <-delivered; count < maximumPendingControls || count > maximumPendingControls+1 {
		t.Fatalf("delivered controls = %d, want the full queue and at most its decoded pending write", count)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	_ = controlWriter.Close()
}

func TestIndependentPartialOrdinaryFrameRemainsInternalFailure(t *testing.T) {
	id, _ := transfernumber.New(1)
	kind, payload, err := encodeControl(newInputControl(id, bytes.Repeat([]byte{'x'}, 64)))
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := encodeFrame(kind, payload)
	if err != nil {
		t.Fatal(err)
	}
	controlReader, controlWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	eventReader, eventWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = controlReader.Close()
		_ = controlWriter.Close()
		_ = eventReader.Close()
		_ = eventWriter.Close()
	})
	served := make(chan error, 1)
	go func() {
		served <- serveWireSession(controlReader, heldCancellationPipe(t), eventWriter,
			func(ctx context.Context, _ *RunSupervisor) Final {
				<-ctx.Done()
				return Final{}
			})
	}()
	if ready, err := readFrame(eventReader); err != nil || ready.kind != frameReady {
		t.Fatalf("ready frame = %#v, %v", ready, err)
	}
	if err := writeFrame(controlWriter, frameStart, nil); err != nil {
		t.Fatal(err)
	}
	expectFrameKind(t, eventReader, frameAccepted)
	if err := writeFrame(controlWriter, frameCommit, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := controlWriter.Write(ordinary[:wireHeaderBytes+3]); err != nil {
		t.Fatal(err)
	}
	if err := controlWriter.Close(); err != nil {
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
	if !final.Cancelled || final.Reason != CancelInternal ||
		!strings.Contains(final.InternalError, "read supervisor control") {
		t.Fatalf("independent partial frame final = %#v", final)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
}

func TestCancelFrameRemainsAuthoritativeWhenTheControlQueueIsFull(t *testing.T) {
	id, _ := transfernumber.New(1)
	controlReader, controlWriter := io.Pipe()
	defer controlReader.Close()
	defer controlWriter.Close()
	go func() {
		_ = writeFrame(controlWriter, frameStart, nil)
		_ = writeFrame(controlWriter, frameCommit, nil)
		resizeKind, resizePayload, _ := encodeControl(newResizeControl(id, 80, 24))
		for range maximumPendingControls {
			_ = writeFrame(controlWriter, resizeKind, resizePayload)
		}
	}()
	var cancellation bytes.Buffer
	if err := writeCancellation(&cancellation, CancelUser); err != nil {
		t.Fatal(err)
	}
	var events bytes.Buffer
	if err := serveWireSession(controlReader, &cancellation, &events, func(ctx context.Context, run *RunSupervisor) Final {
		<-ctx.Done()
		for range maximumPendingControls {
			<-run.Controls()
		}
		return Final{}
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := readFrame(&events); err != nil {
		t.Fatal(err)
	}
	expectFrameKind(t, &events, frameAccepted)
	frame, err := readFrame(&events)
	if err != nil {
		t.Fatal(err)
	}
	final, err := decodeFinal(frame.payload)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Cancelled || final.Reason != CancelUser || final.InternalError != "" {
		t.Fatalf("full-queue cancel final = %#v", final)
	}
}

func expectFrameKind(t *testing.T, reader io.Reader, kind frameKind) wireFrame {
	t.Helper()
	frame, err := readFrame(reader)
	if err != nil || frame.kind != kind {
		t.Fatalf("supervisor frame = %#v, %v; want kind %d", frame, err, kind)
	}
	return frame
}

func TestHandshakeRejectsAnythingBeforeStart(t *testing.T) {
	controlReader, controlWriter := io.Pipe()
	eventReader, eventWriter := io.Pipe()
	served := make(chan error, 1)
	ran := make(chan struct{}, 1)
	go func() {
		served <- serveWireSession(controlReader, heldCancellationPipe(t), eventWriter, func(context.Context, *RunSupervisor) Final {
			ran <- struct{}{}
			return Final{}
		})
	}()
	if _, err := readFrame(eventReader); err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(controlWriter, frameResize, []byte{1, 0, 80, 0, 24}); err != nil {
		t.Fatal(err)
	}
	if err := <-served; err == nil || !errors.Is(err, errHandshake) {
		t.Fatalf("invalid handshake error = %v", err)
	}
	select {
	case <-ran:
		t.Fatal("work ran before a valid start handshake")
	default:
	}
}

func TestRunSupervisorRejectsRuntimeControlBeforeCommitWithoutStartingWork(t *testing.T) {
	controlReader, controlWriter := io.Pipe()
	eventReader, eventWriter := io.Pipe()
	ran := make(chan struct{}, 1)
	served := make(chan error, 1)
	go func() {
		served <- serveWireSession(controlReader, heldCancellationPipe(t), eventWriter,
			func(context.Context, *RunSupervisor) Final {
				ran <- struct{}{}
				return Final{}
			})
	}()
	expectFrameKind(t, eventReader, frameReady)
	if err := writeFrame(controlWriter, frameStart, nil); err != nil {
		t.Fatal(err)
	}
	expectFrameKind(t, eventReader, frameAccepted)
	if err := writeFrame(controlWriter, frameResize, []byte{1, 0, 80, 0, 24}); err != nil {
		t.Fatal(err)
	}
	finalFrame := expectFrameKind(t, eventReader, frameFinal)
	final, err := decodeFinal(finalFrame.payload)
	if err != nil {
		t.Fatal(err)
	}
	if !final.Cancelled || final.Reason != CancelInternal ||
		!strings.Contains(final.InternalError, "commit") {
		t.Fatalf("pre-commit control final = %#v", final)
	}
	select {
	case <-ran:
		t.Fatal("supervisor began work after a runtime control replaced Commit")
	default:
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	_ = controlWriter.Close()
}

func heldCancellationPipe(t *testing.T) io.Reader {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = writer.Close()
		_ = reader.Close()
	})
	return reader
}
