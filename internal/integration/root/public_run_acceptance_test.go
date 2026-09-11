//go:build linux && rootintegration && protocolacceptance

package root_test

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	acceptanceMeasureDuration = 250 * time.Millisecond
	acceptanceDNSDelay        = 400 * time.Millisecond
)

func TestPublicHostnameAutoWeightRsyncAcrossThreeSourcesOnOneProvider(t *testing.T) {
	for _, mode := range protocolSourceModes() {
		t.Run(mode.name, func(t *testing.T) {
			testPublicHostnameAutoWeightRsync(t, mode)
		})
	}
}

func testPublicHostnameAutoWeightRsync(t *testing.T, mode protocolSourceMode) {
	fixture := newSingleProviderFixture(t)
	fixture.install()
	runsBefore := authorityEntries(t)
	drainSourceObservations(fixture.measured)
	hostname := fmt.Sprintf("%s-rsync.transferlanes.test.", fixture.prefix)
	resolver := startDelayedHostLocalResolver(t, hostname, fixture.remote, acceptanceDNSDelay)
	resolver.installHostConfiguration(t)
	scenario := fixture.newRunSupervisorScenario(t, "hostname-auto-weight-rsync")
	sourceTree := createProtocolSourceTree(t, scenario)
	receiver := startRsyncSSHReceiver(t, fixture)
	marker := "rsync-acceptance-" + filepath.Base(scenario.markers)
	t.Setenv("TRANSFERLANES_PROTOCOL_RUN", marker)
	baseline := captureHostState(t, fixture, "TRANSFERLANES_PROTOCOL_RUN="+marker)

	arguments := hostnameAutoWeightArguments(fixture, hostname)
	if mode.followSymlinks {
		arguments = append(arguments, "--follow-symlinks")
	}
	childArguments := []string{"/usr/bin/rsync", "-a", "-e", receiver.server.clientShell(),
		"{}", "root@" + strings.TrimSuffix(hostname, ".") + ":./"}
	t.Logf("executing ordinary rsync child argv without link-following options: %q", childArguments)
	arguments = append(arguments, "--")
	arguments = append(arguments, childArguments...)
	stdout, stderr, err := runPublicTransferLanesPipesWithAction(t, arguments, func(_ *exec.Cmd) error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := receiver.gate.WaitUntilActive(ctx); err != nil {
			return err
		}
		assertThreeUserNetworkNamespaces(t, "TRANSFERLANES_PROTOCOL_RUN="+marker, "/usr/bin/rsync")
		receiver.gate.Release()
		return nil
	})
	receiver.gate.Release()
	if err != nil {
		t.Fatalf("hostname rsync acceptance failed: %v\nstdout=%s\nstderr=%s\nsshd:\n%s",
			err, stdout, stderr, receiver.server.output())
	}
	assertCompletedTransfers(t, stdout, len(fixture.sources))
	assertProtocolSourceSummary(t, stdout, sourceTree, mode)
	receiver.assertTransfer(t, os.Getenv(supervisorTransferEnv), sourceTree.entries(mode), fixture.sources)
	assertDefaultResolverTraffic(t, resolver, len(fixture.sources))
	assertCompleteMeasureWindows(t, drainSourceObservations(fixture.measured), fixture.sources,
		acceptanceMeasureDuration)
	assertNoMarkedProcesses(t, "TRANSFERLANES_PROTOCOL_RUN="+marker)
	assertHostState(t, fixture, "TRANSFERLANES_PROTOCOL_RUN="+marker, baseline)
	assertAuthorityUnchanged(t, runsBefore)
}

func TestPublicHostnameAutoWeightS3AcrossThreeProviders(t *testing.T) {
	for _, mode := range protocolSourceModes() {
		t.Run(mode.name, func(t *testing.T) {
			testPublicHostnameAutoWeightS3(t, mode)
		})
	}
}

