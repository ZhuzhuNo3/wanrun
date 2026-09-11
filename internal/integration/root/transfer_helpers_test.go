//go:build linux && rootintegration

package root_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/fileviews"
	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"golang.org/x/sys/unix"
)

type transferViewEvidence struct {
	Argument     string   `json:"argument"`
	Members      []string `json:"members"`
	InheritedFDs []string `json:"inheritedFDs"`
}

func runSupervisorTransferViewChild(argv []string, failOnHidden bool) int {
	if len(argv) != 2 {
		return 2
	}
	files, err := readTransferFiles(argv[1])
	if err != nil {
		return 3
	}
	members := make([]string, len(files))
	for index, file := range files {
		members[index] = filepath.FromSlash(file.Path)
	}
	fds, err := inheritedDescriptorTargets()
	if err != nil {
		return 4
	}
	evidence := transferViewEvidence{Argument: argv[1], Members: members, InheritedFDs: fds}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return 4
	}
	file, err := os.OpenFile(argv[0], os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 5
	}
	_, writeErr := file.Write(append(encoded, '\n'))
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return 6
	}
	if failOnHidden && slices.Contains(members, ".hidden") {
		return 7
	}
	return 0
}

func runSupervisorTransferBlockChild(argv []string) int {
	if len(argv) != 2 || !validPrivateViewAlias(argv[1]) {
		return 2
	}
	id, err := privateViewRunID(argv[1])
	if err != nil {
		return 2
	}
	signals := make(chan os.Signal, 1)
	signalNotify(signals)
	defer signalStop(signals)
	_, _ = fmt.Fprint(os.Stdout, "transferlanes-signal-ready\n")
	ready := fmt.Sprintf("%s.%d.ready", argv[0], os.Getpid())
	if err := writeExclusiveTransferGateMarker(ready, id); err != nil {
		return 3
	}
	<-signals
	return 0
}

func runSupervisorTransferOverlapGateChild(argv []string) int {
	child, err := parseTransferOverlapChild(argv)
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 2
	}
	return child.run()
}

type transferOverlapChild struct {
	directory      string
	transferName   string
	heldReady      string
	followupChecks int
	view           string
	endpoint       string
	expectedSource netip.Addr
	runID          runid.ID
}

func parseTransferOverlapChild(argv []string) (transferOverlapChild, error) {
	if len(argv) != 6 {
		return transferOverlapChild{}, errors.New("transfer overlap child requires six arguments")
	}
	id, err := privateViewRunID(argv[5])
	if err != nil {
		return transferOverlapChild{}, err
	}
	transferName := filepath.Base(filepath.Dir(argv[5]))
	heldReady := argv[1]
	if !strings.HasPrefix(transferName, "transfer-") ||
		heldReady != "" && !strings.HasPrefix(heldReady, "transfer-") {
		return transferOverlapChild{}, errors.New("transfer overlap child has invalid transfer identity")
	}
	transferIndex, err := strconv.Atoi(strings.TrimPrefix(transferName, "transfer-"))
	checks, checksErr := strconv.Atoi(argv[2])
	sources := strings.Split(argv[4], ",")
	if err != nil || checksErr != nil || checks < 0 || transferIndex < 1 || transferIndex > len(sources) {
		return transferOverlapChild{}, errors.Join(errors.New("transfer overlap child has invalid check or source selection"),
			err, checksErr)
	}
	expectedSource, err := netip.ParseAddr(sources[transferIndex-1])
	if err != nil {
		return transferOverlapChild{}, err
	}
	return transferOverlapChild{directory: argv[0], transferName: transferName, heldReady: heldReady,
		followupChecks: checks, view: argv[5], endpoint: argv[3], expectedSource: expectedSource,
		runID: id}, nil
}

func (child transferOverlapChild) run() int {
	if err := child.recordUse("initial"); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 3
	}
	if child.transferName == child.heldReady {
		if err := writeExclusiveTransferGateMarker(child.markerPath("waiting"), child.runID); err != nil {
			return 4
		}
	} else if err := writeExclusiveTransferGateMarker(child.markerPath("ready"), child.runID); err != nil {
		return 5
	}
	for check := 1; check <= child.followupChecks; check++ {
		stage := fmt.Sprintf("check-%d", check)
		if err := waitForFileUntil(filepath.Join(child.directory, stage), 30*time.Second); err != nil {
			return 6
		}
		if err := child.recordUse(stage); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			return 7
		}
	}
	if err := waitForFileUntil(filepath.Join(child.directory, "release"), 30*time.Second); err != nil {
		return 8
	}
	if err := child.recordUse("final"); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 9
	}
	if err := writeExclusiveTransferGateMarker(child.markerPath("done"), child.runID); err != nil {
		return 10
	}
	return 0
}

