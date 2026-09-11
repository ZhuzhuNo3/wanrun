//go:build linux && rootintegration

package root_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"syscall"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/charmbracelet/x/term"
	"golang.org/x/sys/unix"
)

const (
	transferChildArgument        = "--transferlanes-root-transfer-child"
	transferChildView            = "view"
	transferChildViewOneFails    = "view-one-fails"
	transferChildBlock           = "block"
	transferChildOverlapGate     = "overlap-gate"
	transferChildResolver        = "resolver"
	transferChildRsyncWait       = "rsync-wait"
	transferChildCleanupBlock    = "cleanup-block"
	transferChildTail            = "tail"
	transferChildSlowInput       = "slow-input"
	transferChildNoRead          = "no-read"
	transferChildRsyncServerGate = "rsync-server-gate"
	plainChildArgument           = "--transferlanes-root-plain-child"
	plainChildPTY                = "pty"
	plainChildPTYResize          = "pty-resize"
	plainChildPlain              = "plain"
	plainChildExit               = "exit"
	measurementChildArgument     = "--transferlanes-root-measurement-child"
	measurementChildHTTPSource   = "http-source"
	diagnosticChildArgument      = "--transferlanes-root-diagnostic-child"
	diagnosticChildExit          = "exit"
	diagnosticChildHelperFlood   = "helper-stderr-flood"
	containmentChildArgument     = "--transferlanes-root-containment-child"
	containmentChildSignals      = "signals"
	containmentChildCleanSignal  = "clean-signal"
	containmentChildStress       = "descendant-stress"
	containmentChildLongOrphan   = "long-orphan"
	containmentChildGrandchild   = "grandchild"
	containmentChildCLIProxy     = "cli-proxy"
)

func maybeRunTransferChild(argv []string) (bool, int) {
	if len(argv) == 0 || argv[0] != transferChildArgument {
		return false, 0
	}
	return true, runTransferChild(argv[1:])
}

func runTransferChild(argv []string) int {
	if len(argv) < 1 {
		return 2
	}
	switch argv[0] {
	case transferChildView:
		return runSupervisorTransferViewChild(argv[1:], false)
	case transferChildViewOneFails:
		return runSupervisorTransferViewChild(argv[1:], true)
	case transferChildBlock:
		return runSupervisorTransferBlockChild(argv[1:])
	case transferChildOverlapGate:
		return runSupervisorTransferOverlapGateChild(argv[1:])
	case transferChildResolver:
		return runSupervisorTransferResolverChild(argv[1:])
	case transferChildRsyncWait:
		return runSupervisorTransferRsyncChild(argv[1:])
	case transferChildCleanupBlock:
		return runSupervisorTransferCleanupBlockChild(argv[1:])
	case transferChildTail:
		return runSupervisorTransferTailChild(argv[1:])
	case transferChildSlowInput:
		return runSupervisorTransferSlowInputChild(argv[1:])
	case transferChildNoRead:
		return runSupervisorTransferNoReadChild(argv[1:])
	case transferChildRsyncServerGate:
		return runRsyncServerGate(argv[1:])
	default:
		return 2
	}
}

func transferChildCommand(executable, action string, arguments ...string) []string {
	return append([]string{executable, transferChildArgument, action}, arguments...)
}

func maybeRunPlainChild(argv []string) (bool, int) {
	if len(argv) == 0 || argv[0] != plainChildArgument {
		return false, 0
	}
	return true, runPlainChild(argv[1:])
}

func runPlainChild(argv []string) int {
	if len(argv) < 1 {
		return 2
	}
	switch argv[0] {
	case plainChildPTY:
		return runSupervisorPTYChild(argv[1:], plainChildPTY, false)
	case plainChildPTYResize:
		return runSupervisorPTYChild(argv[1:], plainChildPTYResize, true)
	case plainChildPlain:
		return runSupervisorPlainChild(argv[1:])
	case plainChildExit:
		return runSupervisorExitChild(argv[1:])
	default:
		return 2
	}
}

