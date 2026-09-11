//go:build linux

package hostnetwork

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const (
	staleLinkCascadeTimeout      = 2 * time.Second
	staleLinkCascadePollInterval = 10 * time.Millisecond
)

type linuxHost struct {
	conntrack conntrackAccess
	firewalls firewallBackends
	routing   weakFIBKernel
}

func defaultKernelHost() kernelHost {
	return linuxHost{conntrack: netlinkConntrack{}, firewalls: defaultFirewallBackends(),
		routing: netlinkRoutingKernel{}}
}

func defaultFirewallBackends() firewallBackends {
	runner := systemArgvRunner{}
	tables := procLegacyIPTablesTables{}
	iptables := iptablesFirewall{activeRulesCommand: "iptables", activeSaveCommand: "iptables-save",
		runner: runner, legacyTables: tables}
	return firewallBackends{nftables: nftFirewall{}, iptables: iptables,
		legacyIPTables: fixedLegacyIPTablesReader{runner: runner, tables: tables}}
}

func (host linuxHost) Inspect(ctx context.Context, selected []egressSelection) (hostInventory, error) {
	if err := verifyProviderForwarding(selected); err != nil {
		return hostInventory{}, err
	}
	conntrackSources, err := host.conntrackAccess().ObserveOriginalSources()
	if err != nil {
		return hostInventory{}, fmt.Errorf("conntrack observation or cleanup capability is unavailable: %w", err)
	}
	inventory, err := inspectHostInventory(ctx)
	if err != nil {
		return hostInventory{}, err
	}
	for _, source := range conntrackSources {
		if source = source.Unmap(); source.Is4() {
			inventory.conntrackOriginalSources[source] = struct{}{}
		}
	}
	inventory.firewall, err = host.firewallSet().Inspect(ctx)
	if err != nil {
		return hostInventory{}, err
	}
	return inventory, nil
}

func (host linuxHost) ObservePublishedFirewalls(ctx context.Context,
	claims []networkClaim,
) (publishedFirewallObservations, error) {
	return host.firewallSet().ObservePublished(ctx, claims)
}

func (host linuxHost) Install(ctx context.Context, claim networkClaim) (*hostInstallation, error) {
	installation := &hostInstallation{
		namespaces: make(map[transfernumber.Number]*os.File, len(claim.transfers)),
		links:      newLinkReceipts(),
		weakFIB:    newWeakFIBReceipts(),
		firewall:   newFirewallReceipts(firewallKind(claim.backend)),
	}
	if err := host.firewallSet().Preflight(ctx, claim); err != nil {
		return installation, fmt.Errorf("preflight firewall mutation: %w", err)
	}
	if err := installClaimedLinks(ctx, claim, installation); err != nil {
		return installation, err
	}
	receipts, err := installWeakFIB(host.routingKernel(), claim)
	installation.weakFIB = receipts
	if err != nil {
		return installation, err
	}
	installation.firewall, err = host.firewallSet().Install(ctx, claim)
	if err != nil {
		return installation, err
	}
	return installation, nil
}

func installClaimedLinks(ctx context.Context, claim networkClaim, installation *hostInstallation) error {
	for _, transfer := range claim.transfers {
		if err := ctx.Err(); err != nil {
			return err
		}
		namespace, err := createAnonymousNamespace()
		if err != nil {
			return fmt.Errorf("create transfer %d namespace: %w", transfer.number, err)
		}
		installation.namespaces[transfer.transfer] = namespace
		receipt, err := installTransferLinks(transfer, namespace)
		installation.links.add(receipt)
		if err != nil {
			return fmt.Errorf("install transfer %d links: %w", transfer.number, err)
		}
	}
	return nil
}

func (host linuxHost) Verify(ctx context.Context, claim networkClaim, installation *hostInstallation) error {
	if err := installation.requireComplete(claim); err != nil {
		return err
	}
	for _, transfer := range claim.transfers {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := verifyClaimedTransfer(claim, transfer, installation.namespaces[transfer.transfer]); err != nil {
			return fmt.Errorf("verify transfer %d: %w", transfer.number, err)
		}
	}
	if err := verifyClaimedReturnTable(claim); err != nil {
		return err
	}
	return host.firewallSet().Verify(ctx, claim, false)
}

