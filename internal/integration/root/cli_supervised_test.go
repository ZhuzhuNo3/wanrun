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
	"io/fs"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/terminal"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"github.com/creack/pty"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestPublicSupervisedRunExecutesExactArgvAndCleansOwners(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	linkUpdates := subscribeToHostVethUpdates(t, fixture.prefix+"s")
	scenario := fixture.newRunSupervisorScenario(t, "public-cli")
	source := os.Getenv(supervisorTransferEnv)
	if err := os.Symlink("alpha", filepath.Join(source, "ignored-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "ignored-empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(source, "ignored-fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(scenario.markers, "rsync-destination")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}

	arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv)}
	for _, source := range fixture.sources {
		arguments = append(arguments, "--network", source.String())
	}
	arguments = append(arguments, "--", "/usr/bin/rsync", "-a", "{}", destination+"/")
	output, err := runPublicTransferLanesPTY(t, arguments, nil)
	linkNames := finishHostVethObservation(t, linkUpdates)
	if err != nil {
		t.Fatalf("supervised CLI failed: %v\n%s", err, output)
	}
	if len(linkNames) == 0 {
		t.Fatal("supervised CLI emitted no observable host veth creation")
	}
	assertInteractiveSuccessSnapshotTitles(t, output, fixture.sources)
	assertPublicRsyncTree(t, destination, os.Getenv(supervisorTransferEnv), "ignored-empty")
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("public CLI left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicInteractiveChildStartsAtRenderedTargetViewport(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "public-initial-pty-size")
	evidencePath := filepath.Join(scenario.markers, "initial-pty.json")
	arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv),
		"--network", fixture.sources[0].String(), "--"}
	arguments = append(arguments, plainChildCommandLine(os.Args[0], plainChildPTY,
		evidencePath, "argument", "{}")...)
	output, err := runPublicTransferLanesPTY(t, arguments,
		func(_ context.Context, _ *exec.Cmd, master *os.File, capture *boundedPTYCapture) error {
			if err := waitForCapturedText(capture, "child-pty-ready", 10*time.Second); err != nil {
				return err
			}
			if err := writePublicPTY(master, []byte{'1'}); err != nil {
				return err
			}
			if err := waitForCapturedText(capture, "transferlanes | FOCUSED", 5*time.Second); err != nil {
				return err
			}
			return writePublicPTY(master, []byte("exact-input\n"))
		})
	if err != nil {
		t.Fatalf("interactive child failed: %v\n%s", err, output)
	}
	var evidence rootPTYEvidence
	readRunSupervisorJSON(t, evidencePath, &evidence)
	number, _ := transfernumber.New(1)
	selected, err := terminal.NewTransfer(number, fixture.sources[0])
	if err != nil {
		t.Fatal(err)
	}
	display, err := terminal.NewInteractiveTerminal([]terminal.Transfer{selected}, 100, 30,
		terminal.MouseScroll)
	if err != nil {
		t.Fatal(err)
	}
	wantCols, wantRows := display.TargetSize()
	if err := display.Close(); err != nil {
		t.Fatal(err)
	}
	if wantCols != 100 || wantRows >= 30 ||
		int(evidence.InitialCols) != wantCols || int(evidence.InitialRows) != wantRows ||
		evidence.InitialCols != evidence.Cols || evidence.InitialRows != evidence.Rows {
		t.Fatalf("child PTY initial=%dx%d final=%dx%d want target=%dx%d (outer 100x30)",
			evidence.InitialCols, evidence.InitialRows, evidence.Cols, evidence.Rows, wantCols, wantRows)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("public initial-size run left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicInteractiveRunLeavesFinalActiveViewports(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	fixture.newRunSupervisorScenario(t, "public-final-viewports")
	arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv)}
	for _, source := range fixture.sources {
		arguments = append(arguments, "--network", source.String())
	}
	script := strings.Join([]string{
		"printf 'normal-only\\r\\n'",
		"index=0",
		"while [ \"$index\" -lt 40 ]; do printf 'history-%02d\\r\\n' \"$index\"; index=$((index + 1)); done",
		"sleep 1",
		"printf '\\033[?1049h\\033[Halt-final\\r\\nprogress 10%%\\rprogress 100%%\\r\\n\\033[32mgreen\\033[0m\\r\\n\\r\\n'",
	}, "; ")
	arguments = append(arguments, "--", "/bin/sh", "-c", script, "snapshot-command", "{}")
	output, err := runPublicTransferLanesPTY(t, arguments,
		func(_ context.Context, command *exec.Cmd, master *os.File, capture *boundedPTYCapture) error {
			if err := waitForCapturedText(capture, "history-39", 10*time.Second); err != nil {
				return err
			}
			if err := writePublicPTY(master, []byte{'2'}); err != nil {
				return err
			}
			if err := waitForCapturedText(capture, "transferlanes | FOCUSED", 5*time.Second); err != nil {
				return err
			}
			if err := writePublicPTY(master, []byte("\x1b[<64;12;10M")); err != nil {
				return err
			}
			if err := pty.Setsize(master, &pty.Winsize{Cols: 72, Rows: 18}); err != nil {
				return err
			}
			return command.Process.Signal(syscall.SIGWINCH)
		})
	if err != nil {
		t.Fatalf("public final viewport run failed: %v\n%s", err, output)
	}
	assertInteractiveSuccessSnapshotTitles(t, output, fixture.sources)
	tail := finalInteractiveOutput(t, output)
	for _, want := range []string{"progress 100%", "\x1b[32mgreen\x1b[0m"} {
		if count := strings.Count(tail, want); count != len(fixture.sources) {
			t.Fatalf("final viewport content %q count=%d, want %d: %q",
				want, count, len(fixture.sources), tail)
		}
	}
	for _, forbidden := range []string{"normal-only", "history-39", "progress 10%", "transferlanes |", "transferlanes:"} {
		if strings.Contains(tail, forbidden) {
			t.Fatalf("final viewport retained %q: %q", forbidden, tail)
		}
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("final viewport run left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicNonTTYRunKeepsPipeStreamsAndRawLogsSeparate(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "public-plain")
	logDirectory := filepath.Join(scenario.markers, "logs")
	evidence := filepath.Join(scenario.markers, "plain-evidence")
	stdoutRaw := append([]byte("\x1b[31m10%\x1b[0m\r20%\rcomplete\n"),
		bytes.Repeat([]byte("0123456789"), 1200)...)
	stdoutRaw = append(stdoutRaw, '\n')
	stderrRaw := []byte("\x1b[33mwarning\x1b[0m\n")
	arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv), "--log-dir", logDirectory}
	for _, source := range fixture.sources {
		arguments = append(arguments, "--network", source.String())
	}
	arguments = append(arguments, "--")
	arguments = append(arguments, plainChildCommandLine(os.Args[0], plainChildPlain, evidence, "0",
		base64.StdEncoding.EncodeToString(stdoutRaw), base64.StdEncoding.EncodeToString(stderrRaw), "{}")...)
	stdout, stderr, err := runPublicTransferLanesPipes(t, arguments)
	if err != nil {
		t.Fatalf("plain run failed: %v\nstdout=%q\nstderr=%q", err, stdout, stderr)
	}
	if strings.Contains(stdout, "\x1b") || strings.Contains(stderr, "\x1b") {
		t.Fatalf("plain output leaked terminal controls: stdout=%q stderr=%q", stdout, stderr)
	}
	for number, source := range fixture.sources {
		prefix := fmt.Sprintf("[%d %s] ", number+1, source)
		if !strings.Contains(stdout, prefix+"complete\n") || !strings.Contains(stderr, prefix+"warning\n") {
			t.Fatalf("plain stream prefix %q missing: stdout=%q stderr=%q", prefix, stdout, stderr)
		}
		for _, rawLog := range []struct {
			name string
			want []byte
		}{
			{name: fmt.Sprintf("transfer-%02d.stdout.log", number+1), want: stdoutRaw},
			{name: fmt.Sprintf("transfer-%02d.stderr.log", number+1), want: stderrRaw},
		} {
			got, readErr := os.ReadFile(filepath.Join(logDirectory, rawLog.name))
			if readErr != nil || !bytes.Equal(got, rawLog.want) {
				t.Fatalf("raw log %s bytes=%d want=%d error=%v", rawLog.name, len(got), len(rawLog.want), readErr)
			}
		}
	}
	evidencePaths, err := filepath.Glob(evidence + ".*")
	if err != nil || len(evidencePaths) != len(fixture.sources) {
		t.Fatalf("plain child evidence paths=%v error=%v", evidencePaths, err)
	}
	for _, path := range evidencePaths {
		var observed rootPlainEvidence
		readRunSupervisorJSON(t, path, &observed)
		if !observed.StdinEOF || observed.StdoutTTY || observed.StderrTTY {
			t.Errorf("plain child unexpectedly received terminal descriptors: %#v", observed)
		}
	}
	if !strings.Contains(stdout, "transferlanes: completed: succeeded=") ||
		!strings.Contains(stdout, "transferlanes: cleanup completed\n") ||
		strings.Contains(stderr, "transferlanes: completed") {
		t.Fatalf("plain summary ownership is unstable: stdout=%q stderr=%q", stdout, stderr)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("plain CLI left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicPlainRunRepeatedSIGINTDrainsFinalAndPreservesRawTail(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "public-plain-cancel")
	marker := filepath.Base(scenario.markers)
	t.Setenv("TRANSFERLANES_SIGNAL_CHILD", marker)
	readyPrefix := filepath.Join(scenario.markers, "signal")
	logDirectory := filepath.Join(scenario.markers, "logs")
	arguments := []string{"run", "--no-tui", "--source", os.Getenv(supervisorTransferEnv),
		"--log-dir", logDirectory}
	for _, source := range fixture.sources {
		arguments = append(arguments, "--network", source.String())
	}
	arguments = append(arguments, "--")
	arguments = append(arguments, transferChildCommand(os.Args[0], transferChildBlock,
		readyPrefix, "{}")...)
	stdout, stderr, runErr := runPublicTransferLanesPipesWithAction(t, arguments, func(command *exec.Cmd) error {
		if err := waitForGlobCount(readyPrefix+".*.ready", len(fixture.sources), 10*time.Second); err != nil {
			return err
		}
		if err := command.Process.Signal(syscall.SIGINT); err != nil {
			return err
		}
		return command.Process.Signal(syscall.SIGINT)
	})
	var exit *exec.ExitError
	if !errors.As(runErr, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("plain cancellation=%v stdout=%q stderr=%q", runErr, stdout, stderr)
	}
	for number := 1; number <= len(fixture.sources); number++ {
		content, err := os.ReadFile(filepath.Join(logDirectory,
			fmt.Sprintf("transfer-%02d.stdout.log", number)))
		if err != nil || !bytes.Equal(content, []byte("transferlanes-signal-ready\n")) {
			t.Fatalf("plain cancelled log %d=%q error=%v", number, content, err)
		}
		stderrLog, err := os.ReadFile(filepath.Join(logDirectory,
			fmt.Sprintf("transfer-%02d.stderr.log", number)))
		if err != nil || len(stderrLog) != 0 {
			t.Fatalf("plain cancelled stderr log %d=%q error=%v", number, stderrLog, err)
		}
	}
	if !strings.Contains(stdout, "transferlanes: completed:") || !strings.Contains(stdout, "transferlanes: cleanup completed") {
		t.Fatalf("plain cancellation omitted stable final summary: %q", stdout)
	}
	assertNoMarkedProcesses(t, "TRANSFERLANES_SIGNAL_CHILD="+marker)
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("plain cancellation left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicRunRejectsActiveLegacyForwardWhenGenericIPTablesCommandsAreHidden(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	fixture.run("iptables-legacy", "-w", "5", "-t", "filter", "-P", "FORWARD", "DROP")
	t.Cleanup(func() {
		fixture.runCleanup("iptables-legacy", "-w", "5", "-t", "filter", "-P", "FORWARD", "ACCEPT")
	})
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "payload"), []byte("payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "child-started")
	commands := filepath.Join(t.TempDir(), "commands")
	if err := os.Mkdir(commands, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"iptables-legacy", "iptables-legacy-save"} {
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(commands, name)); err != nil {
			t.Fatal(err)
		}
	}
	arguments := []string{"run", "--source", source, "--network", fixture.sources[0].String(), "--",
		"/bin/sh", "-c", "touch \"$1\"", "{}", marker}
	output, err := runPublicTransferLanesPTYWithEnvironment(t, arguments, []string{"PATH=" + commands}, nil)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("mixed legacy/native CLI result = %v, output=%q", err, output)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight-rejected CLI started child command: %v", err)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("preflight-rejected CLI left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicRunRejectsActiveLegacyPostroutingWithIPTablesNFT(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	useIPTablesNFTForwardOnly(fixture)
	legacyBefore := installLegacyPostroutingJump(fixture)
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "payload"), []byte("payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "child-started")
	arguments := []string{"run", "--source", source, "--network", fixture.sources[0].String(), "--",
		"/bin/sh", "-c", "touch \"$1\"", "{}", marker}

	output, err := runPublicTransferLanesPTY(t, arguments, nil)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("mixed legacy/nft NAT CLI result = %v, output=%q", err, output)
	}
	if !strings.Contains(output,
		"active nft and legacy iptables POSTROUTING surfaces cannot be modified safely together") {
		t.Fatalf("mixed legacy/nft NAT CLI did not reach firewall rejection: %q", output)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight-rejected CLI started child command: %v", err)
	}
	if after := fixture.run("iptables-legacy", "-w", "5", "-t", "nat", "-S"); after != legacyBefore {
		t.Fatalf("legacy NAT surface changed\nbefore:\n%s\nafter:\n%s", legacyBefore, after)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("preflight-rejected CLI left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicNativeNFTRunDoesNotRequireLegacyNATInventory(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	legacyBefore := installLegacyPostroutingJump(fixture)
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "native-nft-with-legacy-nat")
	endpoint := "http://" + net.JoinHostPort(fixture.remote.String(), "18081") + "/source"
	logDirectory := filepath.Join(scenario.markers, "logs")
	arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv),
		"--log-dir", logDirectory, "--network", fixture.sources[0].String(), "--"}
	arguments = append(arguments, measurementChildCommand(os.Args[0],
		endpoint, "{}")...)

	output, err := runPublicTransferLanesPTYWithEnvironment(t, arguments,
		[]string{"PATH=" + legacyNATUnreadableCommandPath(t)}, nil)
	if err != nil {
		t.Fatalf("native nft CLI failed with unreadable legacy NAT: %v\n%s", err, output)
	}
	transferLog, readErr := os.ReadFile(filepath.Join(logDirectory, "transfer-01.ptylog"))
	if readErr != nil || !strings.Contains(string(transferLog), "observed-source="+fixture.sources[0].String()) {
		t.Fatalf("native nft CLI did not preserve selected source: log=%q error=%v output=%q",
			transferLog, readErr, output)
	}
	if after := fixture.run("iptables-legacy", "-w", "5", "-t", "nat", "-S"); after != legacyBefore {
		t.Fatalf("legacy NAT surface changed\nbefore:\n%s\nafter:\n%s", legacyBefore, after)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("native nft CLI left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicRunRejectsExhaustedNFTNATPriorityBeforeHostMutation(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	table := fixture.prefix + "e"
	fixture.run("nft", "add", "table", "inet", table)
	fixture.run("nft", "add", "chain", "inet", table, "postrouting",
		"{ type nat hook postrouting priority -199; policy accept; }")
	fixture.run("nft", "add", "rule", "inet", table, "postrouting",
		"ip", "saddr", "198.18.0.0/15", "snat", "to", fixture.sources[1].String())
	t.Cleanup(func() { fixture.runCleanup("nft", "delete", "table", "inet", table) })
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	updates := subscribeToHostVethUpdates(t, fixture.prefix+"s")
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "payload"), []byte("payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "child-started")
	arguments := []string{"run", "--source", source, "--network", fixture.sources[0].String(), "--",
		"/bin/sh", "-c", "touch \"$1\"", "{}", marker}

	output, err := runPublicTransferLanesPTY(t, arguments, nil)
	linkNames := finishHostVethObservation(t, updates)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("exhausted NAT priority CLI result = %v, output=%q", err, output)
	}
	if !strings.Contains(output, "no earlier kernel-supported NAT priority") {
		t.Fatalf("exhausted NAT priority CLI did not reach firewall rejection: %q", output)
	}
	if len(linkNames) != 0 {
		t.Fatalf("priority preflight created transferlanes links: %v", linkNames)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("priority preflight started child command: %v", err)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("priority preflight left run roots: before=%v after=%v", runsBefore, after)
	}
}

type linkUpdateObservation struct {
	done         chan struct{}
	stopOnce     *sync.Once
	sentinelName string
	crossing     <-chan linkObservationResult
	stopped      <-chan struct{}
}

func (observation linkUpdateObservation) stop() {
	observation.stopOnce.Do(func() { close(observation.done) })
}

type linkObservationResult struct {
	names []string
	err   error
}

type linkObservationErrors struct {
	mutex sync.Mutex
	first error
}

func (observed *linkObservationErrors) report(err error) {
	observed.mutex.Lock()
	defer observed.mutex.Unlock()
	if observed.first == nil {
		observed.first = err
	}
}

func (observed *linkObservationErrors) firstReported() error {
	observed.mutex.Lock()
	defer observed.mutex.Unlock()
	return observed.first
}

func subscribeToHostVethUpdates(t *testing.T, sentinelName string) linkUpdateObservation {
	t.Helper()
	updates := make(chan netlink.LinkUpdate, 128)
	done := make(chan struct{})
	reported := &linkObservationErrors{}
	options := netlink.LinkSubscribeOptions{ErrorCallback: reported.report,
		ReceiveBufferSize: 1024 * 1024}
	if err := netlink.LinkSubscribeWithOptions(updates, done, options); err != nil {
		close(done)
		t.Fatalf("subscribe to host link updates: %v", err)
	}
	baseline, err := currentLinkIndexes()
	if err != nil {
		close(done)
		if closeErr := waitForLinkUpdatesClose(updates); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
		t.Fatalf("capture host link baseline: %v", err)
	}
	crossing := make(chan linkObservationResult, 1)
	stopped := make(chan struct{})
	go observeNewHostVeths(updates, sentinelName, baseline, reported, crossing, stopped)
	observation := linkUpdateObservation{done: done, stopOnce: &sync.Once{}, sentinelName: sentinelName,
		crossing: crossing, stopped: stopped}
	t.Cleanup(func() {
		observation.stop()
		if err := waitForLinkObservationStop(observation); err != nil {
			t.Errorf("stop host link observation: %v", err)
		}
	})
	return observation
}

func waitForLinkUpdatesClose(updates <-chan netlink.LinkUpdate) error {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case _, open := <-updates:
			if !open {
				return nil
			}
		case <-timer.C:
			return errors.New("timed out stopping host link subscription")
		}
	}
}

