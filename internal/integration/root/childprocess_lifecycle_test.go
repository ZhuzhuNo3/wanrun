//go:build linux && rootintegration

package root_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"golang.org/x/sys/unix"
)

func newChildProcessRootFixture(t *testing.T) (*hostNetworkFixture, string, []byte) {
	t.Helper()
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	hostResolver, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	return fixture, baseline, hostResolver
}

func TestPlainChildKeepsPipeStreamsSeparateAndReportsSiblingFailure(t *testing.T) {
	fixture, baseline, hostResolver := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "plain")
	observed := scenario.run(t, nil)
	if observed.final.Cancelled || observed.final.InternalError != "" {
		t.Fatalf("plain supervisor final = %#v", observed.final)
	}
	assertRunSupervisorTransferResult(t, observed.final, 1, 7, 0)
	assertRunSupervisorTransferResult(t, observed.final, 2, 0, 0)
	for number, expected := range map[int]struct{ stdout, stderr string }{
		1: {"stdout-one\x00\r\n", "stderr-one\x1b[31m\n"},
		2: {"stdout-two\rprogress\n", "stderr-two\n"},
	} {
		stdout, stdoutErr := os.ReadFile(filepath.Join(scenario.markers,
			fmt.Sprintf("plain-%d.stdout", number)))
		stderr, stderrErr := os.ReadFile(filepath.Join(scenario.markers,
			fmt.Sprintf("plain-%d.stderr", number)))
		if stdoutErr != nil || stderrErr != nil || string(stdout) != expected.stdout ||
			string(stderr) != expected.stderr {
			t.Fatalf("plain transfer %d streams stdout=%q (%v) stderr=%q (%v)",
				number, stdout, stdoutErr, stderr, stderrErr)
		}
		var evidence rootPlainEvidence
		readRunSupervisorJSON(t, filepath.Join(scenario.markers,
			fmt.Sprintf("plain-%d.json", number)), &evidence)
		if evidence.WorkingDirectory != scenario.markers ||
			evidence.Resolver != "nameserver 192.0.2.53\n" || !evidence.ResolverReadOnly ||
			!evidence.StdinEOF || evidence.StdoutTTY || evidence.StderrTTY ||
			!evidence.TemporaryIPv4 {
			t.Errorf("plain transfer %d evidence = %#v", number, evidence)
		}
		for _, descriptor := range evidence.InheritedFDs {
			if strings.HasPrefix(descriptor, "net:[") || strings.HasPrefix(descriptor, "pipe:[") {
				t.Errorf("plain transfer %d inherited protected descriptor %q", number, descriptor)
			}
		}
	}
	if current, err := os.ReadFile("/etc/resolv.conf"); err != nil || string(current) != string(hostResolver) {
		t.Fatalf("plain children changed host resolver: %v", err)
	}
	assertScenarioProcessesGone(t, scenario.markers)
	fixture.assertSurface(baseline)
}

func TestPlainChildBarrierFailureRunsNoCommand(t *testing.T) {
	fixture, baseline, _ := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "plain-barrier-fail")
	observed := scenario.run(t, nil)
	if !strings.Contains(observed.final.InternalError, "prepare transfer 2 before barrier") ||
		len(observed.final.Transfers) != 0 {
		t.Fatalf("plain barrier failure final = %#v", observed.final)
	}
	for _, name := range []string{"should-not-run", "also-should-not-run"} {
		if _, err := os.Stat(filepath.Join(scenario.markers, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("plain barrier failure executed command %s: %v", name, err)
		}
	}
	fixture.assertSurface(baseline)
}

