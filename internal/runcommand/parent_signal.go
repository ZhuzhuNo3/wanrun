package runcommand

import (
	"context"
	"os"
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
)

type signalCancellation struct {
	reason runsupervisor.CancelReason
}

func (cause *signalCancellation) Error() string {
	return "parent signal cancelled supervisor startup"
}

type parentSignalWatcher struct {
	preparation context.Context
	cancel      context.CancelCauseFunc
	inbox       *terminationInbox
	resize      chan struct{}
	raw         chan os.Signal
	stop        chan struct{}
	done        chan struct{}
	stopNotify  func()
	closeOnce   sync.Once
	mu          sync.Mutex
	signalCause *signalCancellation
}

func watchParentSignals(parent context.Context, inbox *terminationInbox) *parentSignalWatcher {
	preparation, cancel := context.WithCancelCause(context.Background())
	raw := make(chan os.Signal, 8)
	watcher := &parentSignalWatcher{preparation: preparation, cancel: cancel, inbox: inbox,
		resize: make(chan struct{}, 1), raw: raw, stop: make(chan struct{}), done: make(chan struct{})}
	watcher.stopNotify = subscribeParentSignals(raw)
	go watcher.forward(parent)
	return watcher
}

func (watcher *parentSignalWatcher) forward(parent context.Context) {
	defer close(watcher.done)
	for {
		select {
		case received := <-watcher.raw:
			reason, resize := classifyParentSignal(received)
			if resize {
				select {
				case watcher.resize <- struct{}{}:
				default:
				}
				continue
			}
			if reason != runsupervisor.CancelNone {
				cause := &signalCancellation{reason: reason}
				watcher.mu.Lock()
				if watcher.signalCause == nil {
					watcher.signalCause = cause
				}
				watcher.mu.Unlock()
				watcher.inbox.publish(terminationRequest{reason: reason})
				watcher.cancel(cause)
			}
		case <-parent.Done():
			watcher.cancel(context.Cause(parent))
			return
		case <-watcher.stop:
			return
		}
	}
}

func (watcher *parentSignalWatcher) signalReason() (runsupervisor.CancelReason, bool) {
	watcher.mu.Lock()
	defer watcher.mu.Unlock()
	if watcher.signalCause == nil {
		return runsupervisor.CancelNone, false
	}
	return watcher.signalCause.reason, true
}

func (watcher *parentSignalWatcher) close() {
	if watcher == nil {
		return
	}
	watcher.closeOnce.Do(func() {
		watcher.stopNotify()
		close(watcher.stop)
		<-watcher.done
		watcher.cancel(context.Canceled)
	})
}
