//go:build linux

package childprocesses

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const (
	maximumHelperOutput           = 64 * 1024
	maximumHelperStderrDiagnostic = 256
)

// RunHelpers starts the complete helper group at one barrier and terminates it on any helper
// failure. Its error contains execution and supervision evidence only. Its ProcessSet result is nil
// after confirmed reaping; a non-nil ProcessSet transfers sole ownership when containment failed.
func (owner *ChildProcesses) RunHelpers(ctx context.Context, network *hostnetwork.Session,
	commands []Command) (HelperResult, *ProcessSet, error) {
	processes, startErr := owner.StartPlain(ctx, network, commands)
	if processes == nil {
		return HelperResult{}, nil, startErr
	}
	result, collectErr := collectHelpers(processes, commands)
	runErr := errors.Join(startErr, collectErr)
	select {
	case <-processes.ContainmentDone():
		return result, nil, runErr
	default:
		return result, processes, errors.Join(runErr,
			errors.New("helper process containment is unconfirmed"))
	}
}

type completedGroup struct {
	result Result
	err    error
}

func collectHelpers(processes *ProcessSet, commands []Command) (HelperResult, error) {
	evidence := newHelperEvidence(commands, func() { go processes.cancelBackground() })
	completed := make(chan completedGroup, 1)
	go func() {
		result, err := processes.Wait()
		completed <- completedGroup{result: result, err: err}
	}()
	output, statuses := processes.Output(), processes.Statuses()
	var result Result
	for output != nil || statuses != nil || completed != nil {
		select {
		case event, open := <-output:
			if !open {
				output = nil
				continue
			}
			evidence.acceptOutput(event)
		case status, open := <-statuses:
			if !open {
				statuses = nil
				continue
			}
			evidence.acceptStatus(status)
		case finished := <-completed:
			result = finished.result
			evidence.failures = append(evidence.failures, finished.err)
			completed = nil
		}
	}
	if result.Cancelled {
		evidence.failures = append(evidence.failures, fmt.Errorf("helper group was cancelled"))
	}
	helperResult, resultErr := newHelperResult(result, evidence.outputs)
	return helperResult, errors.Join(evidence.diagnosticError(), resultErr)
}

type helperEvidence struct {
	outputs        map[transfernumber.Number][]byte
	outputExceeded map[transfernumber.Number]bool
	stderr         map[transfernumber.Number]*helperStderrDiagnostic
	transfers      []transfernumber.Number
	failures       []error
	cancel         func()
}

type helperStderrDiagnostic struct {
	sample   []byte
	exceeded bool
}

func newHelperEvidence(commands []Command, cancel func()) *helperEvidence {
	evidence := &helperEvidence{outputs: make(map[transfernumber.Number][]byte, len(commands)),
		outputExceeded: make(map[transfernumber.Number]bool),
		stderr:         make(map[transfernumber.Number]*helperStderrDiagnostic, len(commands)),
		transfers:      make([]transfernumber.Number, 0, len(commands)), cancel: sync.OnceFunc(cancel)}
	for _, command := range commands {
		evidence.outputs[command.transfer] = nil
		evidence.transfers = append(evidence.transfers, command.transfer)
	}
	return evidence
}

func (evidence *helperEvidence) acceptOutput(event Output) {
	if event.Stream == StreamStderr {
		evidence.acceptStderr(event)
		return
	}
	if event.Stream != StreamStdout {
		evidence.failures = append(evidence.failures, fmt.Errorf("transfer %d helper used unexpected %s output",
			event.Transfer.Value(), event.Stream))
		evidence.cancel()
		return
	}
	if len(evidence.outputs[event.Transfer])+len(event.Bytes) > maximumHelperOutput {
		if !evidence.outputExceeded[event.Transfer] {
			evidence.failures = append(evidence.failures,
				fmt.Errorf("transfer %d helper output exceeds limit", event.Transfer.Value()))
			evidence.outputExceeded[event.Transfer] = true
		}
		evidence.cancel()
		return
	}
	evidence.outputs[event.Transfer] = append(evidence.outputs[event.Transfer], event.Bytes...)
}

func (evidence *helperEvidence) acceptStderr(event Output) {
	diagnostic := evidence.stderr[event.Transfer]
	if diagnostic == nil {
		diagnostic = &helperStderrDiagnostic{sample: make([]byte, 0, maximumHelperStderrDiagnostic)}
		evidence.stderr[event.Transfer] = diagnostic
		evidence.cancel()
	}
	remaining := maximumHelperStderrDiagnostic - len(diagnostic.sample)
	if len(event.Bytes) > remaining {
		diagnostic.exceeded = true
	}
	if remaining > 0 {
		diagnostic.sample = append(diagnostic.sample, event.Bytes[:min(remaining, len(event.Bytes))]...)
	}
}

func (evidence *helperEvidence) diagnosticError() error {
	failures := append([]error(nil), evidence.failures...)
	for _, transfer := range evidence.transfers {
		diagnostic := evidence.stderr[transfer]
		if diagnostic == nil {
			continue
		}
		failures = append(failures, fmt.Errorf("transfer %d helper wrote stderr: %q",
			transfer.Value(), diagnostic.sample))
		if diagnostic.exceeded {
			failures = append(failures, fmt.Errorf(
				"transfer %d helper stderr diagnostic exceeds limit", transfer.Value()))
		}
	}
	return errors.Join(failures...)
}

func (evidence *helperEvidence) acceptStatus(status Status) {
	if status.Kind != StatusExited || status.ExitCode == 0 && status.Signal == 0 {
		return
	}
	evidence.failures = append(evidence.failures, fmt.Errorf("transfer %d helper failed: exit=%d signal=%d",
		status.Transfer.Value(), status.ExitCode, status.Signal))
	evidence.cancel()
}