func TestPlainChildCancellationReapsDescendants(t *testing.T) {
	fixture, baseline, _ := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "plain-signals")
	observed := scenario.runAfterLaunch(t, func(client *runsupervisor.RunSupervisorClient) error {
		if err := waitForChildPaths(10*time.Second,
			filepath.Join(scenario.markers, "transfer-1.pid"),
			filepath.Join(scenario.markers, "transfer-2.pid")); err != nil {
			return err
		}
		return client.Cancel(runsupervisor.CancelUser)
	})
	if !observed.final.Cancelled || observed.final.Reason != runsupervisor.CancelUser {
		t.Fatalf("plain cancellation final = %#v", observed.final)
	}
	assertScenarioProcessesGone(t, scenario.markers)
	fixture.assertSurface(baseline)
}

func TestHelperFailureCancelsHelperGroup(t *testing.T) {
	fixture, baseline, _ := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "helper-fail")
	observed := scenario.run(t, nil)
	if observed.final.InternalError == "" {
		t.Fatalf("failed helper group final = %#v", observed.final)
	}
	assertRunSupervisorTransferResult(t, observed.final, 1, 7, 0)
	assertRunSupervisorTransferResult(t, observed.final, 2, -1, int(syscall.SIGKILL))
	signals, err := os.ReadFile(filepath.Join(scenario.markers, "transfer-2.signals"))
	if err != nil || !strings.Contains(string(signals), "interrupt\n") ||
		!strings.Contains(string(signals), "terminated\n") {
		t.Fatalf("helper sibling signal escalation = %q, %v", signals, err)
	}
	assertScenarioProcessesGone(t, scenario.markers)
	fixture.assertSurface(baseline)
}

func TestHelperCancellationIsNotSuccessWhenChildrenExitZero(t *testing.T) {
	fixture, baseline, _ := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "helper-cancel-clean")
	observed := scenario.runAfterLaunch(t, func(client *runsupervisor.RunSupervisorClient) error {
		if err := waitForChildPaths(10*time.Second,
			filepath.Join(scenario.markers, "transfer-1.pid"),
			filepath.Join(scenario.markers, "transfer-2.pid")); err != nil {
			return err
		}
		return client.Cancel(runsupervisor.CancelUser)
	})
	if !observed.final.Cancelled || observed.final.Reason != runsupervisor.CancelUser ||
		!strings.Contains(observed.final.InternalError, "helper group was cancelled") {
		t.Fatalf("clean helper cancellation final = %#v", observed.final)
	}
	assertRunSupervisorTransferResult(t, observed.final, 1, 0, 0)
	assertRunSupervisorTransferResult(t, observed.final, 2, 0, 0)
	for number := 1; number <= 2; number++ {
		received, err := os.ReadFile(filepath.Join(scenario.markers, fmt.Sprintf("transfer-%d.signal", number)))
		if err != nil || string(received) != "interrupt\n" {
			t.Fatalf("transfer %d clean signal = %q, %v", number, received, err)
		}
	}
	assertScenarioProcessesGone(t, scenario.markers)
	fixture.assertSurface(baseline)
}

