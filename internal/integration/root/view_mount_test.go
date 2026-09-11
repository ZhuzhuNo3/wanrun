//go:build linux && rootintegration

package root_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"golang.org/x/sys/unix"
)

func TestDescriptorBoundViewMountSurvivesClosedDescriptorsAndReplacement(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "transfer-rsync-replacement")
	var exactRunRoot string
	observed := scenario.runAfterLaunch(t, func(client *runsupervisor.RunSupervisorClient) error {
		id, err := waitForReadyRunID(filepath.Join(scenario.markers, "rsync.*.ready"),
			len(fixture.sources), 10*time.Second)
		if err != nil {
			_ = client.Cancel(runsupervisor.CancelInternal)
			return err
		}
		exactRunRoot = filepath.Join(rundirectory.AuthorityRoot, id.String())
		err = replaceExactPublishedViewsDuringRsync(exactRunRoot, scenario.markers, len(fixture.sources))
		if err != nil {
			_ = client.Cancel(runsupervisor.CancelInternal)
		}
		return err
	})
	if observed.final.Cancelled || observed.final.InternalError != "" ||
		len(observed.final.Transfers) != len(fixture.sources) {
		t.Fatalf("replacement rsync final = %#v", observed.final)
	}
	assertRsyncViewCopies(t, scenario.markers, len(fixture.sources))
	if _, err := os.Stat(exactRunRoot); !os.IsNotExist(err) {
		t.Fatalf("replacement rsync run remains: %v", err)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("replacement rsync left run roots: before=%v after=%v", runsBefore, after)
	}
}

func runSupervisorTransferRsyncChild(argv []string) int {
	if (len(argv) != 3 && len(argv) != 4) || !validPrivateViewAlias(argv[2]) {
		return 2
	}
	if len(argv) == 4 {
		inherited, err := hasDirectoryDescriptor(argv[3])
		if err != nil || inherited {
			return 10
		}
	}
	id, err := privateViewRunID(argv[2])
	if err != nil {
		return 2
	}
	pid := strconv.Itoa(os.Getpid())
	if err := writeExclusiveTransferGateMarker(filepath.Join(argv[0], "rsync."+pid+".ready"), id); err != nil {
		return 3
	}
	if err := waitForFileUntil(filepath.Join(argv[0], "rsync.release"), 10*time.Second); err != nil {
		return 4
	}
	destination := filepath.Join(argv[1], pid)
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return 5
	}
	if err := markNonStandardDescriptorsCloseOnExec(); err != nil {
		_ = os.WriteFile(filepath.Join(argv[0], "rsync."+pid+".error"), []byte(err.Error()), 0o600)
		return 6
	}
	command := exec.Command("/usr/bin/rsync", "-a", argv[2], destination+string(filepath.Separator))
	if output, err := command.CombinedOutput(); err != nil {
		_ = os.WriteFile(filepath.Join(argv[0], "rsync."+pid+".error"), output, 0o600)
		return 7
	}
	if err := os.WriteFile(filepath.Join(argv[0], "rsync."+pid+".copied"), []byte("copied\n"), 0o600); err != nil {
		return 8
	}
	if err := waitForFileUntil(filepath.Join(argv[0], "rsync.finish"), 10*time.Second); err != nil {
		return 9
	}
	return 0
}

func hasDirectoryDescriptor(path string) (bool, error) {
	want, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil || fd < 3 {
			continue
		}
		got, err := os.Stat(filepath.Join("/proc/self/fd", entry.Name()))
		if err == nil && os.SameFile(want, got) {
			return true, nil
		}
	}
	return false, nil
}

func markNonStandardDescriptorsCloseOnExec() error {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			return err
		}
		if fd >= 3 {
			flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
			if err == nil {
				_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC)
			}
			if err != nil && err != unix.EBADF {
				return err
			}
		}
	}
	return nil
}

func replaceExactPublishedViewsDuringRsync(runRoot, markers string, transferCount int) (result error) {
	views := filepath.Join(runRoot, "views")
	original := filepath.Join(runRoot, ".rsync-original-views")
	if err := os.Rename(views, original); err != nil {
		return err
	}
	restored := false
	defer func() {
		if !restored {
			_ = os.RemoveAll(views)
			_ = os.Rename(original, views)
		}
		_ = os.WriteFile(filepath.Join(markers, "rsync.release"), []byte("release\n"), 0o600)
		_ = os.WriteFile(filepath.Join(markers, "rsync.finish"), []byte("finish\n"), 0o600)
	}()
	if err := os.Mkdir(views, 0o700); err != nil {
		return err
	}
	for number := 1; number <= transferCount; number++ {
		replacement := filepath.Join(views, fmt.Sprintf("transfer-%03d", number),
			filepath.Base(os.Getenv(supervisorTransferEnv)))
		if err := os.MkdirAll(replacement, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(replacement, "evil"), []byte("replacement\n"), 0o600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(markers, "rsync.release"), []byte("release\n"), 0o600); err != nil {
		return err
	}
	if err := waitForGlobCount(filepath.Join(markers, "rsync.*.copied"), transferCount, 10*time.Second); err != nil {
		paths, _ := filepath.Glob(filepath.Join(markers, "rsync.*.error"))
		var diagnostics []string
		for _, path := range paths {
			contents, readErr := os.ReadFile(path)
			if readErr == nil {
				diagnostics = append(diagnostics, string(contents))
			}
		}
		return fmt.Errorf("%w: %s", err, strings.Join(diagnostics, "; "))
	}
	if err := os.RemoveAll(views); err != nil {
		return err
	}
	if err := os.Rename(original, views); err != nil {
		return err
	}
	restored = true
	return os.WriteFile(filepath.Join(markers, "rsync.finish"), []byte("finish\n"), 0o600)
}

func assertRsyncViewCopies(t *testing.T, markers string, transferCount int) {
	t.Helper()
	baseName := filepath.Base(os.Getenv(supervisorTransferEnv))
	parents, err := filepath.Glob(filepath.Join(markers, "rsync-destination", "*", baseName))
	if err != nil || len(parents) != transferCount {
		t.Fatalf("rsync basename copies = %v, %v", parents, err)
	}
	seen := make(map[string]int)
	for _, parent := range parents {
		if filepath.Base(parent) != baseName {
			t.Fatalf("rsync lost source basename in %q", parent)
		}
		if err := filepath.WalkDir(parent, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || path == parent || entry.IsDir() {
				return walkErr
			}
			relative, err := filepath.Rel(parent, path)
			if err != nil {
				return err
			}
			seen[relative]++
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".hidden", "alpha", "bravo", "charlie", "nested/echo", "nested/foxtrot"} {
		if seen[name] != 1 {
			t.Errorf("rsync source member %q appeared %d times", name, seen[name])
		}
	}
	if seen["evil"] != 0 {
		t.Fatal("rsync followed replacement file views")
	}
}
