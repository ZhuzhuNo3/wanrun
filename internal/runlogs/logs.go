package runlogs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

// Layout selects the raw files needed by one already-decided display mode.
type Layout uint8

const (
	Interactive Layout = iota + 1
	Plain
)

type logKey struct {
	transfer transfernumber.Number
	stream   Stream
}

type rawLog struct {
	mu       sync.Mutex
	file     *os.File
	closed   bool
	writeErr error
	closeErr error
}

// RunLogSet owns the complete explicitly requested raw-log collection for one run.
type RunLogSet struct {
	mu     sync.Mutex
	files  map[logKey]*rawLog
	closed bool
}

// Open validates the source/recovery relationship and opens every mode-specific log before a
// supervisor can start. An empty path means logging is disabled and returns no owner.
func Open(ctx context.Context, source *sourcefiles.SourceRoot, recovery *rundirectory.RecoveryRoot,
	path string, layout Layout, transfers []transfernumber.Number,
) (*RunLogSet, error) {
	if path == "" {
		return nil, nil
	}
	if ctx == nil || source == nil || recovery == nil {
		return nil, errors.New("run logs require context, source, and recovery boundaries")
	}
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	streams, err := streamsFor(layout)
	if err != nil {
		return nil, err
	}
	if err := validateTransfers(transfers); err != nil {
		return nil, err
	}
	directory, err := openSafeLogDirectory(source, recovery, path)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	set := &RunLogSet{files: make(map[logKey]*rawLog, len(transfers)*len(streams))}
	for _, transfer := range transfers {
		for _, stream := range streams {
			if err := contextFailure(ctx); err != nil {
				closeErr := set.closeFiles()
				if closeErr != nil {
					return nil, errors.Join(err, closeErr)
				}
				return nil, err
			}
			name := logName(transfer, stream)
			file, err := createRawLog(directory, name)
			if err != nil {
				return nil, errors.Join(err, set.closeFiles())
			}
			set.files[logKey{transfer: transfer, stream: stream}] = &rawLog{file: file}
		}
	}
	if err := directory.Sync(); err != nil {
		return nil, errors.Join(fmt.Errorf("sync raw log directory: %w", err), set.closeFiles())
	}
	return set, nil
}

func streamsFor(layout Layout) ([]Stream, error) {
	switch layout {
	case Interactive:
		return []Stream{PTY}, nil
	case Plain:
		return []Stream{Stdout, Stderr}, nil
	default:
		return nil, errors.New("run log layout is invalid")
	}
}

func validateTransfers(transfers []transfernumber.Number) error {
	if len(transfers) == 0 || len(transfers) > transfernumber.Maximum {
		return errors.New("run logs require a complete transfer set")
	}
	for index, transfer := range transfers {
		if transfer.Value() != uint8(index+1) {
			return errors.New("run log transfers must be complete and ordered")
		}
	}
	return nil
}

func logName(transfer transfernumber.Number, stream Stream) string {
	switch stream {
	case PTY:
		return fmt.Sprintf("transfer-%02d.ptylog", transfer.Value())
	case Stdout:
		return fmt.Sprintf("transfer-%02d.stdout.log", transfer.Value())
	case Stderr:
		return fmt.Sprintf("transfer-%02d.stderr.log", transfer.Value())
	default:
		return ""
	}
}

// Write appends byte-exact child output to its already-open transfer/stream file.
func (logs *RunLogSet) Write(transfer transfernumber.Number, stream Stream, content []byte) error {
	if logs == nil {
		return nil
	}
	logs.mu.Lock()
	if logs.closed {
		logs.mu.Unlock()
		return errors.New("run logs are closed")
	}
	log := logs.files[logKey{transfer: transfer, stream: stream}]
	logs.mu.Unlock()
	if log == nil {
		return errors.New("raw log transfer or stream is not open")
	}
	return log.write(content)
}

// Close syncs and closes every raw log in stable transfer/stream order.
func (logs *RunLogSet) Close() error {
	if logs == nil {
		return nil
	}
	logs.mu.Lock()
	if logs.closed {
		logs.mu.Unlock()
		return nil
	}
	logs.closed = true
	logs.mu.Unlock()
	return logs.closeFiles()
}

func (logs *RunLogSet) closeFiles() error {
	keys := make([]logKey, 0, len(logs.files))
	for key := range logs.files {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		if keys[left].transfer != keys[right].transfer {
			return keys[left].transfer.Value() < keys[right].transfer.Value()
		}
		return keys[left].stream < keys[right].stream
	})
	var failures []error
	for _, key := range keys {
		failures = append(failures, logs.files[key].close())
	}
	return errors.Join(failures...)
}

func (log *rawLog) write(content []byte) error {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.closed {
		return errors.New("raw log is closed")
	}
	if log.writeErr != nil {
		return log.writeErr
	}
	log.writeErr = writeAll(log.file, content)
	return log.writeErr
}

func (log *rawLog) close() error {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.closed {
		return log.closeErr
	}
	log.closed = true
	log.closeErr = errors.Join(wrapLogError("sync", log.file.Sync()),
		wrapLogError("close", log.file.Close()))
	return log.closeErr
}

func writeAll(writer io.Writer, content []byte) error {
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

func wrapLogError(action string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s raw log: %w", action, err)
}

func contextFailure(ctx context.Context) error {
	select {
	case <-ctx.Done():
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return ctx.Err()
	default:
		return nil
	}
}
