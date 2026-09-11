//go:build linux

package hostnetwork

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"reflect"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

type nftFirewall struct {
	inspectInventory func(context.Context) (firewallInventory, error)
	commitProgram    func(nftProgram) error
	observeClaim     func(context.Context, networkClaim, bool) error
}

func (nftFirewall) Kind() firewallKind { return firewallNFTables }

func (firewall nftFirewall) Preflight(ctx context.Context, claim networkClaim) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	program, err := buildNFTProgram(claim)
	if err != nil {
		return err
	}
	inventory, err := firewall.inventory(ctx)
	if err != nil {
		return err
	}
	return requireNFTInstallTopology(inventory, program)
}

func (firewall nftFirewall) Install(ctx context.Context, claim networkClaim) (firewallReceipts, error) {
	receipts := newFirewallReceipts(firewallNFTables)
	if err := ctx.Err(); err != nil {
		return receipts, err
	}
	program, err := buildNFTProgram(claim)
	if err != nil {
		return receipts, err
	}
	inventory, err := firewall.inventory(ctx)
	if err != nil {
		return receipts, err
	}
	if err := requireNFTInstallTopology(inventory, program); err != nil {
		return receipts, err
	}
	if err := firewall.commit(program); err != nil {
		return receipts, err
	}
	receipts.add("nft:" + claim.firewallTable)
	if err := firewall.observe(ctx, claim, false); err != nil {
		return receipts, fmt.Errorf("verify installed nftables ownership: %w", err)
	}
	return receipts, nil
}

func (firewall nftFirewall) Verify(ctx context.Context, claim networkClaim, allowAbsent bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return firewall.observe(ctx, claim, allowAbsent)
}

func (firewall nftFirewall) ObservePublished(ctx context.Context,
	claims []networkClaim,
) ([]bool, error) {
	return observePublishedNFTClaims(ctx, claims)
}

func observePublishedNFTClaims(ctx context.Context, claims []networkClaim) ([]bool, error) {
	present := make([]bool, len(claims))
	programs := make([]nftProgram, 0, len(claims))
	for index, claim := range claims {
		if err := verifyNFTAbsent(claim); err == nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		program, err := buildNFTProgram(claim)
		if err != nil {
			return nil, err
		}
		if err := verifyPublishedNFTProgram(claim, program); err != nil {
			return nil, fmt.Errorf("confirm complete claimed firewall: %w", err)
		}
		present[index] = true
		programs = append(programs, program)
	}
	if err := verifyPublishedNFTForwardRules(programs); err != nil {
		return nil, err
	}
	return present, nil
}

func verifyPublishedNFTProgram(claim networkClaim, program nftProgram) error {
	connection := &nftables.Conn{}
	if _, err := resolveNFTForwardChains(connection, program); err != nil {
		return err
	}
	table, err := findOwnedNFTTable(connection, claim.firewallTable)
	if err != nil {
		return err
	}
	chains, err := verifyNFTChains(connection, table, claim.nftPostroutingPriority)
	if err != nil {
		return err
	}
	if err := verifyNFTRules(connection, table, chains, program); err != nil {
		return err
	}
	owned, err := observeNFTForwardRules(connection, program, true)
	if err != nil {
		return err
	}
	if len(owned) != len(program.forwardRules) {
		return fmt.Errorf("nftables forward ownership is incomplete: found=%d expected=%d",
			len(owned), len(program.forwardRules))
	}
	return nil
}

func (firewall nftFirewall) inventory(ctx context.Context) (firewallInventory, error) {
	if firewall.inspectInventory != nil {
		return firewall.inspectInventory(ctx)
	}
	return firewall.Inspect(ctx)
}

func (firewall nftFirewall) commit(program nftProgram) error {
	if firewall.commitProgram != nil {
		return firewall.commitProgram(program)
	}
	return installNFT(program)
}

func (firewall nftFirewall) observe(ctx context.Context, claim networkClaim, allowAbsent bool) error {
	if firewall.observeClaim != nil {
		return firewall.observeClaim(ctx, claim, allowAbsent)
	}
	return verifyNFTClaim(claim, allowAbsent)
}

