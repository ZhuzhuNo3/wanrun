//go:build linux && rootintegration

package root_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/namespaceresolvers"
	"github.com/ZhuzhuNo3/transferlanes/internal/networkcatalog"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

const (
	supervisorScenarioEnv = "TRANSFERLANES_ROOT_SUPERVISOR_SCENARIO"
	supervisorSourcesEnv  = "TRANSFERLANES_ROOT_SUPERVISOR_SOURCES"
	supervisorRunIDEnv    = "TRANSFERLANES_ROOT_SUPERVISOR_RUN_ID"
	supervisorWorkDirEnv  = "TRANSFERLANES_ROOT_SUPERVISOR_WORK_DIR"
	supervisorMarkerEnv   = "TRANSFERLANES_ROOT_SUPERVISOR_MARKERS"
	supervisorMeasureEnv  = "TRANSFERLANES_ROOT_SUPERVISOR_MEASURE_URL"
	supervisorTransferEnv = "TRANSFERLANES_ROOT_SUPERVISOR_TRANSFER_SOURCE"
)

const (
	plainSupervisorNormal            plainSupervisorScenario = "normal"
	plainSupervisorPipes             plainSupervisorScenario = "plain"
	plainSupervisorBarrierFailure    plainSupervisorScenario = "plain-barrier-fail"
	plainSupervisorExecFailureOutput plainSupervisorScenario = "plain-exec-fail-output"
	plainSupervisorSignals           plainSupervisorScenario = "plain-signals"

	measurementSupervisorMeasure measurementSupervisorScenario = "measure"

	diagnosticSupervisorFinal         diagnosticSupervisorScenario = "diagnostic"
	diagnosticSupervisorFallback      diagnosticSupervisorScenario = "diagnostic-fallback"
	diagnosticSupervisorBarrier       diagnosticSupervisorScenario = "barrier-fail"
	diagnosticSupervisorPreExec       diagnosticSupervisorScenario = "preexec-fail"
	diagnosticSupervisorExec          diagnosticSupervisorScenario = "exec-fail"
	diagnosticSupervisorExecOutput    diagnosticSupervisorScenario = "exec-fail-output"
	diagnosticSupervisorHelperFailure diagnosticSupervisorScenario = "helper-fail"
	diagnosticSupervisorHelperFlood   diagnosticSupervisorScenario = "helper-stderr-flood"

	containmentSupervisorHelperClean  containmentSupervisorScenario = "helper-cancel-clean"
	containmentSupervisorBackpressure containmentSupervisorScenario = "control-backpressure"
	containmentSupervisorSignals      containmentSupervisorScenario = "signals"
	containmentSupervisorStress       containmentSupervisorScenario = "descendant-stress"
	containmentSupervisorLongOrphan   containmentSupervisorScenario = "long-orphan"
	containmentSupervisorHandleCycles containmentSupervisorScenario = "process-handle-cycles"
	containmentSupervisorPanic        containmentSupervisorScenario = "panic-descendant"
)

type plainSupervisorScenario string
type measurementSupervisorScenario string
type diagnosticSupervisorScenario string
type containmentSupervisorScenario string

func parsePlainSupervisorScenario(value string) (plainSupervisorScenario, bool) {
	switch scenario := plainSupervisorScenario(value); scenario {
	case plainSupervisorNormal, plainSupervisorPipes, plainSupervisorBarrierFailure,
		plainSupervisorExecFailureOutput, plainSupervisorSignals:
		return scenario, true
	default:
		return "", false
	}
}

func parseMeasurementSupervisorScenario(value string) (measurementSupervisorScenario, bool) {
	scenario := measurementSupervisorScenario(value)
	return scenario, scenario == measurementSupervisorMeasure
}

func parseDiagnosticSupervisorScenario(value string) (diagnosticSupervisorScenario, bool) {
	switch scenario := diagnosticSupervisorScenario(value); scenario {
	case diagnosticSupervisorFinal, diagnosticSupervisorFallback, diagnosticSupervisorBarrier,
		diagnosticSupervisorPreExec, diagnosticSupervisorExec, diagnosticSupervisorExecOutput,
		diagnosticSupervisorHelperFailure, diagnosticSupervisorHelperFlood:
		return scenario, true
	default:
		return "", false
	}
}

func parseContainmentSupervisorScenario(value string) (containmentSupervisorScenario, bool) {
	switch scenario := containmentSupervisorScenario(value); scenario {
	case containmentSupervisorHelperClean, containmentSupervisorBackpressure,
		containmentSupervisorSignals, containmentSupervisorStress, containmentSupervisorLongOrphan,
		containmentSupervisorHandleCycles, containmentSupervisorPanic:
		return scenario, true
	default:
		return "", false
	}
}

func waitForGlobCount(pattern string, want int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			return err
		}
		if len(matches) == want {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %d paths matching %q", want, pattern)
}

type supervisorRootScenario struct {
	id      runid.ID
	markers string
}

