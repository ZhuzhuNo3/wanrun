package runsupervisor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

type supervisorSession struct {
	control      io.Reader
	cancellation io.Reader
	events       *eventPipeSender
	source       *os.File
	work         Work
}

func serveWireSession(control, cancellation io.Reader, eventOutput io.Writer, work Work) error {
	return serveWireSessionWithSource(control, cancellation, eventOutput, nil, work)
}

func serveWireSessionWithSource(control, cancellation io.Reader, eventOutput io.Writer,
	source *os.File, work Work,
) error {
	if control == nil || cancellation == nil || eventOutput == nil || work == nil {
		return fmt.Errorf("supervisor inherited session is incomplete")
	}
	events, err := newEventPipeSender(eventOutput, eventBackpressureLimit)
	if err != nil {
		return errors.Join(err, closeSourceFile(source))
	}
	session := &supervisorSession{
		control: control, cancellation: cancellation, events: events, source: source, work: work,
	}
	return session.serve()
}

func (session *supervisorSession) serve() (sessionErr error) {
	defer func() { sessionErr = errors.Join(sessionErr, session.closeSource()) }()
	if err := session.events.send(frameReady, nil); err != nil {
		return err
	}
	start, err := readFrame(session.control)
	if err != nil || start.kind != frameStart || validateStartRequest(start.payload) != nil {
		return errors.Join(errHandshake, err)
	}
	if err := session.events.send(frameAccepted, nil); err != nil {
		return fmt.Errorf("acknowledge supervisor source handoff: %w", err)
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	cancellation := &sessionCancellation{cancel: cancel}
	go readCancellation(session.cancellation, cancellation)
	if !waitForCommit(ctx, session.control, cancellation) {
		final := Final{CleanupError: diagnosticFor("close accepted supervisor source", session.closeSource())}
		return sendSessionFinal(session.events, applyCancellation(final, cancellation.freeze()))
	}

	controls := make(chan Control, maximumPendingControls)
	go readControls(ctx, session.control, controls, cancellation)
	run := &RunSupervisor{
		controls: controls,
		events:   session.events,
		request:  append([]byte(nil), start.payload...),
		source:   session.takeSource(),
	}
	final := callWork(ctx, run, session.work)
	final.CleanupError = joinDiagnostic(final.CleanupError,
		diagnosticFor("close untaken supervisor source", run.closeUntakenSource()))
	return sendSessionFinal(session.events, applyCancellation(final, cancellation.freeze()))
}

func (session *supervisorSession) takeSource() *os.File {
	source := session.source
	session.source = nil
	return source
}

func (session *supervisorSession) closeSource() error {
	source := session.takeSource()
	return closeSourceFile(source)
}

func closeSourceFile(source *os.File) error {
	if source == nil {
		return nil
	}
	return source.Close()
}

type commitRead struct {
	frame wireFrame
	err   error
}

func waitForCommit(ctx context.Context, control io.Reader, outcome *sessionCancellation) bool {
	read := make(chan commitRead, 1)
	go func() {
		frame, err := readFrame(control)
		read <- commitRead{frame: frame, err: err}
	}()
	select {
	case <-ctx.Done():
		return false
	case received := <-read:
		if received.err != nil {
			recordControlReadFailure(outcome, received.err)
			return false
		}
		if received.frame.kind != frameCommit || len(received.frame.payload) != 0 {
			err := errors.New("supervisor commit frame is invalid")
			outcome.record(CancelInternal, err.Error(), err)
			return false
		}
		return true
	}
}

func readControls(ctx context.Context, reader io.Reader, controls chan<- Control,
	outcome *sessionCancellation,
) {
	defer close(controls)
	for {
		frame, err := readFrame(reader)
		if err != nil {
			recordControlReadFailure(outcome, err)
			return
		}
		control, err := decodeControl(frame)
		if err != nil {
			outcome.record(CancelInternal, "decode supervisor control: "+err.Error(), err)
			return
		}
		select {
		case controls <- control:
		case <-ctx.Done():
			return
		}
	}
}

func sendSessionFinal(events *eventPipeSender, final Final) error {
	payload, encodeErr := encodeFinal(final)
	if encodeErr != nil {
		fallback := finalEncodingFallback(final, encodeErr)
		payload, encodeErr = encodeFinal(fallback)
		if encodeErr != nil {
			return fmt.Errorf("encode supervisor final fallback: %w", encodeErr)
		}
	}
	return events.send(frameFinal, payload)
}
