package runcommand

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/runlogs"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type preparationSource struct {
	closes   int
	closeErr error
}

func (source *preparationSource) Close() error {
	source.closes++
	return source.closeErr
}

type preparationLogs struct {
	closes   int
	closeErr error
}

func (*preparationLogs) Write(transfernumber.Number, runlogs.Stream, []byte) error { return nil }
func (logs *preparationLogs) Close() error {
	logs.closes++
	return logs.closeErr
}

type preparationSourceOpener struct {
	entered     chan struct{}
	block       bool
	independent error
	joinCause   bool
	release     <-chan struct{}
	cancelled   chan<- struct{}
	source      *preparationSource
}

func (opener *preparationSourceOpener) Open(ctx context.Context, _ string) (sourceOwner, error) {
	if opener.entered != nil {
		close(opener.entered)
	}
	if opener.block {
		<-ctx.Done()
		if opener.cancelled != nil {
			close(opener.cancelled)
		}
		if opener.release != nil {
			<-opener.release
		}
		if opener.independent != nil {
			if opener.joinCause {
				return nil, errors.Join(context.Cause(ctx), opener.independent)
			}
			return nil, opener.independent
		}
		return nil, context.Cause(ctx)
	}
	return opener.source, nil
}

type preparationLogOpener struct {
	entered     chan struct{}
	block       bool
	independent error
	joinCause   bool
	release     <-chan struct{}
	cancelled   chan<- struct{}
	logs        *preparationLogs
}

func (opener *preparationLogOpener) Open(ctx context.Context, _ sourceOwner, _ string,
	_ DisplayMode, _ []transfernumber.Number,
) (runLogOwner, error) {
	if opener.entered != nil {
		close(opener.entered)
	}
	if opener.block {
		<-ctx.Done()
		if opener.cancelled != nil {
			close(opener.cancelled)
		}
		if opener.release != nil {
			<-opener.release
		}
		if opener.independent != nil {
			if opener.joinCause {
				return nil, errors.Join(context.Cause(ctx), opener.independent)
			}
			return nil, opener.independent
		}
		return nil, context.Cause(ctx)
	}
	if opener.logs == nil {
		return nil, nil
	}
	return opener.logs, nil
}

type preparationLauncher struct {
	entered     chan struct{}
	block       bool
	independent error
	joinCause   bool
	release     <-chan struct{}
	cancelled   chan<- struct{}
}

func (launcher *preparationLauncher) Launch(ctx context.Context, _ []byte,
	source sourceOwner,
) (supervisorClient, error) {
	if launcher.entered != nil {
		close(launcher.entered)
	}
	if launcher.block {
		<-ctx.Done()
		if launcher.cancelled != nil {
			close(launcher.cancelled)
		}
		if launcher.release != nil {
			<-launcher.release
		}
		failure := context.Cause(ctx)
		if launcher.independent != nil {
			failure = launcher.independent
			if launcher.joinCause {
				failure = errors.Join(context.Cause(ctx), launcher.independent)
			}
		}
		return nil, errors.Join(failure, source.Close())
	}
	return nil, errors.New("unexpected launch")
}

func TestCommandPreparationSignalsReturnUnstartedCancellationAndReleaseReceipts(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("effective-root preparation contract")
	}
	for _, stage := range []string{"source", "logs", "launch"} {
		t.Run(stage, func(t *testing.T) {
			entered := make(chan struct{})
			source := &preparationSource{}
			sources := &preparationSourceOpener{source: source}
			logs := &preparationLogOpener{}
			launcher := &preparationLauncher{}
			switch stage {
			case "source":
				sources.entered, sources.block = entered, true
			case "logs":
				logs.entered, logs.block = entered, true
			case "launch":
				launcher.entered, launcher.block = entered, true
			}
			request := preparationRequest(t)
			finished := make(chan struct {
				result Result
				err    error
			}, 1)
			go func() {
				result, err := newCommand(sources, logs, launcher).Run(context.Background(),
					request, strings.NewReader(""), io.Discard, io.Discard)
				finished <- struct {
					result Result
					err    error
				}{result, err}
			}()
			<-entered
			process, err := os.FindProcess(os.Getpid())
			if err != nil || process.Signal(syscall.SIGINT) != nil {
				t.Fatalf("signal process: %v", err)
			}
			select {
			case outcome := <-finished:
				reason, cancelled := outcome.result.Cancellation()
				if outcome.err != nil || outcome.result.LiveSessionEstablished() || !cancelled ||
					reason != runsupervisor.CancelUser {
					t.Fatalf("result=%#v error=%v", outcome.result, outcome.err)
				}
				wantCloses := 0
				if stage != "source" {
					wantCloses = 1
				}
				if source.closes != wantCloses {
					t.Fatalf("source closes=%d, want %d", source.closes, wantCloses)
				}
			case <-time.After(time.Second):
				t.Fatal("preparation signal did not finish")
			}
		})
	}
}

