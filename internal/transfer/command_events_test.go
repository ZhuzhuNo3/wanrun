package transfer

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestCommandProcessGroupRejectsNilCleanupContextsAndReleasesProcessContext(t *testing.T) {
	owner := newDelayedCommandProcessOwner()
	released := make(chan struct{})
	var releaseOnce sync.Once
	group := newCommandProcessGroup(owner, discardCommandEvents{}, func() {
		releaseOnce.Do(func() { close(released) })
	})

	failed := group.FinishStartFailure(nil)
	containmentErr := group.ConfirmContainment(nil)
	waitForTestSignal(t, released, time.Second, "command process context release")
	owner.closeEventStreams()
	waitForTestSignal(t, group.done, time.Second, "command event drain")

	if failed.ContainmentError == nil ||
		!strings.Contains(failed.ContainmentError.Error(), "context is required") ||
		containmentErr == nil || !strings.Contains(containmentErr.Error(), "context is required") {
		t.Fatalf("nil cleanup contexts were not rejected: failed=%#v containment=%v",
			failed, containmentErr)
	}
}

func TestCommandProcessGroupPrefersCompletedDrainAndContainmentOverCancelledContext(t *testing.T) {
	owner := newDelayedCommandProcessOwner()
	owner.closeEventStreams()
	close(owner.contained)
	group := newCommandProcessGroup(owner, discardCommandEvents{}, func() {})
	waitForTestSignal(t, group.done, time.Second, "command event drain")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	failed := group.FinishStartFailure(ctx)
	if failed.SupervisionError != nil || failed.ContainmentError != nil {
		t.Fatalf("completed failed-start cleanup lost to cancelled context: %#v", failed)
	}
}

func TestCommandProcessGroupReportsEventFailureAndStillDrains(t *testing.T) {
	owner := newDelayedCommandProcessOwner()
	first, _ := transfernumber.New(1)
	publishErr := errors.New("event destination unavailable")
	owner.output <- childprocesses.Output{Transfer: first, Stream: childprocesses.StreamStdout,
		Bytes: []byte("output")}
	owner.closeEventStreams()
	close(owner.contained)
	released := make(chan struct{})
	var releaseOnce sync.Once
	group := newCommandProcessGroup(owner, failingCommandEvents{err: publishErr}, func() {
		releaseOnce.Do(func() { close(released) })
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	failed := group.FinishStartFailure(ctx)
	waitForTestSignal(t, group.done, time.Second, "command event drain")
	waitForTestSignal(t, released, time.Second, "command process context release")
	if !errors.Is(failed.SupervisionError, publishErr) || failed.ContainmentError != nil {
		t.Fatalf("event forwarding failure result=%#v", failed)
	}
}

type failingCommandEvents struct{ err error }

func (events failingCommandEvents) SendOutput(childprocesses.Output) error { return events.err }
func (events failingCommandEvents) SendStatus(childprocesses.Status) error { return events.err }
func (failingCommandEvents) Controls() <-chan CommandControl               { return nil }

type delayedCommandProcessOwner struct {
	output      chan childprocesses.Output
	statuses    chan childprocesses.Status
	contained   chan struct{}
	eventsClose sync.Once
}

func newDelayedCommandProcessOwner() *delayedCommandProcessOwner {
	return &delayedCommandProcessOwner{output: make(chan childprocesses.Output, 1),
		statuses: make(chan childprocesses.Status, 1), contained: make(chan struct{})}
}

func (owner *delayedCommandProcessOwner) Output() <-chan childprocesses.Output { return owner.output }

func (owner *delayedCommandProcessOwner) Statuses() <-chan childprocesses.Status {
	return owner.statuses
}

func (*delayedCommandProcessOwner) WriteInput(transfernumber.Number, []byte) error { return nil }

func (*delayedCommandProcessOwner) Resize(transfernumber.Number, int, int) error { return nil }

func (*delayedCommandProcessOwner) Wait() (childprocesses.Result, error) {
	return childprocesses.Result{}, nil
}

func (owner *delayedCommandProcessOwner) FinishStartFailure(ctx context.Context) childprocesses.StartFailureResult {
	if ctx == nil {
		return childprocesses.StartFailureResult{ContainmentError: errors.New("failed-start context is required")}
	}
	select {
	case <-owner.contained:
		return childprocesses.StartFailureResult{}
	default:
	}
	select {
	case <-owner.contained:
		return childprocesses.StartFailureResult{}
	case <-ctx.Done():
		select {
		case <-owner.contained:
			return childprocesses.StartFailureResult{}
		default:
			return childprocesses.StartFailureResult{ContainmentError: ctx.Err()}
		}
	}
}

func (owner *delayedCommandProcessOwner) ConfirmContainment(ctx context.Context) error {
	if ctx == nil {
		return errors.New("process containment context is required")
	}
	select {
	case <-owner.contained:
		return nil
	default:
	}
	select {
	case <-owner.contained:
		return nil
	case <-ctx.Done():
		select {
		case <-owner.contained:
			return nil
		default:
			return ctx.Err()
		}
	}
}

func (owner *delayedCommandProcessOwner) ContainmentDone() <-chan struct{} { return owner.contained }

func (owner *delayedCommandProcessOwner) closeEventStreams() {
	owner.eventsClose.Do(func() {
		close(owner.output)
		close(owner.statuses)
	})
}

func waitForTestSignal(t *testing.T, done <-chan struct{}, limit time.Duration, description string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(limit):
		t.Fatalf("timed out waiting for %s", description)
	}
}
