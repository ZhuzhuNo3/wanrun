//go:build linux && rootintegration

package root_test

import (
	"net/netip"
	"slices"
	"sync"
)

type receiverSourceLog struct {
	mu      sync.Mutex
	sources []netip.Addr
}

func (log *receiverSourceLog) record(source netip.Addr) {
	log.mu.Lock()
	log.sources = append(log.sources, source)
	log.mu.Unlock()
}

func (log *receiverSourceLog) snapshot() []netip.Addr {
	log.mu.Lock()
	defer log.mu.Unlock()
	return slices.Clone(log.sources)
}
