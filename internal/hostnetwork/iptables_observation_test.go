package hostnetwork

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestIPTablesObservationAcceptsExactAndPartialDirectRules(t *testing.T) {
	claim := iptablesFixtureClaim(t, "2c112233445566778899aabbccddeeff")
	program, err := buildIPTablesProgram(claim)
	if err != nil {
		t.Fatal(err)
	}
	filter, nat := iptablesExpectedTableLines(program, "filter"), iptablesExpectedTableLines(program, "nat")
	foreign := []string{"-A FORWARD -s 203.0.113.1/32 -j ACCEPT"}
	if _, err := observeIPTablesProgram([]byte(strings.Join(append(renderIPTablesLines(filter), foreign...), "\n")),
		[]byte(strings.Join(renderIPTablesLines(nat), "\n")), program, false); err != nil {
		t.Fatalf("observe exact iptables topology: %v", err)
	}
	partialFilter := filter[:2]
	if _, err := observeIPTablesProgram([]byte(strings.Join(renderIPTablesLines(partialFilter), "\n")),
		nil, program, true); err != nil {
		t.Fatalf("observe exact partial iptables topology: %v", err)
	}
	if _, err := observeIPTablesProgram([]byte(strings.Join(renderIPTablesLines(partialFilter), "\n")),
		nil, program, false); err == nil {
		t.Fatal("partial iptables topology passed complete verification")
	}
}

func TestIPTablesObservationRejectsDuplicateUnknownAndWrongShapeOwnerRules(t *testing.T) {
	claim := iptablesFixtureClaim(t, "2d112233445566778899aabbccddeeff")
	program, err := buildIPTablesProgram(claim)
	if err != nil {
		t.Fatal(err)
	}
	filter, nat := iptablesExpectedTableLines(program, "filter"), iptablesExpectedTableLines(program, "nat")
	wrongShape := cloneArgvLines(filter)
	wrongShape[0][2] = "203.0.113.99/32"
	cases := map[string][][]string{
		"unknown owner suffix": append(cloneArgvLines(filter), []string{"-A", "FORWARD", "-m", "comment",
			"--comment", program.ownerPrefix + "1:unknown", "-j", "ACCEPT"}),
		"duplicate exact owner": append(cloneArgvLines(filter), cloneArgv(filter[0])),
		"wrong exact shape":     wrongShape,
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := observeIPTablesProgram([]byte(strings.Join(renderIPTablesLines(corrupt), "\n")),
				[]byte(strings.Join(renderIPTablesLines(nat), "\n")), program, true); err == nil {
				t.Fatal("foreign or extra iptables ownership was accepted")
			}
		})
	}
}

func TestIPTablesCompleteObservationRequiresLeadingDirectRules(t *testing.T) {
	claim := iptablesFixtureClaim(t, "2d312233445566778899aabbccddeeff")
	program, err := buildIPTablesProgram(claim)
	if err != nil {
		t.Fatal(err)
	}
	filter := iptablesExpectedTableLines(program, "filter")
	nat := iptablesExpectedTableLines(program, "nat")
	filter = append([][]string{{"-A", "FORWARD", "-s", "203.0.113.1/32", "-j", "DROP"}}, filter...)
	if _, err := observeIPTablesProgram([]byte(strings.Join(renderIPTablesLines(filter), "\n")),
		[]byte(strings.Join(renderIPTablesLines(nat), "\n")), program, false); err == nil {
		t.Fatal("iptables rules below a foreign FORWARD rule passed complete verification")
	}
	if _, err := observeIPTablesProgram([]byte(strings.Join(renderIPTablesLines(filter), "\n")),
		[]byte(strings.Join(renderIPTablesLines(nat), "\n")), program, true); err != nil {
		t.Fatalf("cleanup rejected exact owned rules after host ordering changed: %v", err)
	}
}

func TestPublishedIPTablesObservationAcceptsCompleteOwnerProgramsInKernelOrder(t *testing.T) {
	older := iptablesFixtureClaim(t, "2d412233445566778899aabbccddeeff")
	newer := iptablesFixtureClaim(t, "2d512233445566778899aabbccddeeff")
	programs := buildFixtureIPTablesPrograms(t, older, newer)
	filter := append(iptablesExpectedTableLines(programs[1], "filter"),
		iptablesExpectedTableLines(programs[0], "filter")...)
	filter = append(filter, []string{"-A", "FORWARD", "-s", "203.0.113.1/32", "-j", "ACCEPT"})
	nat := append(iptablesExpectedTableLines(programs[1], "nat"),
		iptablesExpectedTableLines(programs[0], "nat")...)

	present, err := observePublishedIPTablesPrograms(
		[]byte(strings.Join(renderIPTablesLines(filter), "\n")),
		[]byte(strings.Join(renderIPTablesLines(nat), "\n")), programs)
	if err != nil {
		t.Fatalf("observe two complete published programs: %v", err)
	}
	if !reflect.DeepEqual(present, []bool{true, true}) {
		t.Fatalf("published program presence = %v, want both present", present)
	}
}

