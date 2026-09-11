package runcommand

import (
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
)

type terminationRequest struct {
	reason         runsupervisor.CancelReason
	controllerLost bool
}

type terminationInbox struct {
	mu      sync.Mutex
	pending []terminationRequest
	wake    chan struct{}
}

func newTerminationInbox() *terminationInbox {
	return &terminationInbox{wake: make(chan struct{}, 1)}
}

func (inbox *terminationInbox) publish(request terminationRequest) {
	if inbox == nil {
		return
	}
	inbox.mu.Lock()
	duplicate := false
	for _, pending := range inbox.pending {
		if pending.controllerLost == request.controllerLost &&
			(pending.controllerLost || pending.reason == request.reason) {
			duplicate = true
			break
		}
	}
	if !duplicate {
		inbox.pending = append(inbox.pending, request)
	}
	inbox.mu.Unlock()
	select {
	case inbox.wake <- struct{}{}:
	default:
	}
}

func (inbox *terminationInbox) take() (terminationRequest, bool) {
	if inbox == nil {
		return terminationRequest{}, false
	}
	inbox.mu.Lock()
	defer inbox.mu.Unlock()
	if len(inbox.pending) == 0 {
		return terminationRequest{}, false
	}
	request := inbox.pending[0]
	inbox.pending = inbox.pending[1:]
	if len(inbox.pending) != 0 {
		select {
		case inbox.wake <- struct{}{}:
		default:
		}
	}
	return request, true
}

func (inbox *terminationInbox) notifications() <-chan struct{} {
	if inbox == nil {
		return nil
	}
	return inbox.wake
}
