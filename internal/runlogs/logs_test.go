package runlogs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

func TestDirectoryRelationshipKeepsOneDescriptorOwnerUnderLowLimitAndReuse(t *testing.T) {
	rootPath := filepath.Join(canonicalLogTemp(t), "unrelated-root")
	candidatePath := filepath.Join(canonicalLogTemp(t), "candidate", "one", "two", "three")
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(candidatePath, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	candidate, err := os.Open(candidatePath)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()

	var original unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
		t.Fatal(err)
	}
	highest, baseline := openDescriptorSnapshot(t)
	limited := original
	limited.Cur = uint64(highest + 16)
	if limited.Cur > original.Cur {
		limited.Cur = original.Cur
	}
	if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &limited); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := unix.Setrlimit(unix.RLIMIT_NOFILE, &original); err != nil {
			t.Errorf("restore descriptor limit: %v", err)
		}
	}()

	stopReuse := make(chan struct{})
	reuseDone := make(chan error, 1)
	go reuseDescriptors(stopReuse, reuseDone)
	var relationshipErr error
	for range 64 {
		inside, err := directoryAtOrBelow(int(root.Fd()), int(candidate.Fd()))
		if err != nil {
			relationshipErr = err
			break
		}
		if inside {
			relationshipErr = errors.New("unrelated directory reported inside root")
			break
		}
		runtime.Gosched()
	}
	close(stopReuse)
	reuseErr := <-reuseDone
	_, after := openDescriptorSnapshot(t)
	if relationshipErr != nil || reuseErr != nil || after != baseline {
		t.Fatalf("relationship=%v reuse=%v open descriptors=%d, want %d",
			relationshipErr, reuseErr, after, baseline)
	}
}

func reuseDescriptors(stop <-chan struct{}, done chan<- error) {
	for {
		select {
		case <-stop:
			done <- nil
			return
		default:
		}
		fd, err := unix.Open(os.DevNull, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.EMFILE) {
			runtime.Gosched()
			continue
		}
		if err != nil {
			done <- err
			return
		}
		runtime.Gosched()
		var info unix.Stat_t
		if err := unix.Fstat(fd, &info); err != nil {
			done <- fmt.Errorf("reused descriptor was closed: %w", err)
			return
		}
		if err := unix.Close(fd); err != nil {
			done <- fmt.Errorf("close reused descriptor: %w", err)
			return
		}
	}
}

func openDescriptorSnapshot(t *testing.T) (int, int) {
	t.Helper()
	var limit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	ceiling := int(limit.Cur)
	if ceiling > 4096 {
		ceiling = 4096
	}
	highest := -1
	count := 0
	for fd := range ceiling {
		_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if errors.Is(err, unix.EBADF) {
			continue
		}
		if err != nil {
			t.Fatalf("inspect descriptor %d: %v", fd, err)
		}
		count++
		if fd > highest {
			highest = fd
		}
	}
	return highest, count
}

func TestRunLogsAreCreatedOnlyWhenExplicitlyEnabled(t *testing.T) {
	source, recovery, closeCapabilities := logCapabilities(t)
	defer closeCapabilities()
	id, _ := transfernumber.New(1)
	logs, err := Open(context.Background(), source, recovery, "", Interactive, []transfernumber.Number{id})
	if err != nil || logs != nil {
		t.Fatalf("disabled logs=%#v error=%v", logs, err)
	}
}

