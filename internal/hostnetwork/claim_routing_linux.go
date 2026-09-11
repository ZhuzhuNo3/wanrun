//go:build linux

package hostnetwork

import (
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

type netlinkRoutingKernel struct{}

var errReturnRouteLinkCascadePending = errors.New("return route is awaiting kernel link cascade")

func (netlinkRoutingKernel) addReturnRoute(claim networkClaim, transfer transferAllocation) error {
	host, err := ownedHostVeth(transfer)
	if err != nil {
		return err
	}
	route := claimedReturnRoute(claim, transfer, host.Attrs().Index)
	if err := netlink.RouteAdd(&route); err != nil {
		return fmt.Errorf("add return route: %w", err)
	}
	return nil
}

func (netlinkRoutingKernel) addOutboundRule(claim networkClaim, transfer transferAllocation) error {
	rule := claimedOutboundRule(claim, transfer)
	if err := netlink.RuleAdd(&rule); err != nil {
		return fmt.Errorf("add outbound policy rule: %w", err)
	}
	return nil
}

func (netlinkRoutingKernel) addReturnRule(claim networkClaim, transfer transferAllocation) error {
	rule := claimedReturnRule(claim, transfer)
	if err := netlink.RuleAdd(&rule); err != nil {
		return fmt.Errorf("add return policy rule: %w", err)
	}
	return nil
}

func (netlinkRoutingKernel) deleteReturnRoute(claim networkClaim, transfer transferAllocation) error {
	routes, err := routesForReturnPrefix(claim.returnTable, transfer)
	if err != nil || len(routes) == 0 {
		return err
	}
	if len(routes) != 1 || !exactClaimedReturnRouteWithoutOwnerLink(routes[0], claim, transfer) {
		return errors.New("return route identity is ambiguous before deletion")
	}
	host, err := ownedHostVeth(transfer)
	if err != nil {
		if isLinkNotFound(err) {
			return fmt.Errorf("%w for transfer %d", errReturnRouteLinkCascadePending,
				transfer.number)
		}
		return fmt.Errorf("identify return-route owner graph: %w", err)
	}
	expected := claimedReturnRoute(claim, transfer, host.Attrs().Index)
	if !exactClaimedReturnRoute(routes[0], expected) {
		return errors.New("return route identity is ambiguous before deletion")
	}
	return exactReturnRouteDeleteResult(netlink.RouteDel(&routes[0]))
}

func exactReturnRouteDeleteResult(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, unix.ESRCH) {
		return errReturnRouteLinkCascadePending
	}
	return fmt.Errorf("delete return route: %w", err)
}

func exactClaimedReturnRouteWithoutOwnerLink(actual netlink.Route, claim networkClaim,
	transfer transferAllocation,
) bool {
	if actual.LinkIndex <= 0 {
		return false
	}
	return exactClaimedReturnRoute(actual, claimedReturnRoute(claim, transfer, actual.LinkIndex))
}

func (netlinkRoutingKernel) deleteOutboundRule(claim networkClaim, transfer transferAllocation) error {
	return deleteClaimedRule(claimedOutboundRule(claim, transfer))
}

func (netlinkRoutingKernel) deleteReturnRule(claim networkClaim, transfer transferAllocation) error {
	return deleteClaimedRule(claimedReturnRule(claim, transfer))
}

func claimedOutboundRule(claim networkClaim, transfer transferAllocation) netlink.Rule {
	rule := basePolicyRuleWithProtocol(transfer.outboundPriority, transfer.routeTable, claim.protocol)
	rule.IifName = transfer.hostVeth
	rule.Src = ipNet(transfer.subnet)
	return rule
}

func claimedReturnRule(claim networkClaim, transfer transferAllocation) netlink.Rule {
	rule := basePolicyRuleWithProtocol(transfer.returnPriority, claim.returnTable, claim.protocol)
	rule.IifName = transfer.providerName
	rule.Dst = ipNet(transfer.subnet)
	return rule
}

func basePolicyRuleWithProtocol(priority, table int, protocol uint8) netlink.Rule {
	rule := basePolicyRule(priority, table)
	rule.Protocol = protocol
	return rule
}

func claimedReturnRoute(claim networkClaim, transfer transferAllocation, linkIndex int) netlink.Route {
	route := returnRoute(claim.returnTable, transfer, linkIndex)
	route.Protocol = netlink.RouteProtocol(claim.protocol)
	return route
}

func verifyClaimedRouting(claim networkClaim) error {
	for _, transfer := range claim.transfers {
		if err := verifyClaimedRule(claimedOutboundRule(claim, transfer), false); err != nil {
			return err
		}
		if err := verifyClaimedRule(claimedReturnRule(claim, transfer), false); err != nil {
			return err
		}
		if err := verifyClaimedReturnRoute(claim, transfer, false); err != nil {
			return err
		}
	}
	return verifyClaimedReturnTable(claim)
}

