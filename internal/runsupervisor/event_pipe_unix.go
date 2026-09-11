//go:build unix

package runsupervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func prepareEventPipe(output io.Writer) (int, error) {
	file, ok := output.(*os.File)
	if !ok {
		return -1, nil
	}
	fd := int(file.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		return -1, fmt.Errorf("make supervisor event output interruptible: %w", err)
	}
	return fd, nil
}

func writeEventPipe(fd int, encoded []byte, deadline time.Time) error {
	for {
		written, err := unix.Write(fd, encoded)
		switch {
		case err == nil && written == len(encoded):
			return nil
		case err == unix.EINTR:
			continue
		case err == unix.EAGAIN || err == unix.EWOULDBLOCK:
			if err := waitForEventPipe(fd, deadline); err != nil {
				return err
			}
		default:
			if err == nil {
				err = io.ErrShortWrite
			}
			return err
		}
	}
}

func waitForEventPipe(fd int, deadline time.Time) error {
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return errors.New("supervisor event backpressure did not advance")
		}
		milliseconds := int((remaining + time.Millisecond - 1) / time.Millisecond)
		poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT | unix.POLLERR | unix.POLLHUP}}
		count, err := unix.Poll(poll, milliseconds)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if count == 0 {
			return errors.New("supervisor event backpressure did not advance")
		}
		if poll[0].Revents&unix.POLLOUT != 0 {
			return nil
		}
		if poll[0].Revents&(unix.POLLERR|unix.POLLHUP) != 0 {
			return io.ErrClosedPipe
		}
	}
}