func (nftFirewall) Remove(ctx context.Context, claim networkClaim, receipts firewallReceipts) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if receipts.kind != firewallNFTables {
		return errors.New("live nftables receipts name another backend")
	}
	owner := "nft:" + claim.firewallTable
	if len(receipts.owners) == 0 {
		return nil
	}
	if len(receipts.owners) != 1 || !receipts.has(owner) {
		return errors.New("live nftables receipt set is invalid")
	}
	return removeNFTClaim(claim)
}

func (nftFirewall) Recover(ctx context.Context, claim networkClaim) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return removeNFTClaim(claim)
}

func (nftFirewall) VerifyAbsent(ctx context.Context, claim networkClaim) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return verifyNFTAbsent(claim)
}

func installNFT(program nftProgram) error {
	connection := &nftables.Conn{}
	forwardChains, err := resolveNFTForwardChains(connection, program)
	if err != nil {
		return err
	}
	table := connection.CreateTable(&nftables.Table{Name: program.table, Family: nftables.TableFamilyIPv4})
	chains := addNFTChains(connection, table, program.postroutingPriority)
	for _, rule := range program.rules {
		connection.AddRule(&nftables.Rule{Table: table, Chain: chains[rule.chain],
			Exprs: nftExpressions(rule), UserData: []byte(rule.owner)})
	}
	insertNFTForwardRules(connection, forwardChains, program.forwardRules)
	if err := connection.Flush(); err != nil {
		return fmt.Errorf("commit nftables transaction: %w", err)
	}
	return nil
}

func addNFTChains(connection *nftables.Conn, table *nftables.Table,
	postroutingValue int32,
) map[string]*nftables.Chain {
	forwardPriority := nftables.ChainPriority(-1)
	postroutingPriority := nftables.ChainPriority(postroutingValue)
	return map[string]*nftables.Chain{
		"forward": connection.AddChain(&nftables.Chain{Name: "forward", Table: table,
			Hooknum: nftables.ChainHookForward, Priority: &forwardPriority, Type: nftables.ChainTypeFilter}),
		"postrouting": connection.AddChain(&nftables.Chain{Name: "postrouting", Table: table,
			Hooknum: nftables.ChainHookPostrouting, Priority: &postroutingPriority, Type: nftables.ChainTypeNAT}),
	}
}

func requireNFTInstallTopology(inventory firewallInventory, program nftProgram) error {
	if inventory.kind != firewallNFTables {
		return fmt.Errorf("nftables mutation requires a complete nftables inventory")
	}
	if err := validateTransferLanesNFTPostroutingPriority(program.postroutingPriority); err != nil {
		return fmt.Errorf("nftables mutation has unsupported postrouting priority: %w", err)
	}
	if _, exists := inventory.nftTableNames[program.table]; exists {
		return fmt.Errorf("nftables table identity %q appeared before mutation", program.table)
	}
	for _, chain := range inventory.nftPostroutingNATChains {
		if chain.HasRules && chain.Priority <= program.postroutingPriority {
			return fmt.Errorf("nftables postrouting NAT topology changed before mutation: chain %s/%s/%s priority %d is not later than claimed priority %d",
				chain.Family, chain.Table, chain.Name, chain.Priority, program.postroutingPriority)
		}
	}
	return nil
}

func nftExpressions(rule nftRule) []expr.Any {
	switch rule.kind {
	case "forward-out":
		result := append(matchInterface(expr.MetaKeyIIFNAME, rule.input),
			matchInterface(expr.MetaKeyOIFNAME, rule.output)...)
		result = append(result, matchIPv4Prefix(12, rule.source)...)
		return append(result, &expr.Verdict{Kind: expr.VerdictAccept})
	case "return":
		result := append(matchInterface(expr.MetaKeyIIFNAME, rule.input),
			matchInterface(expr.MetaKeyOIFNAME, rule.output)...)
		result = append(result, matchIPv4Prefix(16, rule.destination)...)
		return append(result, establishedReturn()...)
	case "drop-out":
		return append(matchInterface(expr.MetaKeyIIFNAME, rule.input), &expr.Verdict{Kind: expr.VerdictDrop})
	case "drop-in":
		return append(matchInterface(expr.MetaKeyOIFNAME, rule.output), &expr.Verdict{Kind: expr.VerdictDrop})
	case "snat":
		return snatRuleExpressions(rule)
	default:
		return nil
	}
}

func matchIPv4Prefix(offset uint32, raw string) []expr.Any {
	prefix := netip.MustParsePrefix(raw).Masked()
	mask := net.CIDRMask(prefix.Bits(), 32)
	return []expr.Any{
		&expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader, Offset: offset, Len: 4},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: []byte{0, 0, 0, 0}},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: prefix.Addr().AsSlice()},
	}
}