func (host linuxHost) ConfirmConntrackEmpty(_ context.Context, claim networkClaim) error {
	observed, err := host.conntrackAccess().ObserveOriginalSources()
	if err != nil {
		return fmt.Errorf("recheck conntrack before traffic activation: %w", err)
	}
	_, claimed := ownedConntrackSources(claim)
	for _, source := range observed {
		if transfer, exists := claimed[source.Unmap()]; exists {
			return fmt.Errorf("transfer %d temporary source %s entered conntrack before activation",
				transfer, source)
		}
	}
	return nil
}

func (host linuxHost) Rollback(ctx context.Context, claim networkClaim,
	installation *hostInstallation, activated bool) error {
	cleanupErrors := host.stopLiveForwarding(ctx, claim, installation)
	if installation != nil {
		cleanupErrors = appendError(cleanupErrors,
			rollbackWeakFIB(host.routingKernel(), claim, installation.weakFIB))
		cleanupErrors = appendError(cleanupErrors, removeReceiptVeths(ctx, claim, installation.links))
		closeNamespaceFiles(installation.namespaces)
	}
	if activated {
		cleanupErrors = appendError(cleanupErrors, clearOwnedConntrack(host.conntrackAccess(), claim))
	}
	cleanupErrors = appendError(cleanupErrors, host.verifyCleanup(ctx, claim, activated))
	return errors.Join(cleanupErrors...)
}

func (host linuxHost) Close(ctx context.Context, claim networkClaim,
	installation *hostInstallation) error {
	if err := installation.requireComplete(claim); err != nil {
		return err
	}
	cleanupErrors := host.stopLiveForwarding(ctx, claim, installation)
	cleanupErrors = appendError(cleanupErrors,
		rollbackWeakFIB(host.routingKernel(), claim, installation.weakFIB))
	cleanupErrors = appendError(cleanupErrors, removeReceiptVeths(ctx, claim, installation.links))
	cleanupErrors = appendError(cleanupErrors, clearOwnedConntrack(host.conntrackAccess(), claim))
	cleanupErrors = appendError(cleanupErrors, host.verifyCleanup(ctx, claim, true))
	return errors.Join(cleanupErrors...)
}

func (host linuxHost) Cleanup(ctx context.Context, claim networkClaim, activated bool) error {
	cleanupErrors := host.stopStaleForwarding(ctx, claim)
	if activated {
		cleanupErrors = appendError(cleanupErrors,
			removeActivatedWeakFIBDuringLinkCascade(ctx, host.routingKernel(), claim,
				staleLinkCascadeTimeout, staleLinkCascadePollInterval))
	} else {
		cleanupErrors = appendError(cleanupErrors, observeClaimedWeakFIB(claim))
	}
	cleanupErrors = appendError(cleanupErrors, removeClaimedVeths(ctx, claim))
	if activated {
		cleanupErrors = appendError(cleanupErrors, clearOwnedConntrack(host.conntrackAccess(), claim))
	}
	cleanupErrors = appendError(cleanupErrors, host.verifyCleanup(ctx, claim, activated))
	return errors.Join(cleanupErrors...)
}

func removeActivatedWeakFIBDuringLinkCascade(ctx context.Context, kernel weakFIBKernel,
	claim networkClaim, timeout, pollInterval time.Duration,
) error {
	waitContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		cleanupErr := removeActivatedWeakFIB(kernel, claim)
		if cleanupErr == nil || !onlyReturnRouteLinkCascadePending(cleanupErr) {
			return cleanupErr
		}
		if err := waitForLinkCascadeObservation(waitContext, pollInterval); err != nil {
			return fmt.Errorf("wait %s for kernel link cascade: %w",
				timeout, errors.Join(cleanupErr, err))
		}
	}
}

func waitForLinkCascadeObservation(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func onlyReturnRouteLinkCascadePending(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !onlyReturnRouteLinkCascadePending(child) {
				return false
			}
		}
		return true
	}
	if child := errors.Unwrap(err); child != nil {
		return onlyReturnRouteLinkCascadePending(child)
	}
	return err == errReturnRouteLinkCascadePending
}

