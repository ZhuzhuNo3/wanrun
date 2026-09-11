//go:build linux && rootintegration

package root_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/listnetworks"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
)

const nonRootListingArgument = "--transferlanes-root-nonroot-list"

const rootMeasureDuration = 500 * time.Millisecond

func TestExplicitHostDiagnosticsUseHostContext(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	hostEndpoint, hostSources, hostObserved, closeHostEndpoint := startHostDiagnosticEndpoint(t, fixture)
	hostBaseline := fixture.hostSurface()
	window, err := throughput.NewSettings(hostEndpoint, rootMeasureDuration)
	if err != nil {
		t.Fatal(err)
	}

	explicit := make([]string, len(hostSources))
	for index, source := range hostSources {
		explicit[index] = source.String()
	}
	request, err := listnetworks.NewRequest(explicit, false, nil, &window)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := listnetworks.New().Run(context.Background(), request)
	if err != nil || listed.RuntimeFailure() || len(listed.Rows()) != len(hostSources) {
		t.Fatalf("host list measure = %#v, %v", listed, err)
	}
	for _, row := range listed.Rows() {
		measured, found := row.Measure()
		if !found || measured.Err() != nil || measured.Mbps() < 10 || measured.Mbps() > 100 {
			t.Fatalf("rate-limited host measure for %s = %#v", row.LocalIP(), measured)
		}
	}
	assertEverySourceObserved(t, hostObserved, hostSources)
	fixture.assertSurface(hostBaseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("list touched %s: before=%v after=%v", rundirectory.AuthorityRoot, runsBefore, after)
	}
	closeHostEndpoint()
	fixture.assertSurface(baseline)
}

func TestNonRootBareListingDoesNotUseRunAuthority(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	if output := fixture.run("setpriv", "--reuid=65534", "--regid=65534", "--clear-groups",
		os.Args[0], nonRootListingArgument); output != "euid=65534\n" {
		t.Fatalf("non-root bare listing output = %q", output)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("non-root list touched run authority: before=%v after=%v", runsBefore, after)
	}
}

func TestNamespaceMeasurementUsesTemporaryRunContext(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	drainObservedSources(fixture.observed)
	drainSourceObservations(fixture.measured)
	endpoint := "http://" + net.JoinHostPort(fixture.remote.String(), "18081") + "/measure"
	scenario := fixture.newRunSupervisorScenario(t, "measure")
	t.Setenv(supervisorMeasureEnv, endpoint)
	observed := scenario.run(t, nil)
	if observed.final.Cancelled || observed.final.InternalError != "" ||
		len(observed.final.Transfers) != len(fixture.sources) {
		t.Fatalf("namespace measure final = %#v", observed.final)
	}
	for _, result := range observed.final.Transfers {
		if result.ExitCode != 0 || result.Signal != 0 {
			t.Fatalf("namespace measure transfer = %#v", result)
		}
	}
	assertEverySourceObserved(t, fixture.observed, fixture.sources)
	assertOverlappingMeasureWindow(t, drainSourceObservations(fixture.measured), fixture.sources)
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("supervisor measure left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestNamespaceMeasurementEndpointFailureReportsProtocolStage(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	failureScenario := fixture.newRunSupervisorScenario(t, "measure")
	t.Setenv(supervisorMeasureEnv,
		"http://"+net.JoinHostPort(fixture.remote.String(), "18081")+"/measure-fail")
	failed := failureScenario.run(t, nil)
	if !strings.Contains(failed.final.InternalError, "validate endpoint: measure endpoint returned status 503") ||
		strings.Contains(failed.final.InternalError, "helper failed") ||
		strings.Contains(failed.final.InternalError, "group was cancelled") {
		t.Fatalf("namespace endpoint failure was not reported once at its protocol stage: %#v", failed.final)
	}
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("failed supervisor measure left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicListCombinesExplicitDiagnosticsAndKeepsRowFailuresLocal(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	measureEndpoint, sources, observed, closeEndpoint := startHostDiagnosticEndpoint(t, fixture)
	endpointBase := strings.TrimSuffix(measureEndpoint, "/measure")
	hostBaseline := fixture.hostSurface()
	nonlocal := "198.51.100.99"
	output := runNonRootPublicDiagnostics(t, endpointBase, sources, nonlocal)
	assertPublicDiagnosticTable(t, output, sources, nonlocal)
	assertEverySourceObserved(t, observed, sources)
	fixture.assertSurface(hostBaseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("public diagnostics touched %s: before=%v after=%v", rundirectory.AuthorityRoot,
			runsBefore, after)
	}
	closeEndpoint()
	fixture.assertSurface(baseline)
}

func runNonRootPublicDiagnostics(t *testing.T, endpointBase string,
	sources []netip.Addr, nonlocal string,
) string {
	t.Helper()
	arguments := []string{"--reuid=65534", "--regid=65534", "--clear-groups",
		os.Getenv("TRANSFERLANES_TEST_TRANSFERLANES"), "list",
		"--network", sources[1].String(), "--network", nonlocal,
		"--network", sources[2].String(), "--network", sources[0].String(),
		"--probe", "--probe-url", endpointBase + "/probe",
		"--measure", "--measure-url", endpointBase + "/measure-selective-fail",
		"--measure-duration", rootMeasureDuration.String()}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "setpriv", arguments...).CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("public diagnostic exit=%v, want 1:\n%s", err, output)
	}
	return string(output)
}

