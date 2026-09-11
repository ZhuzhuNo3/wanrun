package hostnetwork

import (
	"fmt"
	"net/netip"
)

type conntrackAccess interface {
	ObserveOriginalSources() ([]netip.Addr, error)
	DeleteOriginalSources([]netip.Addr) error
}

func requireConntrackCapability(access conntrackAccess) error {
	if _, err := access.ObserveOriginalSources(); err != nil {
		return fmt.Errorf("conntrack observation or cleanup capability is unavailable: %w", err)
	}
	return nil
}

func clearOwnedConntrack(access conntrackAccess, claim networkClaim) error {
	sources, owned := ownedConntrackSources(claim)
	if err := access.DeleteOriginalSources(sources); err != nil {
		return fmt.Errorf("delete owned conntrack entries: %w", err)
	}
	observed, err := access.ObserveOriginalSources()
	if err != nil {
		return fmt.Errorf("confirm conntrack cleanup: %w", err)
	}
	for _, source := range observed {
		if number, exists := owned[source.Unmap()]; exists {
			return fmt.Errorf("transfer %d conntrack entry remains for %s", number, source)
		}
	}
	return nil
}

func ownedConntrackSources(claim networkClaim) ([]netip.Addr, map[netip.Addr]int) {
	sources := make([]netip.Addr, 0, len(claim.transfers))
	owned := make(map[netip.Addr]int, len(claim.transfers))
	for _, value := range claim.transfers {
		source := value.namespaceIP.Unmap()
		if _, exists := owned[source]; !exists {
			sources = append(sources, source)
			owned[source] = value.number
		}
	}
	return sources, owned
}