func TestPublishedIPTablesObservationRejectsUnclaimedLeadingOrInconsistentProgramOrder(t *testing.T) {
	older := iptablesFixtureClaim(t, "2d612233445566778899aabbccddeeff")
	newer := iptablesFixtureClaim(t, "2d712233445566778899aabbccddeeff")
	programs := buildFixtureIPTablesPrograms(t, older, newer)
	newerThenOlderFilter := append(iptablesExpectedTableLines(programs[1], "filter"),
		iptablesExpectedTableLines(programs[0], "filter")...)
	newerThenOlderNAT := append(iptablesExpectedTableLines(programs[1], "nat"),
		iptablesExpectedTableLines(programs[0], "nat")...)
	olderThenNewerNAT := append(iptablesExpectedTableLines(programs[0], "nat"),
		iptablesExpectedTableLines(programs[1], "nat")...)

	foreignLeading := append([][]string{{"-A", "FORWARD", "-s", "203.0.113.1/32", "-j", "DROP"}},
		newerThenOlderFilter...)
	if _, err := observePublishedIPTablesPrograms(
		[]byte(strings.Join(renderIPTablesLines(foreignLeading), "\n")),
		[]byte(strings.Join(renderIPTablesLines(newerThenOlderNAT), "\n")), programs); err == nil {
		t.Fatal("published iptables programs below an unclaimed leading rule were accepted")
	}
	if _, err := observePublishedIPTablesPrograms(
		[]byte(strings.Join(renderIPTablesLines(newerThenOlderFilter), "\n")),
		[]byte(strings.Join(renderIPTablesLines(olderThenNewerNAT), "\n")), programs); err == nil {
		t.Fatal("published iptables programs with different filter and NAT order were accepted")
	}
}

func TestPublishedIPTablesObservationKeepsAbsentClaimsSeparateFromCompletePrograms(t *testing.T) {
	absent := iptablesFixtureClaim(t, "2d812233445566778899aabbccddeeff")
	presentClaim := iptablesFixtureClaim(t, "2d912233445566778899aabbccddeeff")
	programs := buildFixtureIPTablesPrograms(t, absent, presentClaim)
	filter := iptablesExpectedTableLines(programs[1], "filter")
	nat := iptablesExpectedTableLines(programs[1], "nat")

	present, err := observePublishedIPTablesPrograms(
		[]byte(strings.Join(renderIPTablesLines(filter), "\n")),
		[]byte(strings.Join(renderIPTablesLines(nat), "\n")), programs)
	if err != nil {
		t.Fatalf("observe one absent and one complete published program: %v", err)
	}
	if !reflect.DeepEqual(present, []bool{false, true}) {
		t.Fatalf("published program presence = %v, want absent/complete", present)
	}
}

func TestIPTablesBackendObservesPublishedProgramsFromOneRulePlaneSnapshot(t *testing.T) {
	older := iptablesFixtureClaim(t, "2da12233445566778899aabbccddeeff")
	newer := iptablesFixtureClaim(t, "2db12233445566778899aabbccddeeff")
	programs := buildFixtureIPTablesPrograms(t, older, newer)
	filter := append(iptablesExpectedTableLines(programs[1], "filter"),
		iptablesExpectedTableLines(programs[0], "filter")...)
	nat := append(iptablesExpectedTableLines(programs[1], "nat"),
		iptablesExpectedTableLines(programs[0], "nat")...)
	runner := &scriptedArgvRunner{responses: []argvResponse{
		{output: []byte("iptables v1.8.9 (nf_tables)\n")},
		{output: []byte("iptables-save v1.8.9 (nf_tables)\n")},
		{output: []byte(strings.Join(renderIPTablesLines(filter), "\n"))},
		{output: []byte(strings.Join(renderIPTablesLines(nat), "\n"))},
	}}

	present, err := testIPTablesFirewall(runner).ObservePublished(
		context.Background(), []networkClaim{older, newer})
	if err != nil {
		t.Fatalf("observe published iptables claims: %v", err)
	}
	if !reflect.DeepEqual(present, []bool{true, true}) {
		t.Fatalf("published claim presence = %v, want both present", present)
	}
	if len(runner.calls) != 4 || runner.calls[2].name != iptablesNFTSaveCommand ||
		runner.calls[3].name != iptablesNFTSaveCommand {
		t.Fatalf("published observation did not use one rule-plane snapshot: %q", runner.calls)
	}
}

func TestIPTablesObservationIgnoresIndependentForeignComments(t *testing.T) {
	claim := iptablesFixtureClaim(t, "2d212233445566778899aabbccddeeff")
	program, err := buildIPTablesProgram(claim)
	if err != nil {
		t.Fatal(err)
	}
	filter := iptablesExpectedTableLines(program, "filter")
	filter = append(filter, []string{"-A", "FORWARD", "-m", "comment", "--comment",
		"transferlanes:" + claim.runID.String() + ":" + strings.Repeat("f", 64) + ":1:forward-out", "-j", "DROP"})
	if _, err := observeIPTablesProgram([]byte(strings.Join(renderIPTablesLines(filter), "\n")),
		[]byte(strings.Join(renderIPTablesLines(iptablesExpectedTableLines(program, "nat")), "\n")),
		program, true); err != nil {
		t.Fatalf("independent foreign owner token was treated as this claim: %v", err)
	}
}

func TestIPTablesRemovalUsesOnlyObservedExactObjects(t *testing.T) {
	claim := iptablesFixtureClaim(t, "2e112233445566778899aabbccddeeff")
	program, err := buildIPTablesProgram(claim)
	if err != nil {
		t.Fatal(err)
	}
	filter := iptablesExpectedTableLines(program, "filter")
	partial := [][]string{filter[0], filter[2]}
	observation, err := observeIPTablesProgram([]byte(strings.Join(renderIPTablesLines(partial), "\n")), nil,
		program, true)
	if err != nil {
		t.Fatal(err)
	}
	commands := buildIPTablesRemoveCommands(program, observation)
	want := [][]string{
		iptablesRuleCommand("filter", "-D", "FORWARD", filter[0][2:]),
		iptablesRuleCommand("filter", "-D", "FORWARD", filter[2][2:]),
	}
	if !reflect.DeepEqual(commands, want) {
		t.Fatalf("partial cleanup commands =\n%q\nwant\n%q", commands, want)
	}
}