func currentLinkIndexes() (map[int]struct{}, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, err
	}
	indexes := make(map[int]struct{}, len(links))
	for _, link := range links {
		if link != nil && link.Attrs() != nil {
			indexes[link.Attrs().Index] = struct{}{}
		}
	}
	return indexes, nil
}

func observeNewHostVeths(updates <-chan netlink.LinkUpdate, sentinelName string,
	baseline map[int]struct{}, reported *linkObservationErrors,
	crossing chan<- linkObservationResult, stopped chan<- struct{},
) {
	defer close(stopped)
	var names []string
	seen := make(map[int]struct{})
	crossed := false
	for update := range updates {
		if crossed || update.Header.Type != unix.RTM_NEWLINK || update.Link == nil ||
			update.Link.Attrs() == nil {
			continue
		}
		attributes := update.Link.Attrs()
		if attributes.Name == sentinelName {
			crossing <- linkObservationResult{names: names, err: reported.firstReported()}
			crossed = true
			continue
		}
		if update.Link.Type() != "veth" {
			continue
		}
		if _, existed := baseline[attributes.Index]; existed {
			continue
		}
		if _, duplicate := seen[attributes.Index]; duplicate {
			continue
		}
		seen[attributes.Index] = struct{}{}
		names = append(names, attributes.Name)
	}
	if !crossed {
		crossing <- linkObservationResult{err: errors.Join(reported.firstReported(),
			errors.New("link subscription closed before sentinel event"))}
	}
}

