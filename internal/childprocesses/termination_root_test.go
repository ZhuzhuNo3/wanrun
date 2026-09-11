//go:build linux && rootintegration

package childprocesses

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

const reaperFailureNamespaceEnv = "TRANSFERLANES_TEST_REAPER_FAILURE_NAMESPACE"
const concurrentCancelNamespaceEnv = "TRANSFERLANES_TEST_CONCURRENT_CANCEL_NAMESPACE"

func TestReaperFailureStillSignalsNamespaceDescendant(t *testing.T) {
	if os.Getenv(reaperFailureNamespaceEnv) == "1" {
		testReaperFailureSignalEffort(t)
		return
	}
	command := exec.Command("unshare", "--pid", "--fork", "--mount-proc",
		os.Args[0], "-test.run=^TestReaperFailureStillSignalsNamespaceDescendant$")
	command.Env = append(os.Environ(), reaperFailureNamespaceEnv+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run reaper-failure PID namespace: %v\n%s", err, output)
	}
}

func TestConcurrentCancelUsesOneSignalEscalation(t *testing.T) {
	if os.Getenv(concurrentCancelNamespaceEnv) == "1" {
		testConcurrentCancelSignalEscalation(t)
		return
	}
	command := exec.Command("unshare", "--pid", "--fork", "--mount-proc",
		os.Args[0], "-test.run=^TestConcurrentCancelUsesOneSignalEscalation$")
	command.Env = append(os.Environ(), concurrentCancelNamespaceEnv+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("run concurrent-cancel PID namespace: %v\n%s", err, output)
	}
}

func testConcurrentCancelSignalEscalation(t *testing.T) {
	if os.Getpid() != 1 {
		t.Fatalf("concurrent-cancel fixture PID=%d, want namespace init", os.Getpid())
	}
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	signals := filepath.Join(root, "signals")
	script := fmt.Sprintf("trap 'echo interrupt >> %s' INT; "+
		"trap 'echo terminated >> %s' TERM; : > %s; while :; do sleep 10; done",
		signals, signals, ready)
	childProcess := exec.Command("/bin/sh", "-c", script)
	if err := childProcess.Start(); err != nil {
		t.Fatal(err)
	}
	pid := childProcess.Process.Pid
	if err := childProcess.Process.Release(); err != nil {
		t.Fatal(err)
	}
	defer reapNamespaceChildren()
	if err := waitForTestPath(ready); err != nil {
		t.Fatal(err)
	}
	id, _ := transfernumber.New(1)
	launched := &launchedChild{transfer: id, pid: pid}
	reaper := startReaper([]*launchedChild{launched})
	outputWake, controlWake := testProcessWakes(t)
	processes := newProcessSet(context.Background(), []*launchedChild{launched}, reaper,
		outputWake, controlWake)
	drainSessionEvidence(processes)
	const callers = 16
	cancelErrors := make(chan error, callers)
	for range callers {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), abortTimeout)
			defer cancel()
			cancelErrors <- processes.Cancel(ctx)
		}()
	}
	for range callers {
		if err := <-cancelErrors; err != nil {
			t.Errorf("concurrent cancellation: %v", err)
		}
	}
	result, err := processes.Wait()
	if err != nil || !result.Cancelled || len(result.Transfers) != 1 ||
		result.Transfers[0].Signal != int(unix.SIGKILL) {
		t.Fatalf("concurrent-cancel result=%#v error=%v", result, err)
	}
	content, err := os.ReadFile(signals)
	if err != nil || strings.Count(string(content), "interrupt\n") != 1 ||
		strings.Count(string(content), "terminated\n") != 1 {
		t.Fatalf("concurrent-cancel signal evidence=%q error=%v", content, err)
	}
}

func testReaperFailureSignalEffort(t *testing.T) {
	if os.Getpid() != 1 {
		t.Fatalf("reaper-failure fixture PID=%d, want namespace init", os.Getpid())
	}
	root := t.TempDir()
	ready := filepath.Join(root, "ready")
	signals := filepath.Join(root, "signals")
	script := fmt.Sprintf("trap 'echo interrupt >> %s' INT; "+
		"trap 'echo terminated >> %s' TERM; : > %s; while :; do sleep 10; done",
		signals, signals, ready)
	child := exec.Command("/bin/sh", "-c", script)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	pid := child.Process.Pid
	if err := child.Process.Release(); err != nil {
		t.Fatal(err)
	}
	defer reapNamespaceChildren()
	if err := waitForTestPath(ready); err != nil {
		t.Fatal(err)
	}
	id, _ := transfernumber.New(1)
	prepared := &launchedChild{transfer: id, pid: pid}
	waitErr := errors.New("wait4 supervision failed")
	reaper := startReaperWith([]*launchedChild{prepared}, scriptedWait4(
		wait4Result{err: waitErr}))
	outputWake, controlWake := testProcessWakes(t)
	processes := newProcessSet(context.Background(), []*launchedChild{prepared}, reaper,
		outputWake, controlWake)
	drainSessionEvidence(processes)
	result, err := processes.Wait()
	if !errors.Is(err, waitErr) || !result.Cancelled {
		t.Errorf("reaper failure result=%#v error=%v", result, err)
	}
	content, err := os.ReadFile(signals)
	if err != nil || !strings.Contains(string(content), "interrupt\n") ||
		!strings.Contains(string(content), "terminated\n") {
		t.Fatalf("namespace descendant signal evidence=%q error=%v", content, err)
	}
	select {
	case <-processes.ContainmentDone():
		t.Fatal("reaper failure falsely confirmed containment")
	default:
	}
}

func waitForTestPath(path string) error {
	for range 5000 {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if err := unix.Nanosleep(&unix.Timespec{Nsec: 1_000_000}, nil); err != nil && err != unix.EINTR {
			return err
		}
	}
	return fmt.Errorf("timed out waiting for %s", path)
}

func reapNamespaceChildren() {
	_ = unix.Kill(-1, syscall.SIGKILL)
	for {
		var status unix.WaitStatus
		_, err := unix.Wait4(-1, &status, 0, nil)
		if err == unix.ECHILD {
			return
		}
		if err != nil && err != unix.EINTR {
			return
		}
	}
}
