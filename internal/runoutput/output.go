package runoutput

import (
	"errors"
	"fmt"

	"github.com/ZhuzhuNo3/transferlanes/internal/runlogs"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type eventSource interface {
	Next() (runsupervisor.Event, error)
}

type rawLogWriter interface {
	Write(transfernumber.Number, runlogs.Stream, []byte) error
}

// Display receives presentation-neutral events after their raw bytes are durable in enabled logs.
type Display interface {
	Begin() error
	Show(runsupervisor.Event) error
}

// Result is the immutable output evidence published once by the event forwarder.
type Result struct {
	final                 runsupervisor.Final
	finalReceived         bool
	displayEventsComplete bool
	readError             error
	logError              error
	displayError          error
}

func (result Result) Final() (runsupervisor.Final, bool) {
	return result.final.Clone(), result.finalReceived
}
func (result Result) DisplayEventsComplete() bool { return result.displayEventsComplete }
func (result Result) ReadError() error            { return result.readError }
func (result Result) LogError() error             { return result.logError }
func (result Result) DisplayError() error         { return result.displayError }
func (result Result) Failure() error {
	return errors.Join(result.readError, result.logError, result.displayError)
}

// Completion is a one-shot future. Result is complete before Done closes.
type Completion struct {
	done   chan struct{}
	result Result
}

func (completion *Completion) Done() <-chan struct{} {
	if completion == nil {
		return nil
	}
	return completion.done
}

// Result returns detached completion evidence after Done has closed.
func (completion *Completion) Result() Result {
	if completion == nil {
		return Result{readError: errors.New("run output completion is absent")}
	}
	<-completion.done
	result := completion.result
	result.final = result.final.Clone()
	return result
}

// Start launches the sole reader of one supervisor event stream.
func Start(source eventSource, logs rawLogWriter, display Display, requestTermination func()) *Completion {
	completion := &Completion{done: make(chan struct{})}
	go completion.forward(source, logs, display, requestTermination)
	return completion
}

func (completion *Completion) forward(source eventSource, logs rawLogWriter, display Display,
	requestTermination func(),
) {
	var result Result
	defer func() {
		completion.result = result
		close(completion.done)
	}()
	if source == nil || display == nil {
		result.readError = errors.New("run output requires an event source and one display")
		notify(requestTermination)
		return
	}
	notified := false
	requestStop := func() {
		if notified {
			return
		}
		notified = true
		notify(requestTermination)
	}
	displayActive := true
	displayStarted := false
	logsActive := logs != nil
	for {
		event, err := source.Next()
		if err != nil {
			result.readError = fmt.Errorf("read supervisor output before final: %w", err)
			requestStop()
			return
		}
		if !displayStarted {
			displayStarted = true
			if err := display.Begin(); err != nil {
				result.displayError = errors.Join(errors.New("begin run display"), err)
				displayActive = false
				requestStop()
			}
		}
		if event.Kind() == runsupervisor.EventFinal {
			result.final = event.Final()
			result.finalReceived = true
			result.displayEventsComplete = displayActive
			return
		}
		if event.Kind() == runsupervisor.EventOutput && logsActive {
			if err := logs.Write(event.Transfer(), rawStream(event.Stream()), event.Bytes()); err != nil {
				result.logError = errors.Join(result.logError,
					fmt.Errorf("write transfer %d raw log: %w", event.Transfer().Value(), err))
				logsActive, displayActive = false, false
				requestStop()
				continue
			}
		}
		if displayActive {
			if err := display.Show(event); err != nil {
				result.displayError = errors.Join(result.displayError, fmt.Errorf("show run output: %w", err))
				displayActive = false
				requestStop()
			}
		}
	}
}

func notify(request func()) {
	if request != nil {
		request()
	}
}

func rawStream(stream runsupervisor.OutputStream) runlogs.Stream {
	switch stream {
	case runsupervisor.OutputPTY:
		return runlogs.PTY
	case runsupervisor.OutputStdout:
		return runlogs.Stdout
	case runsupervisor.OutputStderr:
		return runlogs.Stderr
	default:
		return 0
	}
}
