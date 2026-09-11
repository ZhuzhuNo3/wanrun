//go:build linux && rootintegration

package root_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/namespaceresolvers"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfer"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

type transferRootScenario uint8

const (
	transferManual transferRootScenario = iota + 1
	transferPlain
	transferAutomatic
	transferAutomaticFailure
	transferBarrierFailure
	transferOneFailure
	transferCancel
	transferRsyncReplacement
	transferCleanupBlock
	transferOutputBackpressure
)

func parseTransferRootScenario(value string) (transferRootScenario, bool) {
	byName := map[string]transferRootScenario{
		"transfer-manual":              transferManual,
		"transfer-plain":               transferPlain,
		"transfer-auto":                transferAutomatic,
		"transfer-auto-fail":           transferAutomaticFailure,
		"transfer-barrier-fail":        transferBarrierFailure,
		"transfer-one-fails":           transferOneFailure,
		"transfer-cancel":              transferCancel,
		"transfer-rsync-replacement":   transferRsyncReplacement,
		"transfer-cleanup-block":       transferCleanupBlock,
		"transfer-output-backpressure": transferOutputBackpressure,
	}
	scenario, ok := byName[value]
	return scenario, ok
}

func runTransferRootFixture(ctx context.Context, wire *runsupervisor.RunSupervisor,
	scenario transferRootScenario,
) runsupervisor.Final {
	sources, err := parseAddresses(os.Getenv(supervisorSourcesEnv))
	if err != nil {
		return internalRunSupervisorFinal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		return internalRunSupervisorFinal(err)
	}
	size, _ := childprocesses.NewTerminalSize(80, 24)
	markers := os.Getenv(supervisorMarkerEnv)
	argv := transferChildCommand(executable, transferChildView,
		filepath.Join(markers, "transfer-views.jsonl"), "{}")
	switch scenario {
	case transferPlain:
		argv = plainChildCommandLine(executable, plainChildPlain,
			filepath.Join(markers, "plain-evidence"), "0",
			base64.StdEncoding.EncodeToString([]byte("plain-stdout\x00\r\n")),
			base64.StdEncoding.EncodeToString([]byte("plain-stderr\x1b[31m\n")), "{}")
	case transferBarrierFailure:
		argv = []string{filepath.Join(markers, "missing-executable"), "{}"}
	case transferOneFailure:
		argv = transferChildCommand(executable, transferChildViewOneFails,
			filepath.Join(markers, "transfer-views.jsonl"), "{}")
	case transferCancel:
		argv = transferChildCommand(executable, transferChildBlock,
			filepath.Join(markers, "transfer-block"), "{}")
	case transferRsyncReplacement:
		argv = transferChildCommand(executable, transferChildRsyncWait,
			markers, filepath.Join(markers, "rsync-destination"), "{}")
	case transferCleanupBlock:
		argv = transferChildCommand(executable, transferChildCleanupBlock, markers, "{}")
	case transferOutputBackpressure:
		argv = []string{"/bin/sh", "-c", `printf 'pid=%s\nview=%s\n' "$$" "$3" > "$1.$$"; while [ ! -e "$2" ]; do sleep 0.01; done; exec head -c 67108864 /dev/zero`,
			"transferlanes-output-backpressure", filepath.Join(markers, "output-stage"),
			filepath.Join(markers, "output.release"), "{}"}
	}
	request, err := transferRootRequest(scenario, sources, argv, size)
	if err != nil {
		return internalRunSupervisorFinal(err)
	}
	source, err := wire.TakeSourceRoot(os.Getenv(supervisorTransferEnv))
	if err != nil {
		return internalRunSupervisorFinal(err)
	}
	defer source.Close()
	result := transfer.NewWithCommandEvents(rootRunSupervisorTransferEvents{wire: wire}).Run(ctx, source, request)
	final := runsupervisor.Final{Cancelled: result.Cancelled(),
		Transfers: make([]runsupervisor.TransferResult, len(result.Transfers()))}
	if final.Cancelled {
		final.Reason = runsupervisor.CancelInternal
	}
	if summary, present := result.SourceSummary(); present {
		final.Source = &runsupervisor.SourceSummary{FollowedSymlinks: summary.FollowedSymlinks(),
			IgnoredSymlinks: summary.IgnoredSymlinks(), EmptyDirectories: summary.EmptyDirectories(),
			IgnoredSpecialFiles: summary.IgnoredSpecialFiles()}
	}
	for index, child := range result.Transfers() {
		final.Transfers[index] = runsupervisor.TransferResult{Transfer: child.Transfer, ExitCode: child.ExitCode,
			Signal: child.Signal}
	}
	if result.RunError() != nil {
		final.RunError = result.RunError().Error()
	}
	if result.CleanupError() != nil {
		final.CleanupError = result.CleanupError().Error()
	}
	return final
}

