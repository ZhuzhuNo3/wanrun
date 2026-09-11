package runcommand

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/runlogs"
	"github.com/ZhuzhuNo3/transferlanes/internal/runoutput"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type sessionClientRecorder struct {
	events         chan runsupervisor.Event
	nextStarted    chan struct{}
	cancelAccepted chan runsupervisor.CancelReason
	lifelineClosed chan struct{}
	waitFinished   chan struct{}
	cancelErr      error
	closeErr       error
	waitErr        error
	lifecycle      *sessionLifecycleRecorder
	nextOnce       sync.Once
	lifelineOnce   sync.Once
	waitOnce       sync.Once
	mu             sync.Mutex
	cancels        []runsupervisor.CancelReason
	closeCalls     int
	waitCalls      int
}

func newSessionClientRecorder() *sessionClientRecorder {
	return &sessionClientRecorder{
		events:         make(chan runsupervisor.Event, 8),
		nextStarted:    make(chan struct{}),
		cancelAccepted: make(chan runsupervisor.CancelReason, 4),
		lifelineClosed: make(chan struct{}),
		waitFinished:   make(chan struct{}),
	}
}

func (client *sessionClientRecorder) Next() (runsupervisor.Event, error) {
	client.nextOnce.Do(func() { close(client.nextStarted) })
	event, open := <-client.events
	if !open {
		return runsupervisor.Event{}, io.EOF
	}
	return event, nil
}

func (client *sessionClientRecorder) Cancel(reason runsupervisor.CancelReason) error {
	client.mu.Lock()
	client.cancels = append(client.cancels, reason)
	err := client.cancelErr
	client.mu.Unlock()
	client.cancelAccepted <- reason
	return err
}

func (*sessionClientRecorder) Resize(transfernumber.Number, uint16, uint16) error { return nil }
func (*sessionClientRecorder) WriteInput(transfernumber.Number, []byte) error     { return nil }

func (client *sessionClientRecorder) CloseLifeline() error {
	client.mu.Lock()
	client.closeCalls++
	err := client.closeErr
	client.mu.Unlock()
	if client.lifecycle != nil {
		client.lifecycle.Record("lifeline")
	}
	client.lifelineOnce.Do(func() { close(client.lifelineClosed) })
	return err
}

func (client *sessionClientRecorder) Wait() error {
	client.mu.Lock()
	client.waitCalls++
	err := client.waitErr
	client.mu.Unlock()
	if client.lifecycle != nil {
		client.lifecycle.Record("wait")
	}
	client.waitOnce.Do(func() { close(client.waitFinished) })
	return err
}

func (client *sessionClientRecorder) actions() ([]runsupervisor.CancelReason, int, int) {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]runsupervisor.CancelReason(nil), client.cancels...), client.closeCalls, client.waitCalls
}

type staticSessionDisplay struct {
	beginErr error
	showErr  error
}

func (display *staticSessionDisplay) Begin() error                   { return display.beginErr }
func (display *staticSessionDisplay) Show(runsupervisor.Event) error { return display.showErr }

type blockingBeginDisplay struct{ phase *sessionPhaseGate }

func newBlockingBeginDisplay() *blockingBeginDisplay {
	return &blockingBeginDisplay{phase: newSessionPhaseGate()}
}

func (display *blockingBeginDisplay) Begin() error {
	display.phase.Block()
	return nil
}

func (*blockingBeginDisplay) Show(runsupervisor.Event) error { return nil }

type completionSessionDriver struct {
	displayEndpoint runoutput.Display
	doneCh          chan struct{}
	finish          chan struct{}
	projection      *sessionPhaseGate
	lifecycle       *sessionLifecycleRecorder
	err             error
	mu              sync.Mutex
	stopCalls       int
}

func newCompletionSessionDriver(display runoutput.Display) *completionSessionDriver {
	return &completionSessionDriver{
		displayEndpoint: display,
		doneCh:          make(chan struct{}),
		finish:          make(chan struct{}),
	}
}

func (*completionSessionDriver) mode() DisplayMode                 { return DisplayPlain }
func (driver *completionSessionDriver) display() runoutput.Display { return driver.displayEndpoint }
func (*completionSessionDriver) commandIO() (int, int, bool)       { return 0, 0, false }
func (*completionSessionDriver) abort() error                      { return nil }
func (driver *completionSessionDriver) done() <-chan struct{}      { return driver.doneCh }
func (driver *completionSessionDriver) result() error              { return driver.err }

