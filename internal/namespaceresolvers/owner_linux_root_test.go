//go:build linux && rootintegration

package namespaceresolvers

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestForwarderCloseInterruptsBlackholedUpstreamIO(t *testing.T) {
	if testing.Short() {
		t.Fatal("root resolver lifecycle test must not be skipped")
	}
	query := resolverTestQuery(t)
	for _, test := range []struct {
		name   string
		server netip.Addr
		tcp    bool
	}{
		{name: "UDP", server: netip.MustParseAddr("127.0.0.56")},
		{name: "TCP", server: netip.MustParseAddr("127.0.0.57"), tcp: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			received, stop := startResolverBlackhole(t, test.server, test.tcp)
			defer stop()
			forwarder := newTestForwarder(newUpstreamConnections(), []netip.Addr{test.server})
			forwarder.config.timeout = 30 * time.Second
			forwarder.config.useTCP = test.tcp
			for range 3 {
				if !forwarder.startHandler(nil) {
					t.Fatal("resolver handler did not start")
				}
				go func() {
					defer forwarder.finishHandler(nil)
					_, _ = forwarder.exchange(query)
				}()
			}
			select {
			case <-received:
			case <-time.After(time.Second):
				t.Fatal("upstream blackhole received no DNS query")
			}
			type closeResult struct {
				stopped bool
				err     error
			}
			closed := make(chan closeResult, 1)
			started := time.Now()
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				stopped, err := forwarder.close(ctx)
				closed <- closeResult{stopped: stopped, err: err}
			}()
			select {
			case result := <-closed:
				if !result.stopped || result.err != nil || time.Since(started) > 500*time.Millisecond {
					t.Fatalf("forwarder close stopped=%v duration=%v error=%v",
						result.stopped, time.Since(started), result.err)
				}
			case <-time.After(500 * time.Millisecond):
				t.Fatal("forwarder close waited for the 30-second upstream timeout")
			}
		})
	}
}

func TestListenInNamespaceRollsBackUDPWhenTCPListenerFails(t *testing.T) {
	if testing.Short() {
		t.Fatal("root resolver listener rollback test must not be skipped")
	}
	occupiedTCP, err := net.Listen("tcp4", loopbackResolver+":53")
	if err != nil {
		t.Fatal(err)
	}
	defer occupiedTCP.Close()
	namespace, err := os.Open("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	defer namespace.Close()
	listeners, err := listenInNamespace(namespace.Fd())
	if err == nil || listeners.udp != nil || listeners.tcp != nil {
		t.Fatalf("partial listeners=%+v error=%v", listeners, err)
	}
	probe, err := net.ListenPacket("udp4", loopbackResolver+":53")
	if err != nil {
		t.Fatalf("failed UDP listener was not rolled back: %v", err)
	}
	_ = probe.Close()
}

func TestOversizedUpstreamResponsesReturnSERVFAILAndDiagnostic(t *testing.T) {
	if testing.Short() {
		t.Fatal("root resolver response-limit test must not be skipped")
	}
	query := resolverTestQuery(t)
	for _, test := range []struct {
		name   string
		server netip.Addr
		tcp    bool
	}{
		{name: "UDP", server: netip.MustParseAddr("127.0.0.58")},
		{name: "TCP", server: netip.MustParseAddr("127.0.0.59"), tcp: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			stop := startOversizedResolverResponse(t, test.server, test.tcp)
			defer stop()
			forwarder := newTestForwarder(newUpstreamConnections(), []netip.Addr{test.server})
			forwarder.config.useTCP = test.tcp
			response, err := forwarder.answer(query, false, "UDP")
			if err != nil {
				t.Fatal(err)
			}
			assertResolverSERVFAIL(t, query, response)
			diagnosticErr := forwarder.diagnosticError()
			if !isDNSMessageLimit(diagnosticErr) {
				t.Fatalf("%s upstream limit diagnostic=%v", test.name, diagnosticErr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if stopped, err := forwarder.close(ctx); !stopped || err != nil {
				t.Fatalf("close response-limit forwarder stopped=%v error=%v", stopped, err)
			}
		})
	}
}

func startOversizedResolverResponse(t *testing.T, address netip.Addr, tcp bool) func() {
	t.Helper()
	if !tcp {
		listener, err := net.ListenPacket("udp4", net.JoinHostPort(address.String(), "53"))
		if err != nil {
			t.Fatalf("listen oversized UDP resolver: %v", err)
		}
		go func() {
			query := make([]byte, maximumDNSMessage)
			count, peer, err := listener.ReadFrom(query)
			if err != nil {
				return
			}
			var message dnsmessage.Message
			if err := message.Unpack(query[:count]); err != nil {
				return
			}
			message.Response = true
			response, err := message.Pack()
			if err != nil {
				return
			}
			response = append(response, make([]byte, maximumDNSMessage+1)...)
			_, _ = listener.WriteTo(response[:maximumDNSMessage+1], peer)
		}()
		return func() { _ = listener.Close() }
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(address.String(), "53"))
	if err != nil {
		t.Fatalf("listen oversized TCP resolver: %v", err)
	}
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		var length [2]byte
		if _, err := io.ReadFull(connection, length[:]); err != nil {
			return
		}
		_, _ = io.CopyN(io.Discard, connection, int64(binary.BigEndian.Uint16(length[:])))
		binary.BigEndian.PutUint16(length[:], maximumDNSMessage+1)
		_, _ = connection.Write(length[:])
	}()
	return func() { _ = listener.Close() }
}

func startResolverBlackhole(t *testing.T, address netip.Addr, tcp bool) (<-chan struct{}, func()) {
	t.Helper()
	received := make(chan struct{})
	var receivedOnce sync.Once
	if !tcp {
		listener, err := net.ListenPacket("udp4", net.JoinHostPort(address.String(), "53"))
		if err != nil {
			t.Fatalf("listen UDP DNS blackhole: %v", err)
		}
		go func() {
			buffer := make([]byte, maximumDNSMessage)
			if _, _, err := listener.ReadFrom(buffer); err == nil {
				receivedOnce.Do(func() { close(received) })
			}
		}()
		return received, func() { _ = listener.Close() }
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(address.String(), "53"))
	if err != nil {
		t.Fatalf("listen TCP DNS blackhole: %v", err)
	}
	var connection net.Conn
	var connectionMu sync.Mutex
	go func() {
		accepted, err := listener.Accept()
		if err != nil {
			return
		}
		connectionMu.Lock()
		connection = accepted
		connectionMu.Unlock()
		var length [2]byte
		if _, err := io.ReadFull(accepted, length[:]); err != nil {
			return
		}
		size := int(length[0])<<8 | int(length[1])
		query := make([]byte, size)
		if _, err := io.ReadFull(accepted, query); err == nil {
			receivedOnce.Do(func() { close(received) })
		}
	}()
	return received, func() {
		_ = listener.Close()
		connectionMu.Lock()
		if connection != nil {
			_ = connection.Close()
		}
		connectionMu.Unlock()
	}
}

func resolverTestQuery(t *testing.T) []byte {
	t.Helper()
	name, err := dnsmessage.NewName("close.transferlanes.test.")
	if err != nil {
		t.Fatal(err)
	}
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 42, RecursionDesired: true})
	if err := builder.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := builder.Question(dnsmessage.Question{Name: name, Type: dnsmessage.TypeA,
		Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	query, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if err := validateDNSMessage(query, false); err != nil {
		t.Fatal(errors.New("resolver test query is invalid"))
	}
	return query
}