func assertPublicDiagnosticTable(t *testing.T, output string, sources []netip.Addr, nonlocal string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	wantHeader := []string{"LOCAL_IP", "IFACE", "ROUTE_TABLE", "GATEWAY", "LOCAL_STATUS",
		"PUBLIC_IP", "ASN", "ORGANIZATION", "PROBE_STATUS", "MBPS", "WEIGHT", "MEASURE_STATUS"}
	if len(lines) != 5 || !slices.Equal(strings.Fields(lines[0]), wantHeader) {
		t.Fatalf("public diagnostic table shape:\n%s", output)
	}
	wantOrder := []string{sources[1].String(), nonlocal, sources[2].String(), sources[0].String()}
	for index, localIP := range wantOrder {
		if !strings.HasPrefix(lines[index+1], localIP+" ") {
			t.Fatalf("row %d=%q, want %s first", index+1, lines[index+1], localIP)
		}
	}
	assertSuccessfulPublicDiagnosticRow(t, lines[1], "203.0.113.11", "Example-2")
	assertSuccessfulPublicDiagnosticRow(t, lines[4], "203.0.113.10", "Example-1")
	if !strings.Contains(lines[2], "not a local address") || strings.Contains(lines[2], "endpoint") {
		t.Fatalf("non-runnable row=%q", lines[2])
	}
	if strings.Count(lines[3], "endpoint returned status 503") != 2 {
		t.Fatalf("per-network failures were not kept in their diagnostic columns: %q", lines[3])
	}
	if strings.Contains(output, "unknown") {
		t.Fatalf("public diagnostic table contains synthetic unknown value:\n%s", output)
	}
}

func assertSuccessfulPublicDiagnosticRow(t *testing.T, row, publicIP, organization string) {
	t.Helper()
	fields := strings.Fields(row)
	if len(fields) != 12 || fields[4] != "runnable" || fields[5] != publicIP ||
		fields[6] != "64512" || fields[7] != organization || fields[8] != "ok" ||
		strings.Contains(fields[10], "/") || fields[11] != "ok" {
		t.Fatalf("successful diagnostic row=%q fields=%v", row, fields)
	}
	if weight, err := strconv.ParseUint(fields[10], 10, 64); err != nil || weight == 0 {
		t.Fatalf("successful diagnostic weight=%q error=%v", fields[10], err)
	}
}

