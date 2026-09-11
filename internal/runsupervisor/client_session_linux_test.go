//go:build linux

package runsupervisor

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

func TestCancelDoesNotWaitForBackpressuredOrdinaryControl(t *testing.T) {
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlRead.Close()
	defer controlWrite.Close()
	cancelRead, cancelWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer cancelRead.Close()
	defer cancelWrite.Close()
	capacity, err := unix.FcntlInt(controlWrite.Fd(), unix.F_SETPIPE_SZ, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if written, err := unix.Write(int(controlWrite.Fd()), make([]byte, capacity)); err != nil || written != capacity {
		t.Fatal(err)
	}
	controlFD := int(controlWrite.Fd())
	if err := unix.SetNonblock(controlFD, true); err != nil {
		t.Fatal(err)
	}
	wake, err := controlWriteWake()
	if err != nil {
		t.Fatal(err)
	}
	defer wake.Close()

	client := &RunSupervisorClient{controls: newControlSender(controlWrite, cancelWrite, wake)}
	id, _ := transfernumber.New(1)
	writeDone := make(chan error, 1)
	go func() { writeDone <- client.WriteInput(id, []byte("blocked")) }()

	cancelDone := make(chan error, 1)
	go func() { cancelDone <- client.Cancel(CancelUser) }()
	select {
	case err := <-cancelDone:
		if err != nil {
			t.Fatalf("independent cancellation: %v", err)
		}
	case <-time.After(250 * time.Millisecond):
		_ = controlWrite.Close()
		<-writeDone
		t.Fatal("cancellation waited behind an ordinary control write")
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("cancelled ordinary write leaked a transport error: %v", err)
	}
}

func TestFinalObservationStopsBackpressuredOrdinaryControl(t *testing.T) {
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer controlRead.Close()
	defer controlWrite.Close()
	eventRead, eventWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer eventRead.Close()
	defer eventWrite.Close()
	capacity, err := unix.FcntlInt(controlWrite.Fd(), unix.F_SETPIPE_SZ, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if written, err := unix.Write(int(controlWrite.Fd()), make([]byte, capacity)); err != nil || written != capacity {
		t.Fatal(err)
	}
	controlFD := int(controlWrite.Fd())
	if err := unix.SetNonblock(controlFD, true); err != nil {
		t.Fatal(err)
	}
	wake, err := controlWriteWake()
	if err != nil {
		t.Fatal(err)
	}
	defer wake.Close()
	client := &RunSupervisorClient{
		controls: newControlSender(controlWrite, nil, wake),
		events:   newEventReceiver(eventRead),
	}
	id, _ := transfernumber.New(1)
	done := make(chan error, 1)
	go func() { done <- client.WriteInput(id, []byte("blocked")) }()
	payload, err := encodeFinal(Final{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(eventWrite, frameFinal, payload); err != nil {
		t.Fatal(err)
	}
	event, err := client.Next()
	if err != nil || event.Kind() != EventFinal {
		t.Fatalf("final event = %v, %v", event.Kind(), err)
	}
	if err := client.Resize(id, 80, 24); err != nil {
		t.Fatalf("late valid resize = %v", err)
	}
	if err := client.WriteInput(id, []byte("late")); err != nil {
		t.Fatalf("late valid input = %v", err)
	}
	if err := client.Resize(transfernumber.Number{}, 80, 24); err == nil {
		t.Fatal("late invalid resize was hidden by Final")
	}
	if _, err := client.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("event after Final = %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Final-stopped control write returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Final did not stop a backpressured ordinary control write")
	}
}

func TestWriteInputPublishesPipeAtomicFramesAndPreservesBytes(t *testing.T) {
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	wake, err := controlWriteWake()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFiles(controlRead, controlWrite, wake) })
	controlFD := int(controlWrite.Fd())
	if err := unix.SetNonblock(controlFD, true); err != nil {
		t.Fatal(err)
	}

	input := bytes.Repeat([]byte("supervisor-input-"), 1000)
	received := make(chan struct {
		contents []byte
		frames   int
		largest  int
		err      error
	}, 1)
	go func() {
		var contents []byte
		frames, largest := 0, 0
		for len(contents) < len(input) {
			frame, err := readFrame(controlRead)
			if err != nil {
				received <- struct {
					contents []byte
					frames   int
					largest  int
					err      error
				}{err: err}
				return
			}
			control, err := decodeControl(frame)
			if err != nil {
				received <- struct {
					contents []byte
					frames   int
					largest  int
					err      error
				}{err: err}
				return
			}
			contents = append(contents, control.Input()...)
			frames++
			largest = max(largest, wireHeaderBytes+len(frame.payload))
		}
		received <- struct {
			contents []byte
			frames   int
			largest  int
			err      error
		}{contents: contents, frames: frames, largest: largest}
	}()

	id, _ := transfernumber.New(1)
	client := &RunSupervisorClient{controls: newControlSender(controlWrite, nil, wake)}
	if err := client.WriteInput(id, input); err != nil {
		t.Fatal(err)
	}
	result := <-received
	if result.err != nil {
		t.Fatal(result.err)
	}
	if !bytes.Equal(result.contents, input) {
		t.Fatal("wire input segmentation changed the input bytes")
	}
	if result.frames < 2 {
		t.Fatalf("input frames = %d, want segmented frames", result.frames)
	}
	if result.largest > linuxPipeAtomicWriteBytes {
		t.Fatalf("largest input frame = %d, exceeds pipe atomic write size %d",
			result.largest, linuxPipeAtomicWriteBytes)
	}
}

func TestCancelledBackpressuredInputLeavesNoPartialFrame(t *testing.T) {
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cancelRead, cancelWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	wake, err := controlWriteWake()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFiles(controlRead, controlWrite, cancelRead, cancelWrite, wake) })
	capacity, err := unix.FcntlInt(controlWrite.Fd(), unix.F_SETPIPE_SZ, linuxPipeAtomicWriteBytes)
	if err != nil {
		t.Fatal(err)
	}
	filler := bytes.Repeat([]byte{'f'}, capacity/2)
	if written, err := unix.Write(int(controlWrite.Fd()), filler); err != nil || written != len(filler) {
		t.Fatalf("fill control pipe = %d, %v", written, err)
	}
	controlFD := int(controlWrite.Fd())
	if err := unix.SetNonblock(controlFD, true); err != nil {
		t.Fatal(err)
	}

	client := &RunSupervisorClient{controls: newControlSender(controlWrite, cancelWrite, wake)}
	id, _ := transfernumber.New(1)
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- client.WriteInput(id, bytes.Repeat([]byte{'x'}, 2*linuxPipeAtomicWriteBytes))
	}()
	if err := client.Cancel(CancelUser); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}

	drained := make([]byte, len(filler))
	if _, err := io.ReadFull(controlRead, drained); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(drained, filler) {
		t.Fatal("ordinary writer changed bytes that preceded the blocked frame")
	}
	poll := []unix.PollFd{{Fd: int32(controlRead.Fd()), Events: unix.POLLIN}}
	if count, err := unix.Poll(poll, 0); err != nil || count != 0 || poll[0].Revents != 0 {
		t.Fatalf("cancelled atomic frame left readable ordinary bytes: count %d, events %#x, err %v",
			count, poll[0].Revents, err)
	}
}

