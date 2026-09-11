package runcommand

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/runlogs"
	"github.com/ZhuzhuNo3/transferlanes/internal/runoutput"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type supervisorClient interface {
	Next() (runsupervisor.Event, error)
	Cancel(runsupervisor.CancelReason) error
	Resize(transfernumber.Number, uint16, uint16) error
	WriteInput(transfernumber.Number, []byte) error
	CloseLifeline() error
	Wait() error
}

type runLogOwner interface {
	Write(transfernumber.Number, runlogs.Stream, []byte) error
	Close() error
}

type signalProducer interface {
	resizeEvents() <-chan struct{}
	stopAndJoin()
}

func (watcher *parentSignalWatcher) resizeEvents() <-chan struct{} { return watcher.resize }
func (watcher *parentSignalWatcher) stopAndJoin()                  { watcher.close() }

type lifecyclePhase uint8

const (
	sessionOpen lifecyclePhase = iota + 1
	sessionTerminating
	sessionCompleted
)

type lifelineOnce struct {
	once sync.Once
	err  error
}

func (lifeline *lifelineOnce) close(client supervisorClient) error {
	lifeline.once.Do(func() { lifeline.err = client.CloseLifeline() })
	return lifeline.err
}

type runSession struct {
	ctx        context.Context
	client     supervisorClient
	logs       runLogOwner
	driver     displayDriver
	signals    signalProducer
	inbox      *terminationInbox
	phase      lifecyclePhase
	first      runsupervisor.CancelReason
	firstSet   bool
	parentErr  error
	output     runoutput.Result
	lifeline   lifelineOnce
	driverRead bool
}

func (session *runSession) run() Result {
	session.phase = sessionOpen
	completion := runoutput.Start(session.client, session.logs, session.driver.display(), func() {
		session.inbox.publish(terminationRequest{reason: runsupervisor.CancelInternal})
	})
	var resize <-chan struct{}
	if session.signals != nil {
		resize = session.signals.resizeEvents()
	}
	session.driver.start(session.client, completion.Done(), resize, session.inbox)
	session.drive(completion)
	if receiver, interactive := session.driver.(finalSnapshotDriver); interactive {
		receiver.complete(session.projection())
	}
	<-session.driver.done()
	session.collectDriverResult()
	if session.signals != nil {
		session.signals.stopAndJoin()
	}
	var logCloseErr error
	if session.logs != nil {
		logCloseErr = session.logs.Close()
	}
	lifelineErr := session.lifeline.close(session.client)
	waitErr := session.client.Wait()
	return session.result(logCloseErr, lifelineErr, waitErr)
}

func (session *runSession) drive(completion *runoutput.Completion) {
	for session.phase != sessionCompleted {
		if session.acceptReady(completion) {
			continue
		}
		select {
		case <-completion.Done():
		case <-session.ctx.Done():
		case <-session.inbox.notifications():
		case <-session.driver.done():
			if err := session.collectDriverResult(); err != nil {
				session.inbox.publish(terminationRequest{reason: runsupervisor.CancelInternal})
			}
		}
	}
}

// acceptReady implements the fixed completion, caller-context, inbox priority.
func (session *runSession) acceptReady(completion *runoutput.Completion) bool {
	select {
	case <-completion.Done():
		session.output = completion.Result()
		session.phase = sessionCompleted
		return true
	default:
	}
	if session.phase == sessionOpen {
		select {
		case <-session.ctx.Done():
			cause := context.Cause(session.ctx)
			if cause == nil {
				cause = session.ctx.Err()
			}
			session.parentErr = errors.Join(session.parentErr,
				fmt.Errorf("caller context cancelled run: %w", cause))
			session.acceptTermination(terminationRequest{reason: runsupervisor.CancelInternal})
			return true
		default:
		}
	}
	request, exists := session.inbox.take()
	if !exists {
		return false
	}
	if request.controllerLost {
		session.acceptControllerLoss(request)
	} else if session.phase == sessionOpen {
		session.acceptTermination(request)
	}
	return true
}

func (session *runSession) acceptTermination(request terminationRequest) {
	if session.phase != sessionOpen {
		return
	}
	session.phase = sessionTerminating
	session.first, session.firstSet = request.reason, true
	session.driver.stopCommands()
	if err := session.client.Cancel(request.reason); err != nil {
		session.parentErr = errors.Join(session.parentErr,
			fmt.Errorf("cancel transfer supervisor: %w", err))
		_ = session.lifeline.close(session.client)
	}
}

func (session *runSession) acceptControllerLoss(request terminationRequest) {
	if session.phase == sessionCompleted {
		return
	}
	if session.phase == sessionOpen {
		session.phase = sessionTerminating
		session.first, session.firstSet = request.reason, true
		session.driver.stopCommands()
	}
	_ = session.lifeline.close(session.client)
}

func (session *runSession) collectDriverResult() error {
	if session.driverRead {
		return nil
	}
	select {
	case <-session.driver.done():
		session.driverRead = true
		err := session.driver.result()
		session.parentErr = errors.Join(session.parentErr, err)
		return err
	default:
	}
	return nil
}

func (session *runSession) projection() finalSnapshotProjection {
	final, received := session.output.Final()
	if !received {
		return finalSnapshotProjection{}
	}
	return finalSnapshotProjection{final: &final,
		displayEventsComplete: session.output.DisplayEventsComplete()}
}

func (session *runSession) result(logCloseErr, lifelineErr, waitErr error) Result {
	final, finalReceived := session.output.Final()
	return Result{liveSessionEstablished: true, displayMode: session.driver.mode(),
		displayModeSelected: true, final: final, finalReceived: finalReceived,
		cancellation: session.first, cancellationReceived: session.firstSet,
		parentError: errors.Join(session.parentErr, session.output.ReadError(),
			session.output.DisplayError(), wrapCloseLifeline(lifelineErr), wrapSupervisorWait(waitErr)),
		logError: errors.Join(session.output.LogError(), wrapLogClose(logCloseErr))}
}

func wrapCloseLifeline(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close supervisor lifeline: %w", err)
}

func wrapSupervisorWait(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("reap transfer supervisor: %w", err)
}

func wrapLogClose(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("close raw logs: %w", err)
}
