package networkcatalog

import (
	"net"
	"net/netip"
	"slices"
	"strings"
	"testing"
)

func TestResolveUsesMainTableFallback(t *testing.T) {
	snapshot := fixtureSnapshot(
		[]policyRule{
			fixtureRule(0, 255, "0.0.0.0/0"),
			fixtureRule(32766, 254, "0.0.0.0/0"),
		},
		[]fibRoute{fixtureDefault(254, 2, "192.0.2.1", routeUnicast)},
	)

	egress := oneEgress(t, snapshot, "192.0.2.10")
	assertRunnableEgress(t, egress, 254, "wan0", "192.0.2.1")
}

func TestResolveHonorsSourceRuleAndPriority(t *testing.T) {
	snapshot := fixtureSnapshot(
		[]policyRule{
			fixtureRule(200, 200, "192.0.2.0/24"),
			fixtureRule(100, 100, "192.0.2.10/32"),
			fixtureRule(32766, 254, "0.0.0.0/0"),
		},
		[]fibRoute{
			fixtureDefault(254, 2, "192.0.2.1", routeUnicast),
			fixtureDefault(200, 2, "192.0.2.200", routeUnicast),
			fixtureDefault(100, 2, "192.0.2.100", routeUnicast),
		},
	)

	egress := oneEgress(t, snapshot, "192.0.2.10")
	assertRunnableEgress(t, egress, 100, "wan0", "192.0.2.100")
}

func TestResolveSkipsForwardedReturnRuleForLocalOutput(t *testing.T) {
	returnRule := fixtureRule(50, 300, "0.0.0.0/0")
	returnRule.destination = mustPrefix("198.18.0.0/30")
	returnRule.inputInterface = fixtureAttachedInput("wan0", 2, false)
	snapshot := fixtureSnapshot(
		[]policyRule{returnRule, fixtureRule(100, 100, "192.0.2.10/32")},
		[]fibRoute{fixtureDefault(100, 2, "192.0.2.1", routeUnicast)},
	)

	assertRunnableEgress(t, oneEgress(t, snapshot, "192.0.2.10"), 100, "wan0", "192.0.2.1")
}

func TestResolveKeepsUncertainLocalOutputRulesFailClosed(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		input inputInterfaceSelector
	}{
		{name: "local output input interface", input: fixtureAttachedInput("lo", 1, false)},
		{name: "no input interface"},
		{name: "unresolved input interface", input: inputInterfaceSelector{
			name: "wan0", state: inputInterfaceUnresolved}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			uncertain := fixtureRule(50, 300, "0.0.0.0/0")
			uncertain.destination = mustPrefix("198.18.0.0/30")
			uncertain.inputInterface = testCase.input
			snapshot := fixtureSnapshot(
				[]policyRule{uncertain, fixtureRule(100, 100, "192.0.2.10/32")},
				[]fibRoute{fixtureDefault(100, 2, "192.0.2.1", routeUnicast)},
			)

			egress := oneEgress(t, snapshot, "192.0.2.10")
			if egress.Runnable() || !strings.Contains(egress.Reason(), "destination prefix") {
				t.Fatalf("egress = %#v, want destination-dependent rule rejected", egress)
			}
		})
	}
}

