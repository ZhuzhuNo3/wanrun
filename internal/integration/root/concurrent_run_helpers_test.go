//go:build linux && rootintegration

package root_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/fileviews"
	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/vishvananda/netlink"
)

type publicRunResult struct {
	stdout string
	stderr string
	err    error
}

type activePublicRun struct {
	markers string
	command *exec.Cmd
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	done    chan struct{}
	waitErr error
	runID   runid.ID

	supervisorPID    int
	identityCaptured bool

	stateMu      sync.Mutex
	released     bool
	forceStopped bool
}

func startActivePublicRun(t *testing.T, arguments, environment []string,
	markers string,
) *activePublicRun {
	t.Helper()
	run := &activePublicRun{markers: markers, done: make(chan struct{})}
	run.command = exec.Command(os.Getenv("TRANSFERLANES_TEST_TRANSFERLANES"), arguments...)
	run.command.Env = environmentOverrides(os.Environ(), environment)
	run.command.Stdin = strings.NewReader("")
	run.command.Stdout, run.command.Stderr = &run.stdout, &run.stderr
	if err := run.command.Start(); err != nil {
		t.Fatalf("start public run: %v", err)
	}
	go func() {
		run.waitErr = run.command.Wait()
		close(run.done)
	}()
	return run
}

func (run *activePublicRun) release() error {
	run.stateMu.Lock()
	defer run.stateMu.Unlock()
	if run.released {
		return nil
	}
	path := filepath.Join(run.markers, "release")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		run.released = true
		return nil
	}
	if err != nil {
		return err
	}
	_, writeErr := file.WriteString("release\n")
	if err := errors.Join(writeErr, file.Close()); err != nil {
		return err
	}
	run.released = true
	return nil
}

func (run *activePublicRun) wait(timeout time.Duration) publicRunResult {
	if !run.waitForExit(timeout) {
		return publicRunResult{err: fmt.Errorf("public run did not exit within %s", timeout)}
	}
	return run.result()
}

func (run *activePublicRun) stopAfterGateRelease() (bool, error) {
	if run.waitForExit(10 * time.Second) {
		return run.wasForceStopped(), nil
	}
	run.recordForceStop()
	signalErr := run.command.Process.Signal(os.Interrupt)
	if run.waitForExit(15 * time.Second) {
		return true, ignoreFinishedProcessError(signalErr)
	}
	killErr := run.command.Process.Kill()
	if !run.waitForExit(5 * time.Second) {
		return true, errors.Join(ignoreFinishedProcessError(signalErr), killErr,
			errors.New("public run did not stop after SIGINT and kill"))
	}
	return true, errors.Join(ignoreFinishedProcessError(signalErr), ignoreFinishedProcessError(killErr))
}

func (run *activePublicRun) recordForceStop() {
	run.stateMu.Lock()
	defer run.stateMu.Unlock()
	run.forceStopped = true
}

func (run *activePublicRun) wasForceStopped() bool {
	run.stateMu.Lock()
	defer run.stateMu.Unlock()
	return run.forceStopped
}

func (run *activePublicRun) waitForExit(timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-run.done:
		return true
	case <-timer.C:
		return false
	}
}

func (run *activePublicRun) exited() bool {
	select {
	case <-run.done:
		return true
	default:
		return false
	}
}

