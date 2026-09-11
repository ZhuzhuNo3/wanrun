//go:build linux

package runsupervisor

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const linuxPipeAtomicWriteBytes = pipeAtomicFrameBytes

func writeLaunchStartFrame(ctx context.Context, control, wake *os.File, kind frameKind, payload []byte) error {
	watchStop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-ctx.Done():
			signalControlWriteWake(wake)
		case <-watchStop:
		}
	}()
	err := writeControlFile(control, wake, kind, payload, false)
	close(watchStop)
	<-watchDone
	drainControlWriteWake(wake)
	return err
}

func controlWriteWake() (*os.File, error) {
	descriptor, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("create supervisor control-write wakeup: %w", err)
	}
	return os.NewFile(uintptr(descriptor), "supervisor-control-wakeup"), nil
}

func signalControlWriteWake(wake *os.File) {
	if wake == nil {
		return
	}
	fd := int(wake.Fd())
	_ = unix.SetNonblock(fd, true)
	var value [8]byte
	binary.NativeEndian.PutUint64(value[:], 1)
	_, _ = unix.Write(fd, value[:])
}

func drainControlWriteWake(wake *os.File) {
	if wake == nil {
		return
	}
	fd := int(wake.Fd())
	_ = unix.SetNonblock(fd, true)
	var value [8]byte
	for {
		_, err := unix.Read(fd, value[:])
		if err == unix.EINTR {
			continue
		}
		return
	}
}

func writeAtomicControlFile(control, wake *os.File, kind frameKind, payload []byte) error {
	return writeControlFile(control, wake, kind, payload, true)
}

func writeControlFile(control, wake *os.File, kind frameKind, payload []byte, atomic bool) error {
	encoded, err := encodeFrame(kind, payload)
	if err != nil {
		return err
	}
	if atomic && len(encoded) > pipeAtomicFrameBytes {
		return fmt.Errorf("supervisor runtime control frame is not pipe-atomic")
	}
	controlFD, wakeFD := int(control.Fd()), int(wake.Fd())
	if err := unix.SetNonblock(controlFD, true); err != nil {
		return fmt.Errorf("make supervisor control write interruptible: %w", err)
	}
	if err := unix.SetNonblock(wakeFD, true); err != nil {
		return fmt.Errorf("make supervisor control wake interruptible: %w", err)
	}
	for len(encoded) != 0 {
		written, writeErr := unix.Write(controlFD, encoded)
		switch {
		case writeErr == nil && written > 0 && (!atomic || written == len(encoded)):
			encoded = encoded[written:]
		case writeErr == unix.EINTR:
			continue
		case writeErr == unix.EAGAIN:
			if err := waitForControlFile(controlFD, wakeFD); err != nil {
				return err
			}
		default:
			if writeErr == nil {
				writeErr = io.ErrShortWrite
			}
			return fmt.Errorf("write supervisor control frame: %w", writeErr)
		}
	}
	return nil
}

func waitForControlFile(controlFD, wakeFD int) error {
	poll := []unix.PollFd{
		{Fd: int32(controlFD), Events: unix.POLLOUT | unix.POLLERR | unix.POLLHUP},
		{Fd: int32(wakeFD), Events: unix.POLLIN | unix.POLLERR | unix.POLLHUP},
	}
	for {
		_, err := unix.Poll(poll, -1)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fmt.Errorf("wait supervisor control write: %w", err)
		}
		if poll[1].Revents != 0 {
			return os.ErrClosed
		}
		if poll[0].Revents&unix.POLLOUT != 0 {
			return nil
		}
		if poll[0].Revents&(unix.POLLERR|unix.POLLHUP) != 0 {
			return io.ErrClosedPipe
		}
	}
}

func closeFiles(files ...*os.File) {
	for _, file := range files {
		if file != nil {
			_ = file.Close()
		}
	}
}