func TestResolveMatchesInputInterfaceByCapturedIdentity(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		input      inputInterfaceSelector
		wantTable  int
		wantReason string
	}{
		{name: "loopback identity matches local output", input: fixtureAttachedInput("lo", 1, false),
			wantTable: 100},
		{name: "provider identity does not match local output", input: fixtureAttachedInput("wan0", 2, false),
			wantTable: 254},
		{name: "detached identity does not match local output", input: inputInterfaceSelector{
			name: "gone0", state: inputInterfaceDetached, index: -1}, wantTable: 254},
		{name: "uncaptured attached identity remains undecidable", input: inputInterfaceSelector{
			name: "wan0", state: inputInterfaceAttached}, wantReason: "input interface wan0"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			selected := fixtureRule(100, 100, "0.0.0.0/0")
			selected.inputInterface = testCase.input
			snapshot := fixtureSnapshot(
				[]policyRule{selected, fixtureRule(32766, 254, "0.0.0.0/0")},
				[]fibRoute{fixtureDefault(100, 2, "192.0.2.100", routeUnicast),
					fixtureDefault(254, 2, "192.0.2.1", routeUnicast)},
			)

			egress := oneEgress(t, snapshot, "192.0.2.10")
			if testCase.wantReason != "" {
				if egress.Runnable() || !strings.Contains(egress.Reason(), testCase.wantReason) {
					t.Fatalf("egress = %#v, want reason containing %q", egress, testCase.wantReason)
				}
				return
			}
			if !egress.Runnable() || egress.Table() != testCase.wantTable {
				t.Fatalf("egress = %#v, want runnable table %d", egress, testCase.wantTable)
			}
		})
	}
}

func TestResolveTreatsL3MasterInputInterfaceAsUndetermined(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		source     string
		inverted   bool
		wantTable  int
		wantReject bool
	}{
		{name: "matching source", source: "192.0.2.10/32", wantReject: true},
		{name: "matching source inverted", source: "192.0.2.10/32", inverted: true,
			wantReject: true},
		{name: "nonmatching source", source: "198.51.100.0/24", wantTable: 254},
		{name: "nonmatching source inverted", source: "198.51.100.0/24", inverted: true,
			wantTable: 100},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			rule := fixtureRule(100, 100, testCase.source)
			rule.inputInterface = fixtureAttachedInput("vrf0", 9, true)
			rule.inverted = testCase.inverted
			snapshot := fixtureSnapshot(
				[]policyRule{rule, fixtureRule(32766, 254, "0.0.0.0/0")},
				[]fibRoute{fixtureDefault(100, 2, "192.0.2.100", routeUnicast),
					fixtureDefault(254, 2, "192.0.2.1", routeUnicast)},
			)

			egress := oneEgress(t, snapshot, "192.0.2.10")
			if testCase.wantReject {
				if egress.Runnable() || !strings.Contains(egress.Reason(), "input interface vrf0") {
					t.Fatalf("egress = %#v, want L3-master selector rejected", egress)
				}
				return
			}
			if !egress.Runnable() || egress.Table() != testCase.wantTable {
				t.Fatalf("egress = %#v, want table %d", egress, testCase.wantTable)
			}
		})
	}
}

func TestResolveAppliesTerminalAndContinuingRouteTypes(t *testing.T) {
	tests := []struct {
		name       string
		custom     []fibRoute
		runnable   bool
		wantTable  int
		wantReason string
	}{
		{name: "table miss falls through", runnable: true, wantTable: 254},
		{name: "throw falls through", custom: []fibRoute{fixtureDefault(100, 2, "", routeThrow)},
			runnable: true, wantTable: 254},
		{name: "unreachable terminates", custom: []fibRoute{fixtureDefault(100, 0, "", routeUnreachable)},
			wantReason: "unreachable"},
		{name: "blackhole terminates", custom: []fibRoute{fixtureDefault(100, 0, "", routeBlackhole)},
			wantReason: "blackhole"},
		{name: "prohibit terminates", custom: []fibRoute{fixtureDefault(100, 0, "", routeProhibit)},
			wantReason: "prohibit"},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			routes := append([]fibRoute{}, testCase.custom...)
			routes = append(routes, fixtureDefault(254, 2, "192.0.2.1", routeUnicast))
			snapshot := fixtureSnapshot(
				[]policyRule{fixtureRule(100, 100, "192.0.2.10/32"), fixtureRule(32766, 254, "0.0.0.0/0")},
				routes,
			)
			egress := oneEgress(t, snapshot, "192.0.2.10")
			if egress.Runnable() != testCase.runnable {
				t.Fatalf("Runnable = %t, reason %q", egress.Runnable(), egress.Reason())
			}
			if testCase.runnable && egress.Table() != testCase.wantTable {
				t.Fatalf("Table = %d, want %d", egress.Table(), testCase.wantTable)
			}
			if testCase.wantReason != "" && !strings.Contains(egress.Reason(), testCase.wantReason) {
				t.Fatalf("Reason = %q, want %q", egress.Reason(), testCase.wantReason)
			}
		})
	}
}