func TestTransferDirectoryAutomaticLifecycle(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	drainObservedSources(fixture.observed)
	drainSourceObservations(fixture.measured)

	endpoint := "http://" + net.JoinHostPort(fixture.remote.String(), "18081") + "/measure"
	scenario := fixture.newRunSupervisorScenario(t, "transfer-auto")
	t.Setenv(supervisorMeasureEnv, endpoint)
	observed := scenario.run(t, nil)
	if observed.final.Cancelled || observed.final.InternalError != "" ||
		len(observed.final.Transfers) != len(fixture.sources) {
		t.Fatalf("automatic transfer final = %#v", observed.final)
	}
	for _, result := range observed.final.Transfers {
		if result.ExitCode != 0 || result.Signal != 0 {
			t.Fatalf("automatic transfer result = %#v", result)
		}
	}
	assertEverySourceObserved(t, fixture.observed, fixture.sources)
	assertOverlappingMeasureWindow(t, drainSourceObservations(fixture.measured), fixture.sources)
	assertTransferViewEvidence(t, scenario.markers, len(fixture.sources))
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("automatic transfer left run roots: before=%v after=%v", runsBefore, after)
	}
}

func maybeRunNonRootListing(argv []string) (bool, int) {
	if len(argv) != 1 || argv[0] != nonRootListingArgument {
		return false, 0
	}
	request, err := listnetworks.NewRequest(nil, false, nil, nil)
	if err == nil {
		_, err = listnetworks.New().Run(context.Background(), request)
	}
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return true, 1
	}
	_, _ = fmt.Printf("euid=%d\n", os.Geteuid())
	return true, 0
}

func startHostDiagnosticEndpoint(t *testing.T, fixture *hostNetworkFixture) (
	string, []netip.Addr, <-chan netip.Addr, func()) {
	t.Helper()
	sequence := int(labSequence.Add(1) & 0xff)
	name := fmt.Sprintf("ld%04x", sequence)
	table := 40000 + sequence
	priority := 14000 + sequence*4
	subnet := fmt.Sprintf("10.210.%d", sequence)
	sources := []netip.Addr{netip.MustParseAddr(subnet + ".2"), netip.MustParseAddr(subnet + ".3"),
		netip.MustParseAddr(subnet + ".4")}
	remote := netip.MustParseAddr(subnet + ".200")
	fixture.run("ip", "link", "add", name, "type", "dummy")
	fixture.run("ip", "link", "set", name, "up")
	for _, address := range append(append([]netip.Addr(nil), sources...), remote) {
		fixture.run("ip", "addr", "add", address.String()+"/24", "dev", name)
	}
	fixture.run("ip", "route", "add", "table", strconv.Itoa(table), "default", "dev", name)
	for index, source := range sources {
		fixture.run("ip", "rule", "add", "priority", strconv.Itoa(priority+index),
			"from", source.String()+"/32", "table", strconv.Itoa(table))
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(remote.String(), "18083"))
	if err != nil {
		t.Fatal(err)
	}
	observed := make(chan netip.Addr, 128)
	limiters := make(map[netip.Addr]*rootPacedUpload, len(sources))
	for _, source := range sources {
		limiters[source] = &rootPacedUpload{bitsPerSecond: 100_000_000}
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		host, _, _ := net.SplitHostPort(request.RemoteAddr)
		source, parseErr := netip.ParseAddr(host)
		if parseErr == nil {
			observed <- source
		}
		if request.Method == http.MethodGet && request.URL.Path == "/probe" {
			index := slices.Index(sources, source)
			if index < 0 || index == 2 {
				http.Error(writer, "controlled probe failure", http.StatusServiceUnavailable)
				return
			}
			_, _ = fmt.Fprintf(writer,
				`{"ip":"203.0.113.%d","success":true,"connection":{"asn":64512,"org":"Example-%d"}}`,
				index+10, index+1)
			return
		}
		time.Sleep(20 * time.Millisecond)
		if limiter := limiters[source]; limiter != nil {
			if consumeErr := limiter.consume(request.Context(), request.Body); consumeErr != nil {
				return
			}
		} else {
			_, _ = io.Copy(io.Discard, request.Body)
		}
		if request.URL.Path == "/measure-selective-fail" && source == sources[2] {
			http.Error(writer, "controlled measure failure", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(writer, "ok")
	})}
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		_ = server.Serve(listener)
	}()
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			_ = server.Close()
			<-serverDone
			for index := len(sources) - 1; index >= 0; index-- {
				fixture.runCleanup("ip", "rule", "del", "priority", strconv.Itoa(priority+index))
			}
			fixture.runCleanup("ip", "route", "flush", "table", strconv.Itoa(table))
			fixture.runCleanup("ip", "link", "del", name)
		})
	}
	t.Cleanup(cleanup)
	return "http://" + net.JoinHostPort(remote.String(), "18083") + "/measure",
		sources, observed, cleanup
}

