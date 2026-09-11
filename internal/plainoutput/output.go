package plainoutput

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const (
	progressInterval        = time.Second
	maximumLogicalLineBytes = 64 * 1024
)

// Transfer supplies the stable human prefix for one command.
type Transfer struct {
	number  transfernumber.Number
	localIP netip.Addr
}

func NewTransfer(number transfernumber.Number, localIP netip.Addr) (Transfer, error) {
	if number.Value() == 0 || !localIP.Is4() {
		return Transfer{}, errors.New("plain output transfer is invalid")
	}
	return Transfer{number: number, localIP: localIP}, nil
}

type outputStream struct {
	line            []byte
	progress        []byte
	progressWritten bool
	segmentWritten  bool
	lastProgress    time.Time
	sanitizer       terminalSanitizer
	write           func([]byte) error
}

type streamKey struct {
	transfer transfernumber.Number
	stream   runsupervisor.OutputStream
}

type synchronizedWriter struct {
	mu         sync.Mutex
	writer     io.Writer
	writeErr   error
	finalizing bool
}

// PlainTextOutput converts plain child pipes into prefixed, append-only human output.
type PlainTextOutput struct {
	stdout    *synchronizedWriter
	stderr    *synchronizedWriter
	transfers map[transfernumber.Number]Transfer
	streams   map[streamKey]*outputStream
	closed    bool
}

func New(stdout, stderr io.Writer, transfers []Transfer) (*PlainTextOutput, error) {
	if stdout == nil || stderr == nil || len(transfers) == 0 || len(transfers) > transfernumber.Maximum {
		return nil, errors.New("plain output requires writers and transfers")
	}
	output := &PlainTextOutput{stdout: &synchronizedWriter{writer: stdout},
		stderr: &synchronizedWriter{writer: stderr}, transfers: make(map[transfernumber.Number]Transfer),
		streams: make(map[streamKey]*outputStream, 2*len(transfers))}
	for expected, transfer := range transfers {
		if transfer.number.Value() != uint8(expected+1) || !transfer.localIP.Is4() {
			return nil, errors.New("plain output transfers must be complete and ordered")
		}
		if _, duplicate := output.transfers[transfer.number]; duplicate {
			return nil, errors.New("plain output repeats a transfer")
		}
		output.transfers[transfer.number] = transfer
		for _, stream := range []runsupervisor.OutputStream{runsupervisor.OutputStdout, runsupervisor.OutputStderr} {
			writer := output.stdout
			if stream == runsupervisor.OutputStderr {
				writer = output.stderr
			}
			prefix := fmt.Sprintf("[%d %s] ", transfer.number.Value(), transfer.localIP)
			output.streams[streamKey{transfer: transfer.number, stream: stream}] =
				&outputStream{write: func(line []byte) error { return writer.writeLine(prefix, line) }}
		}
	}
	return output, nil
}

func (output *PlainTextOutput) Begin() error {
	if output == nil || output.closed {
		return errors.New("plain output is closed")
	}
	return output.stdout.writeLine("transferlanes: ", []byte(fmt.Sprintf("starting %d transfers", len(output.transfers))))
}

func (output *PlainTextOutput) Show(event runsupervisor.Event) error {
	if output == nil || output.closed {
		return errors.New("plain output is closed")
	}
	switch event.Kind() {
	case runsupervisor.EventOutput:
		stream := output.streams[streamKey{transfer: event.Transfer(), stream: event.Stream()}]
		if stream == nil {
			return errors.New("plain output received an unknown transfer or non-pipe stream")
		}
		return stream.accept(event.Bytes(), time.Now())
	case runsupervisor.EventStatus:
		status := event.Status()
		if _, exists := output.transfers[status.Transfer]; !exists {
			return errors.New("plain output received status for an unknown transfer")
		}
		if status.State == runsupervisor.TransferExited {
			return output.flushTransfer(status.Transfer)
		}
		return nil
	default:
		return errors.New("plain output received an unsupported event")
	}
}

// Close flushes the latest unfinished line or carriage-return progress for every stream.
func (output *PlainTextOutput) Close() error {
	if output == nil || output.closed {
		return nil
	}
	output.closed = true
	output.stdout.beginFinalization()
	output.stderr.beginFinalization()
	var failures []error
	for number := 1; number <= len(output.transfers); number++ {
		id, _ := transfernumber.New(number)
		failures = append(failures, output.flushTransfer(id))
	}
	return errors.Join(failures...)
}

func (output *PlainTextOutput) flushTransfer(id transfernumber.Number) error {
	var failures []error
	for _, kind := range []runsupervisor.OutputStream{runsupervisor.OutputStdout, runsupervisor.OutputStderr} {
		stream := output.streams[streamKey{transfer: id, stream: kind}]
		if stream != nil {
			failures = append(failures, stream.flush())
		}
	}
	return errors.Join(failures...)
}