func TestResolveReportsNoDefaultRoute(t *testing.T) {
	snapshot := fixtureSnapshot(
		[]policyRule{fixtureRule(32766, 254, "0.0.0.0/0")},
		[]fibRoute{{table: 254, destination: mustPrefix("198.51.100.0/24"), kind: routeUnicast, linkIndex: 2}},
	)

	egress := oneEgress(t, snapshot, "192.0.2.10")
	if egress.Runnable() || !strings.Contains(egress.Reason(), "no usable default route") {
		t.Fatalf("egress = %#v, want no-default diagnostic", egress)
	}
}

func TestResolveRejectsSelectorsNotDeterminedBySource(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		selector unresolvedRuleSelector
	}{
		{name: "fwmark", selector: fixtureUnresolvedSelector(ruleSelectorFirewallMark)},
		{name: "VRF/l3mdev", selector: fixtureUnresolvedSelector(ruleSelectorL3Master)},
		{name: "UID range", selector: fixtureUnresolvedSelector(ruleSelectorUIDRange)},
		{name: "transport selector", selector: fixtureUnresolvedSelector(ruleSelectorTransport)},
		{name: "unknown attribute", selector: unresolvedRuleSelector{
			kind: ruleSelectorUnknownAttribute, attribute: 99}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			unsupported := fixtureRule(100, 100, "192.0.2.10/32")
			unsupported.unresolvedSelectors = []unresolvedRuleSelector{testCase.selector}
			snapshot := fixtureSnapshot(
				[]policyRule{unsupported, fixtureRule(32766, 254, "0.0.0.0/0")},
				[]fibRoute{fixtureDefault(100, 2, "192.0.2.100", routeUnicast),
					fixtureDefault(254, 2, "192.0.2.1", routeUnicast)},
			)
			egress := oneEgress(t, snapshot, "192.0.2.10")
			if egress.Runnable() || !strings.Contains(egress.Reason(), testCase.selector.description()) {
				t.Fatalf("egress = %#v, want unresolved %s", egress, testCase.selector.description())
			}
		})
	}

	nonmatching := fixtureRule(50, 50, "198.51.100.0/24")
	nonmatching.unresolvedSelectors = []unresolvedRuleSelector{fixtureUnresolvedSelector(ruleSelectorUIDRange)}
	snapshot := fixtureSnapshot(
		[]policyRule{nonmatching, fixtureRule(32766, 254, "0.0.0.0/0")},
		[]fibRoute{fixtureDefault(254, 2, "192.0.2.1", routeUnicast)},
	)
	assertRunnableEgress(t, oneEgress(t, snapshot, "192.0.2.10"), 254, "wan0", "192.0.2.1")
}

func TestResolveKeepsOutputInterfaceRulesFailClosed(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		state outputInterfaceState
	}{
		{name: "attached", state: outputInterfaceAttached},
		{name: "detached", state: outputInterfaceDetached},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			rule := fixtureRule(100, 100, "192.0.2.10/32")
			rule.outputInterface = outputInterfaceSelector{name: "target0", state: testCase.state}
			snapshot := fixtureSnapshot(
				[]policyRule{rule, fixtureRule(32766, 254, "0.0.0.0/0")},
				[]fibRoute{fixtureDefault(100, 2, "192.0.2.100", routeUnicast),
					fixtureDefault(254, 2, "192.0.2.1", routeUnicast)},
			)

			egress := oneEgress(t, snapshot, "192.0.2.10")
			if egress.Runnable() || !strings.Contains(egress.Reason(), "output interface target0") {
				t.Fatalf("egress = %#v, want output-interface selector rejected", egress)
			}
		})
	}
}