func (fixture *hostNetworkFixture) newRunSupervisorScenario(t *testing.T, name string) supervisorRootScenario {
	t.Helper()
	id, err := runid.New()
	if err != nil {
		t.Fatal(err)
	}
	markers := t.TempDir()
	source := filepath.Join(markers, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{
		"alpha": "a", "bravo": "bb", "charlie": "ccc", ".hidden": "dddd",
		"nested/echo": "eeeee", "nested/foxtrot": "ffffff",
	} {
		path := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(supervisorScenarioEnv, name)
	t.Setenv(supervisorSourcesEnv, joinAddresses(fixture.sources))
	t.Setenv(supervisorRunIDEnv, id.String())
	t.Setenv(supervisorWorkDirEnv, markers)
	t.Setenv(supervisorMarkerEnv, markers)
	t.Setenv(supervisorTransferEnv, source)
	return supervisorRootScenario{id: id, markers: markers}
}

type observedRunSupervisor struct {
	final    runsupervisor.Final
	pty      string
	outputs  map[supervisorOutputKey][]byte
	statuses []runsupervisor.TransferStatus
}

type supervisorOutputKey struct {
	transfer transfernumber.Number
	stream   runsupervisor.OutputStream
}

func (scenario supervisorRootScenario) run(t *testing.T,
	onEvent func(*runsupervisor.RunSupervisorClient, runsupervisor.Event)) observedRunSupervisor {
	return scenario.runWith(t, nil, onEvent)
}

func (scenario supervisorRootScenario) runAfterLaunch(t *testing.T,
	action func(*runsupervisor.RunSupervisorClient) error) observedRunSupervisor {
	return scenario.runWith(t, action, nil)
}

func (scenario supervisorRootScenario) runWith(t *testing.T, action func(*runsupervisor.RunSupervisorClient) error,
	onEvent func(*runsupervisor.RunSupervisorClient, runsupervisor.Event)) observedRunSupervisor {
	t.Helper()
	client, err := launchScenarioRunSupervisor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var actionResult <-chan error
	if action != nil {
		finished := make(chan error, 1)
		actionResult = finished
		go func() { finished <- action(client) }()
	}
	observed := observedRunSupervisor{outputs: make(map[supervisorOutputKey][]byte)}
	for {
		event, err := client.Next()
		if err != nil {
			t.Fatal(err)
		}
		if onEvent != nil {
			onEvent(client, event)
		}
		switch event.Kind() {
		case runsupervisor.EventOutput:
			key := supervisorOutputKey{transfer: event.Transfer(), stream: event.Stream()}
			observed.outputs[key] = append(observed.outputs[key], event.Bytes()...)
			if event.Stream() == runsupervisor.OutputPTY {
				observed.pty += string(event.Bytes())
			}
		case runsupervisor.EventStatus:
			observed.statuses = append(observed.statuses, event.Status())
		case runsupervisor.EventFinal:
			observed.final = event.Final()
			_ = client.CloseLifeline()
			if err := client.Wait(); err != nil {
				t.Fatalf("supervisor wait: %v", err)
			}
			if actionResult != nil {
				if err := <-actionResult; err != nil {
					t.Fatalf("supervisor action: %v; final=%#v; pty=%q", err, observed.final, observed.pty)
				}
			}
			return observed
		}
	}
}

func launchScenarioRunSupervisor(ctx context.Context) (*runsupervisor.RunSupervisorClient, error) {
	source, err := sourcefiles.OpenSourceRoot(os.Getenv(supervisorTransferEnv))
	if err != nil {
		return nil, err
	}
	return runsupervisor.Launch(ctx, os.Args[0], nil, source)
}

type supervisorNetworkOwners struct {
	live    *rundirectory.LiveRun
	network *hostnetwork.Session
	count   int
}

type supervisorProcessOwners struct {
	set      *namespaceresolvers.NamespaceResolverSet
	resolver namespaceresolvers.ResolverAccess
	owner    *childprocesses.ChildProcesses
}

func markSupervisorWorkEntered() error {
	if markerRoot := os.Getenv(supervisorMarkerEnv); markerRoot != "" {
		if err := os.WriteFile(filepath.Join(markerRoot, "supervisor-work-entered"), nil, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func openSupervisorNetworkOwners(ctx context.Context) (supervisorNetworkOwners, error) {
	id, err := runid.Parse(os.Getenv(supervisorRunIDEnv))
	if err != nil {
		return supervisorNetworkOwners{}, err
	}
	sources, err := parseAddresses(os.Getenv(supervisorSourcesEnv))
	if err != nil {
		return supervisorNetworkOwners{}, err
	}
	snapshot, err := networkcatalog.New().Capture(ctx)
	if err != nil {
		return supervisorNetworkOwners{}, err
	}
	resolved := snapshot.Resolve(sources)
	egresses := make(map[transfernumber.Number]networkcatalog.Egress, len(resolved))
	for index, egress := range resolved {
		id, _ := transfernumber.New(index + 1)
		if !egress.Runnable() {
			return supervisorNetworkOwners{}, fmt.Errorf("transfer %d egress: %s", id.Value(), egress.Reason())
		}
		egresses[id] = egress
	}
	live, err := rundirectory.New().Create(id)
	if err != nil {
		return supervisorNetworkOwners{}, err
	}
	network, err := hostnetwork.New().Open(ctx, live, egresses)
	if err != nil {
		_ = live.Close()
		return supervisorNetworkOwners{}, err
	}
	return supervisorNetworkOwners{live: live, network: network, count: len(egresses)}, nil
}

func (owners supervisorNetworkOwners) openProcesses(ctx context.Context) (supervisorProcessOwners, error) {
	resolverSet, err := openRunSupervisorResolverSet(ctx, owners.network, owners.count)
	if err != nil {
		return supervisorProcessOwners{}, err
	}
	first, _ := transfernumber.New(1)
	resolver, err := resolverSet.Access(first)
	if err != nil {
		closeRunSupervisorResolverSet(resolverSet)
		return supervisorProcessOwners{}, err
	}
	return supervisorProcessOwners{set: resolverSet, resolver: resolver, owner: childprocesses.New()}, nil
}

func runPlainSupervisorFixture(ctx context.Context, wire *runsupervisor.RunSupervisor,
	scenario plainSupervisorScenario,
) runsupervisor.Final {
	if err := markSupervisorWorkEntered(); err != nil {
		return internalRunSupervisorFinal(err)
	}
	owners, err := openSupervisorNetworkOwners(ctx)
	if err != nil {
		return internalRunSupervisorFinal(err)
	}
	processes, err := owners.openProcesses(ctx)
	if err != nil {
		return finishRunSupervisorOwners(owners.live, owners.network, runsupervisor.Final{}, err)
	}
	defer closeRunSupervisorResolverSet(processes.set)
	prepared, err := plainSupervisorCommands(scenario, os.Getenv(supervisorWorkDirEnv),
		processes.resolver, os.Getenv(supervisorMarkerEnv))
	if err != nil {
		return finishRunSupervisorOwners(owners.live, owners.network, runsupervisor.Final{}, err)
	}
	var final runsupervisor.Final
	if scenario != plainSupervisorNormal {
		final = runSupervisorPlainCommands(ctx, owners.live, owners.network, processes.owner, prepared.commands)
	} else {
		final = runSupervisorCommands(ctx, wire, false, owners.live, owners.network, processes.owner,
			prepared.commands)
	}
	return joinSupervisorCommandPreparation(final, prepared)
}

func runMeasurementSupervisorFixture(ctx context.Context, _ *runsupervisor.RunSupervisor,
	_ measurementSupervisorScenario,
) runsupervisor.Final {
	if err := markSupervisorWorkEntered(); err != nil {
		return internalRunSupervisorFinal(err)
	}
	owners, err := openSupervisorNetworkOwners(ctx)
	if err != nil {
		return internalRunSupervisorFinal(err)
	}
	return runSupervisorMeasurements(ctx, owners.live, owners.network, owners.count)
}

func runDiagnosticSupervisorFixture(ctx context.Context, wire *runsupervisor.RunSupervisor,
	scenario diagnosticSupervisorScenario,
) runsupervisor.Final {
	if err := markSupervisorWorkEntered(); err != nil {
		return internalRunSupervisorFinal(err)
	}
	if final, handled := supervisorDiagnosticFinal(scenario); handled {
		return final
	}
	owners, err := openSupervisorNetworkOwners(ctx)
	if err != nil {
		return internalRunSupervisorFinal(err)
	}
	processes, err := owners.openProcesses(ctx)
	if err != nil {
		return finishRunSupervisorOwners(owners.live, owners.network, runsupervisor.Final{}, err)
	}
	defer closeRunSupervisorResolverSet(processes.set)
	prepared, err := diagnosticSupervisorCommands(scenario, os.Getenv(supervisorWorkDirEnv),
		processes.resolver, os.Getenv(supervisorMarkerEnv))
	if err != nil {
		return finishRunSupervisorOwners(owners.live, owners.network, runsupervisor.Final{}, err)
	}
	if scenario == diagnosticSupervisorHelperFailure || scenario == diagnosticSupervisorHelperFlood {
		final := runSupervisorHelpers(ctx, owners.live, owners.network, processes.owner, prepared.commands,
			scenario == diagnosticSupervisorHelperFlood)
		return joinSupervisorCommandPreparation(final, prepared)
	}
	final := runSupervisorCommands(ctx, wire, false, owners.live, owners.network, processes.owner,
		prepared.commands)
	return joinSupervisorCommandPreparation(final, prepared)
}

func runContainmentSupervisorFixture(ctx context.Context, wire *runsupervisor.RunSupervisor,
	scenario containmentSupervisorScenario,
) runsupervisor.Final {
	if err := markSupervisorWorkEntered(); err != nil {
		return internalRunSupervisorFinal(err)
	}
	if scenario == containmentSupervisorPanic {
		marker := filepath.Join(os.Getenv(supervisorMarkerEnv), "panic-descendant.pid")
		argv := containmentChildCommand(os.Args[0], containmentChildGrandchild, marker)
		command := exec.Command(argv[0], argv[1:]...)
		if err := command.Start(); err != nil {
			return internalRunSupervisorFinal(fmt.Errorf("start panic descendant: %w", err))
		}
		if !waitForChildPath(marker, 5*time.Second) {
			return internalRunSupervisorFinal(errors.New("panic descendant did not start"))
		}
		panic("root integration work panic")
	}
	owners, err := openSupervisorNetworkOwners(ctx)
	if err != nil {
		return internalRunSupervisorFinal(err)
	}
	processes, err := owners.openProcesses(ctx)
	if err != nil {
		return finishRunSupervisorOwners(owners.live, owners.network, runsupervisor.Final{}, err)
	}
	defer closeRunSupervisorResolverSet(processes.set)
	if scenario == containmentSupervisorHandleCycles {
		return runSupervisorProcessHandleCycles(ctx, owners.live, owners.network, processes.owner,
			owners.count, os.Getenv(supervisorWorkDirEnv), processes.resolver)
	}
	commands, err := containmentSupervisorCommands(scenario, os.Getenv(supervisorWorkDirEnv),
		processes.resolver, os.Getenv(supervisorMarkerEnv))
	if err != nil {
		return finishRunSupervisorOwners(owners.live, owners.network, runsupervisor.Final{}, err)
	}
	if scenario == containmentSupervisorHelperClean {
		return runSupervisorHelpers(ctx, owners.live, owners.network, processes.owner, commands, false)
	}
	return runSupervisorCommands(ctx, wire, scenario == containmentSupervisorBackpressure,
		owners.live, owners.network, processes.owner, commands)
}

func runSupervisorProcessHandleCycles(ctx context.Context, live *rundirectory.LiveRun,
	network *hostnetwork.Session, processOwner *childprocesses.ChildProcesses,
	count int, workDir string, resolver namespaceresolvers.ResolverAccess,
) runsupervisor.Final {
	previousGC := debug.SetGCPercent(-1)
	defer debug.SetGCPercent(previousGC)
	commands, err := processHandleCycleCommands(workDir, resolver)
	if err != nil {
		return finishRunSupervisorOwners(live, network, runsupervisor.Final{}, err)
	}
	firstCount := 0
	for cycle := 0; cycle < 8; cycle++ {
		if err := runProcessHandleCycle(ctx, network, processOwner, commands); err != nil {
			return finishRunSupervisorOwners(live, network, runsupervisor.Final{}, fmt.Errorf(
				"process-handle cycle %d: %w", cycle, err))
		}
		openCount, err := openRunSupervisorDescriptorCount()
		if err != nil {
			return finishRunSupervisorOwners(live, network, runsupervisor.Final{}, err)
		}
		if cycle == 0 {
			firstCount = openCount
		} else if openCount > firstCount+count {
			return finishRunSupervisorOwners(live, network, runsupervisor.Final{}, fmt.Errorf(
				"supervisor descriptors grew across process-handle cycles: first=%d current=%d",
				firstCount, openCount))
		}
	}
	return finishRunSupervisorOwners(live, network, runsupervisor.Final{}, nil)
}

type processHandleCycleCommandSet struct {
	normal            childprocesses.Command
	missingDirectory  childprocesses.Command
	nonExecutable     childprocesses.Command
	brokenInterpreter childprocesses.Command
	untilCancelled    childprocesses.Command
}

func processHandleCycleCommands(workDir string, resolver namespaceresolvers.ResolverAccess) (
	processHandleCycleCommandSet, error,
) {
	id, _ := transfernumber.New(1)
	environment := supervisorChildEnvironment()
	command := func(cwd string, argv []string) (childprocesses.Command, error) {
		return childprocesses.NewCommand(id, cwd, resolver, argv, environment)
	}
	brokenExecutable := filepath.Join(workDir, "fd-cycle-broken-executable")
	if err := os.WriteFile(brokenExecutable,
		[]byte("#!"+filepath.Join(workDir, "fd-cycle-missing-interpreter")+"\nexit 0\n"), 0o700); err != nil {
		return processHandleCycleCommandSet{}, err
	}
	var result processHandleCycleCommandSet
	var err error
	if result.normal, err = command(workDir, []string{"/bin/true"}); err != nil {
		return processHandleCycleCommandSet{}, err
	}
	if result.missingDirectory, err = command(filepath.Join(workDir, "fd-cycle-missing-directory"),
		[]string{"/bin/true"}); err != nil {
		return processHandleCycleCommandSet{}, err
	}
	if result.nonExecutable, err = command(workDir, []string{workDir}); err != nil {
		return processHandleCycleCommandSet{}, err
	}
	if result.brokenInterpreter, err = command(workDir, []string{brokenExecutable}); err != nil {
		return processHandleCycleCommandSet{}, err
	}
	result.untilCancelled, err = command(workDir,
		[]string{"/bin/sh", "-c", "while :; do sleep 10; done"})
	return result, err
}

func runProcessHandleCycle(ctx context.Context, network *hostnetwork.Session,
	processOwner *childprocesses.ChildProcesses, commands processHandleCycleCommandSet,
) error {
	result, uncontained, err := processOwner.RunHelpers(ctx, network,
		[]childprocesses.Command{commands.normal})
	if err != nil || uncontained != nil || len(result.Transfers) != 1 {
		return errors.Join(err, errors.New("normal completion failed"))
	}
	for name, command := range map[string]childprocesses.Command{
		"missing working directory": commands.missingDirectory,
		"non-executable target":     commands.nonExecutable,
	} {
		processes, startErr := processOwner.StartPlain(ctx, network, []childprocesses.Command{command})
		if startErr == nil || processes != nil {
			return fmt.Errorf("%s owner=%v error=%v", name, processes, startErr)
		}
	}
	processes, startErr := processOwner.StartPlain(ctx, network,
		[]childprocesses.Command{commands.brokenInterpreter})
	if startErr == nil || processes == nil {
		return fmt.Errorf("broken interpreter owner=%v error=%v", processes, startErr)
	}
	if _, err := drainProcessSet(processes); err != nil {
		return fmt.Errorf("broken interpreter cleanup: %w", err)
	}

	processes, err = processOwner.StartPlain(ctx, network,
		[]childprocesses.Command{commands.untilCancelled})
	if err != nil {
		return fmt.Errorf("start cancellation target: %w", err)
	}
	drained := make(chan error, 1)
	go func() {
		_, waitErr := drainProcessSet(processes)
		drained <- waitErr
	}()
	cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	cancelErr := processes.Cancel(cancelCtx)
	cancel()
	if err := errors.Join(cancelErr, <-drained); err != nil {
		return fmt.Errorf("cancel running target: %w", err)
	}
	return nil
}

func drainProcessSet(processes *childprocesses.ProcessSet) (childprocesses.Result, error) {
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		for range processes.Output() {
		}
	}()
	go func() {
		defer wait.Done()
		for range processes.Statuses() {
		}
	}()
	result, err := processes.Wait()
	wait.Wait()
	return result, err
}

func openRunSupervisorDescriptorCount() (int, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, fmt.Errorf("read supervisor descriptors: %w", err)
	}
	return len(entries), nil
}

func runSupervisorPlainCommands(ctx context.Context, live *rundirectory.LiveRun,
	network *hostnetwork.Session, processOwner *childprocesses.ChildProcesses,
	commands []childprocesses.Command,
) runsupervisor.Final {
	processes, err := processOwner.StartPlain(ctx, network, commands)
	if err != nil {
		if processes != nil {
			_, processErr := collectPlainProcessOutput(processes)
			err = errors.Join(err, processErr)
		}
		return finishRunSupervisorOwners(live, network, runsupervisor.Final{}, err)
	}
	result, processErr := collectPlainProcessOutput(processes)
	final := runsupervisor.Final{Cancelled: result.Cancelled,
		Transfers: make([]runsupervisor.TransferResult, len(result.Transfers))}
	for index, transfer := range result.Transfers {
		final.Transfers[index] = runsupervisor.TransferResult{Transfer: transfer.Transfer,
			ExitCode: transfer.ExitCode, Signal: transfer.Signal}
	}
	return finishRunSupervisorOwners(live, network, final, processErr)
}

func collectPlainProcessOutput(processes *childprocesses.ProcessSet) (childprocesses.Result, error) {
	outputs := make(map[string]*bytes.Buffer)
	completed := make(chan struct {
		result childprocesses.Result
		err    error
	}, 1)
	go func() {
		result, waitErr := processes.Wait()
		completed <- struct {
			result childprocesses.Result
			err    error
		}{result: result, err: waitErr}
	}()
	for event := range processes.Output() {
		key := fmt.Sprintf("plain-%d.%s", event.Transfer.Value(), event.Stream)
		if outputs[key] == nil {
			outputs[key] = &bytes.Buffer{}
		}
		_, _ = outputs[key].Write(event.Bytes)
	}
	finished := <-completed
	for name, content := range outputs {
		if writeErr := os.WriteFile(filepath.Join(os.Getenv(supervisorMarkerEnv), name),
			content.Bytes(), 0o600); writeErr != nil {
			finished.err = errors.Join(finished.err, writeErr)
		}
	}
	return finished.result, finished.err
}

func runSupervisorHelpers(ctx context.Context, live *rundirectory.LiveRun,
	network *hostnetwork.Session, processOwner *childprocesses.ChildProcesses,
	commands []childprocesses.Command, recordDiagnosticLength bool,
) runsupervisor.Final {
	result, uncontained, runErr := processOwner.RunHelpers(ctx, network, commands)
	if recordDiagnosticLength && runErr != nil {
		length := []byte(strconv.Itoa(len(runErr.Error())))
		runErr = errors.Join(runErr, os.WriteFile(filepath.Join(os.Getenv(supervisorMarkerEnv),
			"helper-diagnostic-bytes"), length, 0o600))
	}
	if uncontained != nil {
		retainErr := live.RetainLivenessUntil(uncontained.ContainmentDone())
		return internalRunSupervisorFinal(errors.Join(runErr, retainErr,
			errors.New("helper process containment is unconfirmed")))
	}
	final := runsupervisor.Final{Transfers: make([]runsupervisor.TransferResult, len(result.Transfers))}
	for index, transferResult := range result.Transfers {
		final.Transfers[index] = runsupervisor.TransferResult{Transfer: transferResult.Transfer,
			ExitCode: transferResult.ExitCode, Signal: transferResult.Signal}
	}
	return finishRunSupervisorOwners(live, network, final, runErr)
}

func runSupervisorCommands(ctx context.Context, wire *runsupervisor.RunSupervisor, controlBackpressure bool,
	live *rundirectory.LiveRun, network *hostnetwork.Session, processOwner *childprocesses.ChildProcesses,
	commands []childprocesses.Command,
) runsupervisor.Final {
	size, _ := childprocesses.NewTerminalSize(80, 24)
	processes, err := processOwner.StartInteractive(ctx, network, commands, size)
	if err != nil {
		if processes != nil {
			_, processErr := bridgeRunSupervisorProcesses(ctx, wire, processes)
			runErr := errors.Join(err, processErr)
			select {
			case <-processes.ContainmentDone():
				return finishRunSupervisorOwners(live, network, runsupervisor.Final{}, runErr)
			default:
				retainErr := live.RetainLivenessUntil(processes.ContainmentDone())
				return internalRunSupervisorFinal(errors.Join(runErr,
					retainErr, errors.New("child process containment is unconfirmed")))
			}
		}
		return finishRunSupervisorOwners(live, network, runsupervisor.Final{}, err)
	}
	var result childprocesses.Result
	var processErr error
	if controlBackpressure {
		result, processErr = waitForControlBackpressure(ctx, wire, processes,
			os.Getenv(supervisorMarkerEnv))
	} else {
		result, processErr = bridgeRunSupervisorProcesses(ctx, wire, processes)
	}
	final := runsupervisor.Final{Cancelled: result.Cancelled, Transfers: make([]runsupervisor.TransferResult, len(result.Transfers))}
	for index, transferResult := range result.Transfers {
		final.Transfers[index] = runsupervisor.TransferResult{Transfer: transferResult.Transfer,
			ExitCode: transferResult.ExitCode, Signal: transferResult.Signal}
	}
	if result.Cancelled && ctx.Err() == nil {
		final.Reason = runsupervisor.CancelInternal
	}
	select {
	case <-processes.ContainmentDone():
	default:
		retainErr := live.RetainLivenessUntil(processes.ContainmentDone())
		final.InternalError = errors.Join(processErr, retainErr,
			errors.New("child process containment is unconfirmed")).Error()
		return final
	}
	return finishRunSupervisorOwners(live, network, final, processErr)
}

func waitForControlBackpressure(ctx context.Context, wire *runsupervisor.RunSupervisor,
	processes *childprocesses.ProcessSet, markers string) (childprocesses.Result, error) {
	if err := waitForChildPaths(10*time.Second,
		filepath.Join(markers, "transfer-1.pid"), filepath.Join(markers, "transfer-2.pid"),
		filepath.Join(markers, "transfer-1.grandchild.pid"),
		filepath.Join(markers, "transfer-2.grandchild.pid")); err != nil {
		cancelProcesses(processes)
		result, waitErr := processes.Wait()
		return result, errors.Join(err, waitErr)
	}
	transfer1, _ := transfernumber.New(1)
	if err := wire.SendStatus(runsupervisor.TransferStatus{Transfer: transfer1, State: runsupervisor.TransferRunning}); err != nil {
		cancelProcesses(processes)
		result, waitErr := processes.Wait()
		return result, errors.Join(err, waitErr)
	}
	if err := waitForChildPaths(10*time.Second, filepath.Join(markers, "release-controls")); err != nil {
		cancelProcesses(processes)
		result, waitErr := processes.Wait()
		return result, errors.Join(err, waitErr)
	}
	for {
		select {
		case <-ctx.Done():
			return processes.Wait()
		case _, open := <-wire.Controls():
			if !open {
				return processes.Wait()
			}
		}
	}
}

type preparedSupervisorCommands struct {
	commands    []childprocesses.Command
	done        <-chan error
	releasePath string
}

func joinSupervisorCommandPreparation(final runsupervisor.Final,
	prepared preparedSupervisorCommands,
) runsupervisor.Final {
	if prepared.done == nil {
		return final
	}
	var preparationErr error
	select {
	case preparationErr = <-prepared.done:
	case <-time.After(5 * time.Second):
		preparationErr = abortSupervisorCommandPreparation(prepared.releasePath, prepared.done,
			errors.New("timed out waiting for exec-failure preparation acknowledgement"))
	}
	if preparationErr != nil {
		var current error
		if final.InternalError != "" {
			current = errors.New(final.InternalError)
		}
		final.InternalError = errors.Join(current, preparationErr).Error()
	}
	return final
}

func plainSupervisorCommands(scenario plainSupervisorScenario, workDir string,
	resolver namespaceresolvers.ResolverAccess, markers string,
) (preparedSupervisorCommands, error) {
	executable, err := os.Executable()
	if err != nil {
		return preparedSupervisorCommands{}, err
	}
	transfer1, _ := transfernumber.New(1)
	transfer2, _ := transfernumber.New(2)
	environment := supervisorChildEnvironment()
	command := func(id transfernumber.Number, cwd string, argv []string) (childprocesses.Command, error) {
		return childprocesses.NewCommand(id, cwd, resolver, argv, environment)
	}
	switch scenario {
	case plainSupervisorBarrierFailure:
		first, err := command(transfer1, workDir, plainChildCommandLine(executable, plainChildExit,
			filepath.Join(markers, "should-not-run"), "0", "0"))
		if err != nil {
			return preparedSupervisorCommands{}, err
		}
		second, err := command(transfer2, filepath.Join(workDir, "missing-working-directory"),
			plainChildCommandLine(executable, plainChildExit,
				filepath.Join(markers, "also-should-not-run"), "0", "0"))
		return preparedSupervisorCommands{commands: []childprocesses.Command{first, second}}, err
	case plainSupervisorExecFailureOutput:
		brokenExecutable, releasePath, done, err := brokenSupervisorExecutable(markers, true)
		if err != nil {
			return preparedSupervisorCommands{}, err
		}
		first, err := command(transfer1, workDir, []string{"/bin/sh", "-c",
			`trap '' INT TERM; : > "$1"; printf %s "$2"; printf x > "$3"; while :; do sleep 1; done`,
			"transferlanes-exec-sibling", filepath.Join(markers, "exec-output.produced"),
			"unique-exec-failure-output", releasePath})
		if err != nil {
			return preparedSupervisorCommands{}, abortSupervisorCommandPreparation(releasePath, done, err)
		}
		second, err := command(transfer2, workDir, []string{brokenExecutable})
		if err != nil {
			return preparedSupervisorCommands{}, abortSupervisorCommandPreparation(releasePath, done, err)
		}
		return preparedSupervisorCommands{commands: []childprocesses.Command{first, second}, done: done,
			releasePath: releasePath}, nil
	case plainSupervisorPipes:
		first, err := command(transfer1, workDir, plainChildCommandLine(executable, plainChildPlain,
			filepath.Join(markers, "plain-1.json"), "7",
			base64.StdEncoding.EncodeToString([]byte("stdout-one\x00\r\n")),
			base64.StdEncoding.EncodeToString([]byte("stderr-one\x1b[31m\n"))))
		if err != nil {
			return preparedSupervisorCommands{}, err
		}
		second, err := command(transfer2, workDir, plainChildCommandLine(executable, plainChildPlain,
			filepath.Join(markers, "plain-2.json"), "0",
			base64.StdEncoding.EncodeToString([]byte("stdout-two\rprogress\n")),
			base64.StdEncoding.EncodeToString([]byte("stderr-two\n"))))
		return preparedSupervisorCommands{commands: []childprocesses.Command{first, second}}, err
	case plainSupervisorSignals:
		first, err := command(transfer1, workDir, containmentChildCommand(executable,
			containmentChildSignals, filepath.Join(markers, "transfer-1")))
		if err != nil {
			return preparedSupervisorCommands{}, err
		}
		second, err := command(transfer2, workDir, containmentChildCommand(executable,
			containmentChildSignals, filepath.Join(markers, "transfer-2")))
		return preparedSupervisorCommands{commands: []childprocesses.Command{first, second}}, err
	case plainSupervisorNormal:
		first, err := command(transfer1, workDir, plainChildCommandLine(executable, plainChildPTYResize,
			filepath.Join(markers, "transfer-1.json"), "argument with spaces",
			"$(touch "+filepath.Join(markers, "shell-side-effect")+")"))
		if err != nil {
			return preparedSupervisorCommands{}, err
		}
		second, err := command(transfer2, workDir, plainChildCommandLine(executable, plainChildExit,
			filepath.Join(markers, "transfer-2.finished"), "7", "100"))
		return preparedSupervisorCommands{commands: []childprocesses.Command{first, second}}, err
	default:
		return preparedSupervisorCommands{}, fmt.Errorf("unknown plain supervisor scenario %q", scenario)
	}
}

func diagnosticSupervisorCommands(scenario diagnosticSupervisorScenario, workDir string,
	resolver namespaceresolvers.ResolverAccess, markers string,
) (preparedSupervisorCommands, error) {
	executable, err := os.Executable()
	if err != nil {
		return preparedSupervisorCommands{}, err
	}
	transfer1, _ := transfernumber.New(1)
	transfer2, _ := transfernumber.New(2)
	environment := supervisorChildEnvironment()
	command := func(id transfernumber.Number, cwd string, argv []string) (childprocesses.Command, error) {
		return childprocesses.NewCommand(id, cwd, resolver, argv, environment)
	}
	switch scenario {
	case diagnosticSupervisorBarrier:
		first, err := command(transfer1, workDir, diagnosticChildCommand(executable, diagnosticChildExit,
			filepath.Join(markers, "should-not-run"), "0", "0"))
		if err != nil {
			return preparedSupervisorCommands{}, err
		}
		second, err := command(transfer2, filepath.Join(workDir, "missing-working-directory"),
			diagnosticChildCommand(executable, diagnosticChildExit,
				filepath.Join(markers, "also-should-not-run"), "0", "0"))
		return preparedSupervisorCommands{commands: []childprocesses.Command{first, second}}, err
	case diagnosticSupervisorPreExec:
		first, err := command(transfer1, workDir, diagnosticChildCommand(executable, diagnosticChildExit,
			filepath.Join(markers, "preexec-should-not-run"), "0", "0"))
		if err != nil {
			return preparedSupervisorCommands{}, err
		}
		second, err := command(transfer2, workDir, []string{workDir})
		return preparedSupervisorCommands{commands: []childprocesses.Command{first, second}}, err
	case diagnosticSupervisorExec, diagnosticSupervisorExecOutput:
		brokenExecutable, releasePath, done, err := brokenSupervisorExecutable(markers,
			scenario == diagnosticSupervisorExecOutput)
		if err != nil {
			return preparedSupervisorCommands{}, err
		}
		firstArgv := containmentChildCommand(executable, containmentChildSignals,
			filepath.Join(markers, "exec-sibling"))
		if scenario == diagnosticSupervisorExecOutput {
			firstArgv = []string{"/bin/sh", "-c",
				`trap '' INT TERM; : > "$1"; printf %s "$2"; printf x > "$3"; while :; do sleep 1; done`,
				"transferlanes-exec-sibling", filepath.Join(markers, "exec-output.produced"),
				"unique-exec-failure-output", releasePath}
		}
		first, err := command(transfer1, workDir, firstArgv)
		if err != nil {
			return preparedSupervisorCommands{}, abortSupervisorCommandPreparation(releasePath, done, err)
		}
		second, err := command(transfer2, workDir, []string{brokenExecutable})
		if err != nil {
			return preparedSupervisorCommands{}, abortSupervisorCommandPreparation(releasePath, done, err)
		}
		return preparedSupervisorCommands{commands: []childprocesses.Command{first, second}, done: done,
			releasePath: releasePath}, nil
	case diagnosticSupervisorHelperFailure:
		first, err := command(transfer1, workDir, diagnosticChildCommand(executable, diagnosticChildExit,
			filepath.Join(markers, "transfer-1.finished"), "7", "100"))
		if err != nil {
			return preparedSupervisorCommands{}, err
		}
		second, err := command(transfer2, workDir, containmentChildCommand(executable,
			containmentChildSignals, filepath.Join(markers, "transfer-2")))
		return preparedSupervisorCommands{commands: []childprocesses.Command{first, second}}, err
	case diagnosticSupervisorHelperFlood:
		first, err := command(transfer1, workDir, diagnosticChildCommand(executable,
			diagnosticChildHelperFlood, filepath.Join(markers, "helper-stderr-drained")))
		return preparedSupervisorCommands{commands: []childprocesses.Command{first}}, err
	default:
		return preparedSupervisorCommands{}, fmt.Errorf("unknown diagnostic supervisor scenario %q", scenario)
	}
}

func brokenSupervisorExecutable(markers string, waitForOutput bool) (string, string, <-chan error, error) {
	missingInterpreter := filepath.Join(markers, "missing-interpreter")
	brokenExecutable := filepath.Join(markers, "broken-executable")
	if err := os.WriteFile(brokenExecutable, []byte("#!"+missingInterpreter+"\nexit 0\n"), 0o700); err != nil {
		return "", "", nil, err
	}
	if !waitForOutput {
		return brokenExecutable, "", nil, nil
	}
	releasePath, done, err := holdExecFailureUntilOutputSignal(brokenExecutable,
		filepath.Join(markers, "exec-output.release"))
	return brokenExecutable, releasePath, done, err
}

func holdExecFailureUntilOutputSignal(executable, releasePath string) (string, <-chan error, error) {
	if err := unix.Mkfifo(releasePath, 0o600); err != nil {
		return "", nil, err
	}
	lease, err := os.OpenFile(executable, os.O_RDWR, 0)
	if err != nil {
		_ = os.Remove(releasePath)
		return "", nil, err
	}
	if _, err := unix.FcntlInt(lease.Fd(), unix.F_SETLEASE, unix.F_WRLCK); err != nil {
		_ = lease.Close()
		_ = os.Remove(releasePath)
		return "", nil, fmt.Errorf("hold broken executable with a write lease: %w", err)
	}
	notifications := make(chan os.Signal, 1)
	signal.Notify(notifications, syscall.SIGIO)
	done := make(chan error, 1)
	go func() {
		defer signal.Stop(notifications)
		gate, openErr := os.Open(releasePath)
		var readErr error
		if openErr == nil {
			var acknowledged [1]byte
			_, readErr = io.ReadFull(gate, acknowledged[:])
		}
		_, unlockErr := unix.FcntlInt(lease.Fd(), unix.F_SETLEASE, unix.F_UNLCK)
		done <- errors.Join(openErr, readErr, unlockErr, closeFiles(gate, lease), os.Remove(releasePath))
	}()
	return releasePath, done, nil
}

func abortSupervisorCommandPreparation(releasePath string, done <-chan error, cause error) error {
	if done == nil {
		return cause
	}
	gate, openErr := os.OpenFile(releasePath, os.O_WRONLY, 0)
	var writeErr error
	if openErr == nil {
		_, writeErr = gate.Write([]byte{'x'})
	}
	return errors.Join(cause, openErr, writeErr, closeFiles(gate), <-done)
}

func closeFiles(files ...*os.File) error {
	var result error
	for _, file := range files {
		if file != nil {
			result = errors.Join(result, file.Close())
		}
	}
	return result
}

func containmentSupervisorCommands(scenario containmentSupervisorScenario, workDir string,
	resolver namespaceresolvers.ResolverAccess, markers string,
) ([]childprocesses.Command, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	transfer1, _ := transfernumber.New(1)
	transfer2, _ := transfernumber.New(2)
	environment := supervisorChildEnvironment()
	command := func(id transfernumber.Number, argv []string) (childprocesses.Command, error) {
		return childprocesses.NewCommand(id, workDir, resolver, argv, environment)
	}
	switch scenario {
	case containmentSupervisorSignals, containmentSupervisorBackpressure:
		first, err := command(transfer1, containmentChildCommand(executable, containmentChildSignals,
			filepath.Join(markers, "transfer-1")))
		if err != nil {
			return nil, err
		}
		second, err := command(transfer2, containmentChildCommand(executable, containmentChildSignals,
			filepath.Join(markers, "transfer-2")))
		return []childprocesses.Command{first, second}, err
	case containmentSupervisorHelperClean:
		first, err := command(transfer1, containmentChildCommand(executable, containmentChildCleanSignal,
			filepath.Join(markers, "transfer-1")))
		if err != nil {
			return nil, err
		}
		second, err := command(transfer2, containmentChildCommand(executable, containmentChildCleanSignal,
			filepath.Join(markers, "transfer-2")))
		return []childprocesses.Command{first, second}, err
	case containmentSupervisorStress:
		stress, err := command(transfer1, containmentChildCommand(executable, containmentChildStress,
			filepath.Join(markers, "descendant-stress.finished"), "256"))
		return []childprocesses.Command{stress}, err
	case containmentSupervisorLongOrphan:
		orphan, err := command(transfer1, containmentChildCommand(executable, containmentChildLongOrphan,
			filepath.Join(markers, "long-orphan")))
		return []childprocesses.Command{orphan}, err
	default:
		return nil, fmt.Errorf("unknown containment supervisor scenario %q", scenario)
	}
}

func bridgeRunSupervisorProcesses(ctx context.Context, wire *runsupervisor.RunSupervisor,
	processes *childprocesses.ProcessSet) (childprocesses.Result, error) {
	resultChannel := make(chan struct {
		result childprocesses.Result
		err    error
	}, 1)
	go func() {
		result, err := processes.Wait()
		resultChannel <- struct {
			result childprocesses.Result
			err    error
		}{result, err}
	}()
	output, statuses, controls := processes.Output(), processes.Statuses(), wire.Controls()
	var result childprocesses.Result
	var failures []error
	for output != nil || statuses != nil || resultChannel != nil {
		select {
		case event, open := <-output:
			if !open {
				output = nil
				continue
			}
			if event.Stream != childprocesses.StreamPTY {
				failures = append(failures, fmt.Errorf("interactive child used unexpected %s output", event.Stream))
				go cancelProcesses(processes)
				continue
			}
			if err := wire.SendOutput(event.Transfer, runsupervisor.OutputPTY, event.Bytes); err != nil {
				failures = append(failures, err)
				go cancelProcesses(processes)
			}
		case status, open := <-statuses:
			if !open {
				statuses = nil
				continue
			}
			state := runsupervisor.TransferRunning
			if status.Kind == childprocesses.StatusExited {
				state = runsupervisor.TransferExited
			}
			sendErr := wire.SendStatus(runsupervisor.TransferStatus{Transfer: status.Transfer, State: state,
				ExitCode: status.ExitCode, Signal: status.Signal})
			failures = append(failures, sendErr)
			if sendErr != nil {
				go cancelProcesses(processes)
			}
		case control, open := <-controls:
			if !open {
				controls = nil
				continue
			}
			switch control.Kind() {
			case "resize":
				cols, rows := control.Size()
				failures = append(failures, processes.Resize(control.Transfer(), int(cols), int(rows)))
			case "input":
				failures = append(failures, processes.WriteInput(control.Transfer(), control.Input()))
			}
		case completed := <-resultChannel:
			result, failures = completed.result, append(failures, completed.err)
			resultChannel = nil
		}
	}
	return result, errors.Join(failures...)
}

func cancelProcesses(processes *childprocesses.ProcessSet) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = processes.Cancel(ctx)
}

func finishRunSupervisorOwners(live *rundirectory.LiveRun, network *hostnetwork.Session,
	final runsupervisor.Final, cause error) runsupervisor.Final {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var cleanupErr error
	if network != nil {
		cleanupErr = network.Close(ctx)
	}
	if cleanupErr == nil {
		cleanupErr = live.Complete(context.Background())
	} else {
		_ = live.Close()
	}
	cause = errors.Join(cause, cleanupErr)
	if cause != nil {
		final.InternalError = cause.Error()
	}
	return final
}

func internalRunSupervisorFinal(err error) runsupervisor.Final {
	return runsupervisor.Final{Cancelled: true, Reason: runsupervisor.CancelInternal, InternalError: err.Error()}
}

func supervisorChildEnvironment() []string {
	result := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "TERM=") {
			result = append(result, entry)
		}
	}
	return append(result, "TERM=xterm-256color")
}