func (child transferOverlapChild) recordUse(stage string) error {
	return recordTransferUse(child.directory, child.transferName, stage, child.view, child.endpoint,
		child.expectedSource, child.runID)
}

func (child transferOverlapChild) markerPath(stage string) string {
	return filepath.Join(child.directory, child.transferName+"."+stage)
}

type transferFileEvidence struct {
	Path   string `json:"path"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}

type transferUseEvidence struct {
	PID    int                    `json:"pid"`
	RunID  string                 `json:"run_id"`
	Source string                 `json:"source"`
	Files  []transferFileEvidence `json:"files"`
}

func recordTransferUse(directory, transferName, stage, view, endpoint string,
	expectedSource netip.Addr, id runid.ID,
) error {
	files, err := readTransferFiles(view)
	if err != nil || len(files) == 0 {
		return errors.Join(errors.New("transfer view has no readable files"), err)
	}
	source, err := fetchObservedSource(endpoint)
	if err != nil {
		return err
	}
	if source != expectedSource {
		return fmt.Errorf("transfer source = %s, want %s", source, expectedSource)
	}
	evidence := transferUseEvidence{PID: os.Getpid(), RunID: id.String(),
		Source: source.String(), Files: files}
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	path := filepath.Join(directory, transferName+"."+stage+".json")
	temporary, err := os.CreateTemp(directory, ".transfer-use-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	chmodErr := temporary.Chmod(0o600)
	_, writeErr := temporary.Write(append(encoded, '\n'))
	writeErr = errors.Join(chmodErr, writeErr, temporary.Close())
	if writeErr != nil {
		_ = os.Remove(temporaryPath)
		return writeErr
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	return nil
}

func readTransferFiles(root string) ([]transferFileEvidence, error) {
	view := os.DirFS(root)
	var result []transferFileEvidence
	err := fs.WalkDir(view, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || path == "." || entry.IsDir() {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("transfer view member %q is not a regular file", path)
		}
		content, err := fs.ReadFile(view, path)
		if err != nil {
			return err
		}
		result = append(result, transferFileEvidence{Path: path, Size: len(content),
			SHA256: fmt.Sprintf("%x", sha256.Sum256(content))})
		return nil
	})
	slices.SortFunc(result, func(left, right transferFileEvidence) int {
		return strings.Compare(left.Path, right.Path)
	})
	return result, err
}

func fetchObservedSource(endpoint string) (netip.Addr, error) {
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	response, err := client.Get(endpoint)
	if err != nil {
		return netip.Addr{}, err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, 64))
	if err != nil || response.StatusCode != http.StatusOK {
		return netip.Addr{}, errors.Join(fmt.Errorf("transfer network status = %d", response.StatusCode), err)
	}
	return netip.ParseAddr(strings.TrimSpace(string(content)))
}

func writeExclusiveTransferGateMarker(path string, id runid.ID) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := fmt.Fprintf(file, "pid=%d\nrun_id=%s\n", os.Getpid(), id)
	return errors.Join(writeErr, file.Close())
}

type transferResolverEvidence struct {
	Content  string `json:"content"`
	ReadOnly bool   `json:"readOnly"`
}

type outputBackpressureStage struct {
	PID   int
	RunID runid.ID
}

func waitForOutputBackpressureStages(pattern string, want int, timeout time.Duration) ([]outputBackpressureStage, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		paths, globErr := filepath.Glob(pattern)
		if globErr != nil {
			return nil, globErr
		}
		if len(paths) == want {
			stages := make([]outputBackpressureStage, 0, want)
			seenPIDs := make(map[int]struct{}, want)
			var runID runid.ID
			valid := true
			for index, path := range paths {
				contents, readErr := os.ReadFile(path)
				if readErr != nil {
					lastErr, valid = readErr, false
					break
				}
				stage, parseErr := parseOutputBackpressureStage(contents)
				if parseErr != nil {
					lastErr, valid = parseErr, false
					break
				}
				if _, duplicate := seenPIDs[stage.PID]; duplicate || index != 0 && stage.RunID != runID {
					lastErr, valid = errors.New("output backpressure stages do not identify one run and unique children"), false
					break
				}
				seenPIDs[stage.PID] = struct{}{}
				runID = stage.RunID
				stages = append(stages, stage)
			}
			if valid {
				return stages, nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil, fmt.Errorf("timed out waiting for %d output backpressure stages %q: %v", want, pattern, lastErr)
}

func parseOutputBackpressureStage(contents []byte) (outputBackpressureStage, error) {
	fields := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(contents)), "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok || name == "" || value == "" {
			return outputBackpressureStage{}, fmt.Errorf("invalid output backpressure stage %q", contents)
		}
		fields[name] = value
	}
	pid, err := strconv.Atoi(fields["pid"])
	if err != nil || pid <= 0 {
		return outputBackpressureStage{}, fmt.Errorf("invalid output writer pid %q", fields["pid"])
	}
	id, err := privateViewRunID(fields["view"])
	if err != nil {
		return outputBackpressureStage{}, err
	}
	return outputBackpressureStage{PID: pid, RunID: id}, nil
}

func waitForSupervisorEventPipeFull(pid int, timeout time.Duration) error {
	path := filepath.Join("/proc", strconv.Itoa(pid), "fd", "4")
	writeLink, err := os.Readlink(path)
	if err != nil || !strings.HasPrefix(writeLink, "pipe:[") {
		return fmt.Errorf("supervisor %d event pipe identity = %q: %w", pid, writeLink, err)
	}
	probe, err := os.OpenFile(path, os.O_WRONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open supervisor %d event pipe probe: %w", pid, err)
	}
	defer probe.Close()
	deadline := time.Now().Add(timeout)
	var last unix.PollFd
	for time.Now().Before(deadline) {
		poll := []unix.PollFd{{Fd: int32(probe.Fd()), Events: unix.POLLOUT | unix.POLLERR | unix.POLLHUP}}
		count, pollErr := unix.Poll(poll, 0)
		last = poll[0]
		if pollErr == unix.EINTR {
			continue
		}
		if pollErr != nil {
			return pollErr
		}
		if count == 0 {
			return nil
		}
		if last.Revents&(unix.POLLERR|unix.POLLHUP) != 0 {
			return fmt.Errorf("supervisor %d event pipe %s closed before backpressure: revents=%#x",
				pid, writeLink, last.Revents)
		}
		if err := unix.Kill(pid, 0); err != nil {
			return fmt.Errorf("supervisor %d exited before event-pipe backpressure: %w", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for supervisor %d event pipe %s to block: revents=%#x",
		pid, writeLink, last.Revents)
}

func waitForOutputWriterExit(pid int, timeout time.Duration) error {
	path := filepath.Join("/proc", strconv.Itoa(pid))
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		_, lastErr = os.Stat(path)
		if errors.Is(lastErr, os.ErrNotExist) {
			return nil
		}
		if lastErr != nil {
			return lastErr
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for output writer %d to exit; last stat=%v", pid, lastErr)
}

func runSupervisorTransferResolverChild(argv []string) int {
	if len(argv) != 4 || !validPrivateViewAlias(argv[3]) ||
		argv[2] != "finish" && argv[2] != "block" {
		return 2
	}
	content, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return 3
	}
	writer, writeErr := os.OpenFile("/etc/resolv.conf", os.O_WRONLY, 0)
	if writer != nil {
		_ = writer.Close()
	}
	evidence, err := json.Marshal(transferResolverEvidence{Content: string(content), ReadOnly: writeErr != nil})
	if err != nil {
		return 4
	}
	file, err := os.OpenFile(argv[0], os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 5
	}
	_, writeEvidenceErr := file.Write(append(evidence, '\n'))
	closeErr := file.Close()
	if writeEvidenceErr != nil || closeErr != nil {
		return 6
	}
	if argv[2] == "finish" {
		return 0
	}
	signals := make(chan os.Signal, 1)
	signalNotify(signals)
	defer signalStop(signals)
	ready := fmt.Sprintf("%s.%d.ready", argv[1], os.Getpid())
	if err := os.WriteFile(ready, []byte("ready\n"), 0o600); err != nil {
		return 7
	}
	<-signals
	return 0
}

func runSupervisorTransferCleanupBlockChild(argv []string) int {
	if len(argv) != 2 || !validPrivateViewAlias(argv[1]) {
		return 2
	}
	id, err := privateViewRunID(argv[1])
	if err != nil {
		return 2
	}
	pid := strconv.Itoa(os.Getpid())
	if err := writeExclusiveTransferGateMarker(filepath.Join(argv[0], "cleanup."+pid+".ready"), id); err != nil {
		return 3
	}
	if err := waitForFileUntil(filepath.Join(argv[0], "cleanup.release"), 10*time.Second); err != nil {
		return 4
	}
	return 0
}

func corruptHostNetworkClaim(runRoot string) (func() error, error) {
	claim := filepath.Join(runRoot, "network", "claim")
	original := claim + ".test-original"
	if err := os.Rename(claim, original); err != nil {
		return nil, err
	}
	if err := os.WriteFile(claim, []byte("not a host-network claim\n"), 0o600); err != nil {
		_ = os.Rename(original, claim)
		return nil, err
	}
	return func() error {
		if err := os.Remove(claim); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return os.Rename(original, claim)
	}, nil
}

func replaceFileViewEvidence(runRoot string) (func() error, error) {
	views := filepath.Join(runRoot, "views")
	original := filepath.Join(runRoot, ".test-original-views")
	if err := os.Rename(views, original); err != nil {
		return nil, err
	}
	if err := os.Mkdir(views, 0o700); err != nil {
		_ = os.Rename(original, views)
		return nil, err
	}
	return func() error {
		if err := os.Remove(views); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return os.Rename(original, views)
	}, nil
}

func waitForTransferLivenessRelease(runRoot string, timeout time.Duration) error {
	lock, err := os.Open(filepath.Join(runRoot, "liveness.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("timed out waiting for transfer liveness release")
}

func recoverExactTransferRun(id runid.ID) (int, error) {
	callbacks := 0
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := rundirectory.New().RecoverStale(ctx, func(stale *rundirectory.StaleRun) error {
		if stale.ID() != id {
			return fmt.Errorf("unexpected stale run %s while recovering %s", stale.ID(), id)
		}
		callbacks++
		if err := hostnetwork.New().Recover(ctx, stale); err != nil {
			return err
		}
		return fileviews.New().Recover(ctx, stale)
	})
	if err != nil {
		return callbacks, err
	}
	return callbacks, nil
}

func waitForFileUntil(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
}

func waitForReadyRunID(pattern string, want int, timeout time.Duration) (runid.ID, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		paths, err := filepath.Glob(pattern)
		if err != nil {
			return runid.ID{}, err
		}
		if len(paths) == want {
			var result runid.ID
			valid := true
			for index, path := range paths {
				marker, readErr := readTransferGateMarker(path)
				if readErr != nil {
					lastErr = fmt.Errorf("read run identity marker %q: %w", path, readErr)
					valid = false
					break
				}
				if index == 0 {
					result = marker.runID
				} else if marker.runID != result {
					lastErr = fmt.Errorf("ready markers identify multiple runs: %s and %s", result, marker.runID)
					valid = false
					break
				}
			}
			if valid {
				return result, nil
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	return runid.ID{}, fmt.Errorf("timed out waiting for %d complete run identity markers %q: %v",
		want, pattern, lastErr)
}

func assertTransferViewEvidence(t *testing.T, markers string, transferCount int) {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(markers, "transfer-views.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(contents), []byte{'\n'})
	if len(lines) != transferCount {
		t.Fatalf("transfer view evidence count = %d, want %d", len(lines), transferCount)
	}
	wantMembers := []string{".hidden", "alpha", "bravo", "charlie", "nested/echo", "nested/foxtrot"}
	seen := make(map[string]int, len(wantMembers))
	for _, encoded := range lines {
		var evidence transferViewEvidence
		if err := json.Unmarshal(encoded, &evidence); err != nil {
			t.Fatal(err)
		}
		if !validPrivateViewAlias(evidence.Argument) {
			t.Fatalf("descriptor-bound view evidence = %#v", evidence)
		}
		for _, descriptor := range evidence.InheritedFDs {
			if strings.HasPrefix(descriptor, "net:[") || strings.HasPrefix(descriptor, "pipe:[") ||
				descriptor == os.Getenv(supervisorTransferEnv) {
				t.Errorf("user command inherited protected descriptor %q", descriptor)
			}
		}
		for _, member := range evidence.Members {
			seen[member]++
		}
	}
	for _, member := range wantMembers {
		if seen[member] != 1 {
			t.Errorf("source member %q appeared %d times", member, seen[member])
		}
	}
	if len(seen) != len(wantMembers) {
		t.Errorf("transfer views contained unexpected members: %#v", seen)
	}
}

func validPrivateViewAlias(path string) bool {
	_, err := privateViewRunID(path)
	return err == nil
}

func privateViewRunID(path string) (runid.ID, error) {
	relative, err := filepath.Rel("/run/transferlanes", path)
	if err != nil || filepath.IsAbs(relative) || filepath.Clean(path) != path {
		return runid.ID{}, errors.New("invalid private view path")
	}
	components := strings.Split(relative, string(filepath.Separator))
	if len(components) != 3 || !strings.HasPrefix(components[0], "view-") ||
		!strings.HasPrefix(components[1], "transfer-") ||
		components[2] != filepath.Base(os.Getenv(supervisorTransferEnv)) {
		return runid.ID{}, errors.New("invalid private view alias")
	}
	return runid.Parse(strings.TrimPrefix(components[0], "view-"))
}
