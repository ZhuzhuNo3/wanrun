//go:build linux

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/listnetworks"
	"github.com/ZhuzhuNo3/transferlanes/internal/runcommand"
	"golang.org/x/sys/unix"
)

const (
	startupSignalMode    = "TRANSFERLANES_TEST_STARTUP_SIGNAL_MODE"
	startupSignalMarker  = "TRANSFERLANES_TEST_STARTUP_SIGNAL_MARKER"
	startupSignalGate    = "TRANSFERLANES_TEST_STARTUP_SIGNAL_GATE"
	supervisorPrivateArg = "--transferlanes-internal-run-supervisor"
)

func TestMain(main *testing.M) {
	if runStartupSignalProcess() {
		return
	}
	os.Exit(main.Run())
}

func runStartupSignalProcess() bool {
	mode := os.Getenv(startupSignalMode)
	if mode == "" {
		return false
	}
	if len(os.Args) == 2 && os.Args[1] == supervisorPrivateArg {
		if mode == "failed-handshake" {
			os.Exit(94)
		}
		pid, err := outerNamespacePID()
		if err != nil || os.WriteFile(os.Getenv(startupSignalMarker),
			[]byte(strconv.Itoa(pid)), 0o600) != nil {
			os.Exit(91)
		}
		gate, err := os.OpenFile(os.Getenv(startupSignalGate), os.O_RDONLY, 0)
		if err != nil {
			os.Exit(92)
		}
		_ = gate.Close()
		os.Exit(93)
	}
	os.Exit(Execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr,
		listnetworks.New(), runcommand.New(), func() (string, error) { return "test\n", nil }))
	return true
}

func TestPublicPreparationSignalsKeepExitClassesAndDoNotFabricateFinal(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("supervisor namespace fixture requires root")
	}
	for _, test := range []struct {
		signal syscall.Signal
		exit   int
	}{{syscall.SIGINT, runExitInterrupt}, {syscall.SIGTERM, runExitRuntime},
		{syscall.SIGHUP, runExitRuntime}} {
		t.Run(test.signal.String(), func(t *testing.T) {
			root := t.TempDir()
			source, logs := filepath.Join(root, "source"), filepath.Join(root, "logs")
			marker, gate := filepath.Join(root, "started"), filepath.Join(root, "gate")
			if err := errors.Join(os.Mkdir(source, 0o700), os.Mkdir(logs, 0o700),
				unix.Mkfifo(gate, 0o600)); err != nil {
				t.Fatal(err)
			}
			argv := []string{"run", "--no-tui", "--source", source, "--log-dir", logs,
				"--network", "192.0.2.1", "--", "/bin/true", "{}"}
			command := exec.Command(os.Args[0], argv...)
			command.Env = append(os.Environ(), startupSignalMode+"=blocked-handshake",
				startupSignalMarker+"="+marker, startupSignalGate+"="+gate)
			var output bytes.Buffer
			command.Stdout, command.Stderr = &output, &output
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			waitForStartupMarker(t, marker)
			if err := command.Process.Signal(test.signal); err != nil {
				t.Fatal(err)
			}
			err := command.Wait()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != test.exit {
				t.Fatalf("exit error=%v output=%q", err, output.String())
			}
			if strings.Contains(output.String(), "completed:") ||
				strings.Contains(output.String(), "cleanup completed") {
				t.Fatalf("preparation signal fabricated Final evidence: %q", output.String())
			}
		})
	}
}

func TestPublicIndependentHandshakeFailureRemainsRuntime(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("supervisor namespace fixture requires root")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "run", "--no-tui", "--source", source,
		"--network", "192.0.2.1", "--", "/bin/true", "{}")
	command.Env = append(os.Environ(), startupSignalMode+"=failed-handshake")
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != runExitRuntime ||
		!strings.Contains(output.String(), "supervisor") {
		t.Fatalf("exit error=%v output=%q", err, output.String())
	}
}

func waitForStartupMarker(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		content, err := os.ReadFile(path)
		if err == nil {
			pid, convertErr := strconv.Atoi(string(content))
			if convertErr != nil {
				t.Fatal(convertErr)
			}
			return pid
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor startup marker not observed")
		}
		time.Sleep(time.Millisecond)
	}
}

func outerNamespacePID() (int, error) {
	content, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(content), "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "NSpid:"))
		if len(fields) == 0 {
			break
		}
		return strconv.Atoi(fields[0])
	}
	return 0, fmt.Errorf("supervisor outer PID is absent")
}