type rootRunSupervisorTransferEvents struct{ wire *runsupervisor.RunSupervisor }

func (events rootRunSupervisorTransferEvents) SendOutput(output childprocesses.Output) error {
	stream := runsupervisor.OutputPTY
	switch output.Stream {
	case childprocesses.StreamStdout:
		stream = runsupervisor.OutputStdout
	case childprocesses.StreamStderr:
		stream = runsupervisor.OutputStderr
	case childprocesses.StreamPTY:
	default:
		return errors.New("root transfer produced an unknown output stream")
	}
	return events.wire.SendOutput(output.Transfer, stream, output.Bytes)
}

func (events rootRunSupervisorTransferEvents) SendStatus(status childprocesses.Status) error {
	state := runsupervisor.TransferRunning
	if status.Kind == childprocesses.StatusExited {
		state = runsupervisor.TransferExited
	}
	return events.wire.SendStatus(runsupervisor.TransferStatus{Transfer: status.Transfer, State: state,
		ExitCode: status.ExitCode, Signal: status.Signal})
}

func (rootRunSupervisorTransferEvents) Controls() <-chan transfer.CommandControl { return nil }

func transferRootRequest(scenario transferRootScenario, sources []netip.Addr, argv []string,
	size childprocesses.TerminalSize,
) (transfer.Request, error) {
	sourceRoot := os.Getenv(supervisorTransferEnv)
	resolver := namespaceresolvers.DefaultIntent()
	if scenario == transferAutomatic || scenario == transferAutomaticFailure {
		selected := make([]transfer.AutomaticNetworkSelection, len(sources))
		for index, source := range sources {
			id, _ := transfernumber.New(index + 1)
			var err error
			selected[index], err = transfer.NewAutomaticNetworkSelection(id, source)
			if err != nil {
				return transfer.Request{}, err
			}
		}
		window, err := throughput.NewSettings(os.Getenv(supervisorMeasureEnv), rootMeasureDuration)
		if err != nil {
			return transfer.Request{}, err
		}
		return transfer.NewAutomaticRequest(sourceRoot, selected, argv, supervisorChildEnvironment(),
			resolver, size, window, false)
	}
	selected := make([]transfer.ManualNetworkSelection, len(sources))
	for index, source := range sources {
		id, _ := transfernumber.New(index + 1)
		var err error
		selected[index], err = transfer.NewManualNetworkSelection(id, source, 1)
		if err != nil {
			return transfer.Request{}, err
		}
	}
	if scenario == transferPlain {
		return transfer.NewPlainManualRequest(sourceRoot, selected, argv,
			supervisorChildEnvironment(), resolver, false)
	}
	return transfer.NewManualRequest(sourceRoot, selected, argv, supervisorChildEnvironment(), resolver, size, false)
}