func TestHelperStderrFloodIsDrainedWithBoundedDiagnostic(t *testing.T) {
	fixture, baseline, _ := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "helper-stderr-flood")
	observed := scenario.run(t, nil)
	if observed.final.InternalError == "" {
		t.Fatalf("helper stderr flood final = %#v", observed.final)
	}
	diagnosticBytes, err := os.ReadFile(filepath.Join(scenario.markers,
		"helper-diagnostic-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	length, err := strconv.Atoi(string(diagnosticBytes))
	if err != nil || length > 2048 {
		t.Fatalf("helper stderr diagnostic bytes = %q, %v", diagnosticBytes, err)
	}
	if content, err := os.ReadFile(filepath.Join(scenario.markers,
		"helper-stderr-drained")); err != nil || string(content) != "drained\n" {
		t.Fatalf("helper stderr was not fully drained: %q, %v", content, err)
	}
	assertScenarioProcessesGone(t, scenario.markers)
	fixture.assertSurface(baseline)
}

func TestRunSupervisorControlBackpressurePreservesControlsAndLifelineCleanup(t *testing.T) {
	fixture, baseline, _ := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "control-backpressure")
	var once sync.Once
	observed := scenario.run(t, func(client *runsupervisor.RunSupervisorClient, event runsupervisor.Event) {
		if event.Kind() != runsupervisor.EventStatus || event.Status().State != runsupervisor.TransferRunning {
			return
		}
		once.Do(func() {
			accepted := make(chan struct{})
			writerDone := make(chan error, 1)
			go func() {
				var acceptedOnce sync.Once
				for range 4096 {
					if err := client.Resize(event.Status().Transfer, 80, 24); err != nil {
						writerDone <- err
						return
					}
					acceptedOnce.Do(func() { close(accepted) })
				}
				writerDone <- nil
			}()
			<-accepted
			if err := os.WriteFile(filepath.Join(scenario.markers, "release-controls"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := <-writerDone; err != nil {
				t.Fatal(err)
			}
			_ = client.CloseLifeline()
		})
	})
	if !observed.final.Cancelled || observed.final.Reason != runsupervisor.CancelLifeline ||
		observed.final.InternalError != "" {
		t.Fatalf("control backpressure final = %#v", observed.final)
	}
	for number := 1; number <= 2; number++ {
		assertRunSupervisorTransferResult(t, observed.final, number, -1, int(syscall.SIGKILL))
		signals, err := os.ReadFile(filepath.Join(scenario.markers, fmt.Sprintf("transfer-%d.signals", number)))
		if err != nil || !strings.Contains(string(signals), "interrupt\n") ||
			!strings.Contains(string(signals), "terminated\n") {
			t.Fatalf("transfer %d backpressure cancellation signals = %q, %v", number, signals, err)
		}
	}
	assertScenarioProcessesGone(t, scenario.markers)
	fixture.assertSurface(baseline)
}

func TestPTYChildAppliesResizeAndInputBeforeReportingResult(t *testing.T) {
	fixture, baseline, hostResolver := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "normal")
	var ready sync.Once
	var readyOutput string
	observed := scenario.run(t, func(client *runsupervisor.RunSupervisorClient, event runsupervisor.Event) {
		if event.Kind() == runsupervisor.EventOutput && event.Stream() == runsupervisor.OutputPTY {
			readyOutput += string(event.Bytes())
		}
		if strings.Contains(readyOutput, "child-pty-ready") {
			ready.Do(func() {
				if err := client.Resize(event.Transfer(), 101, 47); err != nil {
					t.Fatal(err)
				}
				if err := client.WriteInput(event.Transfer(), []byte("exact-input\n")); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	assertRunSupervisorTransferResult(t, observed.final, 1, 0, 0)
	assertRunSupervisorTransferResult(t, observed.final, 2, 7, 0)
	if observed.final.Cancelled || !strings.Contains(observed.pty, "child-pty-ready") ||
		!strings.Contains(observed.pty, "child-pty-resize-accepted") {
		t.Fatalf("normal supervisor events = final %#v, pty %q", observed.final, observed.pty)
	}
	var evidence rootPTYEvidence
	readRunSupervisorJSON(t, filepath.Join(scenario.markers, "transfer-1.json"), &evidence)
	if evidence.Cols != 101 || evidence.Rows != 47 || evidence.Input != "exact-input\n" ||
		evidence.WorkingDirectory != scenario.markers || evidence.Resolver != "nameserver 192.0.2.53\n" ||
		!evidence.ResolverReadOnly || !evidence.TemporaryIPv4 || evidence.PID != evidence.PGID ||
		evidence.PID != evidence.SID {
		t.Fatalf("PTY/namespace evidence = %#v", evidence)
	}
	if len(evidence.Argv) != 6 || evidence.Argv[4] != "argument with spaces" ||
		!strings.HasPrefix(evidence.Argv[5], "$(touch ") {
		t.Fatalf("direct argv evidence = %q", evidence.Argv)
	}
	for _, descriptor := range evidence.InheritedFDs {
		if strings.HasPrefix(descriptor, "net:[") || strings.HasPrefix(descriptor, "pipe:[") {
			t.Errorf("interactive transfer inherited protected descriptor %q", descriptor)
		}
	}
	if _, err := os.Stat(filepath.Join(scenario.markers, "shell-side-effect")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("command argv was interpreted by a shell: %v", err)
	}
	if _, err := os.Stat(filepath.Join(scenario.markers, "transfer-2.finished")); err != nil {
		t.Fatalf("failing sibling did not record its independent completion: %v", err)
	}
	if current, err := os.ReadFile("/etc/resolv.conf"); err != nil || string(current) != string(hostResolver) {
		t.Fatalf("child resolver changed host resolver: %v", err)
	}
	fixture.assertSurface(baseline)
}

func TestInteractiveChildCancellationReapsProcesses(t *testing.T) {
	for _, cancellation := range []struct {
		name   string
		reason runsupervisor.CancelReason
		act    func(*runsupervisor.RunSupervisorClient) error
	}{
		{"user-cancel", runsupervisor.CancelUser, func(client *runsupervisor.RunSupervisorClient) error {
			return client.Cancel(runsupervisor.CancelUser)
		}},
		{"lifeline-eof", runsupervisor.CancelLifeline, func(client *runsupervisor.RunSupervisorClient) error {
			return client.CloseLifeline()
		}},
	} {
		t.Run(cancellation.name, func(t *testing.T) {
			fixture, baseline, _ := newChildProcessRootFixture(t)
			scenario := fixture.newRunSupervisorScenario(t, "signals")
			var once sync.Once
			observed := scenario.run(t, func(client *runsupervisor.RunSupervisorClient, event runsupervisor.Event) {
				if event.Kind() == runsupervisor.EventStatus && event.Status().State == runsupervisor.TransferRunning {
					once.Do(func() {
						waitForPath(t, filepath.Join(scenario.markers, "transfer-1.pid"))
						if err := cancellation.act(client); err != nil {
							t.Error(err)
						}
					})
				}
			})
			if !observed.final.Cancelled || observed.final.Reason != cancellation.reason {
				t.Fatalf("cancellation final = %#v", observed.final)
			}
			for number := 1; number <= 2; number++ {
				assertRunSupervisorTransferResult(t, observed.final, number, -1, int(syscall.SIGKILL))
				signals, err := os.ReadFile(filepath.Join(scenario.markers, fmt.Sprintf("transfer-%d.signals", number)))
				if err != nil || !strings.Contains(string(signals), "interrupt\n") ||
					!strings.Contains(string(signals), "terminated\n") {
					t.Fatalf("transfer %d signal escalation = %q, %v", number, signals, err)
				}
			}
			assertScenarioProcessesGone(t, scenario.markers)
			fixture.assertSurface(baseline)
		})
	}
}

func TestProcessHandleOwnershipDoesNotGrowSupervisorDescriptors(t *testing.T) {
	fixture, baseline, _ := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "process-handle-cycles")
	observed := scenario.run(t, nil)
	if observed.final.Cancelled || observed.final.InternalError != "" {
		t.Fatalf("process-handle cycles final=%#v", observed.final)
	}
	assertScenarioProcessesGone(t, scenario.markers)
	fixture.assertSurface(baseline)
}

func TestDirectCompletionTerminatesLongLivedOrphan(t *testing.T) {
	fixture, baseline, _ := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "long-orphan")
	client, err := launchScenarioRunSupervisor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	completed := make(chan struct {
		observed observedRunSupervisor
		err      error
	}, 1)
	go func() {
		observed, observeErr := observeRunSupervisor(client)
		completed <- struct {
			observed observedRunSupervisor
			err      error
		}{observed: observed, err: observeErr}
	}()
	var outcome struct {
		observed observedRunSupervisor
		err      error
	}
	select {
	case outcome = <-completed:
	case <-time.After(10 * time.Second):
		_ = client.Cancel(runsupervisor.CancelUser)
		outcome = <-completed
		t.Fatalf("direct completion did not terminate its long-lived orphan; final=%#v error=%v",
			outcome.observed.final, outcome.err)
	}
	if outcome.err != nil || outcome.observed.final.Cancelled ||
		outcome.observed.final.InternalError != "" {
		t.Fatalf("long orphan final=%#v error=%v", outcome.observed.final, outcome.err)
	}
	assertRunSupervisorTransferResult(t, outcome.observed.final, 1, 0, 0)
	assertScenarioProcessesGone(t, scenario.markers)
	fixture.assertSurface(baseline)
}

func TestManyAdoptedDescendantsPreserveDirectResult(t *testing.T) {
	fixture, baseline, _ := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "descendant-stress")
	observed := scenario.run(t, nil)
	if observed.final.Cancelled || observed.final.InternalError != "" || len(observed.final.Transfers) != 1 {
		t.Fatalf("descendant stress final = %#v", observed.final)
	}
	assertRunSupervisorTransferResult(t, observed.final, 1, 0, 0)
	contents, err := os.ReadFile(filepath.Join(scenario.markers, "descendant-stress.finished"))
	if err != nil || strings.TrimSpace(string(contents)) != "256" {
		t.Fatalf("descendant stress evidence = %q, %v", contents, err)
	}
	assertScenarioProcessesGone(t, scenario.markers)
	fixture.assertSurface(baseline)
}

func TestCLISIGKILLLifelineCleansContainedProcesses(t *testing.T) {
	fixture, baseline, _ := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "signals")
	pidFile := filepath.Join(scenario.markers, "runsupervisor.pid")
	argv := containmentChildCommand(os.Args[0], containmentChildCLIProxy, pidFile)
	proxy := exec.Command(argv[0], argv[1:]...)
	proxy.Env = os.Environ()
	proxy.Stderr = os.Stderr
	if err := proxy.Start(); err != nil {
		t.Fatal(err)
	}
	supervisorPID := waitPIDFile(t, pidFile)
	waitForPath(t, filepath.Join(scenario.markers, "transfer-1.pid"))
	if err := proxy.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = proxy.Wait()
	reapAdoptedProcess(t, supervisorPID)
	assertScenarioProcessesGone(t, scenario.markers)
	waitForSurface(t, fixture, baseline)
}

func TestSupervisorSIGKILLUsesKernelContainmentBeforeRecovery(t *testing.T) {
	fixture, baseline, _ := newChildProcessRootFixture(t)
	scenario := fixture.newRunSupervisorScenario(t, "signals")
	client, err := launchScenarioRunSupervisor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for {
		event, err := client.Next()
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind() == runsupervisor.EventStatus && event.Status().State == runsupervisor.TransferRunning {
			break
		}
	}
	waitForPath(t, filepath.Join(scenario.markers, "transfer-1.pid"))
	if err := unix.Kill(client.PID(), unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := client.Wait(); err == nil {
		t.Fatal("SIGKILLed supervisor exited successfully")
	}
	assertScenarioProcessesGone(t, scenario.markers)
	recoverRunSupervisorRun(t, scenario.id)
	fixture.assertSurface(baseline)
}

func observeRunSupervisor(client *runsupervisor.RunSupervisorClient) (observedRunSupervisor, error) {
	var observed observedRunSupervisor
	for {
		event, err := client.Next()
		if err != nil {
			return observed, err
		}
		switch event.Kind() {
		case runsupervisor.EventOutput:
			if event.Stream() == runsupervisor.OutputPTY {
				observed.pty += string(event.Bytes())
			}
		case runsupervisor.EventStatus:
			observed.statuses = append(observed.statuses, event.Status())
		case runsupervisor.EventFinal:
			observed.final = event.Final()
			closeErr := client.CloseLifeline()
			return observed, errors.Join(closeErr, client.Wait())
		}
	}
}
