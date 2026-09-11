//go:build linux && rootintegration && protocolacceptance

package root_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestS3FixtureRejectsOccupiedMinIOListener(t *testing.T) {
	for _, name := range []string{"API", "console"} {
		t.Run(name, func(t *testing.T) {
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			server := &s3Server{apiAddress: "127.0.0.1:0", consoleAddress: "127.0.0.1:0"}
			if name == "API" {
				server.apiAddress = listener.Addr().String()
			} else {
				server.consoleAddress = listener.Addr().String()
			}
			if err := server.requireMinIOListenersAvailable(); err == nil ||
				!strings.Contains(err.Error(), "MinIO "+name) {
				t.Fatalf("occupied %s listener error=%v", name, err)
			}
		})
	}
}

func TestS3ReadinessRejectsUnownedListener(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer ready.Close()
	command := exec.Command("sleep", "30")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := waitForProcess(command)
	t.Cleanup(func() {
		if !process.exited() {
			_ = command.Process.Kill()
		}
		if !waitForProcessExit(process, time.Second) {
			t.Error("unowned-listener process did not exit")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := waitForS3Ready(ctx, ready.URL, process, command.Process.Pid,
		[]string{ready.Listener.Addr().String()})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unowned ready listener error=%v want deadline", err)
	}
}

func TestS3ReadinessReportsOwnedProcessEarlyExit(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer ready.Close()
	command := exec.Command("sh", "-c", "exit 23")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	process := waitForProcess(command)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := waitForS3Ready(ctx, ready.URL, process, command.Process.Pid,
		[]string{ready.Listener.Addr().String()})
	if err == nil || !strings.Contains(err.Error(), "exited before readiness") {
		t.Fatalf("early process exit error=%v", err)
	}
}

func TestS3ReadinessRequiresConsoleListenerOwnedByCurrentProcess(t *testing.T) {
	ready := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer ready.Close()
	console, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	consoleAddress := console.Addr().String()
	if err := console.Close(); err != nil {
		t.Fatal(err)
	}
	process := &processWait{done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err = waitForS3Ready(ctx, ready.URL, process, os.Getpid(),
		[]string{ready.Listener.Addr().String(), consoleAddress})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing console listener error=%v want deadline", err)
	}
}