func TestTransferDirectoryPlainStreamsAcrossRunSupervisor(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "transfer-plain")
	observed := scenario.run(t, nil)
	if observed.final.Cancelled || observed.final.InternalError != "" || observed.final.RunError != "" ||
		observed.final.CleanupError != "" ||
		len(observed.final.Transfers) != len(fixture.sources) {
		t.Fatalf("plain transfer final = %#v", observed.final)
	}
	for number := 1; number <= len(fixture.sources); number++ {
		id, _ := transfernumber.New(number)
		if !bytes.Equal(observed.outputs[supervisorOutputKey{transfer: id, stream: runsupervisor.OutputStdout}],
			[]byte("plain-stdout\x00\r\n")) ||
			!bytes.Equal(observed.outputs[supervisorOutputKey{transfer: id, stream: runsupervisor.OutputStderr}],
				[]byte("plain-stderr\x1b[31m\n")) {
			t.Fatalf("transfer %d plain output = %#v", number, observed.outputs)
		}
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("plain transfer left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestTransferDirectoryManualLifecycle(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "transfer-manual")
	observed := scenario.run(t, nil)
	if observed.final.Cancelled || observed.final.InternalError != "" || observed.final.RunError != "" ||
		observed.final.CleanupError != "" ||
		observed.final.Source == nil || len(observed.final.Transfers) != len(fixture.sources) {
		t.Fatalf("transfer final = %#v", observed.final)
	}
	for number := 1; number <= len(fixture.sources); number++ {
		assertRunSupervisorTransferResult(t, observed.final, number, 0, 0)
	}
	assertRunSupervisorTransferStatuses(t, observed.statuses, len(fixture.sources))
	assertTransferViewEvidence(t, scenario.markers, len(fixture.sources))
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("transfer left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestTransferDirectoryEventBackpressureCleansOwnersBeforeFinal(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "transfer-output-backpressure")
	client, err := launchScenarioRunSupervisor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	joined := false
	t.Cleanup(func() {
		if joined {
			return
		}
		_ = client.Cancel(runsupervisor.CancelInternal)
		_ = client.CloseLifeline()
		_ = client.Wait()
	})
	stages, err := waitForOutputBackpressureStages(filepath.Join(scenario.markers, "output-stage.*"),
		len(fixture.sources), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scenario.markers, "output.release"), []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForSupervisorEventPipeFull(client.PID(), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	exactRunRoot := filepath.Join(rundirectory.AuthorityRoot, stages[0].RunID.String())
	if err := waitForTransferLivenessRelease(exactRunRoot, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, stage := range stages {
		if err := waitForOutputWriterExit(stage.PID, 10*time.Second); err != nil {
			t.Fatal(err)
		}
	}

	finals := 0
	var final runsupervisor.Final
	for {
		event, err := client.Next()
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind() != runsupervisor.EventFinal {
			continue
		}
		finals++
		final = event.Final()
		break
	}
	if err := client.CloseLifeline(); err != nil {
		t.Fatal(err)
	}
	if err := client.Wait(); err != nil {
		t.Fatal(err)
	}
	joined = true
	if finals != 1 || !final.Cancelled || final.Reason != runsupervisor.CancelInternal ||
		!strings.Contains(final.RunError, "backpressure") || final.CleanupError != "" {
		t.Fatalf("event backpressure finals=%d final=%#v", finals, final)
	}
	assertScenarioProcessesGone(t, scenario.markers)
	if _, err := os.Stat(exactRunRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("event backpressure run %s remains: %v", stages[0].RunID, err)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("event backpressure left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestTransferDirectoryRejectsEmptySourceBeforeLiveRun(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	before := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "transfer-manual")
	source := os.Getenv(supervisorTransferEnv)
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	observed := scenario.run(t, nil)
	if observed.final.RunError == "" || observed.final.InternalError != "" ||
		len(observed.final.Transfers) != 0 {
		t.Fatalf("zero-file transfer final = %#v", observed.final)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, before) {
		t.Fatalf("zero-file transfer left run roots: before=%v after=%v", before, after)
	}
}

func TestTransferDirectoryHostNetworkPreflightFailsBeforeMutation(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	before := authorityEntries(t)
	forwarding := "/proc/sys/net/ipv4/conf/" + fixture.providers[0] + "/forwarding"
	fixture.writeSysctl(forwarding, "0\n")
	t.Cleanup(func() { fixture.writeSysctl(forwarding, "1\n") })
	downSurface := fixture.hostSurface()
	scenario := fixture.newRunSupervisorScenario(t, "transfer-manual")
	observed := scenario.run(t, nil)
	if !strings.Contains(observed.final.RunError, "forwarding") || observed.final.InternalError != "" ||
		len(observed.final.Transfers) != 0 {
		t.Fatalf("network-preflight transfer final = %#v", observed.final)
	}
	fixture.assertSurface(downSurface)
	if after := authorityEntries(t); !slices.Equal(after, before) {
		t.Fatalf("network preflight left run roots: before=%v after=%v", before, after)
	}
	fixture.writeSysctl(forwarding, "1\n")
	fixture.assertSurface(baseline)
}

func TestTransferDirectoryAutomaticMeasureFailureCreatesNoViews(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	before := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "transfer-auto-fail")
	t.Setenv(supervisorMeasureEnv,
		"http://"+net.JoinHostPort(fixture.remote.String(), "18081")+"/measure-fail")
	observed := scenario.run(t, nil)
	if !strings.Contains(observed.final.RunError, "status 503") || observed.final.InternalError != "" ||
		len(observed.final.Transfers) != 0 {
		t.Fatalf("automatic failure final = %#v", observed.final)
	}
	if _, err := os.Stat(filepath.Join(scenario.markers, "transfer-views.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("measurement failure ran user command: %v", err)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, before) {
		t.Fatalf("automatic measurement failure left run roots: before=%v after=%v", before, after)
	}
}

func TestTransferDirectoryBarrierFailureRunsNoCommand(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	before := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "transfer-barrier-fail")
	observed := scenario.run(t, nil)
	if observed.final.RunError == "" || observed.final.InternalError != "" ||
		len(observed.final.Transfers) != 0 {
		t.Fatalf("barrier failure final = %#v", observed.final)
	}
	if _, err := os.Stat(filepath.Join(scenario.markers, "transfer-views.jsonl")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("barrier failure ran user command: %v", err)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, before) {
		t.Fatalf("barrier failure left run roots: before=%v after=%v", before, after)
	}
}

func TestTransferDirectoryOneFailureKeepsSiblingsRunning(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	before := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "transfer-one-fails")
	observed := scenario.run(t, nil)
	failed := 0
	for _, result := range observed.final.Transfers {
		if result.ExitCode != 0 || result.Signal != 0 {
			failed++
		}
	}
	if observed.final.Cancelled || observed.final.RunError != "" || failed != 1 ||
		len(observed.final.Transfers) != len(fixture.sources) {
		t.Fatalf("single-transfer failure final = %#v", observed.final)
	}
	assertTransferViewEvidence(t, scenario.markers, len(fixture.sources))
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, before) {
		t.Fatalf("single-transfer failure left run roots: before=%v after=%v", before, after)
	}
}

func TestTransferDirectoryUserCancellationReapsBeforeCleanup(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	before := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "transfer-cancel")
	var id string
	observed := scenario.runAfterLaunch(t, func(client *runsupervisor.RunSupervisorClient) error {
		readyID, err := waitForReadyRunID(filepath.Join(scenario.markers, "transfer-block.*.ready"),
			len(fixture.sources), 10*time.Second)
		if err != nil {
			return err
		}
		id = readyID.String()
		return client.Cancel(runsupervisor.CancelUser)
	})
	if !observed.final.Cancelled || observed.final.Reason != runsupervisor.CancelUser ||
		observed.final.RunError == "" || len(observed.final.Transfers) != len(fixture.sources) {
		t.Fatalf("cancelled transfer final = %#v", observed.final)
	}
	if _, err := os.Stat(filepath.Join(rundirectory.AuthorityRoot, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled run %s remains: %v", id, err)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, before) {
		t.Fatalf("cancelled transfer left run roots: before=%v after=%v", before, after)
	}
}

func assertRunSupervisorTransferStatuses(t *testing.T, statuses []runsupervisor.TransferStatus, count int) {
	t.Helper()
	progress := make(map[transfernumber.Number]runsupervisor.TransferState, count)
	for _, status := range statuses {
		previous := progress[status.Transfer]
		if previous == 0 && status.State == runsupervisor.TransferRunning ||
			previous == runsupervisor.TransferRunning && status.State == runsupervisor.TransferExited {
			progress[status.Transfer] = status.State
			continue
		}
		t.Fatalf("invalid transfer status sequence: %#v", statuses)
	}
	if len(progress) != count {
		t.Fatalf("status transfer count=%d want=%d: %#v", len(progress), count, statuses)
	}
	for number := 1; number <= count; number++ {
		id, _ := transfernumber.New(number)
		if progress[id] != runsupervisor.TransferExited {
			t.Fatalf("transfer %d did not reach exited: %#v", number, statuses)
		}
	}
}

func TestTransferDirectoryHostNetworkCleanupFailurePreservesAndRecoversOwners(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	before := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "transfer-cleanup-block")
	var exactRunRoot string
	var recoveryCallbacks int
	observed := scenario.runAfterLaunch(t, func(client *runsupervisor.RunSupervisorClient) (result error) {
		id, err := waitForReadyRunID(filepath.Join(scenario.markers, "cleanup.*.ready"),
			len(fixture.sources), 10*time.Second)
		if err != nil {
			return err
		}
		exactRunRoot = filepath.Join(rundirectory.AuthorityRoot, id.String())
		restore, err := corruptHostNetworkClaim(exactRunRoot)
		if err != nil {
			return err
		}
		restored := false
		defer func() {
			_ = os.WriteFile(filepath.Join(scenario.markers, "cleanup.release"), []byte("release\n"), 0o600)
			if !restored {
				_ = restore()
			}
			if result != nil {
				_ = client.Cancel(runsupervisor.CancelInternal)
			}
		}()
		if err := os.WriteFile(filepath.Join(scenario.markers, "cleanup.release"), []byte("release\n"), 0o600); err != nil {
			return err
		}
		if err := waitForTransferLivenessRelease(exactRunRoot, 10*time.Second); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(exactRunRoot, "network")); err != nil {
			return fmt.Errorf("host-network failure lost cleanup record: %w", err)
		}
		if _, err := os.Stat(filepath.Join(exactRunRoot, "views")); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("host-network failure retained independent file views: %v", err)
		}
		if err := restore(); err != nil {
			return err
		}
		restored = true
		recoveryCallbacks, err = recoverExactTransferRun(id)
		return err
	})
	if observed.final.InternalError != "" || !strings.Contains(observed.final.CleanupError, "host network") {
		t.Fatalf("host-network cleanup final = %#v", observed.final)
	}
	if recoveryCallbacks != 1 {
		t.Fatalf("host-network stale recovery callback count=%d", recoveryCallbacks)
	}
	if _, err := os.Stat(exactRunRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("host-network cleanup run remains: %v", err)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, before) {
		t.Fatalf("host-network cleanup failure left run roots: before=%v after=%v", before, after)
	}
}

