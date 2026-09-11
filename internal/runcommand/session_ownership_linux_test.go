//go:build linux

package runcommand

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runlogs"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

func TestRunSessionRunLogSetPublishesWriteFailureOnceAndNewCloseFailure(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(root, "source")
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := sourcefiles.OpenSourceRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	authority, _, err := rundirectory.CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := authority.OpenRecoveryRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := recovery.Close(); err != nil {
			t.Error(err)
		}
		if err := authority.ClosePrivateAuthority(context.Background()); err != nil {
			t.Error(err)
		}
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	}()

	id, _ := transfernumber.New(1)
	logDirectory := filepath.Join(root, "logs")
	logs, err := runlogs.Open(context.Background(), source, recovery, logDirectory, runlogs.Plain,
		[]transfernumber.Number{id})
	if err != nil {
		t.Fatal(err)
	}
	targetFD := descriptorForPath(t, filepath.Join(logDirectory, "transfer-01.stdout.log"))
	pipeRead, pipeWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := pipeRead.Close(); err != nil {
		t.Fatal(err)
	}
	defer pipeWrite.Close()
	if err := unix.Dup2(int(pipeWrite.Fd()), targetFD); err != nil {
		t.Fatal(err)
	}

	client := newSessionClientRecorder()
	output, _ := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStdout, []byte("raw"))
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{Cancelled: true,
		Reason: runsupervisor.CancelInternal, Transfers: []runsupervisor.TransferResult{{Transfer: id}}})
	client.events <- output
	done := make(chan Result, 1)
	go func() {
		done <- (&runSession{ctx: context.Background(), client: client, logs: logs,
			driver: newCompletionSessionDriver(&staticSessionDisplay{}), inbox: newTerminationInbox()}).run()
	}()
	if reason := waitForSessionCancel(t, client.cancelAccepted, "run log write failure"); reason != runsupervisor.CancelInternal {
		t.Fatalf("cancel reason = %v, want %v", reason, runsupervisor.CancelInternal)
	}
	if err := unix.Close(targetFD); err != nil {
		t.Fatal(err)
	}
	client.events <- final
	result := waitForSessionResult(t, done, "run log finalization")
	if got := countErrorOccurrence(result.LogError(), unix.EPIPE); got != 1 {
		t.Fatalf("write failure occurrence=%d, want 1 in %v", got, result.LogError())
	}
	if logErr := result.LogError(); logErr == nil ||
		!strings.Contains(logErr.Error(), "sync raw log: sync transfer-01.stdout.log: bad file descriptor") ||
		!strings.Contains(logErr.Error(), "close raw log: close transfer-01.stdout.log: bad file descriptor") {
		t.Fatalf("finalization error=%v, want independent sync and close evidence", logErr)
	}
	if result.ParentError() != nil {
		t.Fatalf("raw log failures entered ParentError: %v", result.ParentError())
	}
}

func descriptorForPath(t *testing.T, path string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil || target != path {
			continue
		}
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		return fd
	}
	t.Fatalf("no open descriptor for %s", path)
	return -1
}