func TestResolveAppliesInversionAfterSelectorConjunction(t *testing.T) {
	tests := []struct {
		name, source string
		wantTable    int
		wantRejected bool
	}{
		{name: "matching source leaves UID unresolved", source: "192.0.2.10/32", wantRejected: true},
		{name: "nonmatching source decides inverted conjunction", source: "198.51.100.0/24", wantTable: 100},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			inverted := fixtureRule(100, 100, testCase.source)
			inverted.inverted = true
			inverted.unresolvedSelectors = []unresolvedRuleSelector{
				fixtureUnresolvedSelector(ruleSelectorUIDRange),
			}
			snapshot := fixtureSnapshot(
				[]policyRule{inverted, fixtureRule(32766, 254, "0.0.0.0/0")},
				[]fibRoute{fixtureDefault(100, 2, "192.0.2.100", routeUnicast),
					fixtureDefault(254, 2, "192.0.2.1", routeUnicast)},
			)

			egress := oneEgress(t, snapshot, "192.0.2.10")
			if testCase.wantRejected {
				if egress.Runnable() || !strings.Contains(egress.Reason(), "UID range") {
					t.Fatalf("egress = %#v, want inverted unresolved selector rejected", egress)
				}
				return
			}
			if !egress.Runnable() || egress.Table() != testCase.wantTable {
				t.Fatalf("egress = %#v, want inverted rule table %d", egress, testCase.wantTable)
			}
		})
	}
}

func TestResolveSkipsEffectsAfterKnownSelectorMismatch(t *testing.T) {
	nonmatching := fixtureRule(50, 50, "192.0.2.10/32")
	nonmatching.inputInterface = fixtureAttachedInput("wan0", 2, false)
	nonmatching.destination = mustPrefix("198.18.0.0/30")
	nonmatching.unresolvedSelectors = []unresolvedRuleSelector{
		fixtureUnresolvedSelector(ruleSelectorUIDRange),
	}
	nonmatching.unsupportedEffects = []unsupportedRuleEffect{{kind: ruleEffectRouteSuppression}}
	snapshot := fixtureSnapshot(
		[]policyRule{nonmatching, fixtureRule(32766, 254, "0.0.0.0/0")},
		[]fibRoute{fixtureDefault(254, 2, "192.0.2.1", routeUnicast)},
	)

	assertRunnableEgress(t, oneEgress(t, snapshot, "192.0.2.10"), 254, "wan0", "192.0.2.1")
}

func TestResolveRejectsMatchingUnsupportedRuleEffect(t *testing.T) {
	unsupported := fixtureRule(100, 100, "192.0.2.10/32")
	unsupported.unsupportedEffects = []unsupportedRuleEffect{{kind: ruleEffectRouteSuppression}}
	snapshot := fixtureSnapshot(
		[]policyRule{unsupported, fixtureRule(32766, 254, "0.0.0.0/0")},
		[]fibRoute{fixtureDefault(100, 2, "192.0.2.100", routeUnicast),
			fixtureDefault(254, 2, "192.0.2.1", routeUnicast)},
	)

	egress := oneEgress(t, snapshot, "192.0.2.10")
	if egress.Runnable() || !strings.Contains(egress.Reason(), "unsupported effect: route suppression") {
		t.Fatalf("egress = %#v, want matching effect rejected", egress)
	}
}

func TestResolveRejectsSemanticallyDifferentEqualPriorityRules(t *testing.T) {
	first := fixtureRule(100, 100, "192.0.2.10/32")
	second := first
	second.inputInterface = fixtureAttachedInput("lo", 1, false)
	snapshot := fixtureSnapshot(
		[]policyRule{first, second},
		[]fibRoute{fixtureDefault(100, 2, "192.0.2.100", routeUnicast)},
	)

	egress := oneEgress(t, snapshot, "192.0.2.10")
	if egress.Runnable() || !strings.Contains(egress.Reason(), "priority 100 are ambiguous") {
		t.Fatalf("egress = %#v, want semantic ambiguity", egress)
	}
}