func finishHostVethObservation(t *testing.T, observation linkUpdateObservation) []string {
	t.Helper()
	attributes := netlink.NewLinkAttrs()
	attributes.Name = observation.sentinelName
	sentinel := &netlink.Dummy{LinkAttrs: attributes}
	if err := netlink.LinkAdd(sentinel); err != nil {
		observation.stop()
		waitForLinkObservationStop(observation)
		t.Fatalf("create link observation sentinel: %v", err)
	}
	t.Cleanup(func() { removeExactLinkIfPresent(sentinel) })
	result, crossingErr := waitForLinkObservationCrossing(observation)
	removeErr := netlink.LinkDel(sentinel)
	observation.stop()
	stopErr := waitForLinkObservationStop(observation)
	if err := errors.Join(crossingErr, result.err, removeErr, stopErr); err != nil {
		t.Fatalf("finish host link observation: %v", err)
	}
	return result.names
}

func waitForLinkObservationCrossing(observation linkUpdateObservation) (linkObservationResult, error) {
	select {
	case result := <-observation.crossing:
		return result, nil
	case <-time.After(10 * time.Second):
		return linkObservationResult{}, errors.New("timed out waiting for link observation sentinel")
	}
}

func waitForLinkObservationStop(observation linkUpdateObservation) error {
	select {
	case <-observation.stopped:
		return nil
	case <-time.After(10 * time.Second):
		return errors.New("timed out waiting for link subscription to stop")
	}
}

func removeExactLinkIfPresent(expected netlink.Link) {
	link, err := netlink.LinkByName(expected.Attrs().Name)
	if err == nil && link.Attrs().Index == expected.Attrs().Index {
		_ = netlink.LinkDel(link)
	}
}

func legacyNATUnreadableCommandPath(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	for _, name := range []string{"iptables", "iptables-save", "iptables-nft", "iptables-nft-save"} {
		target, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(directory, name)); err != nil {
			t.Fatal(err)
		}
	}
	legacyRules, err := exec.LookPath("iptables-legacy")
	if err != nil {
		t.Fatal(err)
	}
	legacySave, err := exec.LookPath("iptables-legacy-save")
	if err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(directory, "iptables-legacy"), fmt.Sprintf(
		"#!/bin/sh\nif [ \"$#\" -eq 1 ] && [ \"$1\" = --version ]; then exec %s \"$@\"; fi\nexit 127\n",
		strconv.Quote(legacyRules)))
	writeExecutable(t, filepath.Join(directory, "iptables-legacy-save"), fmt.Sprintf(
		"#!/bin/sh\nif [ \"$#\" -eq 1 ] && [ \"$1\" = --version ]; then exec %s \"$@\"; fi\nif [ \"$#\" -eq 2 ] && [ \"$1\" = -t ] && [ \"$2\" = filter ]; then exec %s \"$@\"; fi\nexit 127\n",
		strconv.Quote(legacySave), strconv.Quote(legacySave)))
	return directory
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
}

func useIPTablesNFTForwardOnly(fixture *hostNetworkFixture) {
	fixture.t.Helper()
	fixture.run("nft", "delete", "table", "inet", fixture.filterTable)
	fixture.run("nft", "delete", "table", "inet", fixture.natTable)
	introduced := !rootNFTTableExists(fixture.t, "filter")
	fixture.run("iptables-nft", "-w", "5", "-t", "filter", "-P", "FORWARD", "DROP")
	fixture.t.Cleanup(func() {
		fixture.runCleanup("iptables-nft", "-w", "5", "-t", "filter", "-P", "FORWARD", "ACCEPT")
		if introduced {
			removeIntroducedIPTablesNFTFilter(fixture.t)
		}
	})
}

func rootNFTTableExists(t *testing.T, table string) bool {
	t.Helper()
	err := exec.Command("nft", "list", "table", "ip", table).Run()
	if err == nil {
		return true
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false
	}
	t.Fatalf("inspect iptables-nft compatibility table %s: %v", table, err)
	return false
}

func removeIntroducedIPTablesNFTFilter(t *testing.T) {
	t.Helper()
	document, err := exec.Command("nft", "-j", "list", "table", "ip", "filter").CombinedOutput()
	if err != nil || !emptyIPTablesNFTFilter(document) {
		t.Errorf("preserve changed fixture-created iptables-nft filter table: %v\n%s", err, document)
		return
	}
	if output, err := exec.Command("nft", "delete", "table", "ip", "filter").CombinedOutput(); err != nil {
		t.Errorf("delete fixture-created iptables-nft filter table: %v\n%s", err, output)
		return
	}
	if err := exec.Command("nft", "list", "table", "ip", "filter").Run(); err == nil {
		t.Error("fixture-created iptables-nft filter table remains")
	}
}

func emptyIPTablesNFTFilter(document []byte) bool {
	var listing struct {
		Objects []json.RawMessage `json:"nftables"`
	}
	if json.Unmarshal(document, &listing) != nil {
		return false
	}
	tables, chains := 0, 0
	for _, raw := range listing.Objects {
		kind, valid := iptablesNFTFilterObject(raw)
		if !valid {
			return false
		}
		if kind == "table" {
			tables++
		}
		if kind == "chain" {
			chains++
		}
	}
	return tables == 1 && chains == 1
}

