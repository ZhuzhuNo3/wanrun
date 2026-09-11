//go:build linux && rootintegration && protocolacceptance

package root_test

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
)

type s3GateDecision uint8

const (
	s3GateContinue s3GateDecision = iota + 1
	s3GateFail
	s3GateCancelled
)

type s3RequestGate struct {
	failing  netip.Addr
	expected map[netip.Addr]struct{}

	mu          sync.Mutex
	active      int
	seen        map[netip.Addr]struct{}
	succeeded   map[netip.Addr]struct{}
	cancelled   map[netip.Addr]struct{}
	ready       chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	changed     chan struct{}
}

func newS3RequestGate(sources []netip.Addr, failing netip.Addr) *s3RequestGate {
	gate := &s3RequestGate{failing: failing,
		expected:  make(map[netip.Addr]struct{}, len(sources)),
		seen:      make(map[netip.Addr]struct{}, len(sources)),
		succeeded: make(map[netip.Addr]struct{}, len(sources)),
		cancelled: make(map[netip.Addr]struct{}, len(sources)),
		ready:     make(chan struct{}), release: make(chan struct{}), changed: make(chan struct{})}
	for _, source := range sources {
		gate.expected[source] = struct{}{}
	}
	return gate
}

func (gate *s3RequestGate) await(ctx context.Context, source netip.Addr) s3GateDecision {
	gate.mu.Lock()
	gate.active++
	if _, expected := gate.expected[source]; expected {
		gate.seen[source] = struct{}{}
	}
	if len(gate.seen) == len(gate.expected) {
		select {
		case <-gate.ready:
		default:
			close(gate.ready)
		}
	}
	gate.signalChangedLocked()
	gate.mu.Unlock()
	select {
	case <-gate.ready:
	case <-ctx.Done():
		return s3GateCancelled
	}
	if source == gate.failing {
		return s3GateFail
	}
	select {
	case <-gate.release:
		return s3GateContinue
	case <-ctx.Done():
		return s3GateCancelled
	}
}

func (gate *s3RequestGate) finished(source netip.Addr, decision s3GateDecision,
	request context.Context,
) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.active--
	if decision == s3GateCancelled || request.Err() != nil {
		gate.cancelled[source] = struct{}{}
	}
	gate.signalChangedLocked()
}

func (gate *s3RequestGate) completed(source netip.Addr) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	gate.succeeded[source] = struct{}{}
	gate.signalChangedLocked()
}

func (gate *s3RequestGate) signalChangedLocked() {
	close(gate.changed)
	gate.changed = make(chan struct{})
}

func (gate *s3RequestGate) WaitUntilActive(ctx context.Context) error {
	select {
	case <-gate.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (gate *s3RequestGate) Release() {
	gate.releaseOnce.Do(func() { close(gate.release) })
}

func (gate *s3RequestGate) WaitUntilStopped(ctx context.Context) error {
	for {
		gate.mu.Lock()
		if gate.active == 0 && len(gate.seen) == len(gate.expected) {
			gate.mu.Unlock()
			return nil
		}
		changed := gate.changed
		gate.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			gate.mu.Lock()
			err := fmt.Errorf("%w (active=%d seen=%v completed=%v cancelled=%v)",
				ctx.Err(), gate.active, addressSet(gate.seen), addressSet(gate.succeeded),
				addressSet(gate.cancelled))
			gate.mu.Unlock()
			return err
		}
	}
}

func (gate *s3RequestGate) CompletedSources() []netip.Addr {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return addressSet(gate.succeeded)
}

func (gate *s3RequestGate) CancelledSources() []netip.Addr {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return addressSet(gate.cancelled)
}