func matchInterface(key expr.MetaKey, name string) []expr.Any {
	data := make([]byte, unix.IFNAMSIZ)
	copy(data, name)
	return []expr.Any{&expr.Meta{Key: key, Register: 1},
		&expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: data}}
}

func establishedReturn() []expr.Any {
	mask := binaryutil.NativeEndian.PutUint32(expr.CtStateBitESTABLISHED | expr.CtStateBitRELATED)
	zero := binaryutil.NativeEndian.PutUint32(0)
	return []expr.Any{&expr.Ct{Key: expr.CtKeySTATE, Register: 1},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: mask, Xor: zero},
		&expr.Cmp{Op: expr.CmpOpNeq, Register: 1, Data: zero},
		&expr.Verdict{Kind: expr.VerdictAccept}}
}

func snatRuleExpressions(rule nftRule) []expr.Any {
	source := netip.MustParseAddr(rule.source)
	target := netip.MustParseAddr(rule.address)
	result := append(matchInterface(expr.MetaKeyIIFNAME, rule.input),
		matchInterface(expr.MetaKeyOIFNAME, rule.output)...)
	result = append(result, &expr.Payload{DestRegister: 1, Base: expr.PayloadBaseNetworkHeader,
		Offset: 12, Len: 4}, &expr.Cmp{Op: expr.CmpOpEq, Register: 1, Data: source.AsSlice()})
	return append(result, &expr.Immediate{Register: 1, Data: target.AsSlice()},
		&expr.NAT{Type: expr.NATTypeSourceNAT, Family: unix.NFPROTO_IPV4, RegAddrMin: 1, RegAddrMax: 1})
}

func verifyNFTClaim(claim networkClaim, allowAbsent bool) error {
	connection := &nftables.Conn{}
	program, err := buildNFTProgram(claim)
	if err != nil {
		return err
	}
	if !allowAbsent {
		if _, err := resolveNFTForwardChains(connection, program); err != nil {
			return err
		}
	}
	table, err := findOwnedNFTTable(connection, claim.firewallTable)
	if err != nil {
		if !allowAbsent || !errors.Is(err, errOwnedObjectAbsent) {
			return err
		}
	} else {
		chains, err := verifyNFTChains(connection, table, claim.nftPostroutingPriority)
		if err != nil {
			return err
		}
		if err := verifyNFTRules(connection, table, chains, program); err != nil {
			return err
		}
	}
	_, err = observeNFTForwardRules(connection, program, allowAbsent)
	return err
}

func findOwnedNFTTable(connection *nftables.Conn, name string) (*nftables.Table, error) {
	tables, err := connection.ListTablesOfFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, fmt.Errorf("list nftables tables: %w", err)
	}
	var found *nftables.Table
	for _, table := range tables {
		if table.Name == name {
			if found != nil {
				return nil, errors.New("duplicate nftables table identity")
			}
			found = table
		}
	}
	if found == nil {
		return nil, errOwnedObjectAbsent
	}
	return found, nil
}

func verifyNFTChains(connection *nftables.Conn,
	table *nftables.Table, postroutingPriority int32,
) (map[string]*nftables.Chain, error) {
	all, err := connection.ListChainsOfTableFamily(nftables.TableFamilyIPv4)
	if err != nil {
		return nil, fmt.Errorf("list nftables chains: %w", err)
	}
	owned := make(map[string]*nftables.Chain)
	for _, chain := range all {
		if chain.Table != nil && chain.Table.Family == table.Family && chain.Table.Name == table.Name {
			owned[chain.Name] = chain
		}
	}
	if len(owned) != 2 || !exactNFTChain(owned["forward"], nftables.ChainHookForward, -1,
		nftables.ChainTypeFilter) || !exactNFTChain(owned["postrouting"], nftables.ChainHookPostrouting,
		nftables.ChainPriority(postroutingPriority), nftables.ChainTypeNAT) {
		return nil, fmt.Errorf("nftables chain identity or content mismatch: %s / %s",
			describeNFTChain(owned["forward"]), describeNFTChain(owned["postrouting"]))
	}
	return owned, nil
}