func TestCloseLifelineStopsBackpressuredOrdinaryControl(t *testing.T) {
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cancelRead, cancelWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	wake, err := controlWriteWake()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFiles(controlRead, controlWrite, cancelRead, cancelWrite, wake) })
	capacity, err := unix.FcntlInt(controlWrite.Fd(), unix.F_SETPIPE_SZ, pipeAtomicFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	if written, err := unix.Write(int(controlWrite.Fd()), make([]byte, capacity)); err != nil || written != capacity {
		t.Fatalf("fill control pipe = %d, %v", written, err)
	}
	if err := unix.SetNonblock(int(controlWrite.Fd()), true); err != nil {
		t.Fatal(err)
	}
	client := &RunSupervisorClient{controls: newControlSender(controlWrite, cancelWrite, wake)}
	transfer, _ := transfernumber.New(1)
	writeDone := make(chan error, 1)
	go func() { writeDone <- client.WriteInput(transfer, []byte("blocked")) }()
	if err := client.CloseLifeline(); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("lifeline-stopped control write = %v", err)
	}
}

func TestWaitStopsBackpressuredOrdinaryControlWithoutCancelling(t *testing.T) {
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cancelRead, cancelWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	eventRead, eventWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	wake, err := controlWriteWake()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeFiles(controlRead, controlWrite, cancelRead, cancelWrite, eventRead, eventWrite, wake)
	})
	capacity, err := unix.FcntlInt(controlWrite.Fd(), unix.F_SETPIPE_SZ, pipeAtomicFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	if written, err := unix.Write(int(controlWrite.Fd()), make([]byte, capacity)); err != nil || written != capacity {
		t.Fatalf("fill control pipe = %d, %v", written, err)
	}
	if err := unix.SetNonblock(int(controlWrite.Fd()), true); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("/bin/sh", "-c", "exit 0")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	client := &RunSupervisorClient{
		controls: newControlSender(controlWrite, cancelWrite, wake),
		events:   newEventReceiver(eventRead),
		process:  newSupervisorProcess(command),
	}
	transfer, _ := transfernumber.New(1)
	writeDone := make(chan error, 1)
	go func() { writeDone <- client.WriteInput(transfer, []byte("blocked")) }()
	if err := client.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("Wait-stopped control write = %v", err)
	}
	if err := cancelWrite.Close(); err == nil {
		t.Fatal("Wait did not tear down the parent lifeline after process exit")
	}
}