func TestTransferDirectoryFileViewsCleanupFailurePreservesAndRecoversOwners(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	before := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "transfer-cleanup-block")
	var exactRunRoot string
	var recoveryCallbacks int
	observed := scenario.runAfterLaunch(t, func(client *runsupervisor.RunSupervisorClient) (result error) {
		id, err := waitForReadyRunID(filepath.Join(scenario.markers, "cleanup.*.ready"),
			len(fixture.sources), 10*time.Second)
		if err != nil {
			return err
		}
		exactRunRoot = filepath.Join(rundirectory.AuthorityRoot, id.String())
		restore, err := replaceFileViewEvidence(exactRunRoot)
		if err != nil {
			return err
		}
		restored := false
		defer func() {
			_ = os.WriteFile(filepath.Join(scenario.markers, "cleanup.release"), []byte("release\n"), 0o600)
			if !restored {
				_ = restore()
			}
			if result != nil {
				_ = client.Cancel(runsupervisor.CancelInternal)
			}
		}()
		if err := os.WriteFile(filepath.Join(scenario.markers, "cleanup.release"), []byte("release\n"), 0o600); err != nil {
			return err
		}
		if err := waitForTransferLivenessRelease(exactRunRoot, 10*time.Second); err != nil {
			return err
		}
		if _, err := os.Stat(filepath.Join(exactRunRoot, "network")); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("file-views failure retained host-network evidence: %v", err)
		}
		if err := restore(); err != nil {
			return err
		}
		restored = true
		recoveryCallbacks, err = recoverExactTransferRun(id)
		return err
	})
	if observed.final.InternalError != "" || !strings.Contains(observed.final.CleanupError, "file views") {
		t.Fatalf("file-views cleanup final = %#v", observed.final)
	}
	if recoveryCallbacks != 1 {
		t.Fatalf("file-views stale recovery callback count=%d", recoveryCallbacks)
	}
	if _, err := os.Stat(exactRunRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file-views cleanup run remains: %v", err)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, before) {
		t.Fatalf("file-views cleanup failure left run roots: before=%v after=%v", before, after)
	}
}

