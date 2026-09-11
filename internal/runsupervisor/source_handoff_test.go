package runsupervisor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunSupervisorDeliversBoundedStartRequestAndClassifiedFinal(t *testing.T) {
	request := []byte(`{"version":1,"source":"/src"}`)
	var control bytes.Buffer
	if err := writeFrame(&control, frameStart, request); err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(&control, frameCommit, nil); err != nil {
		t.Fatal(err)
	}
	var events bytes.Buffer
	wantRun := errors.New("transfer execution failed")
	wantCleanup := errors.New("network cleanup failed")
	err := serveWireSession(&control, heldCancellationPipe(t), &events, func(_ context.Context, run *RunSupervisor) Final {
		if !bytes.Equal(run.Request(), request) {
			t.Fatalf("supervisor request = %q", run.Request())
		}
		return Final{RunError: wantRun.Error(), CleanupError: wantCleanup.Error()}
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := readFrame(&events)
	if err != nil || ready.kind != frameReady {
		t.Fatalf("ready = %#v, %v", ready, err)
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
	if final.RunError != wantRun.Error() || final.CleanupError != wantCleanup.Error() {
		t.Fatalf("final = %#v", final)
	}
}

func TestRunSupervisorWaitsForCommitAfterAcceptingTheSource(t *testing.T) {
	controlReader, controlWriter := io.Pipe()
	cancelReader, cancelWriter := io.Pipe()
	eventReader, eventWriter := io.Pipe()
	source, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = controlReader.Close()
		_ = controlWriter.Close()
		_ = cancelReader.Close()
		_ = cancelWriter.Close()
		_ = eventReader.Close()
		_ = eventWriter.Close()
		_ = source.Close()
	})
	ran := make(chan struct{}, 1)
	served := make(chan error, 1)
	go func() {
		served <- serveWireSessionWithSource(controlReader, cancelReader, eventWriter, source,
			func(context.Context, *RunSupervisor) Final {
				ran <- struct{}{}
				return Final{}
			})
	}()
	expectFrameKind(t, eventReader, frameReady)
	if err := writeFrame(controlWriter, frameStart, []byte("request")); err != nil {
		t.Fatal(err)
	}
	expectFrameKind(t, eventReader, frameAccepted)
	if err := cancelWriter.Close(); err != nil {
		t.Fatal(err)
	}
	finalFrame := expectFrameKind(t, eventReader, frameFinal)
	final, err := decodeFinal(finalFrame.payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	select {
	case <-ran:
		t.Fatal("supervisor began owned work after Accepted but before Commit")
	default:
	}
	if !final.Cancelled || final.Reason != CancelLifeline {
		t.Fatalf("pre-commit lifeline final = %#v", final)
	}
	if _, err := source.Stat(); err == nil {
		t.Fatal("pre-commit supervisor retained the accepted source")
	}
}

func TestRunSupervisorTreatsPreCommitControlEOFAsInternalFailure(t *testing.T) {
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
	source, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, file := range []*os.File{controlReader, controlWriter, cancelReader, cancelWriter,
			eventReader, eventWriter, source} {
			_ = file.Close()
		}
	})
	ran := make(chan struct{}, 1)
	served := make(chan error, 1)
	go func() {
		served <- serveWireSessionWithSource(controlReader, cancelReader, eventWriter, source,
			func(context.Context, *RunSupervisor) Final {
				ran <- struct{}{}
				return Final{}
			})
	}()
	expectFrameKind(t, eventReader, frameReady)
	if err := writeFrame(controlWriter, frameStart, []byte("request")); err != nil {
		t.Fatal(err)
	}
	expectFrameKind(t, eventReader, frameAccepted)
	if err := controlWriter.Close(); err != nil {
		t.Fatal(err)
	}
	finalFrame := expectFrameKind(t, eventReader, frameFinal)
	final, err := decodeFinal(finalFrame.payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-served; err != nil {
		t.Fatal(err)
	}
	if !final.Cancelled || final.Reason != CancelInternal ||
		!strings.Contains(final.InternalError, "supervisor control reached EOF before final") {
		t.Fatalf("pre-commit control EOF final = %#v", final)
	}
	select {
	case <-ran:
		t.Fatal("supervisor began work after pre-commit control EOF")
	default:
	}
	if _, err := source.Stat(); err == nil {
		t.Fatal("pre-commit control EOF retained the accepted source")
	}
	if err := eventWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if frame, err := readFrame(eventReader); !errors.Is(err, io.EOF) {
		t.Fatalf("supervisor emitted more than one final after control EOF: %#v, %v", frame, err)
	}
}

func TestRunSupervisorBoundsOpaqueBinaryStartRequest(t *testing.T) {
	if err := validateStartRequest(bytes.Repeat([]byte("x"), maximumWirePayload+1)); err == nil {
		t.Fatal("oversized request unexpectedly accepted")
	}
	if err := validateStartRequest([]byte{'a', 0xff, 'b'}); err != nil {
		t.Fatalf("bounded binary request rejected before its private decoder: %v", err)
	}
}

func TestRunSupervisorRunTransfersInheritedSourceExactlyOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "original"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	parked := root + "-parked"
	if err := os.Rename(root, parked); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "replacement"), []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	var control bytes.Buffer
	if err := writeFrame(&control, frameStart, []byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(&control, frameCommit, nil); err != nil {
		t.Fatal(err)
	}
	var events bytes.Buffer
	err = serveWireSessionWithSource(&control, heldCancellationPipe(t), &events, source,
		func(_ context.Context, run *RunSupervisor) Final {
			owned, err := run.TakeSourceRoot(root)
			if err != nil {
				t.Fatalf("TakeSourceRoot: %v", err)
			}
			defer owned.Close()
			if _, err := run.TakeSourceRoot(root); err == nil {
				t.Fatal("supervisor transferred one source capability twice")
			}
			if runtime.GOOS == "linux" {
				contents, err := os.ReadFile(filepath.Join(owned.StableRoot(), "original"))
				if err != nil || string(contents) != "original" {
					t.Fatalf("supervisor source followed replacement: %q, %v", contents, err)
				}
			}
			return Final{}
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Stat(); err == nil {
		t.Fatal("inherited descriptor remained independently owned after transfer")
	}
}

func TestRunSupervisorClosesInheritedSourceWhenStartHandshakeFails(t *testing.T) {
	source, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var control bytes.Buffer
	if err := writeFrame(&control, frameResize, []byte{1, 0, 80, 0, 24}); err != nil {
		t.Fatal(err)
	}
	var events bytes.Buffer
	err = serveWireSessionWithSource(&control, heldCancellationPipe(t), &events, source,
		func(context.Context, *RunSupervisor) Final {
			t.Fatal("invalid start invoked supervisor work")
			return Final{}
		})
	if err == nil || !errors.Is(err, errHandshake) {
		t.Fatalf("invalid start error = %v", err)
	}
	if _, err := source.Stat(); err == nil {
		t.Fatal("failed start handshake retained inherited source")
	}
}