func testPublicHostnameAutoWeightS3(t *testing.T, mode protocolSourceMode) {
	fixture := newThreeProviderFixture(t)
	fixture.install()
	runsBefore := authorityEntries(t)
	drainSourceObservations(fixture.measured)
	hostname := fmt.Sprintf("%s-s3.transferlanes.test.", fixture.prefix)
	resolver := startDelayedHostLocalResolver(t, hostname, fixture.remote, acceptanceDNSDelay)
	resolver.installHostConfiguration(t)
	scenario := fixture.newRunSupervisorScenario(t, "hostname-auto-weight-s3")
	sourceTree := createProtocolSourceTree(t, scenario)
	server := startGatedS3Server(t, fixture, netip.Addr{})
	marker := "s3-acceptance-" + filepath.Base(scenario.markers)
	t.Setenv("TRANSFERLANES_PROTOCOL_RUN", marker)
	baseline := captureHostState(t, fixture, "TRANSFERLANES_PROTOCOL_RUN="+marker)
	for _, variable := range awsEnvironment() {
		name, value, _ := strings.Cut(variable, "=")
		t.Setenv(name, value)
	}

	arguments := hostnameAutoWeightArguments(fixture, hostname)
	if mode.followSymlinks {
		arguments = append(arguments, "--follow-symlinks")
	}
	childArguments := []string{"/usr/local/bin/aws", "--endpoint-url",
		"http://" + net.JoinHostPort(strings.TrimSuffix(hostname, "."), "19000"),
		"s3", "cp", "--recursive", "--no-progress", "--only-show-errors",
		"{}", "s3://" + server.bucket + "/run/"}
	t.Logf("executing ordinary S3 child argv without link-following options: %q", childArguments)
	arguments = append(arguments, "--")
	arguments = append(arguments, childArguments...)
	stdout, stderr, err := runPublicTransferLanesPipesWithAction(t, arguments, func(_ *exec.Cmd) error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := server.gate.WaitUntilActive(ctx); err != nil {
			return err
		}
		assertThreeUserNetworkNamespaces(t, "TRANSFERLANES_PROTOCOL_RUN="+marker, "/usr/local/bin/aws")
		server.gate.Release()
		return nil
	})
	server.gate.Release()
	if err != nil {
		t.Fatalf("hostname S3 acceptance failed: %v\nstdout=%s\nstderr=%s\nMinIO:\n%s",
			err, stdout, stderr, server.logs())
	}
	assertCompletedTransfers(t, stdout, len(fixture.sources))
	assertProtocolSourceSummary(t, stdout, sourceTree, mode)
	assertS3Objects(t, server, sourceTree.entries(mode))
	assertS3UserSources(t, server, fixture.sources)
	assertDefaultResolverTraffic(t, resolver, len(fixture.sources))
	assertCompleteMeasureWindows(t, drainSourceObservations(fixture.measured), fixture.sources,
		acceptanceMeasureDuration)
	assertNoMarkedProcesses(t, "TRANSFERLANES_PROTOCOL_RUN="+marker)
	assertHostState(t, fixture, "TRANSFERLANES_PROTOCOL_RUN="+marker, baseline)
	assertAuthorityUnchanged(t, runsBefore)
}

func hostnameAutoWeightArguments(fixture *hostNetworkFixture, hostname string) []string {
	arguments := []string{"run", "--no-tui", "--source", os.Getenv(supervisorTransferEnv)}
	for _, source := range fixture.sources {
		arguments = append(arguments, "--network", source.String())
	}
	return append(arguments, "--auto-weight", "--measure-url",
		"http://"+net.JoinHostPort(strings.TrimSuffix(hostname, "."), "18081")+"/measure",
		"--measure-duration", acceptanceMeasureDuration.String())
}

func assertCompletedTransfers(t *testing.T, output string, count int) {
	t.Helper()
	for number := 1; number <= count; number++ {
		if !strings.Contains(output,
			"transfer "+strconv.Itoa(number)+" completed: exit=0 signal=0") {
			t.Errorf("noninteractive summary omits successful transfer %d: %q", number, output)
		}
	}
}

func assertDefaultResolverTraffic(t *testing.T, resolver *hostLocalResolver, transfers int) {
	t.Helper()
	if resolver.udpQueries.Load() < uint64(transfers) ||
		resolver.tcpQueries.Load() < uint64(transfers) {
		t.Fatalf("default resolver traffic UDP=%d TCP=%d, want both for %d transfers",
			resolver.udpQueries.Load(), resolver.tcpQueries.Load(), transfers)
	}
}

func assertS3UserSources(t *testing.T, server *s3Server, sources []netip.Addr) {
	t.Helper()
	seen := make(map[netip.Addr]struct{}, len(sources))
	for _, write := range server.writes.snapshot() {
		seen[write.source] = struct{}{}
	}
	for _, source := range sources {
		if _, found := seen[source]; !found {
			t.Errorf("S3 receiver recorded no successful PUT from %s; observed=%v", source, seen)
		}
	}
	t.Logf("S3 receiver verified %d successful PUTs from sources=%v",
		len(server.writes.snapshot()), seen)
}

func assertCompleteMeasureWindows(t *testing.T, observations []sourceObservation,
	sources []netip.Addr, duration time.Duration,
) {
	t.Helper()
	first := make(map[netip.Addr]time.Time, len(sources))
	last := make(map[netip.Addr]time.Time, len(sources))
	for _, observation := range observations {
		if start, found := first[observation.source]; !found || observation.at.Before(start) {
			first[observation.source] = observation.at
		}
		if observation.finished && observation.at.After(last[observation.source]) {
			last[observation.source] = observation.at
		}
	}
	minimum := duration * 4 / 5
	for _, source := range sources {
		elapsed := last[source].Sub(first[source])
		if elapsed < minimum {
			t.Errorf("source %s upload window=%s, want at least %s after delayed preparation",
				source, elapsed, minimum)
		}
		t.Logf("source %s retained upload window %s after %s DNS preparation delay",
			source, elapsed, acceptanceDNSDelay)
	}
}

func assertAuthorityUnchanged(t *testing.T, before []string) {
	t.Helper()
	if after := authorityEntries(t); !slices.Equal(after, before) {
		t.Fatalf("public acceptance left run roots: before=%v after=%v", before, after)
	}
}
