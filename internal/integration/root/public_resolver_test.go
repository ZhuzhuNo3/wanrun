//go:build linux && rootintegration

package root_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type resolverSymlinkFixture struct {
	host             *hostNetworkFixture
	baselineSurface  string
	hostContent      string
	childHostContent string
	linkBefore       os.FileInfo
	targetBefore     os.FileInfo
}

func newResolverSymlinkFixture(t *testing.T) *resolverSymlinkFixture {
	t.Helper()
	host := newHostNetworkFixture(t)
	host.install()
	fixture := &resolverSymlinkFixture{
		host:            host,
		baselineSurface: host.hostSurface(),
		hostContent: "nameserver 198.51.100.53\nsearch svc.transferlanes.test\n" +
			"options attempts:1 timeout:2 rotate use-vc ndots:2 single-request no-tld-query edns0\n",
		childHostContent: "nameserver 127.0.0.53\nsearch svc.transferlanes.test\n" +
			"options ndots:2 single-request no-tld-query edns0\n",
	}

	runtime.LockOSThread()
	originalMount, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		runtime.UnlockOSThread()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Setns(int(originalMount.Fd()), unix.CLONE_NEWNS); err != nil {
			t.Errorf("restore resolver test mount namespace: %v", err)
		}
		if err := originalMount.Close(); err != nil {
			t.Errorf("close original resolver mount namespace: %v", err)
		}
		runtime.UnlockOSThread()
	})
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		t.Fatalf("create resolver test mount namespace: %v", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		t.Fatal(err)
	}
	resolverRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(resolverRoot, "resolver-target"), []byte(fixture.hostContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("resolver-target", filepath.Join(resolverRoot, "resolv.conf")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mount(resolverRoot, "/etc", "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := unix.Unmount("/etc", unix.MNT_DETACH); err != nil {
			t.Errorf("unmount resolver fixture: %v", err)
		}
	})
	fixture.linkBefore, err = os.Lstat("/etc/resolv.conf")
	if err != nil || fixture.linkBefore.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("resolver link mode=%v error=%v", fixture.linkBefore, err)
	}
	fixture.targetBefore, err = os.Stat("/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *resolverSymlinkFixture) assertHostUnchanged(t *testing.T) {
	t.Helper()
	linkAfter, err := os.Lstat("/etc/resolv.conf")
	if err != nil || linkAfter.Mode()&os.ModeSymlink == 0 || !os.SameFile(fixture.linkBefore, linkAfter) {
		t.Fatalf("host resolver symlink changed: before=%v after=%v error=%v",
			fixture.linkBefore, linkAfter, err)
	}
	targetAfter, err := os.Stat("/etc/resolv.conf")
	current, readErr := os.ReadFile("/etc/resolv.conf")
	if err != nil || readErr != nil || !os.SameFile(fixture.targetBefore, targetAfter) ||
		string(current) != fixture.hostContent {
		t.Fatalf("host resolver target changed: content=%q stat=%v read=%v", current, err, readErr)
	}
}

func TestPublicSupervisedResolverSymlinkSurvivesSuccess(t *testing.T) {
	fixture := newResolverSymlinkFixture(t)
	runsBefore := authorityEntries(t)
	scenario := fixture.host.newRunSupervisorScenario(t, "resolver-symlink-success")
	marker := filepath.Base(scenario.markers)
	t.Setenv("TRANSFERLANES_RESOLVER_CHILD", marker)
	evidence := filepath.Join(scenario.markers, "resolver.jsonl")
	ready := filepath.Join(scenario.markers, "resolver")
	arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv)}
	for _, source := range fixture.host.sources {
		arguments = append(arguments, "--network", source.String())
	}
	arguments = append(arguments, "--")
	arguments = append(arguments, transferChildCommand(os.Args[0], transferChildResolver,
		evidence, ready, "finish", "{}")...)

	output, runErr := runPublicTransferLanesPTY(t, arguments, nil)
	if runErr != nil {
		t.Fatalf("resolver run failed: %v output=%q", runErr, output)
	}
	assertResolverEvidence(t, evidence, len(fixture.host.sources), fixture.childHostContent)
	fixture.assertHostUnchanged(t)
	assertNoMarkedProcesses(t, "TRANSFERLANES_RESOLVER_CHILD="+marker)
	fixture.host.assertSurface(fixture.baselineSurface)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("resolver run left roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicSupervisedResolverSymlinkSurvivesCancellation(t *testing.T) {
	fixture := newResolverSymlinkFixture(t)
	runsBefore := authorityEntries(t)
	scenario := fixture.host.newRunSupervisorScenario(t, "resolver-symlink-cancellation")
	marker := filepath.Base(scenario.markers)
	t.Setenv("TRANSFERLANES_RESOLVER_CHILD", marker)
	evidence := filepath.Join(scenario.markers, "resolver.jsonl")
	ready := filepath.Join(scenario.markers, "resolver")
	arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv),
		"--dns", "192.0.2.53", "--dns", "2001:db8::53"}
	for _, source := range fixture.host.sources {
		arguments = append(arguments, "--network", source.String())
	}
	arguments = append(arguments, "--")
	arguments = append(arguments, transferChildCommand(os.Args[0], transferChildResolver,
		evidence, ready, "block", "{}")...)

	output, runErr := runPublicTransferLanesPTY(t, arguments,
		func(_ context.Context, _ *exec.Cmd, master *os.File, _ *boundedPTYCapture) error {
			if err := waitForGlobCount(ready+".*.ready", len(fixture.host.sources), 10*time.Second); err != nil {
				return err
			}
			_, err := master.Write([]byte{0x03})
			return err
		})
	var exit *exec.ExitError
	if !errors.As(runErr, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("resolver cancel error=%v output=%q", runErr, output)
	}
	assertResolverEvidence(t, evidence, len(fixture.host.sources),
		"nameserver 192.0.2.53\nnameserver 2001:db8::53\n")
	fixture.assertHostUnchanged(t)
	assertNoMarkedProcesses(t, "TRANSFERLANES_RESOLVER_CHILD="+marker)
	fixture.host.assertSurface(fixture.baselineSurface)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("resolver cancellation left roots: before=%v after=%v", runsBefore, after)
	}
}

func assertResolverEvidence(t *testing.T, path string, count int, want string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(lines) != count {
		t.Fatalf("resolver evidence count=%d want=%d content=%q", len(lines), count, content)
	}
	for _, encoded := range lines {
		var evidence transferResolverEvidence
		if err := json.Unmarshal([]byte(encoded), &evidence); err != nil ||
			evidence.Content != want || !evidence.ReadOnly {
			t.Fatalf("resolver evidence=%+v error=%v", evidence, err)
		}
	}
}