func hasTemporaryIPv4() bool {
	interfaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, networkInterface := range interfaces {
		addresses, _ := networkInterface.Addrs()
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err == nil && netip.MustParsePrefix("198.18.0.0/15").Contains(prefix.Addr().Unmap()) {
				return true
			}
		}
	}
	return false
}

func parseAddresses(value string) ([]netip.Addr, error) {
	parts := strings.Split(value, ",")
	result := make([]netip.Addr, len(parts))
	for index, part := range parts {
		address, err := netip.ParseAddr(part)
		if err != nil {
			return nil, err
		}
		result[index] = address
	}
	return result, nil
}

func joinAddresses(addresses []netip.Addr) string {
	values := make([]string, len(addresses))
	for index, address := range addresses {
		values[index] = address.String()
	}
	return strings.Join(values, ",")
}

func assertRunSupervisorTransferResult(t *testing.T, final runsupervisor.Final, number, exitCode, signal int) {
	t.Helper()
	for _, result := range final.Transfers {
		if int(result.Transfer.Value()) == number {
			if result.ExitCode != exitCode || result.Signal != signal {
				t.Fatalf("transfer %d result = %#v", number, result)
			}
			return
		}
	}
	t.Fatalf("transfer %d result absent from %#v", number, final)
}

func readRunSupervisorJSON(t *testing.T, path string, target any) {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(encoded, target) != nil {
		t.Fatalf("read JSON evidence %s: %v", path, err)
	}
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func waitPIDFile(t *testing.T, path string) int {
	t.Helper()
	waitForPath(t, path)
	content, err := os.ReadFile(path)
	pid, parseErr := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil || parseErr != nil || pid <= 0 {
		t.Fatalf("read PID file %s: %q, %v, %v", path, content, err, parseErr)
	}
	return pid
}

func assertScenarioProcessesGone(t *testing.T, marker string) {
	t.Helper()
	if err := waitForScenarioProcessesGone(marker, 5*time.Second); err != nil {
		t.Fatal(err)
	}
}

func waitForScenarioProcessesGone(marker string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(processesContainingArgument(marker)) == 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("scenario processes remain: %v", processesContainingArgument(marker))
}

func processesContainingArgument(marker string) []int {
	entries, _ := os.ReadDir("/proc")
	var result []int
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		commandLine, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err == nil && bytes.Contains(commandLine, []byte(marker)) {
			result = append(result, pid)
		}
	}
	return result
}

