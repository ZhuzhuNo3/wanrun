//go:build linux

package hostnetwork

import (
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

func resolveNFTForwardChains(connection *nftables.Conn,
	program nftProgram,
) (map[nftForwardChain]*nftables.Chain, error) {
	targets := nftForwardTargets(program.forwardRules)
	current, err := activeNFTForwardChains(connection)
	if err != nil {
		return nil, err
	}
	if !slices.Equal(current, targets) {
		return nil, fmt.Errorf("nftables forward targets changed before mutation: current=%v claimed=%v",
			current, targets)
	}
	resolved := make(map[nftForwardChain]*nftables.Chain, len(program.forwardRules))
	for _, forward := range program.forwardRules {
		if _, exists := resolved[forward.target]; exists {
			continue
		}
		chain, err := findNFTForwardChain(connection, forward.target)
		if err != nil {
			return nil, err
		}
		if !isNFTForwardBaseChain(chain) {
			return nil, fmt.Errorf("nftables forward target %s/%s/%s changed identity",
				forward.target.Family, forward.target.Table, forward.target.Name)
		}
		resolved[forward.target] = chain
	}
	return resolved, nil
}

func nftForwardTargets(rules []nftForwardRule) []nftForwardChain {
	var result []nftForwardChain
	for _, forward := range rules {
		if len(result) == 0 || result[len(result)-1] != forward.target {
			result = append(result, forward.target)
		}
	}
	return result
}

func activeNFTForwardChains(connection *nftables.Conn) ([]nftForwardChain, error) {
	var result []nftForwardChain
	for _, family := range nftIPv4Families() {
		chains, err := connection.ListChainsOfTableFamily(family)
		if err != nil {
			return nil, fmt.Errorf("list nftables %s chains: %w", nftFamilyName(family), err)
		}
		for _, chain := range chains {
			if chain == nil || chain.Table == nil || chain.Name == "" || isTransferLanesNFTTable(chain.Table) {
				continue
			}
			rules, err := connection.GetRules(chain.Table, chain)
			if err != nil {
				return nil, fmt.Errorf("list nftables rules for %s/%s: %w", chain.Table.Name, chain.Name, err)
			}
			if nftForwardChainCanReject(chain, rules) {
				result = append(result, nftForwardChain{Family: nftFamilyName(family),
					Table: chain.Table.Name, Name: chain.Name})
			}
		}
	}
	sort.Slice(result, func(left, right int) bool { return lessNFTForwardChain(result[left], result[right]) })
	return result, nil
}

func findNFTForwardChain(connection *nftables.Conn,
	target nftForwardChain,
) (*nftables.Chain, error) {
	family, err := nftTableFamily(target.Family)
	if err != nil {
		return nil, err
	}
	chains, err := connection.ListChainsOfTableFamily(family)
	if err != nil {
		return nil, fmt.Errorf("list nftables %s chains: %w", target.Family, err)
	}
	var found *nftables.Chain
	for _, chain := range chains {
		if chain.Table != nil && chain.Table.Name == target.Table && chain.Name == target.Name {
			if found != nil {
				return nil, fmt.Errorf("duplicate nftables forward target %s/%s/%s",
					target.Family, target.Table, target.Name)
			}
			found = chain
		}
	}
	if found == nil {
		return nil, fmt.Errorf("nftables forward target %s/%s/%s is absent",
			target.Family, target.Table, target.Name)
	}
	return found, nil
}

func insertNFTForwardRules(connection *nftables.Conn, chains map[nftForwardChain]*nftables.Chain,
	rules []nftForwardRule,
) {
	for index := len(rules) - 1; index >= 0; index-- {
		forward := rules[index]
		chain := chains[forward.target]
		connection.InsertRule(&nftables.Rule{Table: chain.Table, Chain: chain,
			Exprs: nftExpressions(forward.rule), UserData: []byte(forward.rule.owner)})
	}
}

type expectedNFTForwardRule struct {
	target      nftForwardChain
	expressions []expr.Any
	position    int
}

func observeNFTForwardRules(connection *nftables.Conn, program nftProgram,
	allowPartial bool,
) ([]*nftables.Rule, error) {
	expected := expectedNFTForwardRules(program.forwardRules)
	found := make(map[string]struct{}, len(expected))
	var owned []*nftables.Rule
	for _, family := range nftIPv4Families() {
		chains, err := connection.ListChainsOfTableFamily(family)
		if err != nil {
			return nil, fmt.Errorf("list nftables %s chains: %w", nftFamilyName(family), err)
		}
		for _, chain := range chains {
			if chain == nil || chain.Table == nil || chain.Name == "" ||
				chain.Table.Family == nftables.TableFamilyIPv4 && chain.Table.Name == program.table {
				continue
			}
			rules, err := connection.GetRules(chain.Table, chain)
			if err != nil {
				return nil, fmt.Errorf("list nftables rules for %s/%s: %w", chain.Table.Name, chain.Name, err)
			}
			matched, err := matchNFTForwardRules(chain, rules, program.ownerPrefix, expected, found, allowPartial)
			if err != nil {
				return nil, err
			}
			owned = append(owned, matched...)
		}
	}
	if !allowPartial && len(found) != len(expected) {
		return nil, fmt.Errorf("nftables forward ownership is incomplete: found=%d expected=%d",
			len(found), len(expected))
	}
	return owned, nil
}

func expectedNFTForwardRules(rules []nftForwardRule) map[string]expectedNFTForwardRule {
	positions := make(map[nftForwardChain]int)
	result := make(map[string]expectedNFTForwardRule, len(rules))
	for _, forward := range rules {
		result[forward.rule.owner] = expectedNFTForwardRule{target: forward.target,
			expressions: nftExpressions(forward.rule), position: positions[forward.target]}
		positions[forward.target]++
	}
	return result
}

func matchNFTForwardRules(chain *nftables.Chain, rules []*nftables.Rule, ownerPrefix string,
	expected map[string]expectedNFTForwardRule, found map[string]struct{}, allowPartial bool,
) ([]*nftables.Rule, error) {
	target := nftForwardChain{Family: nftFamilyName(chain.Table.Family), Table: chain.Table.Name, Name: chain.Name}
	if !allowPartial && hasExpectedNFTForwardTarget(expected, target) && !isNFTForwardBaseChain(chain) {
		return nil, fmt.Errorf("nftables forward target %s/%s/%s changed identity",
			target.Family, target.Table, target.Name)
	}
	var matched []*nftables.Rule
	for index, rule := range rules {
		owner := string(rule.UserData)
		if !strings.HasPrefix(owner, ownerPrefix) {
			continue
		}
		wanted, exists := expected[owner]
		if !exists || wanted.target != target || !reflect.DeepEqual(rule.Exprs, wanted.expressions) {
			return nil, fmt.Errorf("nftables forward rule %s identity or content mismatch", owner)
		}
		if _, duplicate := found[owner]; duplicate {
			return nil, fmt.Errorf("nftables forward rule %s is duplicated", owner)
		}
		if !allowPartial && index != wanted.position {
			return nil, fmt.Errorf("nftables forward rule %s is not at its claimed leading position", owner)
		}
		found[owner] = struct{}{}
		matched = append(matched, rule)
	}
	return matched, nil
}

func hasExpectedNFTForwardTarget(expected map[string]expectedNFTForwardRule, target nftForwardChain) bool {
	for _, rule := range expected {
		if rule.target == target {
			return true
		}
	}
	return false
}

func verifyPublishedNFTForwardRules(programs []nftProgram) error {
	expected := publishedNFTForwardRules(programs)
	if len(expected) == 0 {
		return nil
	}
	connection := &nftables.Conn{}
	for target, rules := range expected {
		chain, err := findNFTForwardChain(connection, target)
		if err != nil {
			return err
		}
		actual, err := connection.GetRules(chain.Table, chain)
		if err != nil {
			return fmt.Errorf("list nftables rules for %s/%s: %w", chain.Table.Name, chain.Name, err)
		}
		if err := requireLeadingPublishedNFTRules(target, chain, actual, rules); err != nil {
			return err
		}
	}
	return nil
}

func publishedNFTForwardRules(programs []nftProgram) map[nftForwardChain]map[string][]expr.Any {
	result := make(map[nftForwardChain]map[string][]expr.Any)
	for _, program := range programs {
		for _, forward := range program.forwardRules {
			rules := result[forward.target]
			if rules == nil {
				rules = make(map[string][]expr.Any)
				result[forward.target] = rules
			}
			rules[forward.rule.owner] = nftExpressions(forward.rule)
		}
	}
	return result
}

func requireLeadingPublishedNFTRules(target nftForwardChain, chain *nftables.Chain,
	actual []*nftables.Rule,
	expected map[string][]expr.Any,
) error {
	if !matchesNFTForwardTarget(chain, target) || !isNFTForwardBaseChain(chain) {
		return fmt.Errorf("nftables forward target %s/%s/%s changed identity",
			target.Family, target.Table, target.Name)
	}
	if len(actual) < len(expected) {
		return fmt.Errorf("published nftables forward rules for %s/%s/%s are incomplete",
			target.Family, target.Table, target.Name)
	}
	found := make(map[string]struct{}, len(expected))
	for index, rule := range actual {
		owner := string(rule.UserData)
		want, claimed := expected[owner]
		if index < len(expected) {
			if !claimed || !reflect.DeepEqual(rule.Exprs, want) {
				return fmt.Errorf("published nftables forward rules for %s/%s/%s are not the leading exact programs",
					target.Family, target.Table, target.Name)
			}
		}
		if !claimed {
			continue
		}
		if !reflect.DeepEqual(rule.Exprs, want) {
			return fmt.Errorf("nftables forward rule %s identity or content mismatch", owner)
		}
		if _, duplicate := found[owner]; duplicate {
			return fmt.Errorf("nftables forward rule %s is duplicated", owner)
		}
		found[owner] = struct{}{}
		if index >= len(expected) {
			return fmt.Errorf("nftables forward rule %s is outside the claimed leading programs", owner)
		}
	}
	if len(found) != len(expected) {
		return fmt.Errorf("published nftables forward ownership is incomplete: found=%d expected=%d",
			len(found), len(expected))
	}
	return nil
}

func matchesNFTForwardTarget(chain *nftables.Chain, target nftForwardChain) bool {
	return chain != nil && chain.Table != nil && nftFamilyName(chain.Table.Family) == target.Family &&
		chain.Table.Name == target.Table && chain.Name == target.Name
}
