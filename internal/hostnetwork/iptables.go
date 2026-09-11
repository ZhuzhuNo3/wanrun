package hostnetwork

import (
	"bufio"
	"bytes"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

type iptablesRule struct {
	table string
	chain string
	argv  []string
	owner string
}

type iptablesProgram struct {
	ownerPrefix string
	rules       []iptablesRule
}

type iptablesMutation struct {
	owner string
	argv  []string
}

type iptablesObservation struct {
	present map[string]bool
}

func (observation iptablesObservation) count() int {
	count := 0
	for _, present := range observation.present {
		if present {
			count++
		}
	}
	return count
}

func buildIPTablesProgram(claim networkClaim) (iptablesProgram, error) {
	if err := validateClaim(claim, claim.runID); err != nil {
		return iptablesProgram{}, err
	}
	if firewallKind(claim.backend) != firewallIPTables {
		return iptablesProgram{}, fmt.Errorf("iptables program requires an iptables network claim")
	}
	ownerPrefix := "transferlanes:" + claim.runID.String() + ":" + claim.ownerToken + ":"
	program := iptablesProgram{ownerPrefix: ownerPrefix}
	for _, transfer := range claim.transfers {
		owner := fmt.Sprintf("%s%d", ownerPrefix, transfer.number)
		program.rules = append(program.rules,
			newIPTablesRule("filter", "FORWARD", owner+":forward-out",
				"-s", transfer.subnet.String(), "-i", transfer.hostVeth, "-o", transfer.providerName,
				"-j", "ACCEPT"),
			newIPTablesRule("filter", "FORWARD", owner+":return",
				"-d", transfer.subnet.String(), "-i", transfer.providerName, "-o", transfer.hostVeth,
				"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-j", "ACCEPT"),
			newIPTablesRule("filter", "FORWARD", owner+":drop-out",
				"-i", transfer.hostVeth, "-j", "DROP"),
			newIPTablesRule("filter", "FORWARD", owner+":drop-in",
				"-o", transfer.hostVeth, "-j", "DROP"),
			newIPTablesRule("nat", "POSTROUTING", owner+":snat",
				"-s", transfer.namespaceIP.String()+"/32", "-o", transfer.providerName,
				"-j", "SNAT", "--to-source", transfer.source.String()),
		)
	}
	return program, nil
}

func newIPTablesRule(table, chain, owner string, argv ...string) iptablesRule {
	comment := []string{"-m", "comment", "--comment", owner}
	jump := slices.Index(argv, "-j")
	if jump < 0 {
		return iptablesRule{table: table, chain: chain, argv: append(argv, comment...), owner: owner}
	}
	result := append([]string(nil), argv[:jump]...)
	result = append(result, comment...)
	result = append(result, argv[jump:]...)
	return iptablesRule{table: table, chain: chain, argv: result, owner: owner}
}

// Rules are inserted at position one in reverse desired order. This makes the
// complete Transfer Lanes program precede the pre-existing chain without reordering it.
func buildIPTablesInstallCommands(claim networkClaim) ([][]string, error) {
	mutations, err := buildIPTablesInstallMutations(claim)
	if err != nil {
		return nil, err
	}
	commands := make([][]string, 0, len(mutations))
	for _, mutation := range mutations {
		commands = append(commands, mutation.argv)
	}
	return commands, nil
}

func buildIPTablesInstallMutations(claim networkClaim) ([]iptablesMutation, error) {
	program, err := buildIPTablesProgram(claim)
	if err != nil {
		return nil, err
	}
	var mutations []iptablesMutation
	for _, table := range []string{"filter", "nat"} {
		for index := len(program.rules) - 1; index >= 0; index-- {
			rule := program.rules[index]
			if rule.table != table {
				continue
			}
			command := iptablesTableCommand(rule.table, "-I")
			command = append(command, rule.chain, "1")
			mutations = append(mutations, iptablesMutation{owner: rule.owner,
				argv: append(command, rule.argv...)})
		}
	}
	return mutations, nil
}

func iptablesExpectedTableLines(program iptablesProgram, table string) [][]string {
	var result [][]string
	for _, rule := range program.rules {
		if rule.table == table {
			result = append(result, append([]string{"-A", rule.chain}, rule.argv...))
		}
	}
	return result
}

func observeIPTablesProgram(filterOutput, natOutput []byte, program iptablesProgram,
	allowPartial bool,
) (iptablesObservation, error) {
	observation := iptablesObservation{present: make(map[string]bool, len(program.rules))}
	for _, rule := range program.rules {
		observation.present[rule.owner] = false
	}
	for _, table := range []struct {
		name   string
		output []byte
	}{{"filter", filterOutput}, {"nat", natOutput}} {
		if err := observeIPTablesTable(table.output, table.name, program, observation.present); err != nil {
			return iptablesObservation{}, fmt.Errorf("observe iptables %s ownership: %w", table.name, err)
		}
	}
	if !allowPartial {
		if observation.count() != len(program.rules) {
			return iptablesObservation{}, fmt.Errorf("owned iptables program is incomplete")
		}
		if err := requireLeadingIPTablesProgram(filterOutput, natOutput, program); err != nil {
			return iptablesObservation{}, err
		}
	}
	return observation, nil
}

func observeIPTablesTable(output []byte, table string, program iptablesProgram,
	present map[string]bool,
) error {
	actual, err := parseIPTablesLines(output)
	if err != nil {
		return err
	}
	expected := make(map[string][]string)
	for _, rule := range program.rules {
		if rule.table == table {
			expected[rule.owner] = append([]string{"-A", rule.chain}, rule.argv...)
		}
	}
	for _, line := range actual {
		if len(line) < 2 || line[0] != "-A" {
			continue
		}
		owner := iptablesOption(line[2:], "--comment")
		if !strings.HasPrefix(owner, program.ownerPrefix) {
			continue
		}
		want, known := expected[owner]
		if !known {
			return fmt.Errorf("unknown rule has the claim owner prefix %q", owner)
		}
		if present[owner] {
			return fmt.Errorf("owned rule %q appears more than once", owner)
		}
		if !slices.Equal(line, want) {
			return fmt.Errorf("owned rule %q has conflicting shape: actual=%q expected=%q", owner, line, want)
		}
		present[owner] = true
	}
	return nil
}

func iptablesOption(argv []string, name string) string {
	for index := 0; index+1 < len(argv); index++ {
		if argv[index] == name {
			return argv[index+1]
		}
	}
	return ""
}

func requireLeadingIPTablesProgram(filterOutput, natOutput []byte, program iptablesProgram) error {
	for _, table := range []struct {
		name   string
		output []byte
	}{{"filter", filterOutput}, {"nat", natOutput}} {
		actual, err := parseIPTablesLines(table.output)
		if err != nil {
			return err
		}
		chain := "FORWARD"
		if table.name == "nat" {
			chain = "POSTROUTING"
		}
		var chainRules [][]string
		for _, line := range actual {
			if len(line) >= 2 && line[0] == "-A" && line[1] == chain {
				chainRules = append(chainRules, line)
			}
		}
		expected := iptablesExpectedTableLines(program, table.name)
		if len(chainRules) < len(expected) || !slices.EqualFunc(chainRules[:len(expected)], expected,
			func(left, right []string) bool { return slices.Equal(left, right) }) {
			return fmt.Errorf("owned iptables %s rules are not the leading exact program", table.name)
		}
	}
	return nil
}

func observePublishedIPTablesPrograms(filterOutput, natOutput []byte,
	programs []iptablesProgram,
) ([]bool, error) {
	present := make([]bool, len(programs))
	var complete []indexedIPTablesProgram
	for index, program := range programs {
		observation, err := observeIPTablesProgram(filterOutput, natOutput, program, true)
		if err != nil {
			return nil, fmt.Errorf("observe published iptables program %q: %w", program.ownerPrefix, err)
		}
		switch observation.count() {
		case 0:
			continue
		case len(program.rules):
			present[index] = true
			complete = append(complete, indexedIPTablesProgram{index: index, program: program})
		default:
			return nil, fmt.Errorf("published iptables program %q is incomplete", program.ownerPrefix)
		}
	}
	filterOrder, err := leadingIPTablesProgramOrder(filterOutput, "filter", "FORWARD", complete)
	if err != nil {
		return nil, err
	}
	natOrder, err := leadingIPTablesProgramOrder(natOutput, "nat", "POSTROUTING", complete)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(filterOrder, natOrder) {
		return nil, fmt.Errorf("published iptables filter and nat programs have different owner order")
	}
	return present, nil
}

type indexedIPTablesProgram struct {
	index   int
	program iptablesProgram
}

func leadingIPTablesProgramOrder(output []byte, table, chain string,
	programs []indexedIPTablesProgram,
) ([]int, error) {
	if len(programs) == 0 {
		return nil, nil
	}
	actual, err := parseIPTablesLines(output)
	if err != nil {
		return nil, err
	}
	chainRules := iptablesChainRules(actual, chain)
	remaining := append([]indexedIPTablesProgram(nil), programs...)
	order := make([]int, 0, len(programs))
	cursor := 0
	for len(remaining) > 0 {
		match := slices.IndexFunc(remaining, func(candidate indexedIPTablesProgram) bool {
			expected := iptablesExpectedTableLines(candidate.program, table)
			return len(expected) > 0 && len(chainRules)-cursor >= len(expected) &&
				slices.EqualFunc(chainRules[cursor:cursor+len(expected)], expected,
					func(left, right []string) bool { return slices.Equal(left, right) })
		})
		if match < 0 {
			return nil, fmt.Errorf("published iptables %s rules are not the leading exact programs", table)
		}
		matched := remaining[match]
		cursor += len(iptablesExpectedTableLines(matched.program, table))
		order = append(order, matched.index)
		remaining = slices.Delete(remaining, match, match+1)
	}
	return order, nil
}

func iptablesChainRules(lines [][]string, chain string) [][]string {
	var result [][]string
	for _, line := range lines {
		if len(line) >= 2 && line[0] == "-A" && line[1] == chain {
			result = append(result, line)
		}
	}
	return result
}

func buildIPTablesRemoveCommands(program iptablesProgram, observation iptablesObservation) [][]string {
	return buildIPTablesRemoveCommandsFor(program, observation, nil)
}

func buildIPTablesRemoveCommandsFor(program iptablesProgram, observation iptablesObservation,
	receipts *firewallReceipts) [][]string {
	var result [][]string
	for _, rule := range program.rules {
		if observation.present[rule.owner] && (receipts == nil || receipts.has(rule.owner)) {
			result = append(result, iptablesRuleCommand(rule.table, "-D", rule.chain, rule.argv))
		}
	}
	return result
}

func parseIPTablesLines(output []byte) ([][]string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var result [][]string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "*") || line == "COMMIT" {
			continue
		}
		if strings.HasPrefix(line, ":") {
			declaration, err := parseIPTablesSaveChain(line)
			if err != nil {
				return nil, err
			}
			if declaration != nil {
				result = append(result, declaration)
			}
			continue
		}
		argv, err := splitIPTablesLine(line)
		if err != nil {
			return nil, err
		}
		result = append(result, argv)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan iptables output: %w", err)
	}
	return result, nil
}

