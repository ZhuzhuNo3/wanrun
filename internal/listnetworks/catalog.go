package listnetworks

import (
	"context"
	"net/netip"

	"github.com/ZhuzhuNo3/transferlanes/internal/networkcatalog"
)

type localNetwork struct {
	localIP         netip.Addr
	interfaceName   string
	interfaceIndex  int
	included        bool
	exclusionReason string
}

type resolvedNetwork struct {
	localIP        netip.Addr
	interfaceName  string
	interfaceIndex int
	table          int
	gateway        netip.Addr
	hasGateway     bool
	runnable       bool
	reason         string
}

type capturedNetworks struct {
	addresses []localNetwork
	defaults  []localNetwork
	resolve   func([]netip.Addr) []resolvedNetwork
}

func captureSystemNetworks(ctx context.Context) (capturedNetworks, error) {
	snapshot, err := networkcatalog.New().Capture(ctx)
	if err != nil {
		return capturedNetworks{}, err
	}
	return capturedNetworks{addresses: localNetworks(snapshot.LocalAddresses()),
		defaults: localNetworks(snapshot.DefaultCandidates()),
		resolve: func(sources []netip.Addr) []resolvedNetwork {
			return resolvedNetworks(snapshot.Resolve(sources))
		}}, nil
}

func localNetworks(addresses []networkcatalog.LocalAddress) []localNetwork {
	result := make([]localNetwork, len(addresses))
	for index, address := range addresses {
		link := address.Interface()
		result[index] = localNetwork{localIP: address.IP(),
			interfaceName: link.Name(), interfaceIndex: link.Index(), included: address.IncludedByDefault(),
			exclusionReason: address.ExclusionReason()}
	}
	return result
}

func resolvedNetworks(egresses []networkcatalog.Egress) []resolvedNetwork {
	result := make([]resolvedNetwork, len(egresses))
	for index, egress := range egresses {
		link := egress.Interface()
		gateway, hasGateway := egress.Gateway()
		result[index] = resolvedNetwork{localIP: egress.LocalIP(), interfaceName: link.Name(),
			interfaceIndex: link.Index(), table: egress.Table(), gateway: gateway, hasGateway: hasGateway,
			runnable: egress.Runnable(), reason: egress.Reason()}
	}
	return result
}