func TestRunLogsOpenCompleteModeSpecificSetAndPreserveRawBytes(t *testing.T) {
	for _, test := range []struct {
		name   string
		layout Layout
		files  []string
		writes []struct {
			stream Stream
			name   string
		}
	}{
		{name: "interactive", layout: Interactive,
			files: []string{"transfer-01.ptylog", "transfer-02.ptylog"},
			writes: []struct {
				stream Stream
				name   string
			}{{PTY, "transfer-01.ptylog"}}},
		{name: "plain", layout: Plain,
			files: []string{"transfer-01.stdout.log", "transfer-01.stderr.log", "transfer-02.stdout.log", "transfer-02.stderr.log"},
			writes: []struct {
				stream Stream
				name   string
			}{{Stdout, "transfer-01.stdout.log"}, {Stderr, "transfer-01.stderr.log"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, recovery, closeCapabilities := logCapabilities(t)
			defer closeCapabilities()
			first, _ := transfernumber.New(1)
			second, _ := transfernumber.New(2)
			directory := filepath.Join(canonicalLogTemp(t), "logs")
			logs, err := Open(context.Background(), source, recovery, directory, test.layout,
				[]transfernumber.Number{first, second})
			if err != nil {
				t.Fatal(err)
			}
			raw := []byte("\x00\x1b[31m10%\r90%\xff\n")
			for _, write := range test.writes {
				if err := logs.Write(first, write.stream, raw); err != nil {
					t.Fatal(err)
				}
			}
			if err := logs.Close(); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != len(test.files) {
				t.Fatalf("entries=%v error=%v", entries, err)
			}
			for _, name := range test.files {
				if _, err := os.Stat(filepath.Join(directory, name)); err != nil {
					t.Fatalf("missing %s: %v", name, err)
				}
			}
			for _, write := range test.writes {
				content, err := os.ReadFile(filepath.Join(directory, write.name))
				if err != nil || !bytes.Equal(content, raw) {
					t.Fatalf("%s=%q error=%v", write.name, content, err)
				}
			}
		})
	}
}

func TestRunLogsRejectSourceAndRecoveryRelationsBeforeCreation(t *testing.T) {
	root := canonicalLogTemp(t)
	sourcePath := filepath.Join(root, "source")
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := sourcefiles.OpenSourceRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	owner, recoveryPath, err := rundirectory.CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := owner.OpenRecoveryRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := recovery.Close(); err != nil {
			t.Error(err)
		}
		if err := owner.ClosePrivateAuthority(context.Background()); err != nil {
			t.Error(err)
		}
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	}()
	id, _ := transfernumber.New(1)
	for name, path := range map[string]string{
		"source":          sourcePath,
		"source future":   filepath.Join(sourcePath, "future", "logs"),
		"recovery":        recoveryPath,
		"recovery future": filepath.Join(recoveryPath, "future", "logs"),
	} {
		t.Run(name, func(t *testing.T) {
			if logs, err := Open(context.Background(), source, recovery, path, Interactive,
				[]transfernumber.Number{id}); err == nil {
				_ = logs.Close()
				t.Fatalf("protected path %s was accepted", path)
			}
		})
	}
	for _, path := range []string{filepath.Join(sourcePath, "future"), filepath.Join(recoveryPath, "future")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("protected path was created at %s: %v", path, err)
		}
	}
	for name, path := range map[string]string{
		"source sibling":  filepath.Join(root, "logs"),
		"source ancestor": root,
	} {
		t.Run(name, func(t *testing.T) {
			logs, err := Open(context.Background(), source, recovery, path, Interactive,
				[]transfernumber.Number{id})
			if err != nil {
				t.Fatal(err)
			}
			if err := logs.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunLogsPreserveFilesCreatedBeforeALaterOpenFailure(t *testing.T) {
	source, recovery, closeCapabilities := logCapabilities(t)
	defer closeCapabilities()
	first, _ := transfernumber.New(1)
	directory := filepath.Join(canonicalLogTemp(t), "logs")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	blocked := filepath.Join(directory, "transfer-01.stderr.log")
	if err := os.WriteFile(blocked, []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if logs, err := Open(context.Background(), source, recovery, directory, Plain,
		[]transfernumber.Number{first}); err == nil {
		_ = logs.Close()
		t.Fatal("existing destination was replaced")
	}
	if content, err := os.ReadFile(blocked); err != nil || string(content) != "foreign" {
		t.Fatalf("foreign file=%q error=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "transfer-01.stdout.log")); err != nil {
		t.Fatalf("already generated log was removed: %v", err)
	}
}

func TestRunLogsReportCloseFailureAfterFinalWithoutRemovingEvidence(t *testing.T) {
	source, recovery, closeCapabilities := logCapabilities(t)
	defer closeCapabilities()
	id, _ := transfernumber.New(1)
	directory := filepath.Join(canonicalLogTemp(t), "logs")
	logs, err := Open(context.Background(), source, recovery, directory, Interactive,
		[]transfernumber.Number{id})
	if err != nil {
		t.Fatal(err)
	}
	if err := logs.Write(id, PTY, []byte("complete\n")); err != nil {
		t.Fatal(err)
	}
	if err := logs.files[logKey{transfer: id, stream: PTY}].file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := logs.Close(); err == nil {
		t.Fatal("raw log close failure was hidden")
	}
	content, err := os.ReadFile(filepath.Join(directory, "transfer-01.ptylog"))
	if err != nil || string(content) != "complete\n" {
		t.Fatalf("close failure removed evidence: content=%q error=%v", content, err)
	}
}

func TestRunLogsRejectSymlinkAliasesWithoutTouchingTheirTargets(t *testing.T) {
	root := canonicalLogTemp(t)
	sourcePath := filepath.Join(root, "source")
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := sourcefiles.OpenSourceRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	owner, recoveryPath, err := rundirectory.CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := owner.OpenRecoveryRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = recovery.Close()
		_ = owner.ClosePrivateAuthority(context.Background())
		_ = source.Close()
	}()
	id, _ := transfernumber.New(1)
	for name, target := range map[string]string{"source": sourcePath, "recovery": recoveryPath} {
		t.Run(name, func(t *testing.T) {
			alias := filepath.Join(root, name+"-alias")
			if err := os.Symlink(target, alias); err != nil {
				t.Fatal(err)
			}
			for _, candidate := range []string{alias, filepath.Join(alias, "future", "logs")} {
				if logs, err := Open(context.Background(), source, recovery, candidate, Interactive,
					[]transfernumber.Number{id}); err == nil {
					_ = logs.Close()
					t.Fatalf("symlink candidate %s was accepted", candidate)
				}
			}
		})
	}
	for _, target := range []string{sourcePath, recoveryPath} {
		entries, err := os.ReadDir(target)
		if err != nil || len(entries) != 0 {
			t.Fatalf("protected target %s changed: entries=%v error=%v", target, entries, err)
		}
	}
}

func TestRunLogsStayDescriptorBoundDuringAncestorReplacement(t *testing.T) {
	root := canonicalLogTemp(t)
	pivot := filepath.Join(root, "pivot")
	parked := filepath.Join(root, "parked")
	sourcePath := filepath.Join(root, "source")
	if err := os.Mkdir(pivot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := sourcefiles.OpenSourceRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := rundirectory.CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := owner.OpenRecoveryRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = recovery.Close()
		_ = owner.ClosePrivateAuthority(context.Background())
		_ = source.Close()
	}()
	var stop atomic.Bool
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started)
		for !stop.Load() {
			if os.Rename(pivot, parked) != nil {
				continue
			}
			if os.Symlink(sourcePath, pivot) == nil {
				_ = os.Remove(pivot)
			}
			_ = os.Rename(parked, pivot)
		}
	}()
	<-started
	id, _ := transfernumber.New(1)
	for index := range 64 {
		logs, err := Open(context.Background(), source, recovery,
			filepath.Join(pivot, fmt.Sprintf("logs-%02d", index)), Interactive,
			[]transfernumber.Number{id})
		if err == nil {
			if err := logs.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
	stop.Store(true)
	<-done
	entries, err := os.ReadDir(sourcePath)
	if err != nil || len(entries) != 0 {
		t.Fatalf("replacement source entries=%v error=%v", entries, err)
	}
}

func logCapabilities(t *testing.T) (*sourcefiles.SourceRoot, *rundirectory.RecoveryRoot, func()) {
	t.Helper()
	root := canonicalLogTemp(t)
	sourcePath := filepath.Join(root, "source")
	if err := os.Mkdir(sourcePath, 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := sourcefiles.OpenSourceRoot(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	owner, _, err := rundirectory.CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := owner.OpenRecoveryRoot()
	if err != nil {
		t.Fatal(err)
	}
	return source, recovery, func() {
		if err := recovery.Close(); err != nil {
			t.Error(err)
		}
		if err := owner.ClosePrivateAuthority(context.Background()); err != nil {
			t.Error(err)
		}
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	}
}

func canonicalLogTemp(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
