package runcommand

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runlogs"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/terminal"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"github.com/ZhuzhuNo3/transferlanes/internal/transferstart"
)

type sourceOwner interface {
	Close() error
}

type sourceOpener interface {
	Open(context.Context, string) (sourceOwner, error)
}

type logOpener interface {
	Open(context.Context, sourceOwner, string, DisplayMode, []transfernumber.Number) (runLogOwner, error)
}

type supervisorLauncher interface {
	Launch(context.Context, []byte, sourceOwner) (supervisorClient, error)
}

// Command establishes and owns one supervised run until all parent resources are released.
type Command struct {
	sources  sourceOpener
	logs     logOpener
	launcher supervisorLauncher
}

// New constructs the production run use case.
func New() *Command {
	return &Command{sources: operatingSystemSourceOpener{}, logs: operatingSystemLogOpener{},
		launcher: operatingSystemSupervisorLauncher{}}
}

func newCommand(sources sourceOpener, logs logOpener, launcher supervisorLauncher) *Command {
	return &Command{sources: sources, logs: logs, launcher: launcher}
}

// Run performs preparation, transfers live ownership once, and returns only after cleanup.
func (command *Command) Run(ctx context.Context, request Request, stdin io.Reader,
	stdout, stderr io.Writer,
) (Result, error) {
	if ctx == nil || command == nil || command.sources == nil || command.logs == nil || command.launcher == nil {
		return Result{}, errors.New("run command dependencies are incomplete")
	}
	if err := requireEffectiveRoot(); err != nil {
		return Result{}, err
	}
	inbox := newTerminationInbox()
	watcher := watchParentSignals(ctx, inbox)
	mode, modeSelected := DisplayMode(0), false
	if interrupted := observePreparationInterruption(ctx, watcher); interrupted {
		return finishPreparation(ctx, watcher, mode, modeSelected, nil, preparationCleanup{})
	}
	driver, err := prepareDisplay(request, stdin, stdout, stderr)
	if err != nil {
		watcher.close()
		return Result{}, fmt.Errorf("prepare run display: %w", err)
	}
	mode, modeSelected = driver.mode(), true
	if interrupted := observePreparationInterruption(ctx, watcher); interrupted {
		return finishPreparation(ctx, watcher, mode, modeSelected, nil,
			releasePreparation(nil, nil, driver))
	}
	source, err := command.sources.Open(watcher.preparation, request.SourcePath())
	if err != nil {
		return finishPreparation(ctx, watcher, mode, modeSelected,
			fmt.Errorf("open transfer source: %w", err),
			releasePreparation(nil, nil, driver))
	}
	if source == nil {
		return finishPreparation(ctx, watcher, mode, modeSelected,
			errors.New("open transfer source: source receipt is absent"),
			releasePreparation(nil, nil, driver))
	}
	if interrupted := observePreparationInterruption(ctx, watcher); interrupted {
		return finishPreparation(ctx, watcher, mode, modeSelected, nil,
			releasePreparation(nil, source, driver))
	}
	logs, err := command.logs.Open(watcher.preparation, source, request.LogDirectory(), mode,
		transferNumbers(request))
	if err != nil {
		return finishPreparation(ctx, watcher, mode, modeSelected,
			fmt.Errorf("open transfer logs: %w", err),
			releasePreparation(nil, source, driver))
	}
	if interrupted := observePreparationInterruption(ctx, watcher); interrupted {
		return finishPreparation(ctx, watcher, mode, modeSelected, nil,
			releasePreparation(logs, source, driver))
	}
	payload, err := encodeStartRequest(request, driver)
	if err != nil {
		return finishPreparation(ctx, watcher, mode, modeSelected,
			fmt.Errorf("encode supervised transfer request: %w", err),
			releasePreparation(logs, source, driver))
	}
	client, err := command.launcher.Launch(watcher.preparation, payload, source)
	source = nil // Launch consumes the source receipt on both success and failure.
	if err != nil {
		return finishPreparation(ctx, watcher, mode, modeSelected,
			fmt.Errorf("launch transfer supervisor: %w", err),
			releasePreparation(logs, nil, driver))
	}
	if client == nil {
		return finishPreparation(ctx, watcher, mode, modeSelected,
			errors.New("launch transfer supervisor: client receipt is absent"),
			releasePreparation(logs, nil, driver))
	}
	return (&runSession{ctx: ctx, client: client, logs: logs, driver: driver,
		signals: watcher, inbox: inbox}).run(), nil
}

type preparationCleanup struct {
	parent error
	log    error
}

func (cleanup preparationCleanup) failure() error {
	return errors.Join(cleanup.parent, cleanup.log)
}

func releasePreparation(logs runLogOwner, source sourceOwner, driver displayDriver) preparationCleanup {
	cleanup := preparationCleanup{log: wrapLogClose(closeRunLogs(logs))}
	if source != nil {
		cleanup.parent = errors.Join(cleanup.parent, wrapSourceClose(source.Close()))
	}
	if driver != nil {
		cleanup.parent = errors.Join(cleanup.parent, wrapDisplayAbort(driver.abort()))
	}
	return cleanup
}

