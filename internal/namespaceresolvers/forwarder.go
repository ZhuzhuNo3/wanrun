package namespaceresolvers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
)

type dnsForwarder struct {
	context     context.Context
	cancel      context.CancelFunc
	config      resolverConfig
	upstream    resolverUpstream
	diagnostics *resolverDiagnostics
	udp         []net.PacketConn
	tcp         []net.Listener
	servers     sync.WaitGroup
	handlers    sync.WaitGroup
	handlerMu   sync.Mutex
	activeTCP   map[net.Conn]struct{}
	closing     bool
	rotateMu    sync.Mutex
	next        int
	shutdown    sync.Once
	shutdownErr error
	workersDone chan struct{}
}

func newDNSForwarder(parent context.Context, upstream resolverUpstream,
	config resolverConfig,
) *dnsForwarder {
	ctx, cancel := context.WithCancel(parent)
	return &dnsForwarder{context: ctx, cancel: cancel, config: config, upstream: upstream,
		diagnostics: &resolverDiagnostics{}, activeTCP: make(map[net.Conn]struct{}),
		workersDone: make(chan struct{})}
}

func (forwarder *dnsForwarder) addListeners(udp net.PacketConn, tcp net.Listener) {
	forwarder.udp = append(forwarder.udp, udp)
	forwarder.tcp = append(forwarder.tcp, tcp)
}

func (forwarder *dnsForwarder) startServers() {
	for _, listener := range forwarder.udp {
		forwarder.servers.Add(1)
		go forwarder.serveUDP(listener)
	}
	for _, listener := range forwarder.tcp {
		forwarder.servers.Add(1)
		go forwarder.serveTCP(listener)
	}
}

func (forwarder *dnsForwarder) close(ctx context.Context) (bool, error) {
	if forwarder == nil {
		return true, nil
	}
	if ctx == nil {
		return false, errors.New("DNS resolver cleanup context is unavailable")
	}
	forwarder.shutdown.Do(forwarder.beginShutdown)
	select {
	case <-forwarder.workersDone:
		return true, forwarder.shutdownErr
	default:
	}
	select {
	case <-forwarder.workersDone:
		return true, forwarder.shutdownErr
	case <-ctx.Done():
		select {
		case <-forwarder.workersDone:
			return true, forwarder.shutdownErr
		default:
			return false, errors.Join(forwarder.shutdownErr,
				fmt.Errorf("confirm DNS resolver workers stopped: %w", ctx.Err()))
		}
	}
}

func (forwarder *dnsForwarder) beginShutdown() {
	forwarder.cancel()
	connections := forwarder.stopAcceptingHandlers()
	var failures []error
	if forwarder.upstream != nil {
		failures = append(failures, forwarder.upstream.close())
	}
	for _, listener := range forwarder.udp {
		failures = appendResolverCloseError(failures, listener.Close())
	}
	for _, listener := range forwarder.tcp {
		failures = appendResolverCloseError(failures, listener.Close())
	}
	for _, connection := range connections {
		failures = appendResolverCloseError(failures, connection.Close())
	}
	forwarder.shutdownErr = errors.Join(failures...)
	go forwarder.confirmWorkersStopped()
}

func (forwarder *dnsForwarder) stopAcceptingHandlers() []net.Conn {
	forwarder.handlerMu.Lock()
	defer forwarder.handlerMu.Unlock()
	forwarder.closing = true
	connections := make([]net.Conn, 0, len(forwarder.activeTCP))
	for connection := range forwarder.activeTCP {
		connections = append(connections, connection)
	}
	return connections
}

func (forwarder *dnsForwarder) confirmWorkersStopped() {
	forwarder.servers.Wait()
	forwarder.handlers.Wait()
	close(forwarder.workersDone)
}

func appendResolverCloseError(failures []error, err error) []error {
	if err != nil && !errors.Is(err, net.ErrClosed) {
		return append(failures, err)
	}
	return failures
}

func (forwarder *dnsForwarder) serveUDP(listener net.PacketConn) {
	defer forwarder.servers.Done()
	buffer := make([]byte, maximumDNSWireMessage)
	for {
		count, peer, err := listener.ReadFrom(buffer)
		if err != nil {
			if forwarder.context.Err() != nil {
				return
			}
			continue
		}
		query := slices.Clone(buffer[:count])
		if !forwarder.startHandler(nil) {
			return
		}
		go func() {
			defer forwarder.finishHandler(nil)
			response, err := forwarder.answer(query, count > maximumDNSMessage, "UDP")
			if err == nil {
				_, _ = listener.WriteTo(response, peer)
			}
		}()
	}
}

func (forwarder *dnsForwarder) serveTCP(listener net.Listener) {
	defer forwarder.servers.Done()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if forwarder.context.Err() != nil {
				return
			}
			continue
		}
		if !forwarder.startHandler(connection) {
			_ = connection.Close()
			return
		}
		go func() {
			defer forwarder.finishHandler(connection)
			defer connection.Close()
			_ = forwarder.serveTCPConnection(connection)
		}()
	}
}

func (forwarder *dnsForwarder) startHandler(connection net.Conn) bool {
	forwarder.handlerMu.Lock()
	defer forwarder.handlerMu.Unlock()
	if forwarder.closing {
		return false
	}
	forwarder.handlers.Add(1)
	if connection != nil {
		forwarder.activeTCP[connection] = struct{}{}
	}
	return true
}

func (forwarder *dnsForwarder) finishHandler(connection net.Conn) {
	if connection != nil {
		forwarder.handlerMu.Lock()
		delete(forwarder.activeTCP, connection)
		forwarder.handlerMu.Unlock()
	}
	forwarder.handlers.Done()
}

func (forwarder *dnsForwarder) serveTCPConnection(connection net.Conn) error {
	for {
		var length [2]byte
		if _, err := io.ReadFull(connection, length[:]); err != nil {
			return err
		}
		size := int(length[0])<<8 | int(length[1])
		if size < 12 {
			return errors.New("DNS query size is invalid")
		}
		query := make([]byte, size)
		if _, err := io.ReadFull(connection, query); err != nil {
			return err
		}
		response, err := forwarder.answer(query, size > maximumDNSMessage, "TCP")
		if err != nil {
			return err
		}
		length[0], length[1] = byte(len(response)>>8), byte(len(response))
		if _, err := connection.Write(append(length[:], response...)); err != nil {
			return err
		}
	}
}
