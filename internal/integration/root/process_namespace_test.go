//go:build linux && rootintegration

package root_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func assertThreeUserNetworkNamespaces(t *testing.T, marker, executable string) {
	t.Helper()
	hostNamespace, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	namespaces := make(map[string][]int)
	for _, process := range markedProcesses(t, marker) {
		if !bytes.Contains(process.command, []byte(executable)) {
			continue
		}
		namespace, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/net", process.pid))
		if err == nil && namespace != hostNamespace {
			namespaces[namespace] = append(namespaces[namespace], process.pid)
		}
	}
	if len(namespaces) != 3 {
		t.Fatalf("%s user processes occupied network namespaces=%v, want three distinct non-host namespaces",
			executable, namespaces)
	}
	t.Logf("%s user process namespaces: %v", executable, namespaces)
}

type markedProcess struct {
	pid     int
	command []byte
}

func markedProcesses(t *testing.T, marker string) []markedProcess {
	t.Helper()
	paths, err := filepath.Glob("/proc/[0-9]*/environ")
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(marker), 0)
	var result []markedProcess
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		environment, readErr := io.ReadAll(io.LimitReader(file, 64<<10))
		_ = file.Close()
		if readErr != nil || !bytes.Contains(environment, want) {
			continue
		}
		pid, err := strconv.Atoi(filepath.Base(filepath.Dir(path)))
		command, commandErr := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err == nil && commandErr == nil {
			result = append(result, markedProcess{pid: pid, command: command})
		}
	}
	return result
}