func observePreparationInterruption(ctx context.Context, watcher *parentSignalWatcher) bool {
	select {
	case <-ctx.Done():
		return true
	default:
	}
	select {
	case <-watcher.preparation.Done():
		return true
	default:
		return false
	}
}

func finishPreparation(ctx context.Context, watcher *parentSignalWatcher, mode DisplayMode,
	modeSelected bool, failure error, cleanup preparationCleanup,
) (Result, error) {
	callerCause := context.Cause(ctx)
	signalReason, signalled := watcher.signalReason()
	preparationCause := context.Cause(watcher.preparation)
	watcher.close()

	if callerCause != nil {
		if failure == nil || !errors.Is(failure, callerCause) {
			failure = errors.Join(callerCause, failure)
		}
		return Result{}, errors.Join(failure, cleanup.failure())
	}
	if signalled && cancellationOnly(failure, preparationCause) {
		result := newPreparationCancellation(signalReason, mode, modeSelected)
		result.parentError = cleanup.parent
		result.logError = cleanup.log
		return result, nil
	}
	if failure == nil {
		failure = preparationCause
	}
	return Result{}, errors.Join(failure, cleanup.failure())
}

// cancellationOnly reports whether every leaf in failure is the preparation cause. Wrappers
// add context but not a second failure; multi-error nodes retain every independently owned leaf.
func cancellationOnly(failure, cause error) bool {
	if failure == nil || cause == nil {
		return failure == nil
	}
	if joined, ok := failure.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return errors.Is(failure, cause)
		}
		for _, child := range children {
			if !cancellationOnly(child, cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := failure.(interface{ Unwrap() error }); ok {
		if child := wrapped.Unwrap(); child != nil {
			return cancellationOnly(child, cause)
		}
	}
	return errors.Is(failure, cause)
}

func wrapSourceClose(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close transfer source: %w", err)
}

func wrapDisplayAbort(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("abort run display: %w", err)
}

func prepareDisplay(request Request, stdin io.Reader, stdout, stderr io.Writer) (displayDriver, error) {
	if request.Display().NoTUI() {
		return newPlainDisplayDriver(request, stdout, stderr)
	}
	columns, rows, err := terminal.CurrentSize(stdin, stdout)
	if err != nil {
		return newPlainDisplayDriver(request, stdout, stderr)
	}
	return newInteractiveDisplayDriver(request, stdin, stdout, columns, rows)
}

func encodeStartRequest(request Request, driver displayDriver) ([]byte, error) {
	commandIO := transferstart.Pipes()
	if columns, rows, interactive := driver.commandIO(); interactive {
		var err error
		commandIO, err = transferstart.NewPTY(columns, rows)
		if err != nil {
			return nil, err
		}
	}
	start, err := NewStartRequest(request, commandIO)
	if err != nil {
		return nil, err
	}
	return transferstart.Encode(start)
}

func transferNumbers(request Request) []transfernumber.Number {
	networks := displayNetworks(request)
	result := make([]transfernumber.Number, len(networks))
	for index, network := range networks {
		result[index] = network.number
	}
	return result
}

func closeRunLogs(logs runLogOwner) error {
	if logs == nil {
		return nil
	}
	return logs.Close()
}

type operatingSystemSourceOpener struct{}

func (operatingSystemSourceOpener) Open(ctx context.Context, path string) (sourceOwner, error) {
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	source, err := sourcefiles.OpenSourceRoot(path)
	if err != nil {
		return nil, err
	}
	if err := context.Cause(ctx); err != nil {
		return nil, errors.Join(err, source.Close())
	}
	return source, nil
}

type operatingSystemLogOpener struct{}

func (operatingSystemLogOpener) Open(ctx context.Context, source sourceOwner, path string,
	mode DisplayMode, transfers []transfernumber.Number,
) (runLogOwner, error) {
	if path == "" {
		return nil, nil
	}
	concrete, ok := source.(*sourcefiles.SourceRoot)
	if !ok {
		return nil, errors.New("raw logs require an operating-system source")
	}
	recovery, err := rundirectory.New().OpenRecoveryRoot()
	if err != nil {
		return nil, fmt.Errorf("open recovery boundary for raw logs: %w", err)
	}
	layout := runlogs.Plain
	if mode == DisplayInteractive {
		layout = runlogs.Interactive
	}
	logs, openErr := runlogs.Open(ctx, concrete, recovery, path, layout, transfers)
	recoveryCloseErr := recovery.Close()
	if openErr != nil {
		return nil, errors.Join(openErr, recoveryCloseErr)
	}
	if recoveryCloseErr != nil {
		return nil, errors.Join(recoveryCloseErr, logs.Close())
	}
	return logs, nil
}

type operatingSystemSupervisorLauncher struct{}

func (operatingSystemSupervisorLauncher) Launch(ctx context.Context, payload []byte,
	source sourceOwner,
) (supervisorClient, error) {
	concrete, ok := source.(*sourcefiles.SourceRoot)
	if !ok {
		return nil, errors.Join(errors.New("supervisor requires an operating-system source"), source.Close())
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("resolve transferlanes executable: %w", err), concrete.Close())
	}
	return runsupervisor.Launch(ctx, executable, payload, concrete)
}
