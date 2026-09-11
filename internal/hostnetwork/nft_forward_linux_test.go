//go:build linux

package hostnetwork

import (
	"testing"

	"github.com/google/nftables"
	"github.com/google/nftables/expr"
)

func TestPublishedNFTForwardRuleSetRequiresTheCompleteOwnedPrefix(t *testing.T) {
	target := nftForwardChain{Family: "ip", Table: "filter", Name: "FORWARD"}
	chain := testNFTForwardChain(target)
	firstExpressions := []expr.Any{&expr.Counter{Bytes: 1}}
	secondExpressions := []expr.Any{&expr.Counter{Bytes: 2}}
	expected := map[string][]expr.Any{"owner-1": firstExpressions, "owner-2": secondExpressions}
	foreign := testNFTForwardRule(chain, "foreign", &expr.Counter{Bytes: 9})
	first := testNFTForwardRule(chain, "owner-1", firstExpressions...)
	second := testNFTForwardRule(chain, "owner-2", secondExpressions...)

	tests := []struct {
		name   string
		chain  *nftables.Chain
		rules  []*nftables.Rule
		accept bool
	}{
		{name: "complete owned prefix", chain: chain,
			rules: []*nftables.Rule{first, second, foreign}, accept: true},
		{name: "foreign rule before owned prefix", chain: chain,
			rules: []*nftables.Rule{foreign, first, second}},
		{name: "owner missing", chain: chain,
			rules: []*nftables.Rule{first, foreign}},
		{name: "owner duplicated", chain: chain,
			rules: []*nftables.Rule{first, first, second}},
		{name: "owned expression changed", chain: chain,
			rules: []*nftables.Rule{first,
				testNFTForwardRule(chain, "owner-2", &expr.Counter{Bytes: 3})}},
		{name: "owned target changed", chain: testNFTForwardChain(nftForwardChain{
			Family: "ip", Table: "other", Name: "FORWARD"}),
			rules: []*nftables.Rule{first, second}},
		{name: "owned rule outside leading prefix", chain: chain,
			rules: []*nftables.Rule{first, foreign, second}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := requireLeadingPublishedNFTRules(target, test.chain, test.rules, expected)
			if test.accept && err != nil {
				t.Fatalf("complete owner prefix rejected: %v", err)
			}
			if !test.accept && err == nil {
				t.Fatal("invalid owner prefix accepted")
			}
		})
	}
}

func testNFTForwardChain(target nftForwardChain) *nftables.Chain {
	hook := nftables.ChainHookForward
	return &nftables.Chain{Table: &nftables.Table{Family: nftables.TableFamilyIPv4,
		Name: target.Table}, Name: target.Name, Hooknum: hook, Type: nftables.ChainTypeFilter}
}

func testNFTForwardRule(chain *nftables.Chain, owner string,
	expressions ...expr.Any,
) *nftables.Rule {
	return &nftables.Rule{Table: chain.Table, Chain: chain, UserData: []byte(owner), Exprs: expressions}
}
