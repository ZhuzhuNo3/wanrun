package transfer

import (
	"context"
	"fmt"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
)

type commandGroupStarter interface {
	start(context.Context, *hostnetwork.Session, resolverGroup, []TransferExecution, Request) (
		supervisedProcesses, error)
}

type childCommandStarter struct {
	owner  *childprocesses.ChildProcesses
	events CommandEvents
}

func (starter childCommandStarter) start(ctx context.Context, network *hostnetwork.Session,
	resolvers resolverGroup, executions []TransferExecution, request Request,
) (supervisedProcesses, error) {
	commands := make([]childprocesses.Command, len(executions))
	for index, execution := range executions {
		resolver, err := resolvers.Access(execution.number)
		if err != nil {
			return nil, err
		}
		command, err := execution.command(resolver, request.environment)
		if err != nil {
			return nil, fmt.Errorf("construct transfer %d command: %w", execution.number.Value(), err)
		}
		commands[index] = command
	}
	processCtx, cancel := context.WithCancel(ctx)
	var processSet *childprocesses.ProcessSet
	var err error
	if request.commandIO == InteractiveCommandIO {
		processSet, err = starter.owner.StartInteractive(processCtx, network, commands, request.terminal)
	} else {
		processSet, err = starter.owner.StartPlain(processCtx, network, commands)
	}
	if processSet == nil {
		cancel()
		return nil, err
	}
	group := newCommandProcessGroup(processSet, starter.events, cancel)
	return group, err
}
