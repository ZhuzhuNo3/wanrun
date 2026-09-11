package transfer

import (
	"context"
	"errors"
	"fmt"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

// Result preserves user-process evidence separately from orchestration and cleanup failures.
type Result struct {
	id               runid.ID
	hasRun           bool
	transfers        []childprocesses.TransferResult
	cancelled        bool
	runErr           error
	cleanupErr       error
	sourceSummary    sourcefiles.SourceSummary
	hasSourceSummary bool
}

// RunID returns the identity once a live run root was acquired.
func (result Result) RunID() (runid.ID, bool) { return result.id, result.hasRun }

// Transfers returns every trustworthy direct-command result collected before cleanup.
func (result Result) Transfers() []childprocesses.TransferResult {
	return append([]childprocesses.TransferResult(nil), result.transfers...)
}

// Cancelled reports a whole-group cancellation, not an individual non-zero exit.
func (result Result) Cancelled() bool { return result.cancelled }

// RunError returns preparation, execution supervision, result collection, or cancellation errors.
func (result Result) RunError() error { return result.runErr }

// CleanupError reports the first cleanup boundary that could not be confirmed.
func (result Result) CleanupError() error { return result.cleanupErr }

// SourceSummary returns frozen ignored-entry counts once source membership was captured.
func (result Result) SourceSummary() (sourcefiles.SourceSummary, bool) {
	return result.sourceSummary, result.hasSourceSummary
}

// Err gives cleanup failure precedence while retaining the original run failure in the chain.
func (result Result) Err() error {
	if result.cleanupErr != nil {
		return errors.Join(fmt.Errorf("transfer cleanup failed: %w", result.cleanupErr), result.runErr)
	}
	return result.runErr
}

func resultWithSourceSummary(result Result, snapshot sourcefiles.SourceSnapshot, captured bool) Result {
	if captured {
		result.sourceSummary = snapshot.Summary()
		result.hasSourceSummary = true
	}
	return result
}

func transferResult(request Request, processResult childprocesses.Result, waitErr error,
	runContext context.Context,
) ([]childprocesses.TransferResult, bool, error) {
	transfers, evidenceErr := orderedTransferResults(request.selectedNetworks(), processResult.Transfers)
	var failures []error
	failures = append(failures, waitErr, evidenceErr)
	if processResult.Cancelled {
		cause := context.Cause(runContext)
		if cause == nil {
			cause = errors.New("child process group was cancelled")
		}
		failures = append(failures, cause)
	}
	return transfers, processResult.Cancelled, errors.Join(failures...)
}

func orderedTransferResults(selected []selectedNetwork,
	results []childprocesses.TransferResult,
) ([]childprocesses.TransferResult, error) {
	if len(results) != len(selected) {
		return nil, errors.New("child process result set is incomplete or contains extras")
	}
	byTransfer := make(map[transfernumber.Number]childprocesses.TransferResult, len(results))
	for _, result := range results {
		if _, duplicate := byTransfer[result.Transfer]; duplicate {
			return nil, fmt.Errorf("child process result repeats transfer %d", result.Transfer.Value())
		}
		byTransfer[result.Transfer] = result
	}
	ordered := make([]childprocesses.TransferResult, len(selected))
	for index, value := range selected {
		result, exists := byTransfer[value.transfer]
		if !exists {
			return nil, fmt.Errorf("child process result omits transfer %d", value.transfer.Value())
		}
		ordered[index] = result
	}
	return ordered, nil
}
