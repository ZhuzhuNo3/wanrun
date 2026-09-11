//go:build linux

package childprocesses

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

type processIO struct {
	controlsMu       sync.RWMutex
	acceptingControl bool
	streams          []*processStream
	ptyByTransfer    map[transfernumber.Number]int
	outputWake       int
	controlWake      int
	readerWG         sync.WaitGroup
	inputWriterWG    sync.WaitGroup
	output           chan Output
}

type processStream struct {
	transfer transfernumber.Number
	stream   OutputStream
	file     *os.File
	fd       int
}

func newProcessIO(children []*launchedChild, outputBuffer int, outputWake, controlWake int) (
	*processIO, []error,
) {
	processes := &processIO{
		acceptingControl: true,
		ptyByTransfer:    make(map[transfernumber.Number]int, len(children)),
		outputWake:       outputWake,
		controlWake:      controlWake,
		output:           make(chan Output, outputBuffer),
	}
	var failures []error
	for _, child := range children {
		outputs := child.outputs
		child.outputs = nil
		for _, output := range outputs {
			stream, err := bindProcessStream(child.transfer, output)
			if err != nil {
				failures = append(failures, fmt.Errorf(
					"make transfer %d %s output interruptible: %w",
					child.transfer.Value(), output.stream, err))
				closeFile(output.file)
				continue
			}
			index := len(processes.streams)
			processes.streams = append(processes.streams, stream)
			if stream.stream == StreamPTY {
				processes.ptyByTransfer[child.transfer] = index
			}
		}
	}
	return processes, failures
}

func bindProcessStream(id transfernumber.Number, prepared preparedOutput) (*processStream, error) {
	if prepared.file == nil || prepared.stream < StreamPTY || prepared.stream > StreamStderr {
		return nil, errors.New("child output is invalid")
	}
	raw, err := prepared.file.SyscallConn()
	if err != nil {
		return nil, err
	}
	output := &processStream{transfer: id, stream: prepared.stream, file: prepared.file, fd: -1}
	var configureErr error
	if err := raw.Control(func(descriptor uintptr) {
		output.fd = int(descriptor)
		configureErr = unix.SetNonblock(output.fd, true)
	}); err != nil {
		return nil, err
	}
	if configureErr != nil {
		return nil, configureErr
	}
	return output, nil
}

func (processes *processIO) startReaders(onFailure func(error)) {
	for _, stream := range processes.streams {
		processes.readerWG.Add(1)
		go processes.readOutput(stream, onFailure)
	}
}

func (processes *processIO) readOutput(output *processStream, onFailure func(error)) {
	defer processes.readerWG.Done()
	buffer := make([]byte, maximumOutputReadSize)
	for {
		if err := waitDescriptor(output.fd, processes.outputWake, unix.POLLIN); err != nil {
			if !errors.Is(err, os.ErrClosed) {
				onFailure(fmt.Errorf("wait to read transfer %d %s: %w",
					output.transfer.Value(), output.stream, err))
			}
			return
		}
		count, err := unix.Read(output.fd, buffer)
		if count > 0 && !processes.deliverOutput(output, buffer[:count], onFailure) {
			return
		}
		if outputReadComplete(output.stream, count, err) {
			return
		}
		if err == nil || err == unix.EINTR || err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			continue
		}
		onFailure(fmt.Errorf("read transfer %d %s: %w",
			output.transfer.Value(), output.stream, err))
		return
	}
}

func outputReadComplete(stream OutputStream, count int, err error) bool {
	if err == nil {
		return count == 0
	}
	return errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) ||
		stream == StreamPTY && errors.Is(err, syscall.EIO)
}

func (processes *processIO) deliverOutput(source *processStream, contents []byte,
	onFailure func(error),
) bool {
	event := Output{Transfer: source.transfer, Stream: source.stream,
		Bytes: append([]byte(nil), contents...)}
	timer := time.NewTimer(abortTimeout)
	defer timer.Stop()
	select {
	case processes.output <- event:
		return true
	case <-timer.C:
		onFailure(fmt.Errorf("deliver transfer %d %s: output backpressure did not advance",
			source.transfer.Value(), source.stream))
		return false
	}
}

func (processes *processIO) writeInput(id transfernumber.Number, contents []byte,
	onFailure func(error),
) error {
	if id.Value() == 0 || len(contents) == 0 {
		return fmt.Errorf("child PTY input is invalid")
	}
	duplicate, err := processes.borrowControl(id)
	if err != nil {
		return err
	}
	defer processes.inputWriterWG.Done()
	writeErr := errors.Join(writePTYInput(duplicate, processes.controlWake, contents),
		unix.Close(duplicate))
	if writeErr != nil && processes.controlIsActive() {
		onFailure(fmt.Errorf("write transfer %d PTY input: %w", id.Value(), writeErr))
	}
	return writeErr
}

