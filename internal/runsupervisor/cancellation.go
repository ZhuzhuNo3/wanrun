package runsupervisor

import (
	"context"
	"errors"
	"io"
	"sync"
)

type cancellationSnapshot struct {
	reason     CancelReason
	diagnostic string
}

type sessionCancellation struct {
	mu         sync.Mutex
	reason     CancelReason
	diagnostic string
	cancel     context.CancelCauseFunc
	frozen     bool
}

func (outcome *sessionCancellation) record(reason CancelReason, diagnostic string, cause error) {
	outcome.mu.Lock()
	if outcome.frozen {
		outcome.mu.Unlock()
		return
	}
	if outcome.reason == CancelNone || reason == CancelInternal ||
		(outcome.reason == CancelLifeline && reason != CancelLifeline) {
		outcome.reason = reason
		outcome.diagnostic = boundedDiagnostic(diagnostic)
	}
	cancel := outcome.cancel
	outcome.mu.Unlock()
	cancel(cause)
}

func (outcome *sessionCancellation) freeze() cancellationSnapshot {
	outcome.mu.Lock()
	defer outcome.mu.Unlock()
	outcome.frozen = true
	return cancellationSnapshot{reason: outcome.reason, diagnostic: outcome.diagnostic}
}

func readCancellation(reader io.Reader, outcome *sessionCancellation) {
	frame, err := readFrame(reader)
	if err != nil {
		if errors.Is(err, io.EOF) {
			outcome.record(CancelLifeline, "", ErrLifelineClosed)
			return
		}
		outcome.record(CancelInternal, "read supervisor cancellation: "+err.Error(), err)
		return
	}
	reason, err := decodeCancellation(frame)
	if err != nil {
		outcome.record(CancelInternal, "decode supervisor cancellation: "+err.Error(), err)
		return
	}
	outcome.record(reason, "", ErrCancelled)
}

func recordControlReadFailure(outcome *sessionCancellation, err error) {
	diagnostic := "read supervisor control: " + err.Error()
	if errors.Is(err, io.EOF) {
		diagnostic = "supervisor control reached EOF before final"
	}
	outcome.record(CancelInternal, diagnostic, err)
}

func applyCancellation(final Final, snapshot cancellationSnapshot) Final {
	if snapshot.reason.validCancellation() {
		final.Cancelled, final.Reason = true, snapshot.reason
	}
	final.InternalError = joinDiagnostic(final.InternalError, snapshot.diagnostic)
	return final
}