func iptablesNFTFilterObject(document []byte) (string, bool) {
	var object map[string]json.RawMessage
	if json.Unmarshal(document, &object) != nil {
		return "", false
	}
	if _, metadata := object["metainfo"]; metadata && len(object) == 1 {
		return "metadata", true
	}
	if value, found := object["table"]; found && len(object) == 1 {
		var identity struct {
			Family string `json:"family"`
			Name   string `json:"name"`
		}
		valid := rootJSONFields(value, "family", "name", "handle") &&
			json.Unmarshal(value, &identity) == nil && identity.Family == "ip" && identity.Name == "filter"
		return "table", valid
	}
	if value, found := object["chain"]; found && len(object) == 1 {
		return "chain", iptablesNFTForwardChain(value)
	}
	return "", false
}

func iptablesNFTForwardChain(document []byte) bool {
	if !rootJSONFields(document, "family", "table", "name", "handle", "type", "hook", "prio", "policy") {
		return false
	}
	var chain struct {
		Family string `json:"family"`
		Table  string `json:"table"`
		Name   string `json:"name"`
		Type   string `json:"type"`
		Hook   string `json:"hook"`
		Policy string `json:"policy"`
		Prio   int    `json:"prio"`
	}
	return json.Unmarshal(document, &chain) == nil && chain.Family == "ip" && chain.Table == "filter" &&
		chain.Name == "FORWARD" && chain.Type == "filter" && chain.Hook == "forward" &&
		chain.Prio == 0 && chain.Policy == "accept"
}

func rootJSONFields(document []byte, allowed ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(document, &fields) != nil {
		return false
	}
	for name := range fields {
		if !slices.Contains(allowed, name) {
			return false
		}
	}
	return true
}

func installLegacyPostroutingJump(fixture *hostNetworkFixture) string {
	fixture.t.Helper()
	chain := strings.ToUpper(fixture.prefix + "ln")
	fixture.run("iptables-legacy", "-w", "5", "-t", "nat", "-N", chain)
	fixture.run("iptables-legacy", "-w", "5", "-t", "nat", "-A", "POSTROUTING", "-j", chain)
	fixture.t.Cleanup(func() {
		fixture.runCleanup("iptables-legacy", "-w", "5", "-t", "nat", "-D", "POSTROUTING", "-j", chain)
		fixture.runCleanup("iptables-legacy", "-w", "5", "-t", "nat", "-F", chain)
		fixture.runCleanup("iptables-legacy", "-w", "5", "-t", "nat", "-X", chain)
	})
	return fixture.run("iptables-legacy", "-w", "5", "-t", "nat", "-S")
}

