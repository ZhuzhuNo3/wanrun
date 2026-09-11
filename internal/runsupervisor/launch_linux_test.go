//go:build linux

package runsupervisor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLaunchStartBackpressureObservesContextAndDrainsWake(t *testing.T) {
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
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := writeLaunchStartFrame(ctx, controlWrite, wake, frameStart, []byte("request")); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("cancelled backpressured start = %v", err)
	}
	drained := make([]byte, capacity)
	if _, err := io.ReadFull(controlRead, drained); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomicControlFile(controlWrite, wake, frameCommit, nil); err != nil {
		t.Fatalf("launch wake remained signalled after watcher joined: %v", err)
	}
}