func reapAdoptedProcess(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var status unix.WaitStatus
		reaped, err := unix.Wait4(pid, &status, unix.WNOHANG, nil)
		if reaped == pid || err == unix.ECHILD {
			return
		}
		if err != nil && err != unix.EINTR {
			t.Fatalf("reap adopted supervisor %d: %v", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("adopted supervisor %d did not exit", pid)
}

func recoverRunSupervisorRun(t *testing.T, id runid.ID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	recovered := 0
	err := rundirectory.New().RecoverStale(ctx, func(stale *rundirectory.StaleRun) error {
		if stale.ID() != id {
			return fmt.Errorf("unexpected stale run %s", stale.ID())
		}
		recovered++
		return hostnetwork.New().Recover(ctx, stale)
	})
	if err != nil || recovered != 1 {
		t.Fatalf("recover killed supervisor run = %d, %v", recovered, err)
	}
}

func waitForSurface(t *testing.T, fixture *hostNetworkFixture, want string) {
	t.Helper()
	if err := waitForHostSurface(fixture, want, 30*time.Second); err != nil {
		t.Fatal(err)
	}
}

func waitForHostSurface(fixture *hostNetworkFixture, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		after, err := fixture.captureHostSurface()
		if err == nil && after == want {
			return nil
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("supervisor cleanup did not restore host surface: %w", lastErr)
	}
	return errors.New("supervisor cleanup did not restore host surface")
}