type rootPacedUpload struct {
	mu            sync.Mutex
	next          time.Time
	bitsPerSecond int64
}

func (link *rootPacedUpload) consume(ctx context.Context, body io.Reader) error {
	const pacingQuantum = 512 * 1024
	buffer := make([]byte, 32*1024)
	pending := 0
	for {
		count, err := body.Read(buffer)
		pending += count
		if pending > 0 && (pending >= pacingQuantum || err == io.EOF) {
			if waitErr := link.reserve(ctx, pending); waitErr != nil {
				return waitErr
			}
			pending = 0
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (link *rootPacedUpload) reserve(ctx context.Context, bytes int) error {
	duration := time.Duration(int64(bytes) * 8 * int64(time.Second) / link.bitsPerSecond)
	link.mu.Lock()
	start := time.Now()
	if link.next.After(start) {
		start = link.next
	}
	finish := start.Add(duration)
	link.next = finish
	link.mu.Unlock()
	timer := time.NewTimer(time.Until(finish))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func assertEverySourceObserved(t *testing.T, observations <-chan netip.Addr, sources []netip.Addr) {
	t.Helper()
	want := make(map[netip.Addr]struct{}, len(sources))
	for _, source := range sources {
		want[source] = struct{}{}
	}
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for len(want) != 0 {
		select {
		case source := <-observations:
			delete(want, source)
		case <-deadline.C:
			t.Fatalf("remote did not observe selected sources %v", want)
		}
	}
}

func drainObservedSources(observations <-chan netip.Addr) {
	for {
		select {
		case <-observations:
		default:
			return
		}
	}
}

func drainSourceObservations(observations <-chan sourceObservation) []sourceObservation {
	var result []sourceObservation
	for {
		select {
		case observation := <-observations:
			result = append(result, observation)
		default:
			return result
		}
	}
}

func assertOverlappingMeasureWindow(t *testing.T, observations []sourceObservation, sources []netip.Addr) {
	t.Helper()
	first := make(map[netip.Addr]time.Time, len(sources))
	last := make(map[netip.Addr]time.Time, len(sources))
	for _, observation := range observations {
		if !observation.finished {
			if _, found := first[observation.source]; !found {
				first[observation.source] = observation.at
			}
		} else {
			last[observation.source] = observation.at
		}
	}
	latestStart := time.Time{}
	earliestEnd := time.Time{}
	for _, source := range sources {
		started, found := first[source]
		if !found {
			t.Fatalf("measure window has no request for source %s", source)
		}
		ended, found := last[source]
		if !found {
			t.Fatalf("measure window has no completed request for source %s", source)
		}
		if latestStart.IsZero() || started.After(latestStart) {
			latestStart = started
		}
		if earliestEnd.IsZero() || ended.Before(earliestEnd) {
			earliestEnd = ended
		}
	}
	if latestStart.After(earliestEnd) {
		t.Fatalf("namespace measure request windows did not overlap: latest start %s, earliest end %s",
			latestStart, earliestEnd)
	}
}

func authorityEntries(t *testing.T) []string {
	t.Helper()
	result, err := readAuthorityEntries()
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func readAuthorityEntries() ([]string, error) {
	entries, err := os.ReadDir(rundirectory.AuthorityRoot)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read run authority: %w", err)
	}
	result := make([]string, len(entries))
	for index, entry := range entries {
		result[index] = fmt.Sprintf("%s:%s", entry.Name(), entry.Type())
	}
	slices.Sort(result)
	return result, nil
}
