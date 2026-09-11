//go:build linux && rootintegration

package root_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func waitForExactProcessExit(pid int, timeout time.Duration) error {
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
	return fmt.Errorf("process %d remains after %s: %v", pid, timeout, lastErr)
}

func assertNoMarkedProcesses(t *testing.T, marker string) {
	t.Helper()
	paths, err := filepath.Glob("/proc/[0-9]*/environ")
	if err != nil {
		t.Fatalf("scan process environments: %v", err)
	}
	want := append([]byte(marker), 0)
	for _, path := range paths {
		file, openErr := os.Open(path)
		if openErr != nil {
			continue
		}
		content, readErr := io.ReadAll(io.LimitReader(file, 64<<10))
		_ = file.Close()
		if readErr == nil && bytes.Contains(content, want) {
			t.Errorf("marked supervisor or child process remains: %s", path)
		}
	}
}

type boundedPTYCapture struct {
	mu       sync.Mutex
	content  bytes.Buffer
	overflow bool
	changed  chan struct{}
}

func (capture *boundedPTYCapture) Write(content []byte) (int, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	written := len(content)
	remaining := (1 << 20) - capture.content.Len()
	if remaining > 0 {
		_, _ = capture.content.Write(content[:min(remaining, len(content))])
	}
	if len(content) > remaining {
		capture.overflow = true
	}
	if capture.changed != nil {
		close(capture.changed)
	}
	capture.changed = make(chan struct{})
	return written, nil
}

func (capture *boundedPTYCapture) String() string {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.content.String()
}

func (capture *boundedPTYCapture) Overflow() bool {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.overflow
}

func (capture *boundedPTYCapture) waitForText(ctx context.Context, wanted string) error {
	for {
		capture.mu.Lock()
		if bytes.Contains(capture.content.Bytes(), []byte(wanted)) {
			capture.mu.Unlock()
			return nil
		}
		if capture.changed == nil {
			capture.changed = make(chan struct{})
		}
		changed := capture.changed
		capture.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
