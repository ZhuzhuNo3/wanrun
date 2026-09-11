package namespaceresolvers

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"sync"
	"time"
)

// upstreamConnections owns every live socket opened toward a host resolver.
// Context cancellation stops dials; Close stops reads and writes that have
// already entered the kernel and would otherwise wait for resolv.conf timeout.
type upstreamConnections struct {
	mu     sync.Mutex
	active map[net.Conn]struct{}
	closed bool
}

func newUpstreamConnections() *upstreamConnections {
	return &upstreamConnections{active: make(map[net.Conn]struct{})}
}

func (upstream *upstreamConnections) exchange(ctx context.Context, server netip.Addr, query []byte,
	tcp bool, timeout time.Duration,
) ([]byte, error) {
	network := "udp"
	if tcp {
		network = "tcp"
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(requestCtx, network, net.JoinHostPort(server.String(), "53"))
	if err != nil {
		return nil, err
	}
	if !upstream.track(connection) {
		_ = connection.Close()
		return nil, net.ErrClosed
	}
	defer upstream.release(connection)
	_ = connection.SetDeadline(deadlineFromContext(requestCtx))
	if tcp {
		return exchangeTCP(connection, query)
	}
	return exchangeUDP(connection, query)
}

func exchangeTCP(connection net.Conn, query []byte) ([]byte, error) {
	framed := append([]byte{byte(len(query) >> 8), byte(len(query))}, query...)
	if _, err := connection.Write(framed); err != nil {
		return nil, err
	}
	var length [2]byte
	if _, err := io.ReadFull(connection, length[:]); err != nil {
		return nil, err
	}
	size := int(length[0])<<8 | int(length[1])
	if size < 12 || size > maximumDNSMessage {
		if size > maximumDNSMessage {
			return nil, &dnsMessageLimitError{transport: "TCP", direction: "response"}
		}
		return nil, errors.New("DNS response size is invalid")
	}
	response := make([]byte, size)
	_, err := io.ReadFull(connection, response)
	return response, err
}

func exchangeUDP(connection net.Conn, query []byte) ([]byte, error) {
	if _, err := connection.Write(query); err != nil {
		return nil, err
	}
	response := make([]byte, maximumDNSMessage+1)
	count, err := connection.Read(response)
	if count > maximumDNSMessage {
		return nil, &dnsMessageLimitError{transport: "UDP", direction: "response"}
	}
	return slices.Clone(response[:count]), err
}

func deadlineFromContext(ctx context.Context) (deadline time.Time) {
	deadline, _ = ctx.Deadline()
	return deadline
}

func (upstream *upstreamConnections) track(connection net.Conn) bool {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.closed {
		return false
	}
	upstream.active[connection] = struct{}{}
	return true
}

func (upstream *upstreamConnections) release(connection net.Conn) {
	upstream.mu.Lock()
	delete(upstream.active, connection)
	upstream.mu.Unlock()
	_ = connection.Close()
}

func (upstream *upstreamConnections) close() error {
	upstream.mu.Lock()
	if upstream.closed {
		upstream.mu.Unlock()
		return nil
	}
	upstream.closed = true
	connections := make([]net.Conn, 0, len(upstream.active))
	for connection := range upstream.active {
		connections = append(connections, connection)
	}
	upstream.mu.Unlock()
	var failures []error
	for _, connection := range connections {
		failures = appendResolverCloseError(failures, connection.Close())
	}
	return errors.Join(failures...)
}
