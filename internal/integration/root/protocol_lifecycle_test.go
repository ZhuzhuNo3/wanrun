//go:build linux && rootintegration && protocolacceptance

package root_test

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

type liveProtocolBehavior uint8

const (
	liveProtocolFailure liveProtocolBehavior = iota + 1
	liveProtocolCancellation
)

type protocolAcceptanceBase struct {
	name      string
	arguments []string
	selected  []netip.Addr
	observed  func() []netip.Addr
}

func newProtocolAcceptanceBase(fixture *hostNetworkFixture, scenario supervisorRootScenario,
	name string,
) protocolAcceptanceBase {
	arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv),
		"--log-dir", filepath.Join(scenario.markers, name+"-transfer-logs")}
	for _, source := range fixture.sources {
		arguments = append(arguments, "--network", source.String())
	}
	return protocolAcceptanceBase{name: name, arguments: arguments,
		selected: append([]netip.Addr(nil), fixture.sources...)}
}

func (acceptance protocolAcceptanceBase) assertSiblingCompletion(t *testing.T,
	completed []netip.Addr,
) {
	t.Helper()
	want := append([]netip.Addr(nil), acceptance.selected[1:]...)
	slices.SortFunc(completed, func(a, b netip.Addr) int { return a.Compare(b) })
	slices.SortFunc(want, func(a, b netip.Addr) int { return a.Compare(b) })
	if !slices.Equal(completed, want) {
		t.Fatalf("%s completed sources=%v, want surviving siblings %v", acceptance.name, completed, want)
	}
}

func (acceptance protocolAcceptanceBase) assertCancellation(t *testing.T,
	cancelled []netip.Addr,
) {
	t.Helper()
	want := append([]netip.Addr(nil), acceptance.selected...)
	slices.SortFunc(cancelled, func(a, b netip.Addr) int { return a.Compare(b) })
	slices.SortFunc(want, func(a, b netip.Addr) int { return a.Compare(b) })
	if !slices.Equal(cancelled, want) {
		t.Fatalf("%s cancelled active sources=%v, want %v", acceptance.name, cancelled, want)
	}
}

type rsyncAcceptance struct {
	protocolAcceptanceBase
	gate *rsyncProtocolGate
}

func newRsyncAcceptance(t *testing.T, fixture *hostNetworkFixture,
	scenario supervisorRootScenario, behavior liveProtocolBehavior,
) *rsyncAcceptance {
	t.Helper()
	acceptance := &rsyncAcceptance{
		protocolAcceptanceBase: newProtocolAcceptanceBase(fixture, scenario, "rsync"),
	}
	failing := netip.Addr{}
	if behavior == liveProtocolFailure {
		failing = fixture.sources[0]
	}
	server, gate := startGatedSSHServer(t, fixture, failing)
	destination := filepath.Join(scenario.markers, "ssh-destination")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	acceptance.gate = gate
	acceptance.observed = server.observed.snapshot
	acceptance.arguments = append(acceptance.arguments, "--", "/usr/bin/rsync", "-a",
		"--itemize-changes", "--out-format=TRANSFERLANES_FILE:%n", "-e", server.clientShell(), "{}",
		"root@"+fixture.remote.String()+":"+destination+"/")
	return acceptance
}

func (acceptance *rsyncAcceptance) waitUntilActive(ctx context.Context) error {
	return acceptance.gate.WaitUntilActive(ctx)
}

func (acceptance *rsyncAcceptance) release() { acceptance.gate.Release() }

func (acceptance *rsyncAcceptance) assertStopped(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := acceptance.gate.WaitUntilStopped(ctx); err != nil {
		t.Fatalf("rsync protocol activity remained: %v", err)
	}
}

func (acceptance *rsyncAcceptance) assertSiblingCompletion(t *testing.T) {
	acceptance.protocolAcceptanceBase.assertSiblingCompletion(t, acceptance.gate.CompletedSources())
}

func (acceptance *rsyncAcceptance) assertCancellation(t *testing.T) {
	acceptance.protocolAcceptanceBase.assertCancellation(t, acceptance.gate.CancelledSources())
}

func (acceptance *rsyncAcceptance) facts() protocolAcceptanceBase {
	return acceptance.protocolAcceptanceBase
}

type s3Acceptance struct {
	protocolAcceptanceBase
	server *s3Server
}

func newS3Acceptance(t *testing.T, fixture *hostNetworkFixture,
	scenario supervisorRootScenario, behavior liveProtocolBehavior,
) *s3Acceptance {
	t.Helper()
	acceptance := &s3Acceptance{
		protocolAcceptanceBase: newProtocolAcceptanceBase(fixture, scenario, "s3"),
	}
	failing := netip.Addr{}
	if behavior == liveProtocolFailure {
		failing = fixture.sources[0]
	}
	acceptance.server = startGatedS3Server(t, fixture, failing)
	acceptance.observed = acceptance.server.observed.snapshot
	for _, variable := range awsEnvironment() {
		name, value, _ := strings.Cut(variable, "=")
		t.Setenv(name, value)
	}
	acceptance.arguments = append(acceptance.arguments, "--", "/usr/local/bin/aws",
		"--endpoint-url", acceptance.server.endpoint, "s3", "cp", "--recursive",
		"--no-progress", "--only-show-errors", "{}", "s3://"+acceptance.server.bucket+"/run/")
	return acceptance
}

func (acceptance *s3Acceptance) waitUntilActive(ctx context.Context) error {
	return acceptance.server.gate.WaitUntilActive(ctx)
}

func (acceptance *s3Acceptance) release() { acceptance.server.gate.Release() }

func (acceptance *s3Acceptance) assertStopped(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := acceptance.server.gate.WaitUntilStopped(ctx); err != nil {
		t.Fatalf("s3 protocol activity remained: %v", err)
	}
}

func (acceptance *s3Acceptance) assertSiblingCompletion(t *testing.T) {
	acceptance.protocolAcceptanceBase.assertSiblingCompletion(t, acceptance.server.gate.CompletedSources())
}

func (acceptance *s3Acceptance) assertCancellation(t *testing.T) {
	acceptance.protocolAcceptanceBase.assertCancellation(t, acceptance.server.gate.CancelledSources())
}

func (acceptance *s3Acceptance) facts() protocolAcceptanceBase {
	return acceptance.protocolAcceptanceBase
}

type protocolLifecycleAcceptance interface {
	facts() protocolAcceptanceBase
	waitUntilActive(context.Context) error
	release()
	assertStopped(*testing.T)
	assertSiblingCompletion(*testing.T)
	assertCancellation(*testing.T)
}