func TestReceiveFailureDoesNotRequestCancellation(t *testing.T) {
	for name, emit := range map[string]func(*os.File) error{
		"truncated frame": func(writer *os.File) error {
			_, err := writer.Write([]byte{'W', 'S', wireVersion})
			return err
		},
		"invalid output identity": func(writer *os.File) error {
			return writeFrame(writer, frameOutput, []byte{0, byte(OutputPTY), 'x'})
		},
	} {
		t.Run(name, func(t *testing.T) {
			eventRead, eventWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			cancelRead, cancelWrite, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer closeFiles(eventRead, eventWrite, cancelRead, cancelWrite)
			if err := emit(eventWrite); err != nil {
				t.Fatal(err)
			}
			if err := eventWrite.Close(); err != nil {
				t.Fatal(err)
			}
			client := &RunSupervisorClient{
				controls: newControlSender(nil, cancelWrite, nil),
				events:   newEventReceiver(eventRead),
			}
			if _, err := client.Next(); err == nil {
				t.Fatal("supervisor receive failure was accepted")
			}
			if err := cancelWrite.Close(); err != nil {
				t.Fatal(err)
			}
			contents, err := io.ReadAll(cancelRead)
			if err != nil || len(contents) != 0 {
				t.Fatalf("receive failure wrote %d cancellation bytes: %v", len(contents), err)
			}
		})
	}
}

func TestCancelAfterFinalIsAnIdempotentNoOp(t *testing.T) {
	eventRead, eventWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cancelRead, cancelWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer closeFiles(eventRead, eventWrite, cancelRead, cancelWrite)
	payload, err := encodeFinal(Final{})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(eventWrite, frameFinal, payload); err != nil {
		t.Fatal(err)
	}
	client := &RunSupervisorClient{
		controls: newControlSender(nil, cancelWrite, nil),
		events:   newEventReceiver(eventRead),
	}
	event, err := client.Next()
	if err != nil || event.Kind() != EventFinal {
		t.Fatalf("final event=%v error=%v", event.Kind(), err)
	}
	if err := client.Cancel(CancelUser); err != nil {
		t.Fatalf("late cancellation returned an operational failure: %v", err)
	}
	poll := []unix.PollFd{{Fd: int32(cancelRead.Fd()), Events: unix.POLLIN}}
	if count, err := unix.Poll(poll, 0); err != nil || count != 0 || poll[0].Revents != 0 {
		t.Fatalf("late cancellation wrote bytes: count=%d events=%#x error=%v",
			count, poll[0].Revents, err)
	}
}