func plainChildCommandLine(executable, action string, arguments ...string) []string {
	return append([]string{executable, plainChildArgument, action}, arguments...)
}

func maybeRunMeasurementChild(argv []string) (bool, int) {
	if len(argv) == 0 || argv[0] != measurementChildArgument {
		return false, 0
	}
	if len(argv) < 2 || argv[1] != measurementChildHTTPSource {
		return true, 2
	}
	return true, runHTTPSourceChild(argv[2:])
}

func measurementChildCommand(executable string, arguments ...string) []string {
	return append([]string{executable, measurementChildArgument, measurementChildHTTPSource}, arguments...)
}

func maybeRunDiagnosticChild(argv []string) (bool, int) {
	if len(argv) == 0 || argv[0] != diagnosticChildArgument {
		return false, 0
	}
	if len(argv) < 2 {
		return true, 2
	}
	switch argv[1] {
	case diagnosticChildExit:
		return true, runSupervisorExitChild(argv[2:])
	case diagnosticChildHelperFlood:
		return true, runSupervisorHelperStderrFloodChild(argv[2:])
	default:
		return true, 2
	}
}

func diagnosticChildCommand(executable, action string, arguments ...string) []string {
	return append([]string{executable, diagnosticChildArgument, action}, arguments...)
}

func maybeRunContainmentChild(argv []string) (bool, int) {
	if len(argv) == 0 || argv[0] != containmentChildArgument {
		return false, 0
	}
	if len(argv) < 2 {
		return true, 2
	}
	switch argv[1] {
	case containmentChildSignals:
		if len(argv) != 3 {
			return true, 2
		}
		return true, runSupervisorSignalChild(argv[2])
	case containmentChildCleanSignal:
		if len(argv) != 3 {
			return true, 2
		}
		return true, runSupervisorCleanSignalChild(argv[2])
	case containmentChildStress:
		return true, runSupervisorDescendantStressChild(argv[2:])
	case containmentChildLongOrphan:
		return true, runSupervisorLongOrphanChild(argv[2:])
	case containmentChildGrandchild:
		if len(argv) != 3 {
			return true, 2
		}
		return true, runSupervisorRootGrandchild(argv[2])
	case containmentChildCLIProxy:
		if len(argv) != 3 {
			return true, 2
		}
		return true, runSupervisorCLIProxy(argv[2])
	default:
		return true, 2
	}
}

func containmentChildCommand(executable, action string, arguments ...string) []string {
	return append([]string{executable, containmentChildArgument, action}, arguments...)
}

type rootPlainEvidence struct {
	WorkingDirectory string   `json:"workingDirectory"`
	Resolver         string   `json:"resolver"`
	ResolverReadOnly bool     `json:"resolverReadOnly"`
	StdinEOF         bool     `json:"stdinEOF"`
	StdoutTTY        bool     `json:"stdoutTTY"`
	StderrTTY        bool     `json:"stderrTTY"`
	TemporaryIPv4    bool     `json:"temporaryIPv4"`
	InheritedFDs     []string `json:"inheritedFDs"`
}