func TestResolveEvaluatesSourceOnlyInvertedRules(t *testing.T) {
	tests := []struct {
		name       string
		ruleSource string
		wantTable  int
	}{
		{name: "source mismatch applies inverted rule", ruleSource: "198.51.100.0/24", wantTable: 100},
		{name: "source match skips inverted rule", ruleSource: "192.0.2.0/24", wantTable: 254},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			inverted := fixtureRule(100, 100, testCase.ruleSource)
			inverted.inverted = true
			snapshot := fixtureSnapshot(
				[]policyRule{inverted, fixtureRule(32766, 254, "0.0.0.0/0")},
				[]fibRoute{fixtureDefault(100, 2, "192.0.2.100", routeUnicast),
					fixtureDefault(254, 2, "192.0.2.1", routeUnicast)},
			)

			egress := oneEgress(t, snapshot, "192.0.2.10")
			if !egress.Runnable() || egress.Table() != testCase.wantTable {
				t.Fatalf("egress = %#v, want runnable table %d", egress, testCase.wantTable)
			}
		})
	}
}

func TestInterfaceAvailabilityIsConsistentAcrossCatalogViews(t *testing.T) {
	tests := []struct {
		name, operationalState, wantReason string
		flags                              net.Flags
		runnable                           bool
	}{
		{name: "administratively down", flags: 0, operationalState: "up",
			wantReason: "administratively down"},
		{name: "operationally down", flags: net.FlagUp, operationalState: "down",
			wantReason: "operational state is down"},
		{name: "lower layer down", flags: net.FlagUp, operationalState: "lower-layer-down",
			wantReason: "operational state is lower-layer-down"},
		{name: "unknown operational state", flags: net.FlagUp, operationalState: "unknown", runnable: true},
		{name: "operationally up", flags: net.FlagUp, operationalState: "up", runnable: true},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			t.Run("default candidate", func(t *testing.T) {
				snapshot := availabilitySnapshot(
					fixtureAvailableLink(2, "wan0", testCase.flags, testCase.operationalState),
					fixtureAvailableLink(3, "wan1", net.FlagUp, "up"),
				)
				defaults := snapshot.DefaultCandidates()
				if got := len(defaults); (got == 1) != testCase.runnable {
					t.Fatalf("DefaultCandidates count = %d, runnable=%t", got, testCase.runnable)
				}
				addresses := snapshot.LocalAddresses()
				if len(addresses) != 1 {
					t.Fatalf("LocalAddresses count = %d, want 1", len(addresses))
				}
				if testCase.wantReason != "" &&
					!strings.Contains(addresses[0].ExclusionReason(), testCase.wantReason) {
					t.Fatalf("ExclusionReason = %q, want %q",
						addresses[0].ExclusionReason(), testCase.wantReason)
				}
			})
			t.Run("source interface", func(t *testing.T) {
				snapshot := availabilitySnapshot(
					fixtureAvailableLink(2, "wan0", testCase.flags, testCase.operationalState),
					fixtureAvailableLink(3, "wan1", net.FlagUp, "up"),
				)
				assertAvailabilityResult(t, oneEgress(t, snapshot, "192.0.2.10"), testCase)
			})
			t.Run("route output interface", func(t *testing.T) {
				snapshot := availabilitySnapshot(
					fixtureAvailableLink(2, "wan0", net.FlagUp, "up"),
					fixtureAvailableLink(3, "wan1", testCase.flags, testCase.operationalState),
				)
				assertAvailabilityResult(t, oneEgress(t, snapshot, "192.0.2.10"), testCase)
			})
		})
	}
}

func availabilitySnapshot(source, output linkObservation) Snapshot {
	return newSnapshot(
		[]linkObservation{source, output},
		[]addressObservation{fixtureAddress(source.index, "192.0.2.10/24")},
		[]policyRule{fixtureRule(32766, 254, "0.0.0.0/0")},
		[]fibRoute{fixtureDefault(254, output.index, "192.0.2.1", routeUnicast)},
	)
}