func TestCommandIndependentPreparationFailureWinsConcurrentSignal(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("effective-root preparation contract")
	}
	entered := make(chan struct{})
	independent := errors.New("source changed while opening")
	sources := &preparationSourceOpener{entered: entered, block: true, independent: independent}
	request := preparationRequest(t)
	finished := make(chan error, 1)
	go func() {
		_, err := newCommand(sources, &preparationLogOpener{}, &preparationLauncher{}).Run(
			context.Background(), request, strings.NewReader(""), io.Discard, io.Discard)
		finished <- err
	}()
	<-entered
	process, _ := os.FindProcess(os.Getpid())
	_ = process.Signal(syscall.SIGINT)
	if err := <-finished; !errors.Is(err, independent) {
		t.Fatalf("error=%v, want independent failure", err)
	}
}

func TestCommandIndependentFailureAtEveryPreparationBoundaryWinsConcurrentSignal(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("effective-root preparation contract")
	}
	for _, joined := range []bool{false, true} {
		for _, stage := range []string{"source", "logs", "handshake"} {
			variant := "/independent"
			if joined {
				variant = "/joined-signal"
			}
			t.Run(stage+variant,
				func(t *testing.T) {
					independent := errors.New(stage + " independently failed")
					entered := make(chan struct{})
					sources := &preparationSourceOpener{source: &preparationSource{}}
					logs := &preparationLogOpener{}
					launcher := &preparationLauncher{}
					request := preparationRequest(t)
					switch stage {
					case "source":
						sources.entered, sources.block = entered, true
						sources.independent, sources.joinCause = independent, joined
					case "logs":
						logs.entered, logs.block = entered, true
						logs.independent, logs.joinCause = independent, joined
					case "handshake":
						launcher.entered, launcher.block = entered, true
						launcher.independent, launcher.joinCause = independent, joined
					}
					finished := make(chan struct {
						result Result
						err    error
					}, 1)
					go func() {
						result, err := newCommand(sources, logs, launcher).Run(context.Background(),
							request, strings.NewReader(""), io.Discard, io.Discard)
						finished <- struct {
							result Result
							err    error
						}{result, err}
					}()
					<-entered
					signalSelf(t, syscall.SIGINT)
					outcome := <-finished
					if !errors.Is(outcome.err, independent) {
						t.Fatalf("result=%#v error=%v, want independent failure", outcome.result, outcome.err)
					}
					if _, cancelled := outcome.result.Cancellation(); cancelled {
						t.Fatal("independent preparation failure was downgraded to cancellation")
					}
				})
		}
	}
}

func TestCommandCallerCancellationWinsAlreadyAcceptedPreparationSignal(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("effective-root preparation contract")
	}
	for _, stage := range []string{"source", "logs", "handshake"} {
		t.Run(stage, func(t *testing.T) {
			entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			sources := &preparationSourceOpener{source: &preparationSource{}}
			logs := &preparationLogOpener{}
			launcher := &preparationLauncher{}
			switch stage {
			case "source":
				sources.entered, sources.block = entered, true
				sources.cancelled, sources.release = cancelled, release
			case "logs":
				logs.entered, logs.block = entered, true
				logs.cancelled, logs.release = cancelled, release
			case "handshake":
				launcher.entered, launcher.block = entered, true
				launcher.cancelled, launcher.release = cancelled, release
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			callerFailure := errors.New("caller stopped during preparation")
			request := preparationRequest(t)
			finished := make(chan struct {
				result Result
				err    error
			}, 1)
			go func() {
				result, err := newCommand(sources, logs, launcher).Run(ctx, request,
					strings.NewReader(""), io.Discard, io.Discard)
				finished <- struct {
					result Result
					err    error
				}{result, err}
			}()
			<-entered
			signalSelf(t, syscall.SIGINT)
			<-cancelled
			cancel(callerFailure)
			close(release)
			outcome := <-finished
			if !errors.Is(outcome.err, callerFailure) {
				t.Fatalf("result=%#v error=%v, want caller failure", outcome.result, outcome.err)
			}
			if _, cancelled := outcome.result.Cancellation(); cancelled {
				t.Fatal("caller cancellation was reported as OS signal cancellation")
			}
		})
	}
}