func (driver *completionSessionDriver) stopCommands() {
	driver.mu.Lock()
	driver.stopCalls++
	driver.mu.Unlock()
}

func (driver *completionSessionDriver) start(_ commandControl, outputDone <-chan struct{}, _ <-chan struct{},
	_ *terminationInbox,
) {
	go func() {
		<-outputDone
		<-driver.finish
		if driver.lifecycle != nil {
			driver.lifecycle.Record("driver")
		}
		close(driver.doneCh)
	}()
}

func (driver *completionSessionDriver) complete(finalSnapshotProjection) {
	if driver.lifecycle != nil {
		driver.lifecycle.Record("projection")
	}
	if driver.projection != nil {
		driver.projection.Block()
	}
	close(driver.finish)
}

type outputJoinedSessionDriver struct{ *completionSessionDriver }

func (driver *outputJoinedSessionDriver) start(control commandControl, outputDone <-chan struct{},
	resize <-chan struct{}, inbox *terminationInbox,
) {
	<-outputDone
	driver.completionSessionDriver.start(control, outputDone, resize, inbox)
}

type sessionLogRecorder struct {
	writePhase *sessionPhaseGate
	writeErr   error
	closeErr   error
	lifecycle  *sessionLifecycleRecorder
	mu         sync.Mutex
	closed     bool
}

func (logs *sessionLogRecorder) Write(transfernumber.Number, runlogs.Stream, []byte) error {
	if logs.writePhase != nil {
		logs.writePhase.Block()
	}
	return logs.writeErr
}

func (logs *sessionLogRecorder) Close() error {
	logs.mu.Lock()
	logs.closed = true
	logs.mu.Unlock()
	if logs.lifecycle != nil {
		logs.lifecycle.Record("logs")
	}
	return logs.closeErr
}

func (logs *sessionLogRecorder) isClosed() bool {
	logs.mu.Lock()
	defer logs.mu.Unlock()
	return logs.closed
}

type sessionSignalRecorder struct {
	resize    chan struct{}
	lifecycle *sessionLifecycleRecorder
}

func (signals *sessionSignalRecorder) resizeEvents() <-chan struct{} { return signals.resize }
func (signals *sessionSignalRecorder) stopAndJoin() {
	if signals.lifecycle != nil {
		signals.lifecycle.Record("signals")
	}
}

type sessionPhaseGate struct {
	reached     chan struct{}
	release     chan struct{}
	reachedOnce sync.Once
	releaseOnce sync.Once
}

func newSessionPhaseGate() *sessionPhaseGate {
	return &sessionPhaseGate{reached: make(chan struct{}), release: make(chan struct{})}
}

func (phase *sessionPhaseGate) Block() {
	phase.reachedOnce.Do(func() { close(phase.reached) })
	<-phase.release
}

func (phase *sessionPhaseGate) Release() {
	phase.releaseOnce.Do(func() { close(phase.release) })
}

type sessionLifecycleRecorder struct {
	mu     sync.Mutex
	events []string
}

func (lifecycle *sessionLifecycleRecorder) Record(event string) {
	lifecycle.mu.Lock()
	lifecycle.events = append(lifecycle.events, event)
	lifecycle.mu.Unlock()
}

func (lifecycle *sessionLifecycleRecorder) Events() []string {
	lifecycle.mu.Lock()
	defer lifecycle.mu.Unlock()
	return append([]string(nil), lifecycle.events...)
}

func waitForSessionPhase(t *testing.T, phase <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-phase:
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for session phase %q", name)
	}
}

func waitForSessionCancel(t *testing.T, accepted <-chan runsupervisor.CancelReason, name string) runsupervisor.CancelReason {
	t.Helper()
	select {
	case reason := <-accepted:
		return reason
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for session cancellation %q", name)
		return 0
	}
}

func waitForSessionResult(t *testing.T, result <-chan Result, name string) Result {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(time.Second):
		t.Fatalf("timed out joining session %q", name)
		return Result{}
	}
}

func countErrorOccurrence(err, target error) int {
	if err == nil {
		return 0
	}
	if err == target {
		return 1
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		count := 0
		for _, child := range joined.Unwrap() {
			count += countErrorOccurrence(child, target)
		}
		return count
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return countErrorOccurrence(wrapped.Unwrap(), target)
	}
	return 0
}