func fixtureAvailableLink(index int, name string, flags net.Flags, operationalState string) linkObservation {
	link := fixtureLink(index, name, "device", flags)
	link.operationalState = operationalState
	return link
}

func assertAvailabilityResult(t *testing.T, got Egress, testCase struct {
	name, operationalState, wantReason string
	flags                              net.Flags
	runnable                           bool
}) {
	t.Helper()
	if got.Runnable() != testCase.runnable {
		t.Fatalf("Runnable = %t, want %t; reason=%q", got.Runnable(), testCase.runnable, got.Reason())
	}
	if testCase.wantReason != "" && !strings.Contains(got.Reason(), testCase.wantReason) {
		t.Fatalf("Reason = %q, want %q", got.Reason(), testCase.wantReason)
	}
}

func TestSnapshotKeepsFactsAndFiltersDefaultCandidates(t *testing.T) {
	links := []linkObservation{
		fixtureLink(8, "wan1", "device", net.FlagUp),
		fixtureLink(2, "wan0", "device", net.FlagUp),
		fixtureLink(1, "lo", "device", net.FlagUp|net.FlagLoopback),
		fixtureLink(4, "br0", "bridge", net.FlagUp),
		fixtureLink(5, "peer0", "veth", net.FlagUp),
		fixtureLink(6, "down0", "device", 0),
		fixtureLink(7, "port0", "device", net.FlagUp),
	}
	links[6].masterIndex = 4
	addresses := []addressObservation{
		fixtureAddress(8, "203.0.113.20/24"),
		fixtureAddress(2, "192.0.2.11/24"),
		fixtureAddress(2, "192.0.2.10/24"),
		fixtureAddress(2, "192.0.2.10/24"),
		fixtureAddress(1, "127.0.0.1/8"),
		fixtureAddress(4, "172.18.0.1/16"),
		fixtureAddress(5, "172.18.0.2/16"),
		fixtureAddress(6, "198.51.100.6/24"),
		fixtureAddress(7, "172.18.0.3/16"),
	}
	snapshot := newSnapshot(links, addresses, nil, nil)

	all := snapshot.LocalAddresses()
	if len(all) != len(addresses) {
		t.Fatalf("LocalAddresses count = %d, want all %d observations", len(all), len(addresses))
	}
	gotAll := make([]string, len(all))
	for index, address := range all {
		gotAll[index] = address.IP().String() + "@" + address.Interface().Name()
	}
	wantAll := []string{
		"127.0.0.1@lo", "172.18.0.1@br0", "172.18.0.2@peer0", "172.18.0.3@port0",
		"192.0.2.10@wan0", "192.0.2.10@wan0", "192.0.2.11@wan0", "198.51.100.6@down0",
		"203.0.113.20@wan1",
	}
	if !slices.Equal(gotAll, wantAll) {
		t.Fatalf("LocalAddresses = %v, want stable %v", gotAll, wantAll)
	}

	defaults := snapshot.DefaultCandidates()
	gotDefaults := make([]string, len(defaults))
	for index, address := range defaults {
		gotDefaults[index] = address.IP().String()
	}
	wantDefaults := []string{"192.0.2.10", "192.0.2.11", "203.0.113.20"}
	if !slices.Equal(gotDefaults, wantDefaults) {
		t.Fatalf("DefaultCandidates = %v, want %v", gotDefaults, wantDefaults)
	}
}

