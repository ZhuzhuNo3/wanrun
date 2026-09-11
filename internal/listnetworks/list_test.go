package listnetworks

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/probe"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
)

func TestExplicitListSelectionDoesNotInheritRunTransferLimit(t *testing.T) {
	const networkCount = 256
	requested := make([]string, networkCount)
	addresses := make([]localNetwork, networkCount)
	for index := range networkCount {
		requested[index] = fmt.Sprintf("198.18.%d.%d", index/254, index%254+1)
		addresses[index] = local(requested[index], "eth0", true, "")
	}
	request, err := NewRequest(requested, false, nil, nil)
	if err != nil {
		t.Fatalf("NewRequest rejected %d networks: %v", networkCount, err)
	}
	listing := newListing(func(context.Context) (capturedNetworks, error) {
		return capturedNetworks{addresses: addresses, resolve: func(sources []netip.Addr) []resolvedNetwork {
			results := make([]resolvedNetwork, len(sources))
			for index := range results {
				results[index] = resolved(sources[index].String(), "eth0", 254, "192.0.2.254")
			}
			return results
		}}, nil
	}, nil, nil)
	result, err := listing.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Rows()) != networkCount {
		t.Fatalf("row count = %d, want %d", len(result.Rows()), networkCount)
	}
}

func TestBareListingCapturesOnceAndPerformsNoActiveRequests(t *testing.T) {
	var captures, probes, measures atomic.Int32
	listing := newListing(func(context.Context) (capturedNetworks, error) {
		captures.Add(1)
		return listingFixture(), nil
	}, func(context.Context, netip.Addr, probe.Endpoint) (publicObservation, error) {
		probes.Add(1)
		return publicObservation{}, errors.New("unexpected probe")
	}, func(context.Context, []throughput.Target, throughput.Settings) []throughputObservation {
		measures.Add(1)
		return nil
	})
	request, err := NewRequest(nil, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := listing.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if captures.Load() != 1 || probes.Load() != 0 || measures.Load() != 0 {
		t.Fatalf("capture/probe/measure = %d/%d/%d", captures.Load(), probes.Load(), measures.Load())
	}
	rows := result.Rows()
	if len(rows) != 2 || result.RuntimeFailure() {
		t.Fatalf("bare result = %#v", result)
	}
	for _, row := range rows {
		if _, requested := row.Probe(); requested {
			t.Fatal("bare row contains probe status")
		}
		if _, requested := row.Measure(); requested {
			t.Fatal("bare row contains measure status")
		}
	}
}

func TestDefaultDiagnosticsCoverEveryRunnableCandidate(t *testing.T) {
	probeEndpoint := mustProbeEndpoint(t)
	measureWindow := mustMeasureWindow(t)
	for _, test := range []struct {
		name          string
		probeEndpoint *probe.Endpoint
		measureWindow *throughput.Settings
	}{
		{name: "probe", probeEndpoint: &probeEndpoint},
		{name: "measure", measureWindow: &measureWindow},
	} {
		t.Run(test.name, func(t *testing.T) {
			var captures atomic.Int32
			var mu sync.Mutex
			var active []netip.Addr
			listing := newListing(func(context.Context) (capturedNetworks, error) {
				captures.Add(1)
				return listingFixture(), nil
			}, func(_ context.Context, source netip.Addr, _ probe.Endpoint) (publicObservation, error) {
				mu.Lock()
				active = append(active, source)
				mu.Unlock()
				return publicObservation{publicIP: netip.MustParseAddr("203.0.113.1"), asn: 64512,
					organization: "Example ISP"}, nil
			}, func(_ context.Context, targets []throughput.Target, _ throughput.Settings) []throughputObservation {
				results := make([]throughputObservation, len(targets))
				for index, target := range targets {
					active = append(active, target.LocalIP())
					results[index] = throughputObservation{localIP: target.LocalIP(), bytes: 1_000_000,
						duration: time.Second, mbps: 8, weight: 1}
				}
				return results
			})
			request, err := NewRequest(nil, false, test.probeEndpoint, test.measureWindow)
			if err != nil {
				t.Fatal(err)
			}
			result, err := listing.Run(context.Background(), request)
			if err != nil || result.RuntimeFailure() || captures.Load() != 1 {
				t.Fatalf("default %s result=%#v captures=%d error=%v", test.name, result, captures.Load(), err)
			}
			if !sameAddressSet(active, []string{"192.0.2.1", "192.0.2.4"}) {
				t.Fatalf("default %s active addresses=%v", test.name, active)
			}
			for _, row := range result.Rows() {
				probeStatus, hasProbe := row.Probe()
				measureStatus, hasMeasure := row.Measure()
				wantProbe, wantMeasure := test.probeEndpoint != nil, test.measureWindow != nil
				if hasProbe != wantProbe || hasMeasure != wantMeasure ||
					(hasProbe && !probeStatus.Attempted()) || (hasMeasure && !measureStatus.Attempted()) {
					t.Fatalf("default %s row %s diagnostics=%#v/%#v", test.name,
						row.LocalIP(), probeStatus, measureStatus)
				}
			}
		})
	}
}

func TestExplicitValidationFailsBeforeCaptureOrHTTP(t *testing.T) {
	var calls atomic.Int32
	listing := newListing(func(context.Context) (capturedNetworks, error) {
		calls.Add(1)
		return listingFixture(), nil
	}, func(context.Context, netip.Addr, probe.Endpoint) (publicObservation, error) {
		calls.Add(1)
		return publicObservation{}, nil
	}, func(context.Context, []throughput.Target, throughput.Settings) []throughputObservation {
		calls.Add(1)
		return nil
	})
	for _, values := range [][]string{{"not-an-ip"}, {"2001:db8::1"}, {"192.0.2.1", "192.0.2.1"}} {
		request := Request{explicitRaw: append([]string(nil), values...)}
		_, err := listing.Run(context.Background(), request)
		var usage *UsageError
		if !errors.As(err, &usage) {
			t.Fatalf("Run(%v) error = %v, want UsageError", values, err)
		}
		if calls.Load() != 0 {
			t.Fatalf("invalid input caused %d side effects", calls.Load())
		}
	}
}

func TestExplicitRowsKeepOrderSkipIneligibleAndContinueDiagnostics(t *testing.T) {
	probeEndpoint := mustProbeEndpoint(t)
	measureWindow := mustMeasureWindow(t)
	request, err := NewRequest([]string{"192.0.2.4", "192.0.2.2", "198.51.100.99",
		"192.0.2.3", "192.0.2.5", "192.0.2.1"},
		false, &probeEndpoint, &measureWindow)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var probed []netip.Addr
	var measured []netip.Addr
	listing := newListing(func(context.Context) (capturedNetworks, error) {
		return listingFixture(), nil
	}, func(_ context.Context, source netip.Addr, _ probe.Endpoint) (publicObservation, error) {
		mu.Lock()
		probed = append(probed, source)
		mu.Unlock()
		if source.String() == "192.0.2.1" {
			return publicObservation{}, errors.New("probe unavailable")
		}
		return publicObservation{publicIP: netip.MustParseAddr("203.0.113.4"), asn: 64512,
			organization: "Example ISP"}, nil
	}, func(_ context.Context, targets []throughput.Target, _ throughput.Settings) []throughputObservation {
		results := make([]throughputObservation, len(targets))
		for index, target := range targets {
			measured = append(measured, target.LocalIP())
			results[index] = throughputObservation{localIP: target.LocalIP(), bytes: 1_000_000,
				duration: time.Second, mbps: 8}
			if target.LocalIP().String() == "192.0.2.1" {
				results[index].err = errors.New("measure unavailable")
			}
		}
		return results
	})
	result, err := listing.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	rows := result.Rows()
	gotOrder := make([]string, len(rows))
	for index, row := range rows {
		gotOrder[index] = row.LocalIP().String()
	}
	if !slices.Equal(gotOrder, []string{"192.0.2.4", "192.0.2.2", "198.51.100.99",
		"192.0.2.3", "192.0.2.5", "192.0.2.1"}) {
		t.Fatalf("row order = %v", gotOrder)
	}
	if !sameAddressSet(probed, []string{"192.0.2.1", "192.0.2.4"}) ||
		!sameAddressSet(measured, []string{"192.0.2.1", "192.0.2.4"}) {
		t.Fatalf("active probe/measure addresses = %v / %v", probed, measured)
	}
	if !result.RuntimeFailure() {
		t.Fatal("explicit invalid rows and diagnostic failures did not produce runtime failure")
	}
	for _, index := range []int{1, 2, 3, 4} {
		probeStatus, _ := rows[index].Probe()
		measureStatus, _ := rows[index].Measure()
		if probeStatus.Attempted() || measureStatus.Attempted() ||
			probeStatus.Err() != nil || measureStatus.Err() != nil {
			t.Fatalf("ineligible row %d active status = %#v / %#v", index, probeStatus, measureStatus)
		}
	}
}

func TestAllDisplaysExcludedWithoutAuthorizingTrafficOrFailure(t *testing.T) {
	probeEndpoint := mustProbeEndpoint(t)
	measureWindow := mustMeasureWindow(t)
	request, err := NewRequest(nil, true, &probeEndpoint, &measureWindow)
	if err != nil {
		t.Fatal(err)
	}
	var probed []netip.Addr
	var measured []netip.Addr
	var probeMu sync.Mutex
	listing := newListing(func(context.Context) (capturedNetworks, error) {
		return listingFixture(), nil
	}, func(_ context.Context, source netip.Addr, _ probe.Endpoint) (publicObservation, error) {
		probeMu.Lock()
		probed = append(probed, source)
		probeMu.Unlock()
		return publicObservation{publicIP: netip.MustParseAddr("203.0.113.1"), asn: 64512,
			organization: "Example ISP"}, nil
	}, func(_ context.Context, targets []throughput.Target, _ throughput.Settings) []throughputObservation {
		results := make([]throughputObservation, len(targets))
		for index, target := range targets {
			measured = append(measured, target.LocalIP())
			results[index] = throughputObservation{localIP: target.LocalIP(), bytes: 1_000_000,
				duration: time.Second, mbps: 8, weight: 1}
		}
		return results
	})
	result, err := listing.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !sameAddressSet(probed, []string{"192.0.2.1", "192.0.2.4"}) ||
		!sameAddressSet(measured, []string{"192.0.2.1", "192.0.2.4"}) {
		t.Fatalf("all active addresses = %v / %v", probed, measured)
	}
	if result.RuntimeFailure() {
		t.Fatal("display-only excluded rows caused runtime failure")
	}
	rows := result.Rows()
	if len(rows) != 5 || rows[1].IncludedByDefault() || rows[2].IncludedByDefault() ||
		!rows[4].IncludedByDefault() {
		t.Fatalf("all rows = %#v", rows)
	}
	weights := 0
	for _, row := range rows {
		status, requested := row.Measure()
		if requested && status.HasWeight() {
			weights++
		}
	}
	if weights != 2 {
		t.Fatalf("weighted row count = %d, want two active rows", weights)
	}
}

func TestAllCollapsesDuplicateAddressObservationsAndKeepsConflictsAsRows(t *testing.T) {
	request, err := NewRequest(nil, true, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	wantSources := []netip.Addr{
		netip.MustParseAddr("192.0.2.10"),
		netip.MustParseAddr("192.0.2.20"),
		netip.MustParseAddr("192.0.2.30"),
	}
	var resolvedSources []netip.Addr
	captured := capturedNetworks{addresses: []localNetwork{
		local("192.0.2.10", "wan0", true, ""),
		local("192.0.2.10", "wan0", true, ""),
		local("192.0.2.20", "wan0", true, ""),
		local("192.0.2.20", "wan1", true, ""),
		local("192.0.2.30", "wan2", true, ""),
	}, resolve: func(sources []netip.Addr) []resolvedNetwork {
		resolvedSources = append([]netip.Addr(nil), sources...)
		result := make([]resolvedNetwork, 0, len(sources))
		for _, source := range sources {
			switch source.String() {
			case "192.0.2.10":
				result = append(result, resolved(source.String(), "wan0", 100, "192.0.2.1"))
			case "192.0.2.20":
				result = append(result, resolvedNetwork{localIP: source,
					reason: "selected source has conflicting facts on multiple interfaces"})
			case "192.0.2.30":
				result = append(result, resolved(source.String(), "wan2", 300, "192.0.2.254"))
			}
		}
		slices.Reverse(result)
		return result
	}}
	listing := newListing(func(context.Context) (capturedNetworks, error) {
		return captured, nil
	}, nil, nil)
	result, err := listing.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(resolvedSources, wantSources) {
		t.Fatalf("resolver sources = %v, want unique ordered %v", resolvedSources, wantSources)
	}
	if result.RuntimeFailure() {
		t.Fatal("display-only conflicting address caused runtime failure")
	}
	rows := result.Rows()
	if len(rows) != 3 || rows[0].LocalIP() != wantSources[0] || rows[0].InterfaceName() != "wan0" ||
		!rows[0].Runnable() || rows[1].LocalIP() != wantSources[1] || rows[1].Runnable() ||
		!strings.Contains(rows[1].LocalReason(), "conflicting facts") ||
		rows[2].LocalIP() != wantSources[2] || rows[2].InterfaceName() != "wan2" ||
		rows[2].RouteTable() != 300 || !rows[2].Runnable() {
		t.Fatalf("all duplicate rows = %#v", rows)
	}
}

func TestNoRouteDefaultCandidateDiffersAcrossSelectionModes(t *testing.T) {
	listing := newListing(func(context.Context) (capturedNetworks, error) {
		return listingFixture(), nil
	}, nil, nil)
	defaultRequest, _ := NewRequest(nil, false, nil, nil)
	defaultResult, err := listing.Run(context.Background(), defaultRequest)
	if err != nil || defaultResult.RuntimeFailure() || rowForIP(defaultResult.Rows(), "192.0.2.5") != nil {
		t.Fatalf("default no-route result = %#v, %v", defaultResult, err)
	}
	allRequest, _ := NewRequest(nil, true, nil, nil)
	allResult, err := listing.Run(context.Background(), allRequest)
	allRow := rowForIP(allResult.Rows(), "192.0.2.5")
	if err != nil || allResult.RuntimeFailure() || allRow == nil || allRow.Runnable() || allRow.LocalReason() == "" {
		t.Fatalf("all no-route result = %#v, %v", allResult, err)
	}
	explicitRequest, _ := NewRequest([]string{"192.0.2.5"}, false, nil, nil)
	explicitResult, err := listing.Run(context.Background(), explicitRequest)
	explicitRows := explicitResult.Rows()
	if err != nil || !explicitResult.RuntimeFailure() || len(explicitRows) != 1 ||
		explicitRows[0].Runnable() || explicitRows[0].LocalReason() == "" {
		t.Fatalf("explicit no-route result = %#v, %v", explicitResult, err)
	}
}

func TestListJoinsResolvedNetworksByLocalIP(t *testing.T) {
	request, err := NewRequest([]string{"192.0.2.1", "192.0.2.4"}, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	captured := listingFixture()
	captured.resolve = func([]netip.Addr) []resolvedNetwork {
		return []resolvedNetwork{
			resolved("192.0.2.4", "eth2", 100, "198.51.100.1"),
			resolved("192.0.2.1", "eth0", 254, "192.0.2.254"),
		}
	}
	listing := newListing(func(context.Context) (capturedNetworks, error) {
		return captured, nil
	}, nil, nil)
	result, err := listing.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	rows := result.Rows()
	firstGateway, firstHasGateway := rows[0].Gateway()
	secondGateway, secondHasGateway := rows[1].Gateway()
	if rows[0].LocalIP() != netip.MustParseAddr("192.0.2.1") || rows[0].RouteTable() != 254 ||
		!firstHasGateway || firstGateway != netip.MustParseAddr("192.0.2.254") ||
		rows[1].LocalIP() != netip.MustParseAddr("192.0.2.4") || rows[1].RouteTable() != 100 ||
		!secondHasGateway || secondGateway != netip.MustParseAddr("198.51.100.1") {
		t.Fatalf("resolved network join = %#v", rows)
	}
}

func TestListRejectsIncompleteDuplicateUnknownOrUnidentifiedResolvedNetworks(t *testing.T) {
	request, err := NewRequest([]string{"192.0.2.1", "192.0.2.4"}, false, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := resolved("192.0.2.1", "eth0", 254, "192.0.2.254")
	second := resolved("192.0.2.4", "eth2", 100, "198.51.100.1")
	unknown := resolved("198.51.100.1", "eth9", 200, "203.0.113.1")
	unidentified := second
	unidentified.localIP = netip.Addr{}
	for name, egresses := range map[string][]resolvedNetwork{
		"incomplete":   {first},
		"duplicate":    {first, first},
		"unknown":      {first, unknown},
		"unidentified": {first, unidentified},
	} {
		t.Run(name, func(t *testing.T) {
			captured := listingFixture()
			captured.resolve = func([]netip.Addr) []resolvedNetwork {
				return append([]resolvedNetwork(nil), egresses...)
			}
			listing := newListing(func(context.Context) (capturedNetworks, error) {
				return captured, nil
			}, nil, nil)
			if _, err := listing.Run(context.Background(), request); err == nil {
				t.Fatal("invalid resolved network set was accepted")
			}
		})
	}
}

func rowForIP(rows []Row, raw string) *Row {
	for index := range rows {
		if rows[index].LocalIP().String() == raw {
			return &rows[index]
		}
	}
	return nil
}

func TestSingleNetworkMeasurementReportsThroughputWithoutRelativeWeight(t *testing.T) {
	window := mustMeasureWindow(t)
	request, err := NewRequest([]string{"192.0.2.1"}, false, nil, &window)
	if err != nil {
		t.Fatal(err)
	}
	listing := newListing(func(context.Context) (capturedNetworks, error) {
		return listingFixture(), nil
	}, nil, func(_ context.Context, targets []throughput.Target, _ throughput.Settings) []throughputObservation {
		return []throughputObservation{{localIP: targets[0].LocalIP(), bytes: 1_000_000,
			duration: time.Second, mbps: 8}}
	})
	result, err := listing.Run(context.Background(), request)
	if err != nil || result.RuntimeFailure() {
		t.Fatalf("single measure = %#v, %v", result, err)
	}
	rows := result.Rows()
	status, requested := rows[0].Measure()
	if !requested || !status.Attempted() || status.Mbps() != 8 || status.HasWeight() {
		t.Fatalf("single measure status = %#v", status)
	}
	if _, requested := rows[0].Probe(); requested {
		t.Fatal("single measure fabricated probe status")
	}
}

func TestListJoinsHostMeasurementsByLocalIP(t *testing.T) {
	window := mustMeasureWindow(t)
	request, err := NewRequest([]string{"192.0.2.1", "192.0.2.4"}, false, nil, &window)
	if err != nil {
		t.Fatal(err)
	}
	listing := newListing(func(context.Context) (capturedNetworks, error) {
		return listingFixture(), nil
	}, nil, func(_ context.Context, targets []throughput.Target, _ throughput.Settings) []throughputObservation {
		return []throughputObservation{
			{localIP: targets[1].LocalIP(), bytes: 2_000_000, duration: time.Second, mbps: 16},
			{localIP: targets[0].LocalIP(), bytes: 1_000_000, duration: time.Second, mbps: 8},
		}
	})
	result, err := listing.Run(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	rows := result.Rows()
	first, _ := rows[0].Measure()
	second, _ := rows[1].Measure()
	if first.Mbps() != 8 || second.Mbps() != 16 {
		t.Fatalf("measurement join = %.0f/%.0f Mbps", first.Mbps(), second.Mbps())
	}
}

func TestListRejectsIncompleteDuplicateOrUnknownHostMeasurements(t *testing.T) {
	window := mustMeasureWindow(t)
	request, _ := NewRequest([]string{"192.0.2.1", "192.0.2.4"}, false, nil, &window)
	first := netip.MustParseAddr("192.0.2.1")
	unknown := netip.MustParseAddr("198.51.100.1")
	for name, observed := range map[string][]throughputObservation{
		"missing":   {{localIP: first}},
		"duplicate": {{localIP: first}, {localIP: first}},
		"unknown":   {{localIP: first}, {localIP: unknown}},
	} {
		t.Run(name, func(t *testing.T) {
			listing := newListing(func(context.Context) (capturedNetworks, error) {
				return listingFixture(), nil
			}, nil, func(context.Context, []throughput.Target, throughput.Settings) []throughputObservation {
				return append([]throughputObservation(nil), observed...)
			})
			if _, err := listing.Run(context.Background(), request); err == nil {
				t.Fatal("invalid host measurement set was accepted")
			}
		})
	}
}

func listingFixture() capturedNetworks {
	addresses := []localNetwork{
		local("192.0.2.1", "eth0", true, ""),
		local("192.0.2.2", "br0", false, "interface is a bridge"),
		local("192.0.2.3", "eth1", false, "interface is administratively down"),
		local("192.0.2.4", "eth2", true, ""),
		local("192.0.2.5", "eth3", true, ""),
	}
	// The catalog's default-candidate facts are intentionally broader than runnable
	// egresses: address 5 is locally eligible but has no usable route.
	return capturedNetworks{addresses: addresses,
		defaults: []localNetwork{addresses[0], addresses[3], addresses[4]},
		resolve: func(sources []netip.Addr) []resolvedNetwork {
			results := make([]resolvedNetwork, len(sources))
			for index, source := range sources {
				results[index] = resolvedNetwork{localIP: source, reason: "selected source is not a local address"}
				switch source.String() {
				case "192.0.2.1":
					results[index] = resolved(source.String(), "eth0", 254, "192.0.2.254")
				case "192.0.2.2":
					results[index] = resolved(source.String(), "br0", 254, "192.0.2.254")
				case "192.0.2.3":
					results[index].reason = "local interface eth1 is administratively down"
				case "192.0.2.4":
					results[index] = resolved(source.String(), "eth2", 100, "198.51.100.1")
				case "192.0.2.5":
					results[index].reason = "no usable default route for selected source"
				}
			}
			return results
		}}
}

func local(raw, name string, included bool, reason string) localNetwork {
	return localNetwork{localIP: netip.MustParseAddr(raw),
		interfaceName: name, interfaceIndex: 2, included: included, exclusionReason: reason}
}

func resolved(localIP, name string, table int, gateway string) resolvedNetwork {
	return resolvedNetwork{localIP: netip.MustParseAddr(localIP), interfaceName: name,
		interfaceIndex: 2, table: table, gateway: netip.MustParseAddr(gateway),
		hasGateway: true, runnable: true}
}

func mustProbeEndpoint(t *testing.T) probe.Endpoint {
	t.Helper()
	endpoint, err := probe.NewEndpoint("https://probe.example/meta")
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func mustMeasureWindow(t *testing.T) throughput.Settings {
	t.Helper()
	window, err := throughput.NewSettings("https://measure.example/upload", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return window
}

func sameAddressSet(got []netip.Addr, want []string) bool {
	values := make([]string, len(got))
	for index, address := range got {
		values[index] = address.String()
	}
	slices.Sort(values)
	copyOfWant := append([]string(nil), want...)
	slices.Sort(copyOfWant)
	return slices.Equal(values, copyOfWant)
}