func runSupervisorPlainChild(argv []string) int {
	if len(argv) != 4 && len(argv) != 5 {
		return 2
	}
	exitCode, err := strconv.Atoi(argv[1])
	if err != nil || exitCode < 0 || exitCode > 255 {
		return 2
	}
	stdout, stdoutDecodeErr := base64.StdEncoding.DecodeString(argv[2])
	stderr, stderrDecodeErr := base64.StdEncoding.DecodeString(argv[3])
	if stdoutDecodeErr != nil || stderrDecodeErr != nil {
		return 2
	}
	var input [1]byte
	count, readErr := os.Stdin.Read(input[:])
	resolver, resolverErr := os.ReadFile("/etc/resolv.conf")
	resolverWriter, resolverWriteErr := os.OpenFile("/etc/resolv.conf", os.O_WRONLY, 0)
	if resolverWriter != nil {
		_ = resolverWriter.Close()
	}
	cwd, cwdErr := os.Getwd()
	evidencePath := argv[0]
	if len(argv) == 5 {
		if !validPrivateViewAlias(argv[4]) {
			return 2
		}
		evidencePath += "." + filepath.Base(filepath.Dir(cwd))
	}
	fds, descriptorErr := inheritedDescriptorTargets()
	evidence := rootPlainEvidence{WorkingDirectory: cwd, Resolver: string(resolver),
		ResolverReadOnly: resolverWriteErr != nil, StdinEOF: count == 0 && readErr == io.EOF,
		StdoutTTY: term.IsTerminal(os.Stdout.Fd()), StderrTTY: term.IsTerminal(os.Stderr.Fd()),
		TemporaryIPv4: hasTemporaryIPv4(), InheritedFDs: fds}
	encoded, encodeErr := json.Marshal(evidence)
	if err := errors.Join(resolverErr, cwdErr, descriptorErr, encodeErr,
		os.WriteFile(evidencePath, encoded, 0o600)); err != nil {
		return 3
	}
	_, stdoutErr := os.Stdout.Write(stdout)
	_, stderrErr := os.Stderr.Write(stderr)
	if stdoutErr != nil || stderrErr != nil {
		return 4
	}
	return exitCode
}

func inheritedDescriptorTargets() ([]string, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return nil, err
	}
	var targets []string
	for _, entry := range entries {
		fd, parseErr := strconv.Atoi(entry.Name())
		if parseErr != nil || fd < 3 {
			continue
		}
		target, readErr := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if readErr == nil {
			targets = append(targets, target)
		}
	}
	slices.Sort(targets)
	return targets, nil
}

func runHTTPSourceChild(argv []string) int {
	if len(argv) != 2 || !validPrivateViewAlias(argv[1]) {
		return 2
	}
	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(argv[0])
	if err != nil {
		return 3
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, 128))
	if err != nil || response.StatusCode != http.StatusOK {
		return 4
	}
	_, _ = fmt.Fprintf(os.Stdout, "observed-source=%s\n", contents)
	return 0
}

func runSupervisorTransferNoReadChild(argv []string) int {
	if len(argv) != 2 || !validPrivateViewAlias(argv[1]) {
		return 2
	}
	oldState, err := term.MakeRaw(os.Stdin.Fd())
	if err != nil {
		return 3
	}
	defer term.Restore(os.Stdin.Fd(), oldState)
	signals := make(chan os.Signal, 1)
	signalNotify(signals)
	defer signalStop(signals)
	_, _ = fmt.Fprintln(os.Stdout, "transferlanes-no-read-ready")
	ready := fmt.Sprintf("%s.%d.ready", argv[0], os.Getpid())
	if err := os.WriteFile(ready, []byte("ready\n"), 0o600); err != nil {
		return 4
	}
	<-signals
	return 0
}

func runSupervisorTransferSlowInputChild(argv []string) int {
	if len(argv) != 4 {
		return 2
	}
	size, err := strconv.Atoi(argv[2])
	if err != nil || size < 1 {
		return 2
	}
	oldState, err := term.MakeRaw(os.Stdin.Fd())
	if err != nil {
		return 3
	}
	defer term.Restore(os.Stdin.Fd(), oldState)
	gate, err := os.Open(argv[0])
	if err != nil {
		return 4
	}
	defer gate.Close()
	_, _ = fmt.Fprintln(os.Stdout, "slow-input-ready")
	if _, err := io.ReadFull(gate, make([]byte, 1)); err != nil {
		return 5
	}
	content := make([]byte, size)
	if _, err := io.ReadFull(os.Stdin, content); err != nil {
		return 6
	}
	if err := os.WriteFile(argv[1], content, 0o600); err != nil {
		return 7
	}
	_, _ = fmt.Fprintln(os.Stdout, "slow-input-complete")
	return 0
}

