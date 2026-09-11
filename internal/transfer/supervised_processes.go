package transfer

import (
	"context"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
)

// supervisedProcesses is the lifecycle evidence shared by throughput helpers and user commands.
type supervisedProcesses interface {
	Wait() (childprocesses.Result, error)
	FinishStartFailure(context.Context) childprocesses.StartFailureResult
	ConfirmContainment(context.Context) error
	ContainmentDone() <-chan struct{}
}