func parseIPTablesSaveChain(line string) ([]string, error) {
	fields := strings.Fields(strings.TrimPrefix(line, ":"))
	if len(fields) < 2 || fields[0] == "" {
		return nil, fmt.Errorf("malformed iptables-save chain declaration %q", line)
	}
	if fields[1] != "-" {
		return nil, nil
	}
	return []string{"-N", fields[0]}, nil
}

func splitIPTablesLine(line string) ([]string, error) {
	var result []string
	var word strings.Builder
	quote := rune(0)
	escaped := false
	flush := func() {
		if word.Len() > 0 {
			result = append(result, word.String())
			word.Reset()
		}
	}
	for _, character := range line {
		if escaped {
			word.WriteRune(character)
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				word.WriteRune(character)
			}
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			continue
		}
		if unicode.IsSpace(character) {
			flush()
			continue
		}
		word.WriteRune(character)
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("unterminated iptables quoting")
	}
	flush()
	if len(result) == 0 {
		return nil, fmt.Errorf("empty iptables command line")
	}
	return result, nil
}

func iptablesTableCommand(table, operation string) []string {
	return []string{"-w", "5", "-t", table, operation}
}

func iptablesChainCommand(table, operation, chain string) []string {
	return append(iptablesTableCommand(table, operation), chain)
}

func iptablesRuleCommand(table, operation, chain string, rule []string) []string {
	return append(iptablesChainCommand(table, operation, chain), rule...)
}

func validateIPTablesProviderNames(transfers []transferAllocation) error {
	for _, transfer := range transfers {
		if strings.HasSuffix(transfer.providerName, "+") || strings.HasSuffix(transfer.hostVeth, "+") {
			return fmt.Errorf("transfer %d interface is not an exact iptables identity", transfer.number)
		}
	}
	return nil
}
