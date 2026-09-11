package cli

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/runcommand"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type presenterResult struct {
	live         bool
	mode         runcommand.DisplayMode
	modeSelected bool
	final        runsupervisor.Final
	finalSet     bool
	reason       runsupervisor.CancelReason
	reasonSet    bool
	parentErr    error
	logErr       error
}

func (result presenterResult) LiveSessionEstablished() bool { return result.live }
func (result presenterResult) DisplayMode() (runcommand.DisplayMode, bool) {
	return result.mode, result.modeSelected
}
func (result presenterResult) Final() (runsupervisor.Final, bool) {
	return result.final.Clone(), result.finalSet
}
func (result presenterResult) Cancellation() (runsupervisor.CancelReason, bool) {
	return result.reason, result.reasonSet
}
func (result presenterResult) ParentError() error { return result.parentErr }
func (result presenterResult) LogError() error    { return result.logErr }

func TestRunExitPriority(t *testing.T) {
	id, _ := transfernumber.New(1)
	failure := runsupervisor.TransferResult{Transfer: id, ExitCode: 7}
	tests := []struct {
		name   string
		result presenterResult
		want   int
	}{
		{name: "success", result: presenterResult{live: true, finalSet: true}, want: 0},
		{name: "parent failure wins", result: presenterResult{live: true, finalSet: true,
			parentErr: errors.New("display"), reason: runsupervisor.CancelUser, reasonSet: true}, want: 1},
		{name: "log failure wins", result: presenterResult{live: true, finalSet: true,
			logErr: errors.New("log"), reason: runsupervisor.CancelUser, reasonSet: true}, want: 1},
		{name: "preparation SIGINT", result: presenterResult{reason: runsupervisor.CancelUser,
			reasonSet: true}, want: 130},
		{name: "preparation SIGTERM", result: presenterResult{reason: runsupervisor.CancelTerminate,
			reasonSet: true}, want: 1},
		{name: "live missing final", result: presenterResult{live: true, reason: runsupervisor.CancelUser,
			reasonSet: true}, want: 1},
		{name: "internal wins user", result: presenterResult{live: true, finalSet: true,
			reason: runsupervisor.CancelUser, reasonSet: true,
			final: runsupervisor.Final{InternalError: "broken"}}, want: 1},
		{name: "cleanup wins user", result: presenterResult{live: true, finalSet: true,
			reason: runsupervisor.CancelUser, reasonSet: true,
			final: runsupervisor.Final{CleanupError: "broken"}}, want: 1},
		{name: "user wins run failure", result: presenterResult{live: true, finalSet: true,
			reason: runsupervisor.CancelUser, reasonSet: true,
			final: runsupervisor.Final{RunError: "cancelled", Transfers: []runsupervisor.TransferResult{failure}}}, want: 130},
		{name: "final user reason", result: presenterResult{live: true, finalSet: true,
			final: runsupervisor.Final{Cancelled: true, Reason: runsupervisor.CancelUser,
				RunError: "cancelled"}}, want: 130},
		{name: "non-user cancellation", result: presenterResult{live: true, finalSet: true,
			final: runsupervisor.Final{Cancelled: true, Reason: runsupervisor.CancelTerminate}}, want: 1},
		{name: "transfer failure", result: presenterResult{live: true, finalSet: true,
			final: runsupervisor.Final{Transfers: []runsupervisor.TransferResult{failure}}}, want: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyRunCompletion(test.result); got != test.want {
				t.Fatalf("exit=%d, want %d", got, test.want)
			}
		})
	}
}

func TestPlainRunSummaryIsStableAndAbsentFinalDoesNotFabricateEvidence(t *testing.T) {
	first, _ := transfernumber.New(1)
	second, _ := transfernumber.New(2)
	result := presenterResult{live: true, mode: runcommand.DisplayPlain, modeSelected: true,
		finalSet: true, parentErr: errors.New("display failed"), logErr: errors.New("log close failed"),
		final: runsupervisor.Final{Source: &runsupervisor.SourceSummary{FollowedSymlinks: 1,
			IgnoredSymlinks: 2, EmptyDirectories: 3, IgnoredSpecialFiles: 4},
			RunError: "supervision failed", CleanupError: "cleanup warning",
			Transfers: []runsupervisor.TransferResult{{Transfer: second, ExitCode: 12}, {Transfer: first}}}}
	var stdout, stderr bytes.Buffer
	if exit := presentRun(&stdout, &stderr, result); exit != 1 {
		t.Fatalf("exit=%d", exit)
	}
	wantStdout := "transferlanes: source: followed_symlinks=1 ignored_symlinks=2 empty_directories=3 ignored_special_files=4\n" +
		"transferlanes: transfer 1 completed: exit=0 signal=0\n" +
		"transferlanes: transfer 2 completed: exit=12 signal=0\n" +
		"transferlanes: completed: succeeded=1 failed=1\n"
	wantStderr := "transferlanes: run: transfer 2 failed: exit=12 signal=0 supervision failed\n" +
		"transferlanes: cleanup: cleanup warning\n" +
		"transferlanes: output: display failed\n" +
		"transferlanes: logs: log close failed\n"
	if stdout.String() != wantStdout || stderr.String() != wantStderr {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	absent := presenterResult{live: true, mode: runcommand.DisplayPlain, modeSelected: true,
		parentErr: errors.New("supervisor event pipe closed")}
	if exit := presentRun(&stdout, &stderr, absent); exit != 1 || stdout.Len() != 0 ||
		stderr.String() != "transferlanes: output: supervisor event pipe closed\n" {
		t.Fatalf("absent final exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
}

func TestInteractiveRunWritesOnlyDiagnosticsInStableOrder(t *testing.T) {
	result := presenterResult{live: true, mode: runcommand.DisplayInteractive, modeSelected: true,
		finalSet: true, parentErr: errors.New("output failure"), logErr: errors.New("log failure"),
		final: runsupervisor.Final{InternalError: "internal failure", RunError: "run failure",
			CleanupError: "cleanup failure"}}
	var stdout, stderr bytes.Buffer
	if exit := presentRun(&stdout, &stderr, result); exit != 1 || stdout.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q", exit, stdout.String())
	}
	want := "transferlanes: internal: internal failure\ntransferlanes: run: run failure\n" +
		"transferlanes: cleanup: cleanup failure\ntransferlanes: output: output failure\ntransferlanes: logs: log failure\n"
	if stderr.String() != want {
		t.Fatalf("stderr=%q, want %q", stderr.String(), want)
	}
}