func runSupervisorTransferTailChild(argv []string) int {
	if len(argv) != 1 {
		return 2
	}
	content := bytes.Repeat([]byte("tail-0123456789"), 16*1024)
	for len(content) != 0 {
		written, err := os.Stdout.Write(content)
		if err != nil || written <= 0 || written > len(content) {
			return 3
		}
		content = content[written:]
	}
	return 0
}

type rootPTYEvidence struct {
	Argv             []string `json:"argv"`
	WorkingDirectory string   `json:"workingDirectory"`
	Resolver         string   `json:"resolver"`
	ResolverReadOnly bool     `json:"resolverReadOnly"`
	Input            string   `json:"input"`
	InitialCols      uint16   `json:"initialCols"`
	InitialRows      uint16   `json:"initialRows"`
	Cols             uint16   `json:"cols"`
	Rows             uint16   `json:"rows"`
	PID              int      `json:"pid"`
	PGID             int      `json:"pgid"`
	SID              int      `json:"sid"`
	TemporaryIPv4    bool     `json:"temporaryIPv4"`
	InheritedFDs     []string `json:"inheritedFDs"`
}

func runSupervisorPTYChild(argv []string, action string, waitForResize bool) int {
	if len(argv) != 3 {
		return 2
	}
	var resizeSignals chan os.Signal
	if waitForResize {
		resizeSignals = make(chan os.Signal, 1)
		signal.Notify(resizeSignals, syscall.SIGWINCH)
		defer signal.Stop(resizeSignals)
	}
	initialSize, err := unix.IoctlGetWinsize(unix.Stdin, unix.TIOCGWINSZ)
	if err != nil {
		return 3
	}
	_, _ = fmt.Fprintln(os.Stdout, "child-pty-ready")
	input, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return 4
	}
	if waitForResize {
		select {
		case <-resizeSignals:
			_, _ = fmt.Fprintln(os.Stdout, "child-pty-resize-accepted")
		case <-time.After(5 * time.Second):
			return 5
		}
	}
	size, err := unix.IoctlGetWinsize(unix.Stdin, unix.TIOCGWINSZ)
	if err != nil {
		return 6
	}
	cwd, _ := os.Getwd()
	resolver, _ := os.ReadFile("/etc/resolv.conf")
	resolverWriter, resolverWriteErr := os.OpenFile("/etc/resolv.conf", os.O_WRONLY, 0)
	if resolverWriter != nil {
		_ = resolverWriter.Close()
	}
	pid := os.Getpid()
	pgid, _ := unix.Getpgid(0)
	sid, _ := unix.Getsid(0)
	fds, _ := inheritedDescriptorTargets()
	evidence := rootPTYEvidence{Argv: append(plainChildCommandLine(os.Args[0], action), argv...),
		WorkingDirectory: cwd, Resolver: string(resolver), ResolverReadOnly: resolverWriteErr != nil,
		Input: input, InitialCols: initialSize.Col, InitialRows: initialSize.Row,
		Cols: size.Col, Rows: size.Row,
		PID: pid, PGID: pgid, SID: sid, TemporaryIPv4: hasTemporaryIPv4(), InheritedFDs: fds}
	encoded, _ := json.Marshal(evidence)
	if err := os.WriteFile(argv[0], encoded, 0o600); err != nil {
		return 7
	}
	return 0
}

func runSupervisorExitChild(argv []string) int {
	if len(argv) != 3 {
		return 2
	}
	code, codeErr := strconv.Atoi(argv[1])
	delay, delayErr := strconv.Atoi(argv[2])
	if codeErr != nil || delayErr != nil {
		return 2
	}
	time.Sleep(time.Duration(delay) * time.Millisecond)
	_ = os.WriteFile(argv[0], []byte("finished\n"), 0o600)
	return code
}

