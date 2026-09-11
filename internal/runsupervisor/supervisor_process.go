package runsupervisor

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
)

const supervisorReapLimit = 5 * time.Second

type processWaitOutcome struct {
	timedOut bool
	killErr  error
	waitErr  error
}

type supervisorProcess struct {
	command    *exec.Cmd
	pid        int
	finishOnce sync.Once
	outcome    processWaitOutcome
}

func newSupervisorProcess(command *exec.Cmd) *supervisorProcess {
	process := &supervisorProcess{command: command}
	if command != nil && command.Process != nil {
		process.pid = command.Process.Pid
	}
	return process
}

func (process *supervisorProcess) PID() int {
	if process == nil {
		return 0
	}
	return process.pid
}

func (process *supervisorProcess) wait() error {
	if process == nil || process.command == nil {
		return errors.New("supervisor process is unavailable")
	}
	process.finishOnce.Do(func() { process.outcome.waitErr = process.command.Wait() })
	return process.outcome.waitErr
}

func (process *supervisorProcess) waitForExit(limit time.Duration) processWaitOutcome {
	if process == nil || process.command == nil {
		return processWaitOutcome{waitErr: errors.New("supervisor process is unavailable")}
	}
	process.finishOnce.Do(func() {
		done := make(chan error, 1)
		go func() { done <- process.command.Wait() }()
		timer := time.NewTimer(limit)
		defer timer.Stop()
		select {
		case process.outcome.waitErr = <-done:
		case <-timer.C:
			process.outcome.timedOut = true
			if process.command.Process != nil {
				process.outcome.killErr = process.command.Process.Kill()
			}
			process.outcome.waitErr = <-done
			if errors.Is(process.outcome.killErr, os.ErrProcessDone) {
				process.outcome.killErr = nil
			}
		}
	})
	return process.outcome
}

func (process *supervisorProcess) terminate() (error, error) {
	if process == nil || process.command == nil || process.command.Process == nil {
		return errors.New("supervisor process is unavailable before termination"), process.wait()
	}
	process.finishOnce.Do(func() {
		process.outcome.killErr = process.command.Process.Kill()
		process.outcome.waitErr = process.command.Wait()
		if errors.Is(process.outcome.killErr, os.ErrProcessDone) {
			process.outcome.killErr = nil
		}
	})
	return process.outcome.killErr, process.outcome.waitErr
}
