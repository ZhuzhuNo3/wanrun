package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ZhuzhuNo3/transferlanes/internal/listnetworks"
	"github.com/ZhuzhuNo3/transferlanes/internal/runcommand"
)

const (
	exitSuccess = 0
	exitRuntime = 1
	exitUsage   = 2
)

type networkListing interface {
	Run(context.Context, listnetworks.Request) (listnetworks.Result, error)
}

type runUseCase interface {
	Run(context.Context, runcommand.Request, io.Reader, io.Writer, io.Writer) (runcommand.Result, error)
}

// Execute owns public parsing, dispatch, presentation, and process-status mapping.
func Execute(argv []string, stdin io.Reader, stdout, stderr io.Writer,
	listing *listnetworks.Listing, runner *runcommand.Command, readVersion func() (string, error),
) int {
	var listUseCase networkListing
	if listing != nil {
		listUseCase = listing
	}
	var runCommand runUseCase
	if runner != nil {
		runCommand = runner
	}
	return execute(argv, stdin, stdout, stderr, listUseCase, runCommand, readVersion)
}

func execute(argv []string, stdin io.Reader, stdout, stderr io.Writer,
	listing networkListing, runner runUseCase, readVersion func() (string, error),
) int {
	parsed, err := Parse(argv)
	if err != nil {
		return writeParsedUsageError(stderr, argv, err)
	}
	switch command := parsed.(type) {
	case Help:
		if _, err := io.WriteString(stdout, command.Text()); err != nil {
			return writeRuntimeError(stderr, fmt.Errorf("write help: %w", err))
		}
		return exitSuccess
	case Version:
		return executeVersion(stdout, stderr, readVersion)
	case List:
		return executeList(command, stdout, stderr, listing)
	case Run:
		if err := runcommand.CheckStartRequestCapacity(command.Request()); err != nil {
			return writeCommandUsageError(stderr, "run", err)
		}
		return executeRun(command, stdin, stdout, stderr, runner)
	default:
		return writeRuntimeError(stderr, errors.New("parsed command is invalid"))
	}
}

func executeVersion(stdout, stderr io.Writer, read func() (string, error)) int {
	if read == nil {
		return writeRuntimeError(stderr, errors.New("build identity is unavailable"))
	}
	value, err := read()
	if err != nil {
		return writeRuntimeError(stderr, fmt.Errorf("read build identity: %w", err))
	}
	if _, err := io.WriteString(stdout, value); err != nil {
		return writeRuntimeError(stderr, fmt.Errorf("write version: %w", err))
	}
	return exitSuccess
}

func executeList(command List, stdout, stderr io.Writer, listing networkListing) int {
	if listing == nil {
		return writeRuntimeError(stderr, errors.New("network list capability is unavailable"))
	}
	request := command.Request()
	result, err := listing.Run(context.Background(), request)
	if err != nil {
		failure := fmt.Errorf("list networks: %w", err)
		var usage *listnetworks.UsageError
		if errors.As(err, &usage) {
			return writeCommandUsageError(stderr, "list", failure)
		}
		return writeRuntimeError(stderr, failure)
	}
	if err := writeNetworkList(stdout, result, request.ProbeRequested(),
		request.MeasureRequested()); err != nil {
		return writeRuntimeError(stderr, fmt.Errorf("write network list: %w", err))
	}
	if result.RuntimeFailure() {
		return exitRuntime
	}
	return exitSuccess
}

func executeRun(command Run, stdin io.Reader, stdout, stderr io.Writer, runner runUseCase) int {
	if runner == nil {
		return writeRuntimeError(stderr, errors.New("supervised transfer capability is unavailable"))
	}
	completion, err := runner.Run(context.Background(), command.Request(), stdin, stdout, stderr)
	if err != nil {
		return writeRuntimeError(stderr, err)
	}
	return presentRun(stdout, stderr, completion)
}

func writeParsedUsageError(stderr io.Writer, argv []string, err error) int {
	if len(argv) != 0 && (argv[0] == "list" || argv[0] == "run") {
		return writeCommandUsageError(stderr, argv[0], err)
	}
	_, _ = fmt.Fprintf(stderr, "transferlanes: %v\n\n%s", err, RootHelpPage.Text())
	return exitUsage
}

func writeCommandUsageError(stderr io.Writer, command string, err error) int {
	_, _ = fmt.Fprintf(stderr, "transferlanes: %v\nRun 'transferlanes %s --help' for usage.\n", err, command)
	return exitUsage
}

func writeRuntimeError(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "transferlanes: %v\n", err)
	return exitRuntime
}