func verifyClaimedRule(expected netlink.Rule, allowAbsent bool) error {
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list host policy rules: %w", err)
	}
	priorityCount, exactCount := 0, 0
	for _, rule := range rules {
		if rule.Priority != expected.Priority {
			continue
		}
		priorityCount++
		if exactPolicyRuleWithProtocol(rule, expected) {
			exactCount++
		}
	}
	if priorityCount == 0 && allowAbsent {
		return nil
	}
	if priorityCount != 1 || exactCount != 1 {
		return fmt.Errorf("policy priority %d has %d rules, %d exact", expected.Priority,
			priorityCount, exactCount)
	}
	return nil
}

func exactPolicyRuleWithProtocol(actual, expected netlink.Rule) bool {
	return exactPolicyRule(actual, expected)
}

func verifyClaimedReturnRoute(claim networkClaim, transfer transferAllocation, allowAbsent bool) error {
	routes, err := routesForReturnPrefix(claim.returnTable, transfer)
	if err != nil {
		return err
	}
	if len(routes) == 0 && allowAbsent {
		return nil
	}
	host, err := ownedHostVeth(transfer)
	if err != nil {
		return fmt.Errorf("identify return-route owner graph: %w", err)
	}
	expected := claimedReturnRoute(claim, transfer, host.Attrs().Index)
	exact := 0
	for _, route := range routes {
		if exactClaimedReturnRoute(route, expected) {
			exact++
		}
	}
	if len(routes) != 1 || exact != 1 {
		return fmt.Errorf("return table %d has %d routes for %s, %d exact",
			claim.returnTable, len(routes), transfer.subnet, exact)
	}
	return nil
}

func exactClaimedReturnRoute(actual, expected netlink.Route) bool {
	return exactReturnRoute(actual, expected)
}

func verifyClaimedReturnTable(claim networkClaim) error {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{Table: claim.returnTable}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return fmt.Errorf("list claimed return table %d: %w", claim.returnTable, err)
	}
	if len(routes) != len(claim.transfers) {
		return fmt.Errorf("return table %d has %d routes, want %d",
			claim.returnTable, len(routes), len(claim.transfers))
	}
	for _, transfer := range claim.transfers {
		if err := verifyClaimedReturnRoute(claim, transfer, false); err != nil {
			return err
		}
	}
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	references := 0
	for _, rule := range rules {
		if rule.Table == claim.returnTable {
			references++
		}
	}
	if references != len(claim.transfers) {
		return fmt.Errorf("return table %d has %d rule references, want %d",
			claim.returnTable, references, len(claim.transfers))
	}
	return nil
}

func deleteClaimedRule(expected netlink.Rule) error {
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	matching := make([]netlink.Rule, 0, 1)
	for _, rule := range rules {
		if rule.Priority == expected.Priority {
			matching = append(matching, rule)
		}
	}
	if len(matching) == 0 {
		return nil
	}
	if len(matching) != 1 || !exactPolicyRuleWithProtocol(matching[0], expected) {
		return fmt.Errorf("policy priority %d is ambiguous before deletion", expected.Priority)
	}
	if err := netlink.RuleDel(&matching[0]); err != nil {
		return fmt.Errorf("delete policy rule: %w", err)
	}
	return nil
}

func verifyClaimedWeakFIBAbsent(claim networkClaim) error {
	for _, transfer := range claim.transfers {
		if err := verifyClaimedRuleAbsent(claimedOutboundRule(claim, transfer)); err != nil {
			return err
		}
		if err := verifyClaimedRuleAbsent(claimedReturnRule(claim, transfer)); err != nil {
			return err
		}
		routes, err := routesForReturnPrefix(claim.returnTable, transfer)
		if err != nil {
			return err
		}
		if len(routes) != 0 {
			return fmt.Errorf("return route for %s remains in table %d", transfer.subnet, claim.returnTable)
		}
	}
	return nil
}

func verifyClaimedRuleAbsent(expected netlink.Rule) error {
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return err
	}
	for _, rule := range rules {
		if rule.Priority == expected.Priority {
			return fmt.Errorf("policy priority %d remains occupied", expected.Priority)
		}
	}
	return nil
}

func observeClaimedWeakFIB(claim networkClaim) error {
	for _, transfer := range claim.transfers {
		if err := verifyClaimedRule(claimedOutboundRule(claim, transfer), true); err != nil {
			return err
		}
		if err := verifyClaimedRule(claimedReturnRule(claim, transfer), true); err != nil {
			return err
		}
		if err := verifyClaimedReturnRoute(claim, transfer, true); err != nil {
			return err
		}
	}
	return nil
}
