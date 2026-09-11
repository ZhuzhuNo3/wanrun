package runsupervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const maximumPendingControls = 32

// Work runs the per-run owner orchestration inside the supervisor process.
type Work func(context.Context, *RunSupervisor) Final

// RunSupervisor provides only the fixed controls and outbound run events.
type RunSupervisor struct {
	controls <-chan Control
	events   *eventPipeSender
	request  []byte
	sourceMu sync.Mutex
	source   *os.File
}

func (run *RunSupervisor) Controls() <-chan Control { return run.controls }

// Request returns the immutable private start payload supplied by the parent handshake.
func (run *RunSupervisor) Request() []byte { return append([]byte(nil), run.request...) }

// TakeSourceRoot transfers the exact inherited source capability once. The caller becomes its
// sole owner and must close it after all source-dependent work.
func (run *RunSupervisor) TakeSourceRoot(label string) (*sourcefiles.SourceRoot, error) {
	if run == nil {
		return nil, errors.New("supervisor source directory is absent")
	}
	run.sourceMu.Lock()
	source := run.source
	run.source = nil
	run.sourceMu.Unlock()
	if source == nil {
		return nil, errors.New("supervisor source directory is absent")
	}
	return sourcefiles.TakeSourceRoot(source, label)
}

func (run *RunSupervisor) closeUntakenSource() error {
	if run == nil {
		return nil
	}
	run.sourceMu.Lock()
	source := run.source
	run.source = nil
	run.sourceMu.Unlock()
	if source == nil {
		return nil
	}
	return source.Close()
}

// SendOutput publishes one byte-exact command chunk without presentation formatting.
func (run *RunSupervisor) SendOutput(id transfernumber.Number, stream OutputStream, contents []byte) error {
	return run.events.sendOutput(id, stream, contents)
}

func (run *RunSupervisor) SendStatus(status TransferStatus) error {
	payload, err := encodeStatus(status)
	if err != nil {
		return err
	}
	return run.events.send(frameStatus, payload)
}

func validateStartRequest(request []byte) error { return ValidateStartRequest(request) }

// ValidateStartRequest applies the exact bound used by both ends of the private supervisor pipe.
func ValidateStartRequest(request []byte) error { return ValidateStartRequestSize(len(request)) }

// ValidateStartRequestSize applies the carrier bound without requiring encoded bytes.
func ValidateStartRequestSize(size int) error {
	if size < 0 || size > maximumWirePayload {
		return fmt.Errorf("supervisor start request is invalid")
	}
	return nil
}

// StartRequestCapacity returns the maximum encoded private start payload size.
func StartRequestCapacity() int { return maximumWirePayload }

func callWork(ctx context.Context, run *RunSupervisor, work Work) (final Final) {
	defer func() {
		if recovered := recover(); recovered != nil {
			final = Final{Cancelled: true, Reason: CancelInternal,
				InternalError: boundedDiagnostic(fmt.Sprintf("supervisor work panic: %v", recovered))}
		}
	}()
	return work(ctx, run)
}

var (
	ErrLifelineClosed = errors.New("supervisor lifeline closed")
	ErrCancelled      = errors.New("supervisor cancelled")
)