func describeNFTChain(chain *nftables.Chain) string {
	if chain == nil {
		return "absent"
	}
	return fmt.Sprintf("%s hook=%v priority=%v type=%s policy=%v device=%q",
		chain.Name, chain.Hooknum, chain.Priority, chain.Type, chain.Policy, chain.Device)
}

func exactNFTChain(chain *nftables.Chain, hook *nftables.ChainHook,
	priority nftables.ChainPriority, chainType nftables.ChainType) bool {
	return chain != nil && chain.Hooknum != nil && *chain.Hooknum == *hook &&
		chain.Priority != nil && *chain.Priority == priority && chain.Type == chainType &&
		nftChainAcceptPolicy(chain.Policy) && chain.Device == ""
}

func nftChainAcceptPolicy(policy *nftables.ChainPolicy) bool {
	return policy == nil || *policy == nftables.ChainPolicyAccept
}

func verifyNFTRules(connection *nftables.Conn, table *nftables.Table,
	chains map[string]*nftables.Chain, program nftProgram) error {
	for _, chainName := range program.chains {
		actual, err := connection.GetRules(table, chains[chainName])
		if err != nil {
			return fmt.Errorf("list nftables rules in %s: %w", chainName, err)
		}
		expected := nftRulesForChain(program.rules, chainName)
		if len(actual) != len(expected) {
			return fmt.Errorf("nftables chain %s has %d rules, want %d", chainName, len(actual), len(expected))
		}
		for index := range expected {
			expectedExpressions := nftExpressions(expected[index])
			if !bytes.Equal(actual[index].UserData, []byte(expected[index].owner)) ||
				!reflect.DeepEqual(actual[index].Exprs, expectedExpressions) {
				return fmt.Errorf("nftables rule %s identity or content mismatch: actual=%#v expected=%#v",
					expected[index].owner, describeNFTExpressions(actual[index].Exprs),
					describeNFTExpressions(expectedExpressions))
			}
		}
	}
	return nil
}

func describeNFTExpressions(expressions []expr.Any) []string {
	result := make([]string, 0, len(expressions))
	for _, expression := range expressions {
		value := reflect.ValueOf(expression)
		if value.Kind() == reflect.Pointer && !value.IsNil() {
			result = append(result, fmt.Sprintf("%#v", value.Elem().Interface()))
		} else {
			result = append(result, fmt.Sprintf("%#v", expression))
		}
	}
	return result
}

func nftRulesForChain(rules []nftRule, chain string) []nftRule {
	result := make([]nftRule, 0, len(rules))
	for _, rule := range rules {
		if rule.chain == chain {
			result = append(result, rule)
		}
	}
	return result
}

func removeNFTClaim(claim networkClaim) error {
	connection := &nftables.Conn{}
	program, err := buildNFTProgram(claim)
	if err != nil {
		return err
	}
	forwardRules, err := observeNFTForwardRules(connection, program, true)
	if err != nil {
		return err
	}
	table, err := findOwnedNFTTable(connection, claim.firewallTable)
	if err != nil && !errors.Is(err, errOwnedObjectAbsent) {
		return err
	}
	if table != nil {
		chains, err := verifyNFTChains(connection, table, claim.nftPostroutingPriority)
		if err != nil {
			return err
		}
		if err := verifyNFTRules(connection, table, chains, program); err != nil {
			return err
		}
	}
	for _, rule := range forwardRules {
		if err := connection.DelRule(rule); err != nil {
			return fmt.Errorf("queue nftables forward rule deletion: %w", err)
		}
	}
	if table != nil {
		connection.DelTable(table)
	}
	if table == nil && len(forwardRules) == 0 {
		return nil
	}
	if err := connection.Flush(); err != nil {
		return fmt.Errorf("delete exact nftables ownership: %w", err)
	}
	return nil
}

func verifyNFTAbsent(claim networkClaim) error {
	connection := &nftables.Conn{}
	_, err := findOwnedNFTTable(connection, claim.firewallTable)
	if err == nil {
		return fmt.Errorf("nftables table %s remains", claim.firewallTable)
	}
	if !errors.Is(err, errOwnedObjectAbsent) {
		return err
	}
	program, err := buildNFTProgram(claim)
	if err != nil {
		return err
	}
	rules, err := observeNFTForwardRules(connection, program, true)
	if err != nil {
		return err
	}
	if len(rules) != 0 {
		return fmt.Errorf("claimed nftables forward rules remain")
	}
	return nil
}
