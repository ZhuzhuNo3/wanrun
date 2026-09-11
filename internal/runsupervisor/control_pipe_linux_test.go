//go:build linux

package runsupervisor

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestControlPipeWakeInterruptsBackpressureWithoutPartialFrame(t *testing.T) {
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	wake, err := controlWriteWake()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFiles(controlRead, controlWrite, wake) })
	capacity, err := unix.FcntlInt(controlWrite.Fd(), unix.F_SETPIPE_SZ, pipeAtomicFrameBytes)
	if err != nil {
		t.Fatal(err)
	}
	filler := bytes.Repeat([]byte{'f'}, capacity)
	if written, err := unix.Write(int(controlWrite.Fd()), filler); err != nil || written != len(filler) {
		t.Fatalf("fill control pipe = %d, %v", written, err)
	}
	if err := unix.SetNonblock(int(controlWrite.Fd()), true); err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() { writeDone <- writeAtomicControlFile(controlWrite, wake, frameCommit, nil) }()
	signalControlWriteWake(wake)
	if err := <-writeDone; !errors.Is(err, os.ErrClosed) {
		t.Fatalf("interrupted control write = %v", err)
	}
	drained := make([]byte, capacity)
	if _, err := io.ReadFull(controlRead, drained); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(drained, filler) {
		t.Fatal("interrupted atomic control write changed preceding bytes")
	}
	poll := []unix.PollFd{{Fd: int32(controlRead.Fd()), Events: unix.POLLIN}}
	if count, err := unix.Poll(poll, 0); err != nil || count != 0 || poll[0].Revents != 0 {
		t.Fatalf("interrupted atomic control write left partial bytes: count=%d events=%#x err=%v",
			count, poll[0].Revents, err)
	}
}