func TestResolvePreservesCallerOrderAndExplainsDuplicateFacts(t *testing.T) {
	links := []linkObservation{
		fixtureLink(2, "wan0", "device", net.FlagUp),
		fixtureLink(3, "wan1", "device", net.FlagUp),
	}
	addresses := []addressObservation{
		fixtureAddress(2, "192.0.2.10/24"),
		fixtureAddress(2, "192.0.2.10/24"),
		fixtureAddress(2, "192.0.2.11/24"),
	}
	snapshot := newSnapshot(links, addresses,
		[]policyRule{fixtureRule(32766, 254, "0.0.0.0/0")},
		[]fibRoute{fixtureDefault(254, 2, "192.0.2.1", routeUnicast)},
	)
	sources := []netip.Addr{mustAddr("192.0.2.11"), mustAddr("192.0.2.10"), mustAddr("192.0.2.11")}
	resolved := snapshot.Resolve(sources)
	if len(resolved) != len(sources) {
		t.Fatalf("Resolve count = %d, want %d", len(resolved), len(sources))
	}
	for index := range sources {
		if resolved[index].LocalIP() != sources[index] || !resolved[index].Runnable() {
			t.Fatalf("Resolve[%d] = %#v, want runnable %s", index, resolved[index], sources[index])
		}
	}
	if !slices.Contains(resolved[1].Diagnostics(), "duplicate address observation on wan0(ifindex=2)") {
		t.Fatalf("duplicate diagnostics = %v", resolved[1].Diagnostics())
	}

	conflict := newSnapshot(links,
		[]addressObservation{fixtureAddress(2, "192.0.2.10/24"), fixtureAddress(3, "192.0.2.10/24")},
		nil, nil,
	)
	got := oneEgress(t, conflict, "192.0.2.10")
	if got.Runnable() || !strings.Contains(got.Reason(), "multiple interfaces") {
		t.Fatalf("conflicting duplicate egress = %#v", got)
	}
}

func fixtureSnapshot(rules []policyRule, routes []fibRoute) Snapshot {
	return newSnapshot(
		[]linkObservation{fixtureLink(2, "wan0", "device", net.FlagUp)},
		[]addressObservation{fixtureAddress(2, "192.0.2.10/24")},
		rules,
		routes,
	)
}

func fixtureLink(index int, name, kind string, flags net.Flags) linkObservation {
	return linkObservation{index: index, name: name, kind: kind, flags: flags, mtu: 1500}
}

func fixtureAddress(linkIndex int, prefix string) addressObservation {
	return addressObservation{linkIndex: linkIndex, prefix: mustPrefix(prefix)}
}

func fixtureRule(priority, table int, source string) policyRule {
	return policyRule{priority: priority, table: table, source: mustPrefix(source), action: ruleLookup}
}

func fixtureAttachedInput(name string, index int, l3Master bool) inputInterfaceSelector {
	return inputInterfaceSelector{name: name, state: inputInterfaceAttached, index: index,
		isL3Master: l3Master}
}

func fixtureUnresolvedSelector(kind unresolvedRuleSelectorKind) unresolvedRuleSelector {
	return unresolvedRuleSelector{kind: kind}
}

func fixtureDefault(table, linkIndex int, gateway string, kind routeKind) fibRoute {
	route := fibRoute{table: table, destination: mustPrefix("0.0.0.0/0"), kind: kind, linkIndex: linkIndex}
	if gateway != "" {
		route.gateway = mustAddr(gateway)
		route.hasGateway = true
	}
	return route
}

func oneEgress(t *testing.T, snapshot Snapshot, source string) Egress {
	t.Helper()
	resolved := snapshot.Resolve([]netip.Addr{mustAddr(source)})
	if len(resolved) != 1 {
		t.Fatalf("Resolve returned %d entries", len(resolved))
	}
	return resolved[0]
}

func assertRunnableEgress(t *testing.T, got Egress, table int, interfaceName, gateway string) {
	t.Helper()
	if !got.Runnable() || got.Table() != table || got.Interface().Name() != interfaceName {
		t.Fatalf("egress = %#v, want runnable table=%d interface=%s", got, table, interfaceName)
	}
	wantGateway := mustAddr(gateway)
	if value, present := got.Gateway(); !present || value != wantGateway {
		t.Fatalf("Gateway = %s/%t, want %s/true", value, present, wantGateway)
	}
}

func mustAddr(value string) netip.Addr {
	return netip.MustParseAddr(value)
}

func mustPrefix(value string) netip.Prefix {
	return netip.MustParsePrefix(value)
}