func (processes *processIO) borrowControl(id transfernumber.Number) (int, error) {
	processes.controlsMu.Lock()
	defer processes.controlsMu.Unlock()
	index, exists := processes.ptyByTransfer[id]
	if !exists || index < 0 || index >= len(processes.streams) || !processes.acceptingControl {
		return -1, fmt.Errorf("transfer %d PTY is unavailable", id.Value())
	}
	duplicate, err := unix.FcntlInt(uintptr(processes.streams[index].fd), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("duplicate transfer %d PTY for input: %w", id.Value(), err)
	}
	processes.inputWriterWG.Add(1)
	return duplicate, nil
}

func writePTYInput(input, wake int, contents []byte) error {
	for len(contents) != 0 {
		if err := waitDescriptor(input, wake, unix.POLLOUT); err != nil {
			return err
		}
		chunk := contents
		if len(chunk) > maximumPTYWriteSize {
			chunk = chunk[:maximumPTYWriteSize]
		}
		written, err := unix.Write(input, chunk)
		if err == unix.EINTR || err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			continue
		}
		if err != nil {
			return err
		}
		if written <= 0 || written > len(chunk) {
			return io.ErrShortWrite
		}
		contents = contents[written:]
	}
	return nil
}

func waitDescriptor(fd, wake int, event int16) error {
	for {
		poll := []unix.PollFd{{Fd: int32(fd), Events: event | unix.POLLHUP | unix.POLLERR},
			{Fd: int32(wake), Events: unix.POLLIN | unix.POLLERR}}
		if _, err := unix.Poll(poll, -1); err != nil {
			if err == unix.EINTR {
				continue
			}
			return err
		}
		if poll[1].Revents != 0 {
			return os.ErrClosed
		}
		if poll[0].Revents != 0 {
			return nil
		}
	}
}

func (processes *processIO) resize(id transfernumber.Number, cols, rows int,
	onFailure func(error),
) error {
	size, err := NewTerminalSize(cols, rows)
	if err != nil || id.Value() == 0 {
		return errors.Join(fmt.Errorf("child PTY resize is invalid"), err)
	}
	processes.controlsMu.RLock()
	index, exists := processes.ptyByTransfer[id]
	if !exists || index < 0 || index >= len(processes.streams) || !processes.acceptingControl {
		processes.controlsMu.RUnlock()
		return fmt.Errorf("transfer %d PTY is unavailable", id.Value())
	}
	err = unix.IoctlSetWinsize(processes.streams[index].fd, unix.TIOCSWINSZ,
		&unix.Winsize{Col: size.cols, Row: size.rows})
	processes.controlsMu.RUnlock()
	if err != nil && processes.controlIsActive() {
		onFailure(fmt.Errorf("resize transfer %d PTY: %w", id.Value(), err))
	}
	return err
}

func (processes *processIO) controlIsActive() bool {
	processes.controlsMu.RLock()
	defer processes.controlsMu.RUnlock()
	return processes.acceptingControl
}

func (processes *processIO) stopControls() {
	processes.controlsMu.Lock()
	defer processes.controlsMu.Unlock()
	if !processes.acceptingControl {
		return
	}
	processes.acceptingControl = false
	wakeDescriptor(processes.controlWake)
}

func (processes *processIO) stopReaders() {
	wakeDescriptor(processes.outputWake)
}

func (processes *processIO) waitReaders() { processes.readerWG.Wait() }

func (processes *processIO) waitInputWriters() { processes.inputWriterWG.Wait() }

func (processes *processIO) close() {
	processes.controlsMu.Lock()
	defer processes.controlsMu.Unlock()
	for _, stream := range processes.streams {
		closeFile(stream.file)
	}
	processes.streams = nil
	clear(processes.ptyByTransfer)
	for _, descriptor := range []*int{&processes.outputWake, &processes.controlWake} {
		if *descriptor >= 0 {
			_ = unix.Close(*descriptor)
			*descriptor = -1
		}
	}
}

func (processes *processIO) closeOutput() { close(processes.output) }

func wakeDescriptor(fd int) {
	if fd < 0 {
		return
	}
	var value [8]byte
	binary.NativeEndian.PutUint64(value[:], 1)
	_, _ = unix.Write(fd, value[:])
}