func TestPublicConcurrentRunsUseTheSameSelectedNetworks(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	concurrent := newConcurrentPublicRunFixture(t, fixture)
	runs, readyPIDs := concurrent.startReadyRuns(t, []int{0, 1, 2})
	overlappingClaims := readExactNetworkClaims(t, runs)
	t.Logf("observed simultaneous public run network objects: %s",
		describeActiveNetworkObjects(overlappingClaims))

	if err := runs[0].release(); err != nil {
		t.Fatalf("release first concurrent run: %v", err)
	}
	assertConcurrentRunCompleted(t, runs[0], readyPIDs[0], len(fixture.sources))
	concurrent.assertActiveRuns(t, runs[1:])
	concurrent.requestTransferCheck(t, runs[1], 1, readyPIDs[1])
	concurrent.requestTransferCheck(t, runs[2], 1, readyPIDs[2])
	concurrent.assertActiveRuns(t, runs[1:])
	t.Log("two surviving runs retained readable FUSE views and usable network namespaces after the first cleanup")

	if err := runs[1].command.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("cancel second concurrent run: %v", err)
	}
	secondResult := runs[1].wait(15 * time.Second)
	var exit *exec.ExitError
	if !errors.As(secondResult.err, &exit) || exit.ExitCode() != 130 ||
		!strings.Contains(secondResult.stdout, "transferlanes: cleanup completed") {
		t.Fatalf("second concurrent run cancellation=%v stdout=%q stderr=%q",
			secondResult.err, secondResult.stdout, secondResult.stderr)
	}
	concurrent.assertActiveRuns(t, runs[2:])
	concurrent.requestTransferCheck(t, runs[2], 2, readyPIDs[2])
	concurrent.assertActiveRuns(t, runs[2:])
	t.Log("the final run retained its readable FUSE view and usable network namespaces after sibling cancellation")

	if err := runs[2].release(); err != nil {
		t.Fatalf("release final concurrent run: %v", err)
	}
	assertConcurrentRunCompleted(t, runs[2], readyPIDs[2], len(fixture.sources))
	for _, run := range runs {
		if run.wasForceStopped() {
			t.Fatalf("staged concurrent run %s required a forced stop", run.runID)
		}
		if _, err := os.Stat(filepath.Join(rundirectory.AuthorityRoot, run.runID.String())); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("completed concurrent run %s remains: %v", run.runID, err)
		}
		if err := waitForExactProcessExit(run.supervisorPID, 5*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if err := waitForScenarioProcessesGone(concurrent.markerRoot, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if after := authorityEntries(t); !slices.Equal(after, concurrent.baselineRuns) {
		t.Fatalf("concurrent runs left authority entries: before=%v after=%v", concurrent.baselineRuns, after)
	}
	fixture.assertSurface(concurrent.baselineSurface)
	if err := checkTemporaryConntrackAbsent(); err != nil {
		t.Fatal(err)
	}
}

func TestPublicOverlapCleanupReleasesTransferHeldBeforeReady(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	concurrent := newConcurrentPublicRunFixture(t, fixture)
	run := concurrent.start(t, "early-cleanup", "transfer-003", 0)
	markers := run.markers
	if err := waitForGlobCount(filepath.Join(markers, "transfer-*.ready"),
		len(fixture.sources)-1, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := waitForGlobCount(filepath.Join(markers, "transfer-*.waiting"), 1, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	id, _, err := waitForHeldTransferGateMarkers(markers, len(fixture.sources), 3, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.captureExactIdentity(id); err != nil {
		t.Fatal(err)
	}
	claim := readExactNetworkClaims(t, []*activePublicRun{run})[0]
	assertActiveRunNetworkObjects(t, fixture, claim)
	if err := run.release(); err != nil {
		t.Fatal(err)
	}
	result := run.wait(15 * time.Second)
	if result.err != nil {
		t.Fatalf("early cleanup public run failed: %v stdout=%q stderr=%q",
			result.err, result.stdout, result.stderr)
	}
	assertSuccessfulPlainRunOutput(t, result, len(fixture.sources))
	doneID, _, err := waitForTransferGateMarkers(markers, "done", len(fixture.sources), 5*time.Second)
	if err != nil || doneID != id {
		t.Fatalf("early cleanup done identity = %s, want %s: %v", doneID, id, err)
	}
	if run.wasForceStopped() {
		t.Fatal("early cleanup required a forced stop")
	}
	if _, err := os.Stat(filepath.Join(rundirectory.AuthorityRoot, id.String())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("early cleanup run %s remains: %v", id, err)
	}
	if err := waitForExactProcessExit(run.supervisorPID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if after := authorityEntries(t); !slices.Equal(after, concurrent.baselineRuns) {
		t.Fatalf("early cleanup left authority entries: before=%v after=%v", concurrent.baselineRuns, after)
	}
	fixture.assertSurface(concurrent.baselineSurface)
}

func TestPublicOverlapCleanupRecoversKilledRunSupervisor(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	concurrent := newConcurrentPublicRunFixture(t, fixture)
	run := concurrent.start(t, "killed-supervisor", "", 0)
	id, _, err := waitForTransferGateMarkers(run.markers, "ready", len(fixture.sources),
		10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.captureExactIdentity(id); err != nil {
		t.Fatal(err)
	}
	claim := readExactNetworkClaims(t, []*activePublicRun{run})[0]
	assertActiveRunNetworkObjects(t, fixture, claim)

	if err := unix.Kill(run.supervisorPID, unix.SIGKILL); err != nil {
		t.Fatalf("kill public run supervisor %d: %v", run.supervisorPID, err)
	}
	if !run.waitForExit(10 * time.Second) {
		t.Fatal("public run parent did not observe killed supervisor")
	}

	recovered, err := recoverExactPublicRun(id)
	if err != nil {
		t.Fatal(err)
	}
	if !recovered {
		t.Fatalf("killed run %s was not offered for exact recovery", id)
	}
	t.Logf("recovered SIGKILLed public run %s through stale owner cleanup", id)
	if run.result().err == nil {
		t.Fatal("public run with killed supervisor exited successfully")
	}
	if _, err := os.Stat(filepath.Join(rundirectory.AuthorityRoot, id.String())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("killed run %s remains after exact recovery: %v", id, err)
	}
	if err := waitForExactProcessExit(run.supervisorPID, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if after := authorityEntries(t); !slices.Equal(after, concurrent.baselineRuns) {
		t.Fatalf("killed run recovery left authority entries: before=%v after=%v", concurrent.baselineRuns, after)
	}
	fixture.assertSurface(concurrent.baselineSurface)
}

func TestPublicRunSupervisorKeepsSourceCapabilityAcrossPathReplacement(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "public-source-capability")
	source := os.Getenv(supervisorTransferEnv)
	boundParent := filepath.Join(scenario.markers, "bound-source-parent")
	boundSource := filepath.Join(boundParent, filepath.Base(source))
	if err := os.MkdirAll(boundSource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount(source, boundSource, "", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}
	boundMounted := true
	t.Cleanup(func() {
		if boundMounted {
			_ = unix.Unmount(boundSource, unix.MNT_DETACH)
		}
	})
	ancestor := filepath.Join(scenario.markers, "source-ancestor")
	if err := os.Symlink(boundParent, ancestor); err != nil {
		t.Fatal(err)
	}
	sourceArgument := filepath.Join(ancestor, filepath.Base(source))
	destination := filepath.Join(scenario.markers, "rsync-destination")
	logs := filepath.Join(scenario.markers, "logs")
	arguments := []string{"run", "--source", sourceArgument, "--log-dir", logs}
	for _, selected := range fixture.sources {
		arguments = append(arguments, "--network", selected.String())
	}
	arguments = append(arguments, "--")
	arguments = append(arguments, transferChildCommand(os.Args[0], transferChildRsyncWait,
		scenario.markers, destination, "{}", sourceArgument)...)

	parked := source + "-parked"
	output, err := runPublicTransferLanesPTY(t, arguments,
		func(_ context.Context, _ *exec.Cmd, _ *os.File, _ *boundedPTYCapture) error {
			if err := waitForGlobCount(filepath.Join(scenario.markers, "rsync.*.ready"),
				len(fixture.sources), 10*time.Second); err != nil {
				return err
			}
			if err := os.Rename(source, parked); err != nil {
				return err
			}
			if err := os.Mkdir(source, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(source, "evil"), []byte("replacement\n"), 0o600); err != nil {
				return err
			}
			if err := unix.Unmount(boundSource, unix.MNT_DETACH); err != nil {
				return err
			}
			boundMounted = false
			if err := unix.Mount(source, boundSource, "", unix.MS_BIND, ""); err != nil {
				return err
			}
			boundMounted = true
			if err := os.WriteFile(filepath.Join(scenario.markers, "rsync.release"), []byte("go\n"), 0o600); err != nil {
				return err
			}
			if err := waitForGlobCount(filepath.Join(scenario.markers, "rsync.*.copied"),
				len(fixture.sources), 10*time.Second); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(scenario.markers, "rsync.finish"), []byte("done\n"), 0o600)
		})
	if boundMounted {
		if unmountErr := unix.Unmount(boundSource, unix.MNT_DETACH); err == nil && unmountErr != nil {
			err = unmountErr
		}
		boundMounted = false
	}
	restoreErr := os.RemoveAll(source)
	if restoreErr == nil {
		restoreErr = os.Rename(parked, source)
	} else {
		_ = os.Rename(parked, source)
	}
	if err != nil || restoreErr != nil {
		t.Fatalf("descriptor-bound public run error=%v restore=%v output=%q", err, restoreErr, output)
	}
	assertRsyncViewCopies(t, scenario.markers, len(fixture.sources))
	if entries, err := os.ReadDir(logs); err != nil || len(entries) != len(fixture.sources) {
		t.Fatalf("transfer logs=%v error=%v", entries, err)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("source capability run left roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicRunRejectsLogsInsideSourceBeforeRunSupervisorMutation(t *testing.T) {
	for _, test := range []struct {
		name string
		path func(source, alias, bindAlias string) string
	}{
		{name: "equal", path: func(source, _, _ string) string { return source }},
		{name: "direct descendant", path: func(source, _, _ string) string {
			return filepath.Join(source, "nested")
		}},
		{name: "prospective descendant", path: func(source, _, _ string) string {
			return filepath.Join(source, "new-logs")
		}},
		{name: "multi-component tail", path: func(source, _, _ string) string {
			return filepath.Join(source, "missing", "logs")
		}},
		{name: "symlink alias", path: func(_, alias, _ string) string { return alias }},
		{name: "symlink nonexistent tail", path: func(_, alias, _ string) string {
			return filepath.Join(alias, "new-logs")
		}},
		{name: "bind alias", path: func(_, _, bindAlias string) string { return bindAlias }},
		{name: "bind nonexistent tail", path: func(_, _, bindAlias string) string {
			return filepath.Join(bindAlias, "new-logs")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHostNetworkFixture(t)
			fixture.install()
			baseline := fixture.hostSurface()
			runsBefore := authorityEntries(t)
			scenario := fixture.newRunSupervisorScenario(t,
				"public-log-source-boundary-"+strings.ReplaceAll(test.name, " ", "-"))
			source := os.Getenv(supervisorTransferEnv)
			alias := filepath.Join(scenario.markers, "source-alias")
			if err := os.Symlink(source, alias); err != nil {
				t.Fatal(err)
			}
			bindAlias := filepath.Join(scenario.markers, "source-bind")
			if err := os.Mkdir(bindAlias, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := unix.Mount(source, bindAlias, "", unix.MS_BIND, ""); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = unix.Unmount(bindAlias, unix.MNT_DETACH) })
			logDirectory := test.path(source, alias, bindAlias)
			evidence := filepath.Join(scenario.markers,
				"unexpected-"+strings.ReplaceAll(test.name, " ", "-"))
			arguments := []string{"run", "--source", source, "--log-dir", logDirectory,
				"--network", fixture.sources[0].String(), "--"}
			arguments = append(arguments, transferChildCommand(os.Args[0], transferChildView,
				evidence, "{}")...)
			output, runErr := runPublicTransferLanesPTY(t, arguments, nil)
			var exit *exec.ExitError
			if !errors.As(runErr, &exit) || exit.ExitCode() != 1 ||
				!strings.Contains(output, "log directory") {
				t.Fatalf("source-related log run error=%v output=%q", runErr, output)
			}
			if _, err := os.Stat(evidence); !os.IsNotExist(err) {
				t.Fatalf("user command ran before log placement rejection: %v", err)
			}
			if _, err := os.Stat(filepath.Join(logDirectory, "transfer-01.ptylog")); !os.IsNotExist(err) {
				t.Fatalf("source-related log was created: %v", err)
			}
			fixture.assertSurface(baseline)
			if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
				t.Fatalf("source-related log validation touched run authority: before=%v after=%v",
					runsBefore, after)
			}
		})
	}
}

func TestPublicRunRejectsFinalSourceSymlinkBeforeRunSupervisorMutation(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "public-source-final-symlink")
	alias := filepath.Join(scenario.markers, "source-link")
	if err := os.Symlink(os.Getenv(supervisorTransferEnv), alias); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(scenario.markers, "unexpected-command")
	arguments := []string{"run", "--source", alias, "--network", fixture.sources[0].String(), "--"}
	arguments = append(arguments, transferChildCommand(os.Args[0], transferChildView,
		evidence, "{}")...)
	output, runErr := runPublicTransferLanesPTY(t, arguments, nil)
	var exit *exec.ExitError
	if !errors.As(runErr, &exit) || exit.ExitCode() != 1 ||
		!strings.Contains(output, "actual non-symlink directory") {
		t.Fatalf("final source symlink error=%v output=%q", runErr, output)
	}
	if _, err := os.Stat(evidence); !os.IsNotExist(err) {
		t.Fatalf("command ran after source rejection: %v", err)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("source rejection touched run authority: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicSupervisedLogsContainEveryFinalPTYByte(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "public-cli-tail")
	logDirectory := filepath.Join(scenario.markers, "logs")
	arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv), "--log-dir", logDirectory}
	for _, source := range fixture.sources {
		arguments = append(arguments, "--network", source.String())
	}
	arguments = append(arguments, "--")
	arguments = append(arguments, transferChildCommand(os.Args[0], transferChildTail, "{}")...)
	output, err := runPublicTransferLanesPTY(t, arguments, nil)
	if err != nil {
		t.Fatalf("supervised CLI tail output failed: %v\n%s", err, output)
	}
	want := bytes.Repeat([]byte("tail-0123456789"), 16*1024)
	for number := 1; number <= len(fixture.sources); number++ {
		content, readErr := os.ReadFile(filepath.Join(logDirectory,
			fmt.Sprintf("transfer-%02d.ptylog", number)))
		if readErr != nil || !bytes.Equal(content, want) {
			t.Errorf("transfer %d final PTY log bytes=%d want=%d error=%v", number,
				len(content), len(want), readErr)
		}
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("tail-output public CLI left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicSupervisedSlowInputPreservesPasteAndOSInterrupt(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		name := "complete"
		if cancelRun {
			name = "interrupt"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newHostNetworkFixture(t)
			fixture.install()
			baseline := fixture.hostSurface()
			runsBefore := authorityEntries(t)
			scenario := fixture.newRunSupervisorScenario(t, "public-cli-slow-input-"+name)
			gate := filepath.Join(scenario.markers, "input.gate")
			if err := unix.Mkfifo(gate, 0o600); err != nil {
				t.Fatal(err)
			}
			evidence := filepath.Join(scenario.markers, "input.received")
			payload := bytes.Repeat([]byte("\xe7\x95\x8c\x1b[31m0123456789"), 2048)
			arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv),
				"--network", fixture.sources[0].String(), "--"}
			arguments = append(arguments, transferChildCommand(os.Args[0], transferChildSlowInput,
				gate, evidence, strconv.Itoa(len(payload)), "{}")...)
			output, runErr := runPublicTransferLanesPTY(t, arguments,
				func(ctx context.Context, command *exec.Cmd, master *os.File,
					capture *boundedPTYCapture,
				) error {
					openCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
					defer cancel()
					release, err := openFIFOWriter(openCtx, gate)
					if err != nil {
						return err
					}
					defer release.Close()
					if err := writePublicPTY(master, append([]byte("1"), payload[:3072]...)); err != nil {
						return err
					}
					if cancelRun {
						return command.Process.Signal(syscall.SIGINT)
					}
					if _, err := release.Write([]byte{1}); err != nil {
						return err
					}
					return writePublicPTY(master, payload[3072:])
				})
			if cancelRun {
				var exit *exec.ExitError
				if !errors.As(runErr, &exit) || exit.ExitCode() != 130 || strings.Contains(output, "slow-input-complete") {
					t.Fatalf("slow-input interrupt error=%v output=%q", runErr, output)
				}
				if _, err := os.Stat(evidence); !os.IsNotExist(err) {
					t.Fatalf("interrupted slow input produced evidence: %v", err)
				}
			} else {
				content, err := os.ReadFile(evidence)
				if runErr != nil || err != nil || !bytes.Equal(content, payload) ||
					!strings.Contains(output, "slow-input-complete") {
					t.Fatalf("slow-input completion error=%v read=%v bytes=%d output=%q",
						runErr, err, len(content), output)
				}
			}
			fixture.assertSurface(baseline)
			if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
				t.Fatalf("slow-input %s left run roots: before=%v after=%v", name, runsBefore, after)
			}
		})
	}
}

func TestFIFOWriterStopsWhenTargetNeverOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unopened.fifo")
	if err := unix.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	writer, err := openFIFOWriter(ctx, path)
	if writer != nil {
		_ = writer.Close()
		t.Fatal("FIFO writer opened without a reader")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unopened FIFO error = %v, want context deadline", err)
	}
}

func TestPublicPTYActionCleansUpWhenTargetNeverOpensFIFO(t *testing.T) {
	requireRoot(t)
	runsBefore := authorityEntries(t)
	gate := filepath.Join(t.TempDir(), "unopened.fifo")
	if err := unix.Mkfifo(gate, 0o600); err != nil {
		t.Fatal(err)
	}
	marker := "transferlanes-unopened-fifo-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	t.Setenv("TRANSFERLANES_UNOPENED_FIFO_CHILD", marker)
	arguments := []string{"run", "--source", t.TempDir(), "--network", "192.0.2.254", "--",
		"/bin/sleep", "30", "{}"}
	_, err := runPublicTransferLanesPTY(t, arguments,
		func(ctx context.Context, _ *exec.Cmd, _ *os.File, _ *boundedPTYCapture) error {
			openCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
			defer cancel()
			writer, openErr := openFIFOWriter(openCtx, gate)
			if writer != nil {
				_ = writer.Close()
			}
			return openErr
		})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("public PTY unopened FIFO error = %v, want context deadline", err)
	}
	assertNoMarkedProcesses(t, "TRANSFERLANES_UNOPENED_FIFO_CHILD="+marker)
	assertNoOpenDescriptorForPath(t, gate)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("unopened FIFO action left run roots: before=%v after=%v", runsBefore, after)
	}
}

