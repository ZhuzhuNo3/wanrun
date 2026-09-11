//go:build linux && rootintegration

package root_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestWorkPanicContainsDescendantsBeforeFinal(t *testing.T) {
	requireRoot(t)
	markers := t.TempDir()
	source := filepath.Join(markers, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(supervisorScenarioEnv, "panic-descendant")
	t.Setenv(supervisorMarkerEnv, markers)
	t.Setenv(supervisorTransferEnv, source)

	client, err := launchScenarioRunSupervisor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := client.Next()
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind() != runsupervisor.EventFinal {
			continue
		}
		final := event.Final()
		if !final.Cancelled || final.Reason != runsupervisor.CancelInternal ||
			!strings.Contains(final.InternalError, "root integration work panic") ||
			!strings.Contains(final.InternalError, "left live descendants") {
			t.Fatalf("panic final = %#v", final)
		}
		assertScenarioProcessesGone(t, markers)
		if err := client.CloseLifeline(); err != nil {
			t.Fatal(err)
		}
		if err := client.Wait(); err != nil {
			t.Fatal(err)
		}
		return
	}
}

func TestRunSupervisorFinalWireNormalization(t *testing.T) {
	for _, test := range []struct {
		name     string
		scenario string
		assert   func(*testing.T, runsupervisor.Final)
	}{
		{name: "bounded UTF-8 diagnostic", scenario: "diagnostic", assert: func(t *testing.T, final runsupervisor.Final) {
			if !final.Cancelled || final.Reason != runsupervisor.CancelInternal ||
				len(final.InternalError) > 1024 || !utf8.ValidString(final.InternalError) {
				t.Fatalf("normalized root final = %#v", final)
			}
			assertRunSupervisorTransferResult(t, final, 1, 7, 0)
		}},
		{name: "encoding fallback", scenario: "diagnostic-fallback", assert: func(t *testing.T, final runsupervisor.Final) {
			if !final.Cancelled || final.Reason != runsupervisor.CancelUser ||
				!strings.Contains(final.InternalError, "encode supervisor final") ||
				!utf8.ValidString(final.InternalError) || len(final.Transfers) != 2 {
				t.Fatalf("fallback root final = %#v", final)
			}
			assertRunSupervisorTransferResult(t, final, 1, 0, 0)
			assertRunSupervisorTransferResult(t, final, 2, -1, int(syscall.SIGTERM))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, baseline, _ := newChildProcessRootFixture(t)
			scenario := fixture.newRunSupervisorScenario(t, test.scenario)
			observed := scenario.run(t, nil)
			test.assert(t, observed.final)
			fixture.assertSurface(baseline)
		})
	}
}

func TestInteractiveBarrierPreparationFailureRunsNoCommand(t *testing.T) {
	fixture, baseline, _ := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "barrier-fail")
	observed := scenario.run(t, nil)
	if !strings.Contains(observed.final.InternalError, "prepare transfer 2 before barrier") ||
		len(observed.final.Transfers) != 0 {
		t.Fatalf("barrier failure final = %#v", observed.final)
	}
	for _, name := range []string{"should-not-run", "also-should-not-run"} {
		if _, err := os.Stat(filepath.Join(scenario.markers, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("barrier failure executed user command %s: %v", name, err)
		}
	}
	fixture.assertSurface(baseline)
}

func TestNonExecutableIsRejectedBeforeBarrier(t *testing.T) {
	fixture, baseline, _ := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "preexec-fail")
	observed := scenario.run(t, nil)
	if !strings.Contains(observed.final.InternalError, "prepare transfer 2 before barrier") ||
		len(observed.final.Transfers) != 0 || len(observed.statuses) != 0 {
		t.Fatalf("pre-exec failure final = %#v, statuses = %#v", observed.final, observed.statuses)
	}
	if _, err := os.Stat(filepath.Join(scenario.markers, "preexec-should-not-run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-exec failure crossed barrier: %v", err)
	}
	assertScenarioProcessesGone(t, scenario.markers)
	fixture.assertSurface(baseline)
}

func TestExecFailureAfterBarrierCancelsValidSibling(t *testing.T) {
	fixture, baseline, _ := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "exec-fail")
	observed := scenario.run(t, func(client *runsupervisor.RunSupervisorClient, event runsupervisor.Event) {
		if event.Kind() == runsupervisor.EventStatus && event.Status().Transfer.Value() == 2 &&
			event.Status().State == runsupervisor.TransferExited {
			_ = client.Cancel(runsupervisor.CancelUser)
		}
	})
	if !strings.Contains(observed.final.InternalError, "exec exact child argv") ||
		len(observed.final.Transfers) != 0 {
		t.Fatalf("exec handoff failure final = %#v, statuses = %#v", observed.final, observed.statuses)
	}
	assertScenarioProcessesGone(t, scenario.markers)
	fixture.assertSurface(baseline)
}

func TestExecFailureRetainsSiblingOutput(t *testing.T) {
	for _, test := range []struct {
		name     string
		scenario string
		plain    bool
	}{
		{name: "interactive output is preserved", scenario: "exec-fail-output"},
		{name: "plain output is preserved", scenario: "plain-exec-fail-output", plain: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, baseline, _ := newChildProcessRootFixture(t)
			const expected = "unique-exec-failure-output"
			scenario := fixture.newRunSupervisorScenario(t, test.scenario)
			observed := scenario.run(t, nil)
			if _, err := os.Stat(filepath.Join(scenario.markers, "exec-output.produced")); err != nil {
				t.Fatalf("valid sibling did not produce output before exec failure: %v", err)
			}
			output := observed.pty
			if test.plain {
				content, err := os.ReadFile(filepath.Join(scenario.markers, "plain-1.stdout"))
				if err != nil {
					t.Fatalf("read retained plain output: %v", err)
				}
				output = string(content)
			}
			if strings.Count(output, expected) != 1 ||
				!strings.Contains(observed.final.InternalError, "exec exact child argv") {
				t.Fatalf("exec failure output=%q final=%#v", output, observed.final)
			}
			assertScenarioProcessesGone(t, scenario.markers)
			fixture.assertSurface(baseline)
		})
	}
}

func supervisorDiagnosticFinal(scenario diagnosticSupervisorScenario) (runsupervisor.Final, bool) {
	transfer1, _ := transfernumber.New(1)
	transfer2, _ := transfernumber.New(2)
	transfer3, _ := transfernumber.New(3)
	switch scenario {
	case diagnosticSupervisorFinal:
		return runsupervisor.Final{Cancelled: true, Reason: runsupervisor.CancelInternal,
			InternalError: strings.Repeat("界", 400) + string([]byte{0xff}),
			Transfers:     []runsupervisor.TransferResult{{Transfer: transfer1, ExitCode: 7}}}, true
	case diagnosticSupervisorFallback:
		return runsupervisor.Final{Cancelled: true, Reason: runsupervisor.CancelUser,
			InternalError: strings.Repeat("original failure; ", 80) + string([]byte{0xff}),
			Transfers: []runsupervisor.TransferResult{{Transfer: transfer1, ExitCode: 0},
				{Transfer: transfer2, ExitCode: -1, Signal: int(syscall.SIGTERM)},
				{Transfer: transfer3, ExitCode: 1, Signal: int(syscall.SIGKILL)}}}, true
	default:
		return runsupervisor.Final{}, false
	}
}
