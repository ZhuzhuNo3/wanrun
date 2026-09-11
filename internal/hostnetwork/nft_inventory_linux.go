//go:build linux

package hostnetwork

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/nftables"
	"golang.org/x/sys/unix"
)

var errNFTFamilyUnsupported = errors.New("nftables address family is unsupported")

func (nftFirewall) Inspect(ctx context.Context) (firewallInventory, error) {
	if err := ctx.Err(); err != nil {
		return firewallInventory{}, err
	}
	connection := &nftables.Conn{}
	inventory := firewallInventory{kind: firewallNFTables, nftTableNames: make(map[string]struct{}),
		transferlanesOwnerReferences: make(map[string]struct{})}
	for _, family := range nftIPv4Families() {
		if err := inspectNFTFamily(ctx, connection, family, &inventory); err != nil {
			if errors.Is(err, errNFTFamilyUnsupported) {
				if family == nftables.TableFamilyIPv4 {
					return firewallInventory{}, fmt.Errorf("%w: %v", errFirewallUnavailable, err)
				}
				continue
			}
			return firewallInventory{}, err
		}
	}
	sort.Slice(inventory.nftForwardChains, func(left, right int) bool {
		return lessNFTForwardChain(inventory.nftForwardChains[left], inventory.nftForwardChains[right])
	})
	sort.Slice(inventory.nftPostroutingNATChains, func(left, right int) bool {
		leftChain, rightChain := inventory.nftPostroutingNATChains[left], inventory.nftPostroutingNATChains[right]
		return strings.Join([]string{leftChain.Family, leftChain.Table, leftChain.Name}, "\x00") <
			strings.Join([]string{rightChain.Family, rightChain.Table, rightChain.Name}, "\x00")
	})
	if err := validateNFTPostroutingNATChains(inventory.nftPostroutingNATChains); err != nil {
		return firewallInventory{}, err
	}
	return inventory, nil
}

func inspectNFTFamily(ctx context.Context, connection *nftables.Conn, family nftables.TableFamily,
	inventory *firewallInventory,
) error {
	tables, err := connection.ListTablesOfFamily(family)
	if err != nil {
		if nftFamilyIsUnsupported(err) {
			return fmt.Errorf("%w: list %s tables: %v", errNFTFamilyUnsupported,
				nftFamilyName(family), err)
		}
		return fmt.Errorf("list %s nftables tables: %w", nftFamilyName(family), err)
	}
	if family == nftables.TableFamilyIPv4 {
		for _, table := range tables {
			if table == nil || table.Name == "" {
				return errors.New("nftables returned an unnamed IPv4 table")
			}
			inventory.nftTableNames[table.Name] = struct{}{}
		}
	}
	chains, err := connection.ListChainsOfTableFamily(family)
	if err != nil {
		return fmt.Errorf("list %s nftables chains: %w", nftFamilyName(family), err)
	}
	for _, chain := range chains {
		if err := inspectNFTChain(ctx, connection, chain, inventory); err != nil {
			return err
		}
	}
	return nil
}

func inspectNFTChain(ctx context.Context, connection *nftables.Conn, chain *nftables.Chain,
	inventory *firewallInventory,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if chain == nil || chain.Table == nil || chain.Name == "" {
		return errors.New("nftables returned a chain without identity")
	}
	rules, err := connection.GetRules(chain.Table, chain)
	if err != nil {
		return fmt.Errorf("list nftables rules for %s/%s: %w", chain.Table.Name, chain.Name, err)
	}
	for _, rule := range rules {
		if id, valid := runIDFromIPTablesComment(string(rule.UserData)); valid {
			inventory.transferlanesOwnerReferences[id] = struct{}{}
		}
	}
	if nftForwardChainCanReject(chain, rules) && !isTransferLanesNFTTable(chain.Table) {
		inventory.nftForwardChains = append(inventory.nftForwardChains, nftForwardChain{
			Family: nftFamilyName(chain.Table.Family), Table: chain.Table.Name, Name: chain.Name})
	}
	postrouting, found, err := nftPostroutingNATFacts(chain, len(rules) > 0)
	if err != nil {
		return err
	}
	if found {
		inventory.nftPostroutingNATChains = append(inventory.nftPostroutingNATChains, postrouting)
	}
	return nil
}

func nftPostroutingNATFacts(chain *nftables.Chain,
	hasRules bool,
) (nftPostroutingNATChain, bool, error) {
	if !isNFTPostroutingNATBaseChain(chain) {
		return nftPostroutingNATChain{}, false, nil
	}
	if chain.Priority == nil {
		return nftPostroutingNATChain{}, false, fmt.Errorf("nftables postrouting NAT chain %s/%s has no priority",
			chain.Table.Name, chain.Name)
	}
	return nftPostroutingNATChain{Family: nftFamilyName(chain.Table.Family), Table: chain.Table.Name,
		Name: chain.Name, Priority: int32(*chain.Priority), HasRules: hasRules}, true, nil
}

func nftFamilyIsUnsupported(err error) bool {
	return errors.Is(err, unix.EAFNOSUPPORT) || errors.Is(err, unix.EPROTONOSUPPORT) ||
		errors.Is(err, unix.ENOPROTOOPT) || errors.Is(err, unix.EOPNOTSUPP)
}

func nftForwardChainCanReject(chain *nftables.Chain, rules []*nftables.Rule) bool {
	if !isNFTForwardBaseChain(chain) {
		return false
	}
	return chain.Policy != nil && *chain.Policy == nftables.ChainPolicyDrop || len(rules) > 0
}

func nftIPv4Families() []nftables.TableFamily {
	return []nftables.TableFamily{nftables.TableFamilyIPv4, nftables.TableFamilyINet}
}

func nftFamilyName(family nftables.TableFamily) string {
	if family == nftables.TableFamilyINet {
		return "inet"
	}
	return "ip"
}

func nftTableFamily(name string) (nftables.TableFamily, error) {
	switch name {
	case "ip":
		return nftables.TableFamilyIPv4, nil
	case "inet":
		return nftables.TableFamilyINet, nil
	default:
		return nftables.TableFamilyUnspecified, fmt.Errorf("unsupported nftables family %q", name)
	}
}

func isNFTForwardBaseChain(chain *nftables.Chain) bool {
	return chain != nil && chain.Table != nil && chain.Hooknum != nil &&
		*chain.Hooknum == *nftables.ChainHookForward && chain.Type == nftables.ChainTypeFilter &&
		chain.Device == ""
}

func isNFTPostroutingNATBaseChain(chain *nftables.Chain) bool {
	return chain != nil && chain.Table != nil && chain.Hooknum != nil &&
		*chain.Hooknum == *nftables.ChainHookPostrouting && chain.Type == nftables.ChainTypeNAT &&
		chain.Device == ""
}

func isTransferLanesNFTTable(table *nftables.Table) bool {
	if table == nil || table.Family != nftables.TableFamilyIPv4 || !strings.HasPrefix(table.Name, "transferlanes_") {
		return false
	}
	_, valid := runIDFromIPTablesComment("transferlanes:" + strings.TrimPrefix(table.Name, "transferlanes_"))
	return valid
}