func ignoreFinishedProcessError(err error) error {
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

func (run *activePublicRun) result() publicRunResult {
	return publicRunResult{stdout: run.stdout.String(), stderr: run.stderr.String(), err: run.waitErr}
}

func (run *activePublicRun) captureExactIdentity(id runid.ID) error {
	if run.identityCaptured {
		return errors.New("public run identity was already captured")
	}
	supervisorPID, err := exactSupervisorChildPID(run.command.Process.Pid)
	if err != nil {
		return err
	}
	run.runID = id
	run.supervisorPID = supervisorPID
	run.identityCaptured = true
	return nil
}

type concurrentPublicRunFixture struct {
	fixture         *hostNetworkFixture
	source          string
	files           map[string]transferFileEvidence
	markerRoot      string
	baselineSurface string
	baselineRuns    []string
	runs            []*activePublicRun
}

func newConcurrentPublicRunFixture(t *testing.T, fixture *hostNetworkFixture) *concurrentPublicRunFixture {
	t.Helper()
	testRoot := t.TempDir()
	source := filepath.Join(testRoot, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	for index := range 6 {
		name := fmt.Sprintf("payload-%02d", index+1)
		content := bytes.Repeat([]byte{byte('a' + index)}, 128)
		if err := os.WriteFile(filepath.Join(source, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := readTransferFiles(source)
	if err != nil {
		t.Fatal(err)
	}
	result := &concurrentPublicRunFixture{
		fixture: fixture, source: source, files: indexTransferFiles(files),
		markerRoot:      filepath.Join(testRoot, "overlapping-runs"),
		baselineSurface: fixture.hostSurface(), baselineRuns: authorityEntries(t),
	}
	if err := os.Mkdir(result.markerRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := result.stopOwnedRunsAndRecoverExactRoots(); err != nil {
			t.Errorf("safety cleanup for concurrent public runs: %v", err)
		}
	})
	return result
}

func (fixture *concurrentPublicRunFixture) start(t *testing.T, name, heldReady string,
	followupChecks int,
) *activePublicRun {
	t.Helper()
	markers := filepath.Join(fixture.markerRoot, name)
	if err := os.Mkdir(markers, 0o700); err != nil {
		t.Fatalf("create %s run markers: %v", name, err)
	}
	run := startActivePublicRun(t,
		concurrentRunArguments(fixture.source, fixture.fixture.sources, fixture.fixture.remote,
			markers, heldReady, followupChecks),
		[]string{supervisorTransferEnv + "=" + fixture.source}, markers)
	fixture.runs = append(fixture.runs, run)
	return run
}

func (fixture *concurrentPublicRunFixture) startReadyRuns(t *testing.T,
	followupChecks []int,
) ([]*activePublicRun, []map[int]int) {
	t.Helper()
	runs := make([]*activePublicRun, 0, len(followupChecks))
	readyPIDs := make([]map[int]int, 0, len(followupChecks))
	for index, checks := range followupChecks {
		run := fixture.start(t, fmt.Sprintf("concurrent-%d", index+1), "", checks)
		id, pids, err := waitForTransferGateMarkers(run.markers, "ready", len(fixture.fixture.sources),
			10*time.Second)
		if err != nil {
			if run.waitForExit(time.Second) {
				result := run.result()
				t.Fatalf("run %d exited before its transfers reached the gate: %v stdout=%q stderr=%q",
					index+1, result.err, result.stdout, result.stderr)
			}
			t.Fatalf("run %d transfers did not reach the gate: %v", index+1, err)
		}
		if err := run.captureExactIdentity(id); err != nil {
			t.Fatalf("capture run %d exact identity: %v", index+1, err)
		}
		runs = append(runs, run)
		readyPIDs = append(readyPIDs, pids)
		fixture.assertTransferUse(t, run, pids, "initial")
		claims := readExactNetworkClaims(t, runs)
		for _, claim := range claims {
			assertActiveRunNetworkObjects(t, fixture.fixture, claim)
		}
		if len(claims) > 1 {
			assertIndependentRunNetworkObjects(t, claims)
		}
	}
	return runs, readyPIDs
}

func assertConcurrentRunCompleted(t *testing.T, run *activePublicRun, readyPIDs map[int]int,
	transfers int,
) {
	t.Helper()
	result := run.wait(15 * time.Second)
	if result.err != nil {
		t.Fatalf("concurrent run failed: %v stdout=%q stderr=%q",
			result.err, result.stdout, result.stderr)
	}
	assertSuccessfulPlainRunOutput(t, result, transfers)
	if err := waitForGlobCount(filepath.Join(run.markers, "transfer-*.done"),
		transfers, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	doneID, donePIDs, err := waitForTransferGateMarkers(run.markers, "done", transfers, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !run.identityCaptured || doneID != run.runID {
		t.Fatalf("done marker run ID = %s, want exact run %s", doneID, run.runID)
	}
	if !maps.Equal(readyPIDs, donePIDs) {
		t.Fatalf("ready/done child processes differ: ready=%v done=%v", readyPIDs, donePIDs)
	}
}

func indexTransferFiles(files []transferFileEvidence) map[string]transferFileEvidence {
	result := make(map[string]transferFileEvidence, len(files))
	for _, file := range files {
		result[file.Path] = file
	}
	return result
}

func (fixture *concurrentPublicRunFixture) requestTransferCheck(t *testing.T,
	run *activePublicRun, check int, readyPIDs map[int]int,
) {
	t.Helper()
	stage := fmt.Sprintf("check-%d", check)
	if err := os.WriteFile(filepath.Join(run.markers, stage), []byte("check\n"), 0o600); err != nil {
		t.Fatalf("request %s: %v", stage, err)
	}
	if err := waitForGlobCount(filepath.Join(run.markers, "transfer-*."+stage+".json"),
		len(fixture.fixture.sources), 10*time.Second); err != nil {
		t.Fatal(err)
	}
	fixture.assertTransferUse(t, run, readyPIDs, stage)
}

func (fixture *concurrentPublicRunFixture) assertTransferUse(t *testing.T, run *activePublicRun,
	readyPIDs map[int]int, stage string,
) {
	t.Helper()
	observed := make(map[string]transferFileEvidence, len(fixture.files))
	for number, source := range fixture.fixture.sources {
		path := filepath.Join(run.markers,
			fmt.Sprintf("transfer-%03d.%s.json", number+1, stage))
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read transfer %d %s evidence: %v", number+1, stage, err)
		}
		var evidence transferUseEvidence
		if err := json.Unmarshal(contents, &evidence); err != nil {
			t.Fatalf("decode transfer %d %s evidence: %v", number+1, stage, err)
		}
		marker, err := readTransferGateMarker(filepath.Join(run.markers,
			fmt.Sprintf("transfer-%03d.ready", number+1)))
		if err != nil || evidence.PID != readyPIDs[number+1] || evidence.RunID != marker.runID.String() ||
			evidence.Source != source.String() || len(evidence.Files) == 0 {
			t.Fatalf("transfer %d %s evidence is incomplete: %#v marker=%#v error=%v",
				number+1, stage, evidence, marker, err)
		}
		for _, file := range evidence.Files {
			if _, duplicate := observed[file.Path]; duplicate {
				t.Fatalf("transfer %d %s duplicated file %q", number+1, stage, file.Path)
			}
			observed[file.Path] = file
		}
	}
	if !maps.Equal(observed, fixture.files) {
		t.Fatalf("%s transfer view contents = %#v, want %#v", stage, observed, fixture.files)
	}
}

func (fixture *concurrentPublicRunFixture) assertActiveRuns(t *testing.T, runs []*activePublicRun) {
	t.Helper()
	for _, run := range runs {
		if run.exited() {
			t.Fatalf("surviving run exited early: %#v", run.result())
		}
		if !run.identityCaptured {
			t.Fatal("surviving run has no exact identity")
		}
	}
	claims := readExactNetworkClaims(t, runs)
	for _, claim := range claims {
		assertActiveRunNetworkObjects(t, fixture.fixture, claim)
	}
	if len(claims) > 1 {
		assertIndependentRunNetworkObjects(t, claims)
	}
}

func (fixture *concurrentPublicRunFixture) releaseAll() error {
	var result error
	for _, run := range fixture.runs {
		result = errors.Join(result, run.release())
	}
	return result
}

func (fixture *concurrentPublicRunFixture) stopOwnedRunsAndRecoverExactRoots() error {
	var result error
	result = errors.Join(result, fixture.releaseAll())
	for _, run := range fixture.runs {
		_, err := run.stopAfterGateRelease()
		if err != nil {
			result = errors.Join(result,
				fmt.Errorf("stop public run using %s: %w", run.markers, err))
		}
		if !run.identityCaptured {
			continue
		}
		runRoot := filepath.Join(rundirectory.AuthorityRoot, run.runID.String())
		if _, statErr := os.Stat(runRoot); errors.Is(statErr, os.ErrNotExist) {
			continue
		} else if statErr != nil {
			result = errors.Join(result, statErr)
			continue
		}
		recovered, recoverErr := recoverExactPublicRun(run.runID)
		if recoverErr != nil {
			result = errors.Join(result, recoverErr)
		} else if !recovered {
			result = errors.Join(result, fmt.Errorf("run %s remained but was not recoverable", run.runID))
		}
	}
	return result
}

func recoverExactPublicRun(id runid.ID) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	recovered := false
	err := rundirectory.New().RecoverStale(ctx, func(stale *rundirectory.StaleRun) error {
		if stale.ID() != id {
			return fmt.Errorf("refuse recovery of run %s while targeting %s", stale.ID(), id)
		}
		if err := hostnetwork.New().Recover(ctx, stale); err != nil {
			return fmt.Errorf("recover host network for run %s: %w", id, err)
		}
		if err := fileviews.New().Recover(ctx, stale); err != nil {
			return fmt.Errorf("recover file views for run %s: %w", id, err)
		}
		recovered = true
		return nil
	})
	return recovered, err
}

func concurrentRunArguments(source string, sources []netip.Addr, remote netip.Addr,
	markers, heldReady string, followupChecks int,
) []string {
	arguments := []string{"run", "--no-tui", "--source", source}
	for _, selected := range sources {
		arguments = append(arguments, "--network", selected.String())
	}
	arguments = append(arguments, "--")
	return append(arguments,
		transferChildCommand(os.Args[0], transferChildOverlapGate, markers, heldReady,
			strconv.Itoa(followupChecks), "http://"+net.JoinHostPort(remote.String(), "18081")+"/overlap",
			strings.Join(networkStrings(sources), ","), "{}")...)
}

func networkStrings(sources []netip.Addr) []string {
	result := make([]string, len(sources))
	for index, source := range sources {
		result[index] = source.String()
	}
	return result
}

func waitForTransferGateMarkers(directory, state string, transferCount int,
	timeout time.Duration,
) (runid.ID, map[int]int, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		seenPIDs := make(map[int]struct{}, transferCount)
		transferPIDs := make(map[int]int, transferCount)
		var runID runid.ID
		valid := true
		for number := 1; number <= transferCount; number++ {
			path := filepath.Join(directory, fmt.Sprintf("transfer-%03d.%s", number, state))
			marker, err := readTransferGateMarker(path)
			if err != nil {
				lastErr, valid = fmt.Errorf("read transfer %d %s marker: %w", number, state, err), false
				break
			}
			if _, duplicate := seenPIDs[marker.pid]; duplicate {
				lastErr, valid = fmt.Errorf("transfer %d reused child PID %d in %s markers", number,
					marker.pid, state), false
				break
			}
			if number == 1 {
				runID = marker.runID
			} else if marker.runID != runID {
				lastErr, valid = fmt.Errorf("%s markers identify multiple runs: %s and %s", state,
					runID, marker.runID), false
				break
			}
			seenPIDs[marker.pid] = struct{}{}
			transferPIDs[number] = marker.pid
		}
		if valid {
			return runID, transferPIDs, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return runid.ID{}, nil, fmt.Errorf("timed out waiting for %d complete %s markers in %s: %v",
		transferCount, state, directory, lastErr)
}

func waitForHeldTransferGateMarkers(directory string, transferCount, heldTransfer int,
	timeout time.Duration,
) (runid.ID, map[int]int, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		seenPIDs := make(map[int]struct{}, transferCount)
		transferPIDs := make(map[int]int, transferCount)
		var runID runid.ID
		valid := true
		for number := 1; number <= transferCount; number++ {
			state := "ready"
			if number == heldTransfer {
				state = "waiting"
			}
			marker, err := readTransferGateMarker(filepath.Join(directory,
				fmt.Sprintf("transfer-%03d.%s", number, state)))
			if err != nil {
				lastErr, valid = err, false
				break
			}
			if _, duplicate := seenPIDs[marker.pid]; duplicate {
				lastErr, valid = fmt.Errorf("transfer %d reused child PID %d", number, marker.pid), false
				break
			}
			if number == 1 {
				runID = marker.runID
			} else if marker.runID != runID {
				lastErr, valid = fmt.Errorf("held gate markers identify multiple runs: %s and %s",
					runID, marker.runID), false
				break
			}
			seenPIDs[marker.pid] = struct{}{}
			transferPIDs[number] = marker.pid
		}
		if valid {
			return runID, transferPIDs, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return runid.ID{}, nil, fmt.Errorf("timed out waiting for held transfer %d identity in %s: %v",
		heldTransfer, directory, lastErr)
}

type transferGateMarker struct {
	pid   int
	runID runid.ID
}

func readTransferGateMarker(path string) (transferGateMarker, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return transferGateMarker{}, err
	}
	return decodeTransferGateMarker(contents)
}

func decodeTransferGateMarker(contents []byte) (transferGateMarker, error) {
	lines := strings.Split(strings.TrimSpace(string(contents)), "\n")
	if len(lines) != 2 {
		return transferGateMarker{}, errors.New("transfer gate marker must contain PID and run ID")
	}
	pidText, hasPID := strings.CutPrefix(lines[0], "pid=")
	idText, hasRunID := strings.CutPrefix(lines[1], "run_id=")
	pid, pidErr := strconv.Atoi(pidText)
	id, idErr := runid.Parse(idText)
	if !hasPID || !hasRunID || pidErr != nil || pid <= 0 || idErr != nil {
		return transferGateMarker{}, errors.Join(errors.New("invalid transfer gate marker"), pidErr, idErr)
	}
	return transferGateMarker{pid: pid, runID: id}, nil
}

func exactSupervisorChildPID(publicPID int) (int, error) {
	if publicPID <= 0 {
		return 0, errors.New("public process PID is invalid")
	}
	taskRoot := filepath.Join("/proc", strconv.Itoa(publicPID), "task")
	tasks, err := os.ReadDir(taskRoot)
	if err != nil {
		return 0, fmt.Errorf("read tasks of public process %d: %w", publicPID, err)
	}
	var supervisors []int
	for _, task := range tasks {
		contents, readErr := os.ReadFile(filepath.Join(taskRoot, task.Name(), "children"))
		if readErr != nil {
			if errors.Is(readErr, os.ErrNotExist) {
				continue
			}
			return 0, fmt.Errorf("read children of public process %d task %s: %w",
				publicPID, task.Name(), readErr)
		}
		for _, field := range strings.Fields(string(contents)) {
			pid, parseErr := strconv.Atoi(field)
			if parseErr != nil {
				return 0, fmt.Errorf("parse child PID %q of public process %d: %w",
					field, publicPID, parseErr)
			}
			arguments, argumentErr := processArguments(pid)
			if argumentErr == nil && len(arguments) == 2 &&
				arguments[0] == os.Getenv("TRANSFERLANES_TEST_TRANSFERLANES") &&
				arguments[1] == "--transferlanes-internal-run-supervisor" {
				supervisors = append(supervisors, pid)
			}
		}
	}
	if len(supervisors) != 1 {
		return 0, fmt.Errorf("public process %d exact supervisor children = %v, want one",
			publicPID, supervisors)
	}
	return supervisors[0], nil
}

func processArguments(pid int) ([]string, error) {
	contents, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return nil, fmt.Errorf("read process %d arguments: %w", pid, err)
	}
	encoded := bytes.Split(bytes.TrimSuffix(contents, []byte{0}), []byte{0})
	arguments := make([]string, len(encoded))
	for index := range encoded {
		arguments[index] = string(encoded[index])
	}
	return arguments, nil
}

type observedNetworkClaim struct {
	root        string
	id          runid.ID
	protocol    uint8
	backend     string
	firewall    string
	returnTable int
	transfers   []observedClaimTransfer
}

type observedClaimTransfer struct {
	number           int
	source           netip.Addr
	provider         string
	subnet           netip.Prefix
	routeTable       int
	outboundPriority int
	returnPriority   int
	linkOwner        string
	hostVeth         string
}

type networkClaimDocument struct {
	RunID         string                         `json:"run_id"`
	Protocol      uint8                          `json:"protocol"`
	Backend       string                         `json:"backend"`
	FirewallTable string                         `json:"firewall_table"`
	ReturnTable   int                            `json:"return_table"`
	Transfers     []networkClaimTransferDocument `json:"transfers"`
}

type networkClaimTransferDocument struct {
	Number           int    `json:"number"`
	Source           string `json:"source"`
	ProviderName     string `json:"provider_name"`
	Subnet           string `json:"subnet"`
	RouteTable       int    `json:"route_table"`
	OutboundPriority int    `json:"outbound_priority"`
	ReturnPriority   int    `json:"return_priority"`
	LinkOwnerID      string `json:"link_owner_id"`
	HostVeth         string `json:"host_veth"`
}

func readExactNetworkClaims(t *testing.T, runs []*activePublicRun) []observedNetworkClaim {
	t.Helper()
	claims := make([]observedNetworkClaim, 0, len(runs))
	for _, run := range runs {
		if !run.identityCaptured {
			t.Fatal("cannot read network claim before exact run identity is captured")
		}
		claims = append(claims, readNetworkClaim(t,
			filepath.Join(rundirectory.AuthorityRoot, run.runID.String())))
	}
	slices.SortFunc(claims, func(left, right observedNetworkClaim) int {
		return strings.Compare(left.id.String(), right.id.String())
	})
	return claims
}

func readNetworkClaim(t *testing.T, root string) observedNetworkClaim {
	t.Helper()
	id, err := runid.Parse(filepath.Base(root))
	if err != nil {
		t.Fatalf("parse active run root %q: %v", root, err)
	}
	contents, err := os.ReadFile(filepath.Join(root, "network", "claim"))
	if err != nil {
		t.Fatalf("read active network claim %s: %v", id, err)
	}
	var document networkClaimDocument
	if err := json.Unmarshal(contents, &document); err != nil {
		t.Fatalf("decode active network claim %s: %v", id, err)
	}
	if document.RunID != id.String() {
		t.Fatalf("network claim run ID = %q, want %s", document.RunID, id)
	}
	claim := observedNetworkClaim{root: root, id: id, protocol: document.Protocol,
		backend: document.Backend, firewall: document.FirewallTable, returnTable: document.ReturnTable}
	for _, value := range document.Transfers {
		source, sourceErr := netip.ParseAddr(value.Source)
		subnet, subnetErr := netip.ParsePrefix(value.Subnet)
		if sourceErr != nil || subnetErr != nil {
			t.Fatalf("decode claim %s transfer %d addresses: %v / %v", id, value.Number, sourceErr, subnetErr)
		}
		claim.transfers = append(claim.transfers, observedClaimTransfer{
			number: value.Number, source: source, provider: value.ProviderName, subnet: subnet,
			routeTable: value.RouteTable, outboundPriority: value.OutboundPriority,
			returnPriority: value.ReturnPriority,
			linkOwner:      value.LinkOwnerID, hostVeth: value.HostVeth,
		})
	}
	return claim
}

func assertActiveRunNetworkObjects(t *testing.T, fixture *hostNetworkFixture, claim observedNetworkClaim) {
	t.Helper()
	info, err := os.Stat(claim.root)
	if err != nil || !info.IsDir() {
		t.Fatalf("active run root %s is unavailable: %v", claim.root, err)
	}
	if len(claim.transfers) != len(fixture.sources) {
		t.Fatalf("run %s transfer count = %d, want %d", claim.id, len(claim.transfers), len(fixture.sources))
	}
	links := make([]observedTransferLink, len(claim.transfers))
	for index, transfer := range claim.transfers {
		if transfer.number != index+1 || transfer.source != fixture.sources[index] ||
			transfer.provider != fixture.providers[fixture.sourceLinks[index]] {
			t.Fatalf("run %s transfer %d does not represent selected network %s: %#v",
				claim.id, index+1, fixture.sources[index], transfer)
		}
		link, linkErr := netlink.LinkByName(transfer.hostVeth)
		if linkErr != nil || link == nil || link.Type() != "veth" {
			t.Fatalf("run %s transfer %d host veth %q is unavailable: %v",
				claim.id, transfer.number, transfer.hostVeth, linkErr)
		}
		links[index] = observedTransferLink{hostName: transfer.hostVeth}
	}
	assertClaimPolicyRules(t, claim)
	fixture.assertOwnedRulesAndNFT(claim.id, links)
	fixture.assertKernelRouteLookups(links)
}

func assertClaimPolicyRules(t *testing.T, claim observedNetworkClaim) {
	t.Helper()
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		t.Fatalf("list policy rules for run %s: %v", claim.id, err)
	}
	for _, transfer := range claim.transfers {
		outbound, returning := false, false
		for _, rule := range rules {
			if rule.Priority == transfer.outboundPriority && rule.Table == transfer.routeTable &&
				rule.Protocol == claim.protocol && rule.IifName == transfer.hostVeth &&
				rule.Src != nil && rule.Src.String() == transfer.subnet.String() {
				outbound = true
			}
			if rule.Priority == transfer.returnPriority && rule.Table == claim.returnTable &&
				rule.Protocol == claim.protocol && rule.IifName == transfer.provider &&
				rule.Dst != nil && rule.Dst.String() == transfer.subnet.String() {
				returning = true
			}
		}
		if !outbound || !returning {
			t.Fatalf("run %s transfer %d active rules: outbound=%t return=%t",
				claim.id, transfer.number, outbound, returning)
		}
	}
}

func assertIndependentRunNetworkObjects(t *testing.T, claims []observedNetworkClaim) {
	t.Helper()
	if len(claims) < 2 {
		t.Fatalf("independence requires overlapping runs: %#v", claims)
	}
	seenRuns := make(map[runid.ID]struct{}, len(claims))
	seenRoots := make(map[string]struct{}, len(claims))
	seenProtocols := make(map[uint8]struct{}, len(claims))
	seenReturnTables := make(map[int]struct{}, len(claims))
	seenFirewalls := make(map[string]struct{}, len(claims))
	seenNames := make(map[string]string)
	seenPriorities := make(map[int]string)
	var subnets []netip.Prefix
	for _, claim := range claims {
		if _, exists := seenRuns[claim.id]; exists {
			t.Fatalf("active runs share run identity %s", claim.id)
		}
		seenRuns[claim.id] = struct{}{}
		if _, exists := seenRoots[claim.root]; claim.root == "" || exists {
			t.Fatalf("active runs share run root %q", claim.root)
		}
		seenRoots[claim.root] = struct{}{}
		if _, exists := seenProtocols[claim.protocol]; claim.protocol == 0 || exists {
			t.Fatalf("active runs share routing protocol %d", claim.protocol)
		}
		seenProtocols[claim.protocol] = struct{}{}
		if _, exists := seenReturnTables[claim.returnTable]; claim.returnTable == 0 || exists {
			t.Fatalf("active runs share return table %d", claim.returnTable)
		}
		seenReturnTables[claim.returnTable] = struct{}{}
		if _, exists := seenFirewalls[claim.firewall]; claim.firewall == "" || exists ||
			claim.backend != claims[0].backend {
			t.Fatalf("active runs lack independent firewall objects: %#v", claims)
		}
		seenFirewalls[claim.firewall] = struct{}{}
		for _, transfer := range claim.transfers {
			owner := claim.id.String() + "/transfer-" + strconv.Itoa(transfer.number)
			for _, name := range []string{transfer.linkOwner, transfer.hostVeth} {
				if previous, exists := seenNames[name]; name == "" || exists {
					t.Fatalf("temporary network identity %q is shared by %s and %s", name, previous, owner)
				}
				seenNames[name] = owner
			}
			for _, priority := range []int{transfer.outboundPriority, transfer.returnPriority} {
				if previous, exists := seenPriorities[priority]; priority == 0 || exists {
					t.Fatalf("policy priority %d is shared by %s and %s", priority, previous, owner)
				}
				seenPriorities[priority] = owner
			}
			for _, existing := range subnets {
				if existing.Overlaps(transfer.subnet) {
					t.Fatalf("temporary subnets overlap: %s and %s", existing, transfer.subnet)
				}
			}
			subnets = append(subnets, transfer.subnet)
		}
	}
}

func describeActiveNetworkObjects(claims []observedNetworkClaim) string {
	parts := make([]string, 0, len(claims))
	for _, claim := range claims {
		transfers := make([]string, 0, len(claim.transfers))
		for _, transfer := range claim.transfers {
			transfers = append(transfers, fmt.Sprintf("%s/%s/out:%d/return:%d",
				transfer.hostVeth, transfer.subnet, transfer.outboundPriority, transfer.returnPriority))
		}
		parts = append(parts, fmt.Sprintf("run=%s root=%s protocol=%d return-table=%d firewall=%s transfers=[%s]",
			claim.id, claim.root, claim.protocol, claim.returnTable, claim.firewall, strings.Join(transfers, ",")))
	}
	return strings.Join(parts, "; ")
}

func assertSuccessfulPlainRunOutput(t *testing.T, result publicRunResult, transferCount int) {
	t.Helper()
	output := result.stdout + result.stderr
	for number := 1; number <= transferCount; number++ {
		if !strings.Contains(output, fmt.Sprintf("transfer %d completed: exit=0 signal=0", number)) {
			t.Fatalf("public run output omits successful transfer %d: stdout=%q stderr=%q",
				number, result.stdout, result.stderr)
		}
	}
}
