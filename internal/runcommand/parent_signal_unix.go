//go:build unix

package runcommand

import (
	"os"
	"os/signal"
	"syscall"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
)

func subscribeParentSignals(destination chan<- os.Signal) func() {
	signal.Notify(destination, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH)
	return func() { signal.Stop(destination) }
}

func classifyParentSignal(received os.Signal) (runsupervisor.CancelReason, bool) {
	switch received {
	case syscall.SIGINT:
		return runsupervisor.CancelUser, false
	case syscall.SIGTERM:
		return runsupervisor.CancelTerminate, false
	case syscall.SIGHUP:
		return runsupervisor.CancelHangup, false
	case syscall.SIGWINCH:
		return runsupervisor.CancelNone, true
	default:
		return runsupervisor.CancelNone, false
	}
}