func (host linuxHost) stopLiveForwarding(ctx context.Context, claim networkClaim,
	installation *hostInstallation) []error {
	if installation == nil {
		return nil
	}
	if err := host.firewallSet().Remove(ctx, claim, installation.firewall); err != nil {
		return []error{err}
	}
	return nil
}

func (host linuxHost) stopStaleForwarding(ctx context.Context, claim networkClaim) []error {
	if err := host.firewallSet().Recover(ctx, claim); err != nil {
		return []error{err}
	}
	return nil
}

func removeActivatedWeakFIB(kernel weakFIBKernel, claim networkClaim) error {
	var cleanupErrors []error
	for index := len(claim.transfers) - 1; index >= 0; index-- {
		transfer := claim.transfers[index]
		cleanupErrors = appendError(cleanupErrors, kernel.deleteReturnRule(claim, transfer))
		cleanupErrors = appendError(cleanupErrors, kernel.deleteOutboundRule(claim, transfer))
		cleanupErrors = appendError(cleanupErrors, kernel.deleteReturnRoute(claim, transfer))
	}
	return errors.Join(cleanupErrors...)
}

func removeClaimedVeths(ctx context.Context, claim networkClaim) error {
	var cleanupErrors []error
	for index := len(claim.transfers) - 1; index >= 0; index-- {
		if err := ctx.Err(); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			break
		}
		cleanupErrors = appendError(cleanupErrors, removeOwnedVeth(claim.transfers[index]))
	}
	return errors.Join(cleanupErrors...)
}

func removeReceiptVeths(ctx context.Context, claim networkClaim, receipts linkReceipts) error {
	var cleanupErrors []error
	for index := len(claim.transfers) - 1; index >= 0; index-- {
		transfer := claim.transfers[index]
		if _, exists := receipts[transfer.transfer]; !exists {
			continue
		}
		if err := ctx.Err(); err != nil {
			cleanupErrors = append(cleanupErrors, err)
			break
		}
		cleanupErrors = appendError(cleanupErrors, removeOwnedVeth(transfer))
	}
	return errors.Join(cleanupErrors...)
}

func (host linuxHost) verifyCleanup(ctx context.Context, claim networkClaim, activated bool) error {
	var cleanupErrors []error
	cleanupErrors = appendError(cleanupErrors, host.firewallSet().VerifyAbsent(ctx, claim))
	cleanupErrors = appendError(cleanupErrors, verifyClaimedWeakFIBAbsent(claim))
	for _, transfer := range claim.transfers {
		cleanupErrors = appendError(cleanupErrors, verifyOwnedVethAbsent(transfer))
	}
	if activated {
		cleanupErrors = appendError(cleanupErrors, confirmOwnedConntrackAbsent(host.conntrackAccess(), claim))
	}
	return errors.Join(cleanupErrors...)
}

func confirmOwnedConntrackAbsent(access conntrackAccess, claim networkClaim) error {
	observed, err := access.ObserveOriginalSources()
	if err != nil {
		return fmt.Errorf("confirm conntrack absence: %w", err)
	}
	_, claimed := ownedConntrackSources(claim)
	for _, source := range observed {
		if transfer, exists := claimed[source.Unmap()]; exists {
			return fmt.Errorf("transfer %d conntrack entry remains for %s", transfer, source)
		}
	}
	return nil
}

func appendError(found []error, err error) []error {
	if err != nil {
		return append(found, err)
	}
	return found
}

func (host linuxHost) conntrackAccess() conntrackAccess {
	if host.conntrack == nil {
		return netlinkConntrack{}
	}
	return host.conntrack
}

func (host linuxHost) routingKernel() weakFIBKernel {
	if host.routing == nil {
		return netlinkRoutingKernel{}
	}
	return host.routing
}

func (host linuxHost) firewallSet() firewallBackends {
	defaults := defaultFirewallBackends()
	if host.firewalls.nftables == nil {
		host.firewalls.nftables = defaults.nftables
	}
	if host.firewalls.iptables == nil {
		host.firewalls.iptables = defaults.iptables
	}
	if host.firewalls.legacyIPTables == nil {
		host.firewalls.legacyIPTables = defaults.legacyIPTables
	}
	return host.firewalls
}

var errOwnedObjectAbsent = errors.New("owned network object is absent")