func runSupervisorSignalChild(prefix string) int {
	signals := make(chan os.Signal, 8)
	signalNotify(signals)
	defer signalStop(signals)
	command := containmentChildCommand(os.Args[0], containmentChildGrandchild, prefix+".grandchild.pid")
	grandchild := exec.Command(command[0], command[1:]...)
	if err := grandchild.Start(); err != nil {
		return 4
	}
	_ = grandchild.Process.Release()
	if !waitForChildPath(prefix+".grandchild.pid", 5*time.Second) {
		return 5
	}
	if err := os.WriteFile(prefix+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return 3
	}
	for received := range signals {
		file, err := os.OpenFile(prefix+".signals", os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
		if err == nil {
			_, _ = fmt.Fprintln(file, received.String())
			_ = file.Close()
		}
	}
	return 0
}

func runSupervisorCleanSignalChild(prefix string) int {
	signals := make(chan os.Signal, 1)
	signalNotify(signals)
	defer signalStop(signals)
	if err := os.WriteFile(prefix+".pid", []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return 3
	}
	received := <-signals
	if err := os.WriteFile(prefix+".signal", []byte(received.String()+"\n"), 0o600); err != nil {
		return 4
	}
	return 0
}

func runSupervisorDescendantStressChild(argv []string) int {
	if len(argv) != 2 {
		return 2
	}
	count, err := strconv.Atoi(argv[1])
	if err != nil || count < 1 {
		return 2
	}
	for index := 0; index < count; index++ {
		child := exec.Command("/bin/true")
		if err := child.Start(); err != nil {
			return 3
		}
		_ = child.Process.Release()
	}
	if err := os.WriteFile(argv[0], []byte(strconv.Itoa(count)+"\n"), 0o600); err != nil {
		return 4
	}
	return 0
}

func runSupervisorLongOrphanChild(argv []string) int {
	if len(argv) != 1 {
		return 2
	}
	grandchildPID := argv[0] + ".grandchild.pid"
	command := containmentChildCommand(os.Args[0], containmentChildGrandchild, grandchildPID)
	grandchild := exec.Command(command[0], command[1:]...)
	grandchild.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := grandchild.Start(); err != nil {
		return 3
	}
	_ = grandchild.Process.Release()
	if !waitForChildPath(grandchildPID, 5*time.Second) {
		return 4
	}
	if err := os.WriteFile(argv[0]+".parent.finished", []byte("finished\n"), 0o600); err != nil {
		return 5
	}
	return 0
}

func runSupervisorHelperStderrFloodChild(argv []string) int {
	if len(argv) != 1 {
		return 2
	}
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
	chunk := bytes.Repeat([]byte{0xff}, 4096)
	for range 128 {
		if _, err := os.Stderr.Write(chunk); err != nil {
			return 3
		}
	}
	if err := os.WriteFile(argv[0], []byte("drained\n"), 0o600); err != nil {
		return 4
	}
	return 7
}

func runSupervisorRootGrandchild(pidFile string) int {
	signals := make(chan os.Signal, 8)
	signalNotify(signals)
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		return 3
	}
	for range signals {
	}
	return 0
}

func waitForChildPath(path string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func waitForChildPaths(timeout time.Duration, paths ...string) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		allPresent := true
		for _, path := range paths {
			if _, err := os.Stat(path); err != nil {
				allPresent = false
				break
			}
		}
		if allPresent {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for child paths %q", paths)
}

func runSupervisorCLIProxy(pidFile string) int {
	client, err := launchScenarioRunSupervisor(context.Background())
	if err != nil {
		return 3
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(client.PID())), 0o600); err != nil {
		return 4
	}
	for {
		event, err := client.Next()
		if err != nil {
			_ = client.CloseLifeline()
			_ = client.Wait()
			return 5
		}
		if event.Kind() == runsupervisor.EventFinal {
			_ = client.CloseLifeline()
			if err := client.Wait(); err != nil {
				return 6
			}
			return 0
		}
	}
}

func signalNotify(channel chan<- os.Signal) {
	signal.Notify(channel, syscall.SIGINT, syscall.SIGTERM)
}

func signalStop(channel chan<- os.Signal) { signal.Stop(channel) }
