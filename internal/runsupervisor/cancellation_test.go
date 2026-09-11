package runsupervisor

import (
	"context"
	"errors"
	"testing"
)

func TestSessionCancellationPrecedenceBeforeFreeze(t *testing.T) {
	_, cancel := context.WithCancelCause(context.Background())
	outcome := &sessionCancellation{cancel: cancel}
	outcome.record(CancelLifeline, "lifeline", ErrLifelineClosed)
	outcome.record(CancelUser, "", ErrCancelled)
	outcome.record(CancelHangup, "ignored", ErrCancelled)
	outcome.record(CancelInternal, "internal", errors.New("internal"))

	snapshot := outcome.freeze()
	if snapshot.reason != CancelInternal || snapshot.diagnostic != "internal" {
		t.Fatalf("cancellation snapshot = %#v", snapshot)
	}
}

func TestSessionCancellationFreezeRejectsLateEvidence(t *testing.T) {
	_, cancel := context.WithCancelCause(context.Background())
	outcome := &sessionCancellation{cancel: cancel}
	outcome.record(CancelUser, "accepted", ErrCancelled)
	want := outcome.freeze()
	outcome.record(CancelInternal, "late internal", errors.New("late"))
	if got := outcome.freeze(); got != want {
		t.Fatalf("late cancellation changed frozen evidence: got=%#v want=%#v", got, want)
	}
}