func TestTransferDirectorySupervisorDeathReapsBeforeExactStaleRecovery(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	before := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "transfer-cleanup-block")
	client, err := launchScenarioRunSupervisor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id, err := waitForReadyRunID(filepath.Join(scenario.markers, "cleanup.*.ready"),
		len(fixture.sources), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	exactRunRoot := filepath.Join(rundirectory.AuthorityRoot, id.String())
	activeCallbacks := 0
	if err := rundirectory.New().RecoverStale(context.Background(), func(stale *rundirectory.StaleRun) error {
		if stale.ID() != id {
			return fmt.Errorf("unexpected stale run %s while observing active run %s", stale.ID(), id)
		}
		activeCallbacks++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if activeCallbacks != 0 {
		t.Fatalf("active supervisor run %s was offered for stale recovery %d times", id, activeCallbacks)
	}
	if err := unix.Kill(client.PID(), unix.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := client.Wait(); err == nil {
		t.Fatal("SIGKILLed transfer supervisor exited successfully")
	}
	assertScenarioProcessesGone(t, scenario.markers)
	callbacks, err := recoverExactTransferRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if callbacks != 1 {
		t.Fatalf("supervisor-death stale recovery callback count=%d", callbacks)
	}
	if _, err := os.Stat(exactRunRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("supervisor-death run %s remains: %v", id, err)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, before) {
		t.Fatalf("supervisor-death recovery left run roots: before=%v after=%v", before, after)
	}
}