func (stream *outputStream) accept(content []byte, now time.Time) error {
	for _, value := range content {
		visible := stream.sanitizer.accept(value)
		if err := stream.acceptSanitized(visible.bytes[:visible.length], now); err != nil {
			return err
		}
	}
	return nil
}

func (stream *outputStream) acceptSanitized(content []byte, now time.Time) error {
	for len(content) != 0 {
		if content[0] < utf8.RuneSelf {
			if err := stream.acceptVisible(content[0], now); err != nil {
				return err
			}
			content = content[1:]
			continue
		}
		_, width := utf8.DecodeRune(content)
		if width == 1 {
			return errors.New("plain output sanitizer emitted invalid UTF-8")
		}
		if err := stream.appendVisibleBytes(content[:width]); err != nil {
			return err
		}
		content = content[width:]
	}
	return nil
}

func (stream *outputStream) acceptVisible(value byte, now time.Time) error {
	switch value {
	case '\n':
		return stream.finishLine()
	case '\r':
		return stream.progressUpdate(now)
	case '\t':
		return stream.appendVisible(value)
	default:
		if value >= 0x20 && value != 0x7f {
			return stream.appendVisible(value)
		}
	}
	return nil
}

func (stream *outputStream) appendVisible(value byte) error {
	return stream.appendVisibleBytes([]byte{value})
}

func (stream *outputStream) appendVisibleBytes(content []byte) error {
	if len(stream.line) != 0 && len(stream.line)+len(content) > maximumLogicalLineBytes {
		if err := stream.writeBufferedSegment(); err != nil {
			return err
		}
	}
	stream.line = append(stream.line, content...)
	if len(stream.line) < maximumLogicalLineBytes {
		return nil
	}
	return stream.writeBufferedSegment()
}

func (stream *outputStream) writeBufferedSegment() error {
	if err := stream.write(stream.line); err != nil {
		return err
	}
	stream.line = stream.line[:0]
	stream.progress = stream.progress[:0]
	stream.progressWritten = false
	stream.segmentWritten = true
	return nil
}

func (stream *outputStream) progressUpdate(now time.Time) error {
	if len(stream.line) == 0 {
		return stream.acceptCarriageReturnWithoutLine(now)
	}
	stream.progress = append(stream.progress[:0], stream.line...)
	stream.line = stream.line[:0]
	stream.progressWritten = false
	if !stream.lastProgress.IsZero() && now.Sub(stream.lastProgress) < progressInterval {
		return nil
	}
	if err := stream.write(stream.progress); err != nil {
		return err
	}
	stream.progressWritten = true
	stream.lastProgress = now
	return nil
}

func (stream *outputStream) acceptCarriageReturnWithoutLine(now time.Time) error {
	if len(stream.progress) != 0 || stream.progressWritten {
		return nil
	}
	if stream.segmentWritten {
		stream.resetLogicalLine()
		stream.progressWritten = true
		stream.lastProgress = now
		return nil
	}
	if err := stream.write(nil); err != nil {
		return err
	}
	stream.progressWritten = true
	stream.lastProgress = now
	return nil
}

func (stream *outputStream) finishLine() error {
	if len(stream.line) == 0 && stream.progressWritten {
		stream.resetLogicalLine()
		return nil
	}
	if len(stream.line) == 0 && len(stream.progress) == 0 && stream.segmentWritten {
		stream.resetLogicalLine()
		return nil
	}
	line := stream.line
	if len(line) == 0 && len(stream.progress) != 0 {
		line = stream.progress
	}
	if err := stream.write(line); err != nil {
		return err
	}
	stream.resetLogicalLine()
	return nil
}

func (stream *outputStream) flush() error {
	visible := stream.sanitizer.finish()
	if err := stream.acceptSanitized(visible.bytes[:visible.length], time.Time{}); err != nil {
		return err
	}
	if len(stream.line) != 0 {
		return stream.finishLine()
	}
	if len(stream.progress) != 0 && !stream.progressWritten {
		return stream.finishLine()
	}
	stream.resetLogicalLine()
	return nil
}

func (stream *outputStream) resetLogicalLine() {
	stream.line = stream.line[:0]
	stream.progress = stream.progress[:0]
	stream.progressWritten = false
	stream.segmentWritten = false
	stream.lastProgress = time.Time{}
}

func (writer *synchronizedWriter) writeLine(prefix string, line []byte) error {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.writeErr != nil {
		if writer.finalizing {
			return nil
		}
		return writer.writeErr
	}
	contents := make([]byte, 0, len(prefix)+len(line)+1)
	contents = append(contents, prefix...)
	contents = append(contents, bytes.ToValidUTF8(line, []byte("\xef\xbf\xbd"))...)
	contents = append(contents, '\n')
	writer.writeErr = writeFull(writer.writer, contents)
	return writer.writeErr
}

func (writer *synchronizedWriter) beginFinalization() {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	writer.finalizing = true
}

func writeFull(writer io.Writer, content []byte) error {
	for len(content) != 0 {
		written, err := writer.Write(content)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(content) {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return nil
}
