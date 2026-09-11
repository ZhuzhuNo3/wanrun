package hostnetwork

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

func buildFixtureIPTablesPrograms(t *testing.T, claims ...networkClaim) []iptablesProgram {
	t.Helper()
	programs := make([]iptablesProgram, 0, len(claims))
	for _, claim := range claims {
		program, err := buildIPTablesProgram(claim)
		if err != nil {
			t.Fatal(err)
		}
		programs = append(programs, program)
	}
	return programs
}

func nftInventoryResponses() []argvResponse {
	return []argvResponse{
		{output: []byte("iptables v1.8.9 (nf_tables)\n")},
		{output: []byte("iptables-save v1.8.9 (nf_tables)\n")},
		{output: []byte("iptables v1.8.9 (nf_tables)\n")},
		{output: []byte("iptables-save v1.8.9 (nf_tables)\n")},
		{output: []byte("*filter\n:FORWARD DROP [0:0]\nCOMMIT\n")},
		{output: []byte("*nat\n:POSTROUTING ACCEPT [0:0]\nCOMMIT\n")},
		{}, {}, {},
	}
}

func fixedFrontendResponses(frontend iptablesFrontend) []argvResponse {
	marker := "(nf_tables)"
	if frontend == iptablesFrontendLegacy {
		marker = "(legacy)"
	}
	return []argvResponse{
		{output: []byte("iptables v1.8.9 " + marker + "\n")},
		{output: []byte("iptables-save v1.8.9 " + marker + "\n")},
	}
}

func assertOnlyFixedFrontendCommands(t *testing.T, calls []directCall, frontend iptablesFrontend) {
	t.Helper()
	rules, save := iptablesNFTRulesCommand, iptablesNFTSaveCommand
	if frontend == iptablesFrontendLegacy {
		rules, save = iptablesLegacyRulesCommand, iptablesLegacySaveCommand
	}
	for _, call := range calls {
		if call.name != rules && call.name != save {
			t.Fatalf("selected %s lifecycle called %s %q", frontend, call.name, call.argv)
		}
	}
}

func iptablesFixtureClaim(t *testing.T, value string) networkClaim {
	t.Helper()
	id := mustRunID(t, value)
	allocation, err := allocateClaimNetwork(id, testOwnerToken, testSelections(), emptyIPTablesInventory())
	if err != nil {
		t.Fatal(err)
	}
	return claimFromAllocation(allocation)
}

func testIPTablesFirewall(runner directArgvRunner) iptablesFirewall {
	return iptablesFirewall{activeRulesCommand: "iptables", activeSaveCommand: "iptables-save", runner: runner,
		legacyTables: staticLegacyIPTablesTables{}}
}

type staticLegacyIPTablesTables []string

func (tables staticLegacyIPTablesTables) Names() (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(tables))
	for _, name := range tables {
		result[name] = struct{}{}
	}
	return result, nil
}

type failingLegacyIPTablesTables struct{ err error }

func (tables failingLegacyIPTablesTables) Names() (map[string]struct{}, error) {
	return nil, tables.err
}

func assertArgvPresent(t *testing.T, commands [][]string, expected []string) {
	t.Helper()
	if !slices.ContainsFunc(commands, func(command []string) bool { return slices.Equal(command, expected) }) {
		t.Fatalf("iptables commands do not contain\n%q\nall commands:\n%q", expected, commands)
	}
}

func renderIPTablesLines(lines [][]string) []string {
	result := make([]string, len(lines))
	for index := range lines {
		result[index] = strings.Join(lines[index], " ")
	}
	return result
}

func cloneArgvLines(lines [][]string) [][]string {
	result := make([][]string, len(lines))
	for index := range lines {
		result[index] = cloneArgv(lines[index])
	}
	return result
}

func cloneArgv(argv []string) []string { return append([]string(nil), argv...) }

type directCall struct {
	name string
	argv []string
}

type argvResponse struct {
	output []byte
	err    error
}

type scriptedArgvRunner struct {
	calls     []directCall
	responses []argvResponse
}

func (runner *scriptedArgvRunner) Run(_ context.Context, name string, argv ...string) ([]byte, error) {
	runner.calls = append(runner.calls, directCall{name: name, argv: cloneArgv(argv)})
	if len(runner.responses) == 0 {
		return nil, errors.New("unexpected direct argv command")
	}
	response := runner.responses[0]
	runner.responses = runner.responses[1:]
	return response.output, response.err
}

