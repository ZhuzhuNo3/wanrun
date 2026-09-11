package transferworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/namespaceresolvers"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfer"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"github.com/ZhuzhuNo3/transferlanes/internal/transferstart"
)

// Run adapts one supervisor start payload to the transfer runtime and its single Final result.
func Run(ctx context.Context, wire *runsupervisor.RunSupervisor) (final runsupervisor.Final) {
	request, err := transferstart.Decode(wire.Request())
	if err != nil {
		return internalFailure(fmt.Errorf("decode transfer start request: %w", err))
	}
	source, err := wire.TakeSourceRoot(request.SourceLabel())
	if err != nil {
		return internalFailure(fmt.Errorf("accept inherited transfer source: %w", err))
	}
	defer func() {
		final.CleanupError = appendCleanupError(final.CleanupError, source.Close())
	}()
	intent, err := transferRequest(request, childEnvironment(os.Environ()))
	if err != nil {
		return internalFailure(fmt.Errorf("construct transfer request: %w", err))
	}
	events := newCommandEvents(wire)
	result := transfer.NewWithCommandEvents(events).Run(ctx, source, intent)
	events.Close()
	return finalFromResult(result)
}

func transferRequest(request transferstart.Request, environment []string) (transfer.Request, error) {
	resolver := namespaceresolvers.DefaultIntent()
	if len(request.DNS()) != 0 {
		var err error
		resolver, err = namespaceresolvers.DirectIntent(request.DNS())
		if err != nil {
			return transfer.Request{}, err
		}
	}
	terminal, pty := request.CommandIO().PTYSize()
	if request.AutomaticWeight() {
		wireNetworks := request.AutomaticNetworks()
		selected := make([]transfer.AutomaticNetworkSelection, len(wireNetworks))
		for index, localIP := range wireNetworks {
			number, _ := transfernumber.New(index + 1)
			selection, err := transfer.NewAutomaticNetworkSelection(number, localIP)
			if err != nil {
				return transfer.Request{}, err
			}
			selected[index] = selection
		}
		settings := request.ThroughputSettings()
		if !pty {
			return transfer.NewPlainAutomaticRequest(request.SourceLabel(), selected, request.ChildArgv(),
				environment, resolver, settings, request.FollowSymlinks())
		}
		return transfer.NewAutomaticRequest(request.SourceLabel(), selected, request.ChildArgv(),
			environment, resolver, terminal, settings, request.FollowSymlinks())
	}
	wireNetworks := request.ManualNetworks()
	selected := make([]transfer.ManualNetworkSelection, len(wireNetworks))
	for index, network := range wireNetworks {
		number, _ := transfernumber.New(index + 1)
		selection, err := transfer.NewManualNetworkSelection(number, network.LocalIP(), network.Weight())
		if err != nil {
			return transfer.Request{}, err
		}
		selected[index] = selection
	}
	if !pty {
		return transfer.NewPlainManualRequest(request.SourceLabel(), selected, request.ChildArgv(),
			environment, resolver, request.FollowSymlinks())
	}
	return transfer.NewManualRequest(request.SourceLabel(), selected, request.ChildArgv(), environment,
		resolver, terminal, request.FollowSymlinks())
}

func childEnvironment(environment []string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, "TERM=") {
			result = append(result, entry)
		}
	}
	return append(result, "TERM=xterm-256color")
}

func finalFromResult(result transfer.Result) runsupervisor.Final {
	final := runsupervisor.Final{Cancelled: result.Cancelled(),
		Transfers: make([]runsupervisor.TransferResult, len(result.Transfers()))}
	if summary, present := result.SourceSummary(); present {
		final.Source = &runsupervisor.SourceSummary{FollowedSymlinks: summary.FollowedSymlinks(),
			IgnoredSymlinks: summary.IgnoredSymlinks(), EmptyDirectories: summary.EmptyDirectories(),
			IgnoredSpecialFiles: summary.IgnoredSpecialFiles()}
	}
	if final.Cancelled {
		final.Reason = runsupervisor.CancelInternal
	}
	for index, value := range result.Transfers() {
		final.Transfers[index] = runsupervisor.TransferResult{Transfer: value.Transfer,
			ExitCode: value.ExitCode, Signal: value.Signal}
	}
	if result.RunError() != nil {
		final.RunError = result.RunError().Error()
	}
	if result.CleanupError() != nil {
		final.CleanupError = result.CleanupError().Error()
	}
	return final
}

func appendCleanupError(existing string, released error) string {
	if released == nil {
		return existing
	}
	if existing == "" {
		return released.Error()
	}
	return existing + "; " + released.Error()
}

func internalFailure(err error) runsupervisor.Final {
	return runsupervisor.Final{Cancelled: true, Reason: runsupervisor.CancelInternal,
		InternalError: err.Error()}
}

type commandEvents struct {
	wire     *runsupervisor.RunSupervisor
	controls chan transfer.CommandControl
	stop     chan struct{}
	done     chan struct{}
}

func newCommandEvents(wire *runsupervisor.RunSupervisor) *commandEvents {
	events := &commandEvents{wire: wire, controls: make(chan transfer.CommandControl, 32),
		stop: make(chan struct{}), done: make(chan struct{})}
	go events.forwardControls()
	return events
}

func (events *commandEvents) SendOutput(value childprocesses.Output) error {
	var stream runsupervisor.OutputStream
	switch value.Stream {
	case childprocesses.StreamPTY:
		stream = runsupervisor.OutputPTY
	case childprocesses.StreamStdout:
		stream = runsupervisor.OutputStdout
	case childprocesses.StreamStderr:
		stream = runsupervisor.OutputStderr
	default:
		return fmt.Errorf("command produced unknown output stream %d", value.Stream)
	}
	return events.wire.SendOutput(value.Transfer, stream, value.Bytes)
}

func (events *commandEvents) SendStatus(value childprocesses.Status) error {
	state := runsupervisor.TransferRunning
	if value.Kind == childprocesses.StatusExited {
		state = runsupervisor.TransferExited
	}
	return events.wire.SendStatus(runsupervisor.TransferStatus{Transfer: value.Transfer, State: state,
		ExitCode: value.ExitCode, Signal: value.Signal})
}

func (events *commandEvents) Controls() <-chan transfer.CommandControl { return events.controls }

func (events *commandEvents) forwardControls() {
	defer close(events.done)
	defer close(events.controls)
	for {
		select {
		case <-events.stop:
			return
		case control, open := <-events.wire.Controls():
			if !open {
				return
			}
			var mapped transfer.CommandControl
			var err error
			switch control.Kind() {
			case "input":
				mapped, err = transfer.InputControl(control.Transfer(), control.Input())
			case "resize":
				columns, rows := control.Size()
				mapped, err = transfer.ResizeControl(control.Transfer(), columns, rows)
			default:
				err = errors.New("supervisor delivered an unknown command control")
			}
			if err != nil {
				return
			}
			select {
			case events.controls <- mapped:
			case <-events.stop:
				return
			}
		}
	}
}

func (events *commandEvents) Close() {
	close(events.stop)
	<-events.done
}