func assertNoOpenDescriptorForPath(t *testing.T, path string) {
	t.Helper()
	descriptors, err := filepath.Glob("/proc/self/fd/*")
	if err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range descriptors {
		target, err := os.Readlink(descriptor)
		if err == nil && target == path {
			t.Fatalf("FIFO descriptor remains open: %s -> %s", descriptor, target)
		}
	}
}

func openFIFOWriter(ctx context.Context, path string) (*os.File, error) {
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		descriptor, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
		if err == nil {
			if err := unix.SetNonblock(descriptor, false); err != nil {
				_ = unix.Close(descriptor)
				return nil, fmt.Errorf("restore blocking FIFO writes: %w", err)
			}
			return os.NewFile(uintptr(descriptor), path), nil
		}
		if !errors.Is(err, unix.ENXIO) {
			return nil, fmt.Errorf("open FIFO writer: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-poll.C:
		}
	}
}

func writePublicPTY(master *os.File, content []byte) error {
	for len(content) != 0 {
		written, err := master.Write(content)
		if err != nil {
			return err
		}
		if written <= 0 || written > len(content) {
			return io.ErrShortWrite
		}
		content = content[written:]
	}
	return nil
}

func TestPublicSupervisedTerminalCtrlCReapsBeforeCleanupAndPreservesLogs(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "public-cli-cancel")
	marker := filepath.Base(scenario.markers)
	t.Setenv("TRANSFERLANES_PUBLIC_CLI_CHILD", marker)
	logDirectory := filepath.Join(scenario.markers, "logs")
	readyPrefix := filepath.Join(scenario.markers, "cancel")
	arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv), "--log-dir", logDirectory}
	for _, source := range fixture.sources {
		arguments = append(arguments, "--network", source.String())
	}
	arguments = append(arguments, "--")
	arguments = append(arguments, transferChildCommand(os.Args[0], transferChildBlock,
		readyPrefix, "{}")...)
	output, err := runPublicTransferLanesPTY(t, arguments, func(_ context.Context, _ *exec.Cmd,
		master *os.File, capture *boundedPTYCapture,
	) error {
		if err := waitForRunningTransfers(capture, fixture.sources, 10*time.Second); err != nil {
			return err
		}
		if err := waitForGlobCount(readyPrefix+".*.ready", len(fixture.sources), 10*time.Second); err != nil {
			return err
		}
		_, err := master.Write([]byte{0x03})
		return err
	})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("SIGINT CLI error=%v output=%q", err, output)
	}
	tail := finalInteractiveOutput(t, output)
	for index, source := range fixture.sources {
		title := fmt.Sprintf("[transfer %d | network %s | exit=0 signal=0]", index+1, source)
		if count := strings.Count(tail, title); count != 1 {
			t.Fatalf("cancelled transfer title %q count=%d: %q", title, count, tail)
		}
	}
	if count := strings.Count(tail, "[transfer "); count != len(fixture.sources) {
		t.Fatalf("cancelled snapshot block count=%d, want %d: %q", count, len(fixture.sources), tail)
	}
	if count := strings.Count(tail, "transferlanes-signal-ready"); count != len(fixture.sources) {
		t.Fatalf("cancelled target final screen count=%d, want %d: %q",
			count, len(fixture.sources), tail)
	}
	if strings.Contains(tail, "SOURCE ") || strings.Contains(tail, "transfer 1: exit=") {
		t.Fatalf("cancelled interactive output retained the old summary: %q", tail)
	}
	for number := 1; number <= len(fixture.sources); number++ {
		path := filepath.Join(logDirectory, fmt.Sprintf("transfer-%02d.ptylog", number))
		content, readErr := os.ReadFile(path)
		if info, statErr := os.Stat(path); statErr != nil || !info.Mode().IsRegular() || readErr != nil ||
			!bytes.Equal(content, []byte("transferlanes-signal-ready\r\n")) {
			t.Errorf("preserved log %s=%q: stat=%v read=%v", path, content, statErr, readErr)
		}
	}
	assertNoMarkedProcesses(t, "TRANSFERLANES_PUBLIC_CLI_CHILD="+marker)
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("cancelled public CLI left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicSupervisedCancellationBypassesSaturatedInput(t *testing.T) {
	for _, test := range []struct {
		name   string
		cancel func(*exec.Cmd, int) error
		exit   int
		killed bool
	}{
		{name: "parent-SIGINT", exit: 130,
			cancel: func(command *exec.Cmd, _ int) error {
				return command.Process.Signal(syscall.SIGINT)
			}},
		{name: "parent-SIGKILL-lifeline", killed: true,
			cancel: func(command *exec.Cmd, _ int) error {
				return command.Process.Kill()
			}},
		{name: "focused-Ctrl-C", exit: 130,
			cancel: func(_ *exec.Cmd, masterFD int) error {
				// The outer PTY's input queue is intentionally full. TIOCSIG exercises the
				// same foreground-process-group SIGINT that its VINTR byte produces without
				// requiring that ordinary byte queue to accept one more character.
				if err := unix.IoctlSetInt(masterFD, unix.TIOCSIG, int(syscall.SIGINT)); err != nil {
					return fmt.Errorf("send focused Ctrl-C through the controlling PTY: %w", err)
				}
				return nil
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHostNetworkFixture(t)
			fixture.install()
			baseline := fixture.hostSurface()
			runsBefore := authorityEntries(t)
			scenario := fixture.newRunSupervisorScenario(t, "public-cli-saturated-"+test.name)
			marker := filepath.Base(scenario.markers)
			t.Setenv("TRANSFERLANES_SATURATED_INPUT_CHILD", marker)
			logDirectory := filepath.Join(scenario.markers, "logs")
			readyPrefix := filepath.Join(scenario.markers, "saturated")
			arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv),
				"--log-dir", logDirectory, "--network", fixture.sources[0].String(), "--"}
			arguments = append(arguments, transferChildCommand(os.Args[0], transferChildNoRead,
				readyPrefix, "{}")...)
			output, runErr := runPublicTransferLanesPTY(t, arguments,
				func(_ context.Context, command *exec.Cmd, master *os.File,
					capture *boundedPTYCapture,
				) error {
					if err := waitForGlobCount(readyPrefix+".*.ready", 1, 10*time.Second); err != nil {
						return err
					}
					if err := writePublicPTY(master, []byte("1")); err != nil {
						return err
					}
					if err := waitForCapturedText(capture, "transferlanes | FOCUSED", 5*time.Second); err != nil {
						return err
					}
					masterFD := int(master.Fd())
					if err := unix.SetNonblock(masterFD, true); err != nil {
						return err
					}
					accepted, err := fillPublicPTYInput(masterFD)
					if err != nil {
						return fmt.Errorf("fill public PTY input after %d bytes: %w", accepted, err)
					}
					if accepted == 0 {
						return errors.New("public PTY accepted no input before backpressure")
					}
					return test.cancel(command, masterFD)
				})
			var exit *exec.ExitError
			if !errors.As(runErr, &exit) {
				t.Fatalf("saturated cancellation error=%v output=%q", runErr, output)
			}
			if test.killed {
				status, ok := exit.ProcessState.Sys().(syscall.WaitStatus)
				if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
					t.Fatalf("saturated parent kill status=%v output=%q", status, output)
				}
			} else if exit.ExitCode() != test.exit {
				t.Fatalf("saturated cancellation exit=%d want=%d output=%q",
					exit.ExitCode(), test.exit, output)
			}
			if test.killed {
				waitForSurface(t, fixture, baseline)
				waitForAuthorityEntries(t, runsBefore)
			}
			content, readErr := os.ReadFile(filepath.Join(logDirectory, "transfer-01.ptylog"))
			if readErr != nil || !bytes.Equal(content, []byte("transferlanes-no-read-ready\n")) {
				t.Fatalf("saturated cancellation log=%q error=%v", content, readErr)
			}
			assertNoMarkedProcesses(t, "TRANSFERLANES_SATURATED_INPUT_CHILD="+marker)
			fixture.assertSurface(baseline)
			if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
				t.Fatalf("saturated cancellation left run roots: before=%v after=%v", runsBefore, after)
			}
		})
	}
}