func TestCommandPreparationSignalKeepsParentCleanupFailureOutOfLogError(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("effective-root preparation contract")
	}
	entered := make(chan struct{})
	parentFailure := errors.New("close source failed")
	sources := &preparationSourceOpener{source: &preparationSource{closeErr: parentFailure}}
	logs := &preparationLogOpener{entered: entered, block: true}
	request := preparationRequest(t)
	finished := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := newCommand(sources, logs, &preparationLauncher{}).Run(context.Background(),
			request, strings.NewReader(""), io.Discard, io.Discard)
		finished <- struct {
			result Result
			err    error
		}{result, err}
	}()
	<-entered
	signalSelf(t, syscall.SIGINT)
	outcome := <-finished
	if outcome.err != nil || !errors.Is(outcome.result.ParentError(), parentFailure) ||
		errors.Is(outcome.result.LogError(), parentFailure) {
		t.Fatalf("result parent/log=%v/%v error=%v", outcome.result.ParentError(),
			outcome.result.LogError(), outcome.err)
	}
}

func TestCommandPreparationSignalKeepsCleanupErrorsInTheirOwnedResultSlots(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("effective-root preparation contract")
	}
	entered := make(chan struct{})
	logFailure := errors.New("close raw log failed")
	logs := &preparationLogs{closeErr: logFailure}
	sources := &preparationSourceOpener{source: &preparationSource{}}
	launcher := &preparationLauncher{entered: entered, block: true}
	request := preparationRequestWithLogs(t)
	finished := make(chan struct {
		result Result
		err    error
	}, 1)
	go func() {
		result, err := newCommand(sources, &preparationLogOpener{logs: logs}, launcher).Run(
			context.Background(), request, strings.NewReader(""), io.Discard,
			io.Discard)
		finished <- struct {
			result Result
			err    error
		}{result, err}
	}()
	<-entered
	signalSelf(t, syscall.SIGINT)
	outcome := <-finished
	if outcome.err != nil || !errors.Is(outcome.result.LogError(), logFailure) ||
		errors.Is(outcome.result.ParentError(), logFailure) {
		t.Fatalf("result parent/log=%v/%v error=%v", outcome.result.ParentError(),
			outcome.result.LogError(), outcome.err)
	}
}

func TestCommandCallerContextIsNotClassifiedAsSignalCancellation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("effective-root preparation contract")
	}
	entered := make(chan struct{})
	sources := &preparationSourceOpener{entered: entered, block: true}
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("caller stopped")
	request := preparationRequest(t)
	finished := make(chan error, 1)
	go func() {
		_, err := newCommand(sources, &preparationLogOpener{}, &preparationLauncher{}).Run(
			ctx, request, strings.NewReader(""), io.Discard, io.Discard)
		finished <- err
	}()
	<-entered
	cancel(cause)
	if err := <-finished; !errors.Is(err, cause) {
		t.Fatalf("error=%v, want caller cause", err)
	}
}

func TestCommandRequiresRootBeforeOpeningResources(t *testing.T) {
	const childEnvironment = "TRANSFERLANES_TEST_NON_ROOT_COMMAND_PREFLIGHT"
	if os.Geteuid() == 0 && os.Getenv(childEnvironment) == "" {
		setpriv, err := exec.LookPath("setpriv")
		if err != nil {
			t.Fatalf("non-root preflight test requires setpriv: %v", err)
		}
		command := exec.Command(setpriv, "--reuid=65534", "--regid=65534", "--clear-groups",
			os.Args[0], "-test.run=^TestCommandRequiresRootBeforeOpeningResources$")
		command.Env = append(os.Environ(), childEnvironment+"=1")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("non-root preflight subprocess: %v\n%s", err, output)
		}
		return
	}
	sources := &preparationSourceOpener{entered: make(chan struct{}), source: &preparationSource{}}
	_, err := newCommand(sources, &preparationLogOpener{}, &preparationLauncher{}).Run(
		context.Background(), preparationRequest(t), strings.NewReader(""), io.Discard, io.Discard)
	if err == nil {
		t.Fatal("non-root run was accepted")
	}
	select {
	case <-sources.entered:
		t.Fatal("non-root run opened its source")
	default:
	}
}

func preparationRequest(t *testing.T) Request {
	t.Helper()
	request, err := NewRequest("/source", false,
		[]NetworkArgument{validNetworkArgument("192.0.2.1", 1, false)}, false,
		throughput.Settings{}, nil, []string{"copy", "{}"}, nil,
		NewDisplayOptions(true, false), false)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func preparationRequestWithLogs(t *testing.T) Request {
	t.Helper()
	request, err := NewRequest("/source", false,
		[]NetworkArgument{validNetworkArgument("192.0.2.1", 1, false)}, false,
		throughput.Settings{}, nil, []string{"copy", "{}"}, textPointer("/logs"),
		NewDisplayOptions(true, false), false)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func signalSelf(t *testing.T, signal os.Signal) {
	t.Helper()
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Signal(signal); err != nil {
		t.Fatal(err)
	}
}
