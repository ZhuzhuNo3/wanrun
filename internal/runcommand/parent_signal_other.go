//go:build !unix

package runcommand

import (
	"os"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
)

func subscribeParentSignals(chan<- os.Signal) func() { return func() {} }

func classifyParentSignal(os.Signal) (runsupervisor.CancelReason, bool) {
	return runsupervisor.CancelNone, false
}