func waitForAuthorityEntries(t *testing.T, want []string) {
	t.Helper()
	if err := waitForAuthorityEntriesValue(want, 30*time.Second); err != nil {
		t.Fatal(err)
	}
}

func waitForAuthorityEntriesValue(want []string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var last []string
	var lastErr error
	for time.Now().Before(deadline) {
		last, lastErr = readAuthorityEntries()
		if lastErr == nil && slices.Equal(last, want) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("read run authority while waiting for %v: %w", want, lastErr)
	}
	return fmt.Errorf("run authority entries did not return to %v; got %v", want, last)
}

func fillPublicPTYInput(masterFD int) (int, error) {
	chunk := bytes.Repeat([]byte{'x'}, 32*1024)
	total := 0
	for {
		written, err := unix.Write(masterFD, chunk)
		total += written
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			poll := []unix.PollFd{{Fd: int32(masterFD), Events: unix.POLLOUT | unix.POLLERR | unix.POLLHUP}}
			ready, pollErr := unix.Poll(poll, 200)
			if pollErr == unix.EINTR {
				continue
			}
			if pollErr != nil {
				return total, pollErr
			}
			if ready == 0 {
				return total, nil
			}
			if poll[0].Revents&(unix.POLLERR|unix.POLLHUP) != 0 {
				return total, io.ErrClosedPipe
			}
			continue
		}
		if err != nil {
			return total, err
		}
		if written <= 0 || written > len(chunk) {
			return total, io.ErrShortWrite
		}
	}
}

func waitForCapturedText(capture *boundedPTYCapture, text string, limit time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	if err := capture.waitForText(ctx, text); err != nil {
		return fmt.Errorf("terminal did not render %q: %w", text, err)
	}
	return nil
}

func TestPublicSupervisedParentSignalsWaitForCleanup(t *testing.T) {
	for _, test := range []struct {
		name   string
		signal os.Signal
		exit   int
	}{
		{name: "SIGINT", signal: syscall.SIGINT, exit: 130},
		{name: "SIGTERM", signal: syscall.SIGTERM, exit: 1},
		{name: "SIGHUP", signal: syscall.SIGHUP, exit: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newHostNetworkFixture(t)
			fixture.install()
			baseline := fixture.hostSurface()
			runsBefore := authorityEntries(t)
			scenario := fixture.newRunSupervisorScenario(t, "public-cli-os-signal-"+test.name)
			marker := filepath.Base(scenario.markers)
			t.Setenv("TRANSFERLANES_SIGNAL_CHILD", marker)
			logDirectory := filepath.Join(scenario.markers, "logs")
			readyPrefix := filepath.Join(scenario.markers, "signal")
			arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv), "--log-dir", logDirectory}
			for _, source := range fixture.sources {
				arguments = append(arguments, "--network", source.String())
			}
			arguments = append(arguments, "--")
			arguments = append(arguments, transferChildCommand(os.Args[0], transferChildBlock,
				readyPrefix, "{}")...)
			output, err := runPublicTransferLanesPTY(t, arguments,
				func(_ context.Context, command *exec.Cmd, _ *os.File, _ *boundedPTYCapture) error {
					if err := waitForGlobCount(readyPrefix+".*.ready", len(fixture.sources), 10*time.Second); err != nil {
						return err
					}
					return command.Process.Signal(test.signal)
				})
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != test.exit {
				t.Fatalf("signal CLI error=%v output=%q", err, output)
			}
			for number := 1; number <= len(fixture.sources); number++ {
				content, readErr := os.ReadFile(filepath.Join(logDirectory,
					fmt.Sprintf("transfer-%02d.ptylog", number)))
				if readErr != nil || !bytes.Equal(content, []byte("transferlanes-signal-ready\r\n")) {
					t.Errorf("transfer %d partial log=%q error=%v", number, content, readErr)
				}
			}
			assertNoMarkedProcesses(t, "TRANSFERLANES_SIGNAL_CHILD="+marker)
			fixture.assertSurface(baseline)
			if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
				t.Fatalf("signal left run roots: before=%v after=%v", runsBefore, after)
			}
		})
	}
}

