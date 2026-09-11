package terminal

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

type inputEvent struct {
	content []byte
	err     error
}

func startOwnedInput(input *os.File) (<-chan inputEvent, func(), error) {
	duplicate, err := unix.FcntlInt(input.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("duplicate terminal input: %w", err)
	}
	stopRead, stopWrite, err := os.Pipe()
	if err != nil {
		_ = unix.Close(duplicate)
		return nil, nil, fmt.Errorf("create terminal reader stop pipe: %w", err)
	}
	events := make(chan inputEvent, 1)
	done := make(chan struct{})
	stopSignal := make(chan struct{})
	go pollTerminalInput(duplicate, int(stopRead.Fd()), events, done, stopSignal)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(stopSignal)
			_, _ = stopWrite.Write([]byte{1})
			<-done
			_ = unix.Close(duplicate)
			_ = stopRead.Close()
			_ = stopWrite.Close()
		})
	}
	return events, stop, nil
}

func pollTerminalInput(inputFD, stopFD int, events chan<- inputEvent, done chan<- struct{},
	stop <-chan struct{},
) {
	defer close(done)
	defer close(events)
	buffer := make([]byte, 32*1024)
	for {
		poll := []unix.PollFd{{Fd: int32(inputFD), Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR},
			{Fd: int32(stopFD), Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR}}
		if _, err := unix.Poll(poll, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			deliverInputEvent(events, inputEvent{err: fmt.Errorf("poll terminal input: %w", err)}, stop)
			return
		}
		if poll[1].Revents != 0 {
			return
		}
		count, err := unix.Read(inputFD, buffer)
		if count > 0 && !deliverInputEvent(events,
			inputEvent{content: append([]byte(nil), buffer[:count]...)}, stop) {
			return
		}
		if err != nil && err != unix.EINTR {
			deliverInputEvent(events, inputEvent{err: fmt.Errorf("read terminal input: %w", err)}, stop)
			return
		}
		if count == 0 || poll[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0 {
			deliverInputEvent(events, inputEvent{err: errors.New("terminal input disappeared")}, stop)
			return
		}
	}
}

func deliverInputEvent(events chan<- inputEvent, event inputEvent, stop <-chan struct{}) bool {
	select {
	case events <- event:
		return true
	case <-stop:
		return false
	}
}
