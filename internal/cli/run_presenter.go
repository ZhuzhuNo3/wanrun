package cli

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"

	"github.com/ZhuzhuNo3/transferlanes/internal/runcommand"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
)

const (
	runExitSuccess   = 0
	runExitRuntime   = 1
	runExitInterrupt = 130
)

type runCompletion interface {
	LiveSessionEstablished() bool
	DisplayMode() (runcommand.DisplayMode, bool)
	Final() (runsupervisor.Final, bool)
	Cancellation() (runsupervisor.CancelReason, bool)
	ParentError() error
	LogError() error
}

func presentRun(stdout, stderr io.Writer, result runCompletion) int {
	mode, modeSelected := result.DisplayMode()
	var writeErr error
	if modeSelected && mode == runcommand.DisplayPlain {
		writeErr = writePlainRunSummary(stdout, stderr, result)
	} else {
		writeErr = writeInteractiveRunDiagnostics(stderr, result)
	}
	if writeErr != nil {
		return runExitRuntime
	}
	return classifyRunCompletion(result)
}

func writePlainRunSummary(stdout, stderr io.Writer, result runCompletion) error {
	final, available := result.Final()
	if !available {
		return writeParentRunFailures(stderr, result.ParentError(), result.LogError())
	}
	var failures []error
	if final.Source != nil {
		failures = append(failures, writeRunLine(stdout,
			"source: followed_symlinks=%d ignored_symlinks=%d empty_directories=%d ignored_special_files=%d",
			final.Source.FollowedSymlinks, final.Source.IgnoredSymlinks,
			final.Source.EmptyDirectories, final.Source.IgnoredSpecialFiles))
	}
	transfers := append([]runsupervisor.TransferResult(nil), final.Transfers...)
	sort.Slice(transfers, func(left, right int) bool {
		return transfers[left].Transfer.Value() < transfers[right].Transfer.Value()
	})
	succeeded := 0
	for _, transfer := range transfers {
		if transfer.ExitCode == 0 && transfer.Signal == 0 {
			succeeded++
		}
		failures = append(failures, writeRunLine(stdout,
			"transfer %d completed: exit=%d signal=%d", transfer.Transfer.Value(),
			transfer.ExitCode, transfer.Signal))
	}
	failures = append(failures, writeRunLine(stdout, "completed: succeeded=%d failed=%d",
		succeeded, len(transfers)-succeeded))
	if final.InternalError != "" {
		failures = append(failures, writeNamedRunFailure(stderr, "internal", final.InternalError))
	}
	if message := runFailureMessage(transfers, final.RunError); message != "" {
		failures = append(failures, writeNamedRunFailure(stderr, "run", message))
	}
	if final.CleanupError != "" {
		failures = append(failures, writeNamedRunFailure(stderr, "cleanup", final.CleanupError))
	} else {
		failures = append(failures, writeRunLine(stdout, "cleanup completed"))
	}
	failures = append(failures,
		writeParentRunFailures(stderr, result.ParentError(), result.LogError()))
	return errors.Join(failures...)
}

func writeInteractiveRunDiagnostics(stderr io.Writer, result runCompletion) error {
	var failures []error
	if final, available := result.Final(); available {
		for _, failure := range []struct{ name, value string }{
			{"internal", final.InternalError}, {"run", final.RunError}, {"cleanup", final.CleanupError},
		} {
			if failure.value != "" {
				failures = append(failures, writeNamedRunFailure(stderr, failure.name, failure.value))
			}
		}
	}
	failures = append(failures,
		writeParentRunFailures(stderr, result.ParentError(), result.LogError()))
	return errors.Join(failures...)
}

func writeParentRunFailures(stderr io.Writer, parentError, logError error) error {
	var failures []error
	if parentError != nil {
		failures = append(failures, writeNamedRunFailure(stderr, "output", parentError.Error()))
	}
	if logError != nil {
		failures = append(failures, writeNamedRunFailure(stderr, "logs", logError.Error()))
	}
	return errors.Join(failures...)
}

func writeNamedRunFailure(stderr io.Writer, name, value string) error {
	_, err := fmt.Fprintf(stderr, "transferlanes: %s: %s\n", name, cleanRunMessage(value))
	return err
}

func writeRunLine(stdout io.Writer, format string, values ...any) error {
	_, err := fmt.Fprintf(stdout, "transferlanes: "+format+"\n", values...)
	return err
}

func runFailureMessage(transfers []runsupervisor.TransferResult, runError string) string {
	var messages []string
	for _, transfer := range transfers {
		if transfer.ExitCode == 0 && transfer.Signal == 0 {
			continue
		}
		messages = append(messages, fmt.Sprintf("transfer %d failed: exit=%d signal=%d",
			transfer.Transfer.Value(), transfer.ExitCode, transfer.Signal))
	}
	if runError != "" {
		messages = append(messages, runError)
	}
	return strings.Join(messages, " ")
}

func classifyRunCompletion(result runCompletion) int {
	if result.ParentError() != nil || result.LogError() != nil {
		return runExitRuntime
	}
	final, available := result.Final()
	reason, cancellation := result.Cancellation()
	if !available {
		if !result.LiveSessionEstablished() && cancellation && reason == runsupervisor.CancelUser {
			return runExitInterrupt
		}
		return runExitRuntime
	}
	if final.InternalError != "" || final.CleanupError != "" {
		return runExitRuntime
	}
	if cancellation && reason == runsupervisor.CancelUser || final.Reason == runsupervisor.CancelUser {
		return runExitInterrupt
	}
	if final.RunError != "" || anyRunTransferFailed(final.Transfers) || final.Cancelled {
		return runExitRuntime
	}
	return runExitSuccess
}

func anyRunTransferFailed(transfers []runsupervisor.TransferResult) bool {
	for _, transfer := range transfers {
		if transfer.ExitCode != 0 || transfer.Signal != 0 {
			return true
		}
	}
	return false
}

func cleanRunMessage(value string) string {
	return strings.TrimSpace(strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value))
}