func waitForRunningTransfers(capture *boundedPTYCapture, sources []netip.Addr, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		content := capture.String()
		all := true
		for index, source := range sources {
			wanted := fmt.Sprintf("transfer %d/%d | network %s | RUNNING", index+1, len(sources), source)
			all = all && strings.Contains(content, wanted)
		}
		if all {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("timed out waiting for every transfer to run")
}

func TestPublicSupervisedDNSIsPrivateAndPredictable(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	hostResolver, hostIdentity := readHostResolver(t)
	scenario := fixture.newRunSupervisorScenario(t, "public-cli-dns")
	logDirectory := filepath.Join(scenario.markers, "logs")
	arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv),
		"--dns", "192.0.2.53", "--dns", "2001:db8::53", "--log-dir", logDirectory}
	for _, source := range fixture.sources {
		arguments = append(arguments, "--network", source.String())
	}
	arguments = append(arguments, "--", "/usr/bin/grep", "-R", "-H", "^nameserver ",
		"/etc/resolv.conf", "{}")
	output, err := runPublicTransferLanesPTY(t, arguments, nil)
	if err != nil {
		t.Fatalf("custom DNS CLI failed: %v\n%s", err, output)
	}
	for number := 1; number <= len(fixture.sources); number++ {
		content, readErr := os.ReadFile(filepath.Join(logDirectory,
			fmt.Sprintf("transfer-%02d.ptylog", number)))
		if readErr != nil || !strings.Contains(string(content), "nameserver 192.0.2.53") ||
			!strings.Contains(string(content), "nameserver 2001:db8::53") {
			t.Errorf("transfer %d resolver log=%q error=%v", number, content, readErr)
		}
	}
	assertHostResolverUnchanged(t, hostResolver, hostIdentity)
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("custom DNS CLI left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicSupervisedTransferFailureDoesNotCancelSiblings(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "public-cli-transfer-failure")
	evidence := filepath.Join(scenario.markers, "views.jsonl")
	arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv)}
	for _, source := range fixture.sources {
		arguments = append(arguments, "--network", source.String())
	}
	arguments = append(arguments, "--")
	arguments = append(arguments, transferChildCommand(os.Args[0], transferChildViewOneFails,
		evidence, "{}")...)
	output, err := runPublicTransferLanesPTY(t, arguments, nil)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("transfer-failure CLI error=%v output=%q", err, output)
	}
	contents, readErr := os.ReadFile(evidence)
	if readErr != nil || len(strings.Split(strings.TrimSpace(string(contents)), "\n")) != len(fixture.sources) {
		t.Fatalf("sibling evidence=%q error=%v", contents, readErr)
	}
	summary := finalInteractiveOutput(t, output)
	var succeeded, failed int
	for line := range strings.SplitSeq(strings.ReplaceAll(summary, "\r\n", "\n"), "\n") {
		if !strings.Contains(line, "[transfer ") {
			continue
		}
		succeeded += strings.Count(line, "exit=0 signal=0")
		failed += strings.Count(line, "exit=7 signal=0")
	}
	if failed != 1 || succeeded != len(fixture.sources)-1 {
		t.Fatalf("transfer-failure summary=%q", output)
	}
	if strings.Contains(summary, "SOURCE ") || strings.Contains(summary, "transfer 1: exit=") {
		t.Fatalf("interactive transfer failure retained the old summary: %q", summary)
	}
	if strings.Contains(summary, "transferlanes: run: transfer ") {
		t.Fatalf("interactive transfer failure duplicated its structured result: %q", summary)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("transfer failure left run roots: before=%v after=%v", runsBefore, after)
	}
}

type publicPTYAction func(context.Context, *exec.Cmd, *os.File, *boundedPTYCapture) error

func finalInteractiveOutput(t *testing.T, output string) string {
	t.Helper()
	index := strings.LastIndex(output, "\x1b[?1049l")
	if index < 0 {
		t.Fatalf("interactive output did not leave the outer alternate screen: %q", output)
	}
	return output[index+len("\x1b[?1049l"):]
}

func assertInteractiveSuccessSnapshotTitles(t *testing.T, output string, sources []netip.Addr) {
	t.Helper()
	tail := finalInteractiveOutput(t, output)
	position := -1
	for index, source := range sources {
		number := index + 1
		title := fmt.Sprintf("[transfer %d | network %s | exit=0 signal=0]", number, source)
		if count := strings.Count(tail, title); count != 1 {
			t.Fatalf("final snapshot title %q count=%d: %q", title, count, tail)
		}
		next := strings.Index(tail, title)
		if next <= position {
			t.Fatalf("final snapshot title %q is out of order: %q", title, tail)
		}
		position = next
	}
	if strings.Contains(tail, "SOURCE ") || strings.Contains(tail, "transfer 1: exit=") {
		t.Fatalf("interactive output retained the old normal summary: %q", tail)
	}
}

func runPublicTransferLanesPTY(t *testing.T, arguments []string,
	action publicPTYAction,
) (string, error) {
	t.Helper()
	return runPublicTransferLanesPTYCommand(arguments, action)
}

func runPublicTransferLanesPTYWithEnvironment(t *testing.T, arguments, environment []string,
	action publicPTYAction,
) (string, error) {
	t.Helper()
	return runPublicTransferLanesPTYCommandWithEnvironment(arguments, environment, action)
}

func runPublicTransferLanesPTYCommand(arguments []string,
	action publicPTYAction,
) (string, error) {
	return runPublicTransferLanesPTYCommandWithEnvironment(arguments, nil, action)
}

func runPublicTransferLanesPTYCommandWithEnvironment(arguments, environment []string,
	action publicPTYAction,
) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Getenv("TRANSFERLANES_TEST_TRANSFERLANES"), arguments...)
	command.Env = environmentOverrides(os.Environ(), environment)
	master, slave, err := pty.Open()
	if err != nil {
		return "", err
	}
	defer master.Close()
	if err := pty.Setsize(master, &pty.Winsize{Cols: 100, Rows: 30}); err != nil {
		_ = slave.Close()
		return "", err
	}
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := command.Start(); err != nil {
		_ = slave.Close()
		return "", err
	}
	_ = slave.Close()
	capture, drained := &boundedPTYCapture{}, make(chan struct{})
	go func() {
		_, _ = io.Copy(capture, master)
		close(drained)
	}()
	if action != nil {
		if err := action(ctx, command, master, capture); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			_ = master.Close()
			<-drained
			return capture.String(), err
		}
	}
	waitErr := command.Wait()
	_ = master.Close()
	<-drained
	return capture.String(), waitErr
}

func runPublicTransferLanesPipes(t *testing.T, arguments []string) (string, string, error) {
	return runPublicTransferLanesPipesWithAction(t, arguments, nil)
}

func runPublicTransferLanesPipesWithAction(t *testing.T, arguments []string,
	action func(*exec.Cmd) error,
) (string, string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Getenv("TRANSFERLANES_TEST_TRANSFERLANES"), arguments...)
	command.Stdin = strings.NewReader("")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Start(); err != nil {
		return stdout.String(), stderr.String(), err
	}
	if action != nil {
		if err := action(command); err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			return stdout.String(), stderr.String(), err
		}
	}
	err := command.Wait()
	if ctx.Err() != nil {
		return stdout.String(), stderr.String(), errors.Join(err, ctx.Err())
	}
	return stdout.String(), stderr.String(), err
}

func environmentOverrides(base, overrides []string) []string {
	result := append([]string(nil), base...)
	for _, override := range overrides {
		key, _, found := strings.Cut(override, "=")
		if !found || key == "" {
			continue
		}
		prefix := key + "="
		for index := len(result) - 1; index >= 0; index-- {
			if strings.HasPrefix(result[index], prefix) {
				result = append(result[:index], result[index+1:]...)
			}
		}
		result = append(result, override)
	}
	return result
}

func assertPublicRsyncTree(t *testing.T, destination, source string,
	additionalDirectories ...string,
) {
	t.Helper()
	root := filepath.Join(destination, filepath.Base(source))
	want := transferSourceContents()
	for relative, expected := range want {
		content, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil || string(content) != expected {
			t.Errorf("copied %s=%q error=%v", relative, content, err)
		}
	}
	wantManifest := []string{"dir:nested"}
	for _, relative := range additionalDirectories {
		wantManifest = append(wantManifest, "dir:"+filepath.ToSlash(relative))
	}
	for relative := range want {
		wantManifest = append(wantManifest, "file:"+filepath.ToSlash(relative))
	}
	slices.Sort(wantManifest)
	var manifest []string
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "." {
			return err
		}
		kind := "file:"
		if entry.IsDir() {
			kind = "dir:"
		} else if entry.Type()&os.ModeType != 0 {
			kind = "other:"
		}
		manifest = append(manifest, kind+filepath.ToSlash(relative))
		return nil
	}); err != nil {
		t.Fatalf("walk rsync destination: %v", err)
	}
	slices.Sort(manifest)
	if !slices.Equal(manifest, wantManifest) {
		t.Fatalf("rsync destination manifest=%v, want %v", manifest, wantManifest)
	}
}

func transferSourceContents() map[string]string {
	return map[string]string{"alpha": "a", "bravo": "bb", "charlie": "ccc",
		".hidden": "dddd", "nested/echo": "eeeee", "nested/foxtrot": "ffffff"}
}