type mutableIPTablesRulePlane struct {
	frontend     iptablesFrontend
	filter       [][]string
	nat          [][]string
	foreign      [][]string
	mutations    int
	observations int
}

func newMutableIPTablesRulePlane(frontend iptablesFrontend) *mutableIPTablesRulePlane {
	foreign := [][]string{
		{"-A", "FORWARD", "-s", "203.0.113.1/32", "-j", "DROP"},
		{"-A", "POSTROUTING", "-s", "203.0.113.2/32", "-j", "MASQUERADE"},
	}
	return &mutableIPTablesRulePlane{frontend: frontend, filter: cloneArgvLines(foreign[:1]),
		nat: cloneArgvLines(foreign[1:]), foreign: cloneArgvLines(foreign)}
}

func (runner *mutableIPTablesRulePlane) Run(_ context.Context, name string, argv ...string) ([]byte, error) {
	if slices.Equal(argv, []string{"--version"}) {
		marker := "(nf_tables)"
		if runner.frontend == iptablesFrontendLegacy {
			marker = "(legacy)"
		}
		return []byte(name + " v1.8.9 " + marker + "\n"), nil
	}
	if strings.HasSuffix(name, "-save") {
		if len(argv) != 2 || argv[0] != "-t" {
			return nil, fmt.Errorf("unexpected save argv %q", argv)
		}
		if argv[1] == "filter" {
			runner.observations++
			return []byte(strings.Join(renderIPTablesLines(runner.filter), "\n")), nil
		}
		return []byte(strings.Join(renderIPTablesLines(runner.nat), "\n")), nil
	}
	if len(argv) < 7 || !slices.Equal(argv[:3], []string{"-w", "5", "-t"}) {
		return nil, fmt.Errorf("unexpected mutation argv %q", argv)
	}
	table, operation, chain := argv[3], argv[4], argv[5]
	var rule []string
	switch operation {
	case "-I":
		if argv[6] != "1" {
			return nil, fmt.Errorf("unexpected insertion position %q", argv)
		}
		rule = append([]string{"-A", chain}, argv[7:]...)
	case "-D":
		rule = append([]string{"-A", chain}, argv[6:]...)
	default:
		return nil, fmt.Errorf("unexpected mutation operation %q", operation)
	}
	rules := &runner.filter
	if table == "nat" {
		rules = &runner.nat
	}
	if operation == "-I" {
		*rules = append([][]string{rule}, *rules...)
	} else {
		index := slices.IndexFunc(*rules, func(actual []string) bool { return slices.Equal(actual, rule) })
		if index < 0 {
			return nil, fmt.Errorf("delete missing rule %q", rule)
		}
		*rules = append((*rules)[:index], (*rules)[index+1:]...)
	}
	runner.mutations++
	return nil, nil
}

type uncertainSecondInsertRulePlane struct {
	*mutableIPTablesRulePlane
	inserts int
}

func newUncertainSecondInsertRulePlane(frontend iptablesFrontend) *uncertainSecondInsertRulePlane {
	return &uncertainSecondInsertRulePlane{mutableIPTablesRulePlane: newMutableIPTablesRulePlane(frontend)}
}

func (runner *uncertainSecondInsertRulePlane) Run(ctx context.Context, name string, argv ...string) ([]byte, error) {
	output, err := runner.mutableIPTablesRulePlane.Run(ctx, name, argv...)
	if err == nil && len(argv) >= 5 && argv[4] == "-I" {
		runner.inserts++
		if runner.inserts == 2 {
			return output, errors.New("injected iptables mutation result uncertainty")
		}
	}
	return output, err
}

type secondFilterObservationFailureRulePlane struct {
	*mutableIPTablesRulePlane
	filterReads int
}

func newSecondFilterObservationFailureRulePlane(frontend iptablesFrontend) *secondFilterObservationFailureRulePlane {
	return &secondFilterObservationFailureRulePlane{mutableIPTablesRulePlane: newMutableIPTablesRulePlane(frontend)}
}

func (runner *secondFilterObservationFailureRulePlane) Run(ctx context.Context, name string, argv ...string) ([]byte, error) {
	if strings.HasSuffix(name, "-save") && slices.Equal(argv, []string{"-t", "filter"}) {
		runner.filterReads++
		if runner.filterReads == 2 {
			return nil, errors.New("injected iptables observation failure")
		}
	}
	return runner.mutableIPTablesRulePlane.Run(ctx, name, argv...)
}
