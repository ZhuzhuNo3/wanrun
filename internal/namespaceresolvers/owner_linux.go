//go:build linux

package namespaceresolvers

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func Open(ctx context.Context, network *hostnetwork.Session, transfers []transfernumber.Number,
	intent Intent,
) (*NamespaceResolverSet, error) {
	if ctx == nil || network == nil || len(transfers) == 0 {
		return nil, errors.New("namespace resolver inputs are incomplete")
	}
	if len(intent.explicit) != 0 {
		file, err := sealedResolverFile(resolverText(intent.explicit, nil))
		if err != nil {
			return nil, err
		}
		return newAccessSet(file, transfers, nil)
	}
	content, err := readHostResolver()
	if err != nil {
		return nil, err
	}
	config, err := parseResolverConfig(content)
	if err != nil {
		return nil, err
	}
	file, err := sealedResolverFile(resolverText(
		[]netip.Addr{netip.MustParseAddr(loopbackResolver)}, config.suffix))
	if err != nil {
		return nil, err
	}
	forwarder, err := openForwarder(ctx, network, transfers, config)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return newAccessSet(file, transfers, forwarder)
}

func openForwarder(parent context.Context, network *hostnetwork.Session,
	transfers []transfernumber.Number, config resolverConfig,
) (*dnsForwarder, error) {
	forwarder := newDNSForwarder(parent, newUpstreamConnections(), config)
	for _, id := range transfers {
		lease, err := network.OpenNamespace(id)
		if err != nil {
			_, _ = forwarder.close(context.Background())
			return nil, err
		}
		listeners, openErr := listenInNamespace(lease.Descriptor())
		closeErr := lease.Close()
		if err := errors.Join(openErr, closeErr); err != nil {
			_, _ = forwarder.close(context.Background())
			return nil, fmt.Errorf("open transfer %d DNS listener: %w", id.Value(), err)
		}
		forwarder.addListeners(listeners.udp, listeners.tcp)
	}
	forwarder.startServers()
	return forwarder, nil
}
