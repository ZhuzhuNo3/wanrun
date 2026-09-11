package runcommand

import "github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"

// DisplayMode records the presentation selected after signal subscription.
type DisplayMode uint8

const (
	DisplayPlain DisplayMode = iota + 1
	DisplayInteractive
)

// Result is detached evidence produced only after every live parent resource is released.
type Result struct {
	liveSessionEstablished bool
	displayMode            DisplayMode
	displayModeSelected    bool
	final                  runsupervisor.Final
	finalReceived          bool
	cancellation           runsupervisor.CancelReason
	cancellationReceived   bool
	parentError            error
	logError               error
}

func (result Result) LiveSessionEstablished() bool { return result.liveSessionEstablished }

func (result Result) DisplayMode() (DisplayMode, bool) {
	return result.displayMode, result.displayModeSelected
}

func (result Result) Final() (runsupervisor.Final, bool) {
	return result.final.Clone(), result.finalReceived
}

func (result Result) Cancellation() (runsupervisor.CancelReason, bool) {
	return result.cancellation, result.cancellationReceived
}

func (result Result) ParentError() error { return result.parentError }
func (result Result) LogError() error    { return result.logError }

func newPreparationCancellation(reason runsupervisor.CancelReason,
	mode DisplayMode, modeSelected bool,
) Result {
	return Result{displayMode: mode, displayModeSelected: modeSelected,
		cancellation: reason, cancellationReceived: true}
}
