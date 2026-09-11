package namespaceresolvers

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/net/dns/dnsmessage"
)

func TestForwarderFallsBackAcrossNameserversAndMalformedResponses(t *testing.T) {
	query := resolverTestMessage(t)
	first := netip.MustParseAddr("192.0.2.1")
	second := netip.MustParseAddr("192.0.2.2")
	for _, test := range []struct {
		name          string
		firstResponse []byte
		firstError    error
	}{
		{name: "transport failure", firstError: errors.New("unreachable")},
		{name: "malformed response", firstResponse: []byte{1, 2, 3}},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := &scriptedResolverUpstream{reply: func(call resolverExchangeCall) ([]byte, error) {
				if call.server == first {
					return test.firstResponse, test.firstError
				}
				return resolverTestResponse(t, query, dnsmessage.RCodeSuccess, false), nil
			}}
			forwarder := newTestForwarder(upstream, []netip.Addr{first, second})
			response, err := forwarder.exchange(query)
			if err != nil || len(response) == 0 {
				t.Fatalf("fallback response=%x error=%v", response, err)
			}
			if got := upstream.servers(); !slices.Equal(got, []netip.Addr{first, second}) {
				t.Fatalf("nameserver order=%v", got)
			}
		})
	}
}

func TestForwarderRetriesTruncatedUDPResponseOverTCP(t *testing.T) {
	query := resolverTestMessage(t)
	server := netip.MustParseAddr("192.0.2.1")
	upstream := &scriptedResolverUpstream{reply: func(call resolverExchangeCall) ([]byte, error) {
		return resolverTestResponse(t, query, dnsmessage.RCodeSuccess, !call.tcp), nil
	}}
	response, err := newTestForwarder(upstream, []netip.Addr{server}).exchange(query)
	if err != nil || dnsTruncated(response) {
		t.Fatalf("TCP fallback response=%x error=%v", response, err)
	}
	calls := upstream.allCalls()
	if len(calls) != 2 || calls[0].tcp || !calls[1].tcp || calls[1].server != server {
		t.Fatalf("transport calls=%+v", calls)
	}
}

func TestForwarderReturnsAuthoritativeFailureCodesWithoutFallback(t *testing.T) {
	query := resolverTestMessage(t)
	first := netip.MustParseAddr("192.0.2.1")
	second := netip.MustParseAddr("192.0.2.2")
	for _, test := range []struct {
		name string
		code dnsmessage.RCode
	}{{name: "NXDOMAIN", code: dnsmessage.RCodeNameError},
		{name: "SERVFAIL", code: dnsmessage.RCodeServerFailure}} {
		t.Run(test.name, func(t *testing.T) {
			want := resolverTestResponse(t, query, test.code, false)
			upstream := &scriptedResolverUpstream{reply: func(resolverExchangeCall) ([]byte, error) {
				return want, nil
			}}
			response, err := newTestForwarder(upstream, []netip.Addr{first, second}).exchange(query)
			if err != nil {
				t.Fatal(err)
			}
			var message dnsmessage.Message
			if err := message.Unpack(response); err != nil || message.RCode != test.code || !slices.Equal(response, want) {
				t.Fatalf("response code=%v unpack=%v", message.RCode, err)
			}
			if calls := upstream.allCalls(); len(calls) != 1 || calls[0].server != first {
				t.Fatalf("failure code unexpectedly fell back: %+v", calls)
			}
		})
	}
}

func TestResolverSetRetainsCapabilityUntilEveryWorkerStops(t *testing.T) {
	query := resolverTestMessage(t)
	upstream := &uncooperativeResolverUpstream{started: make(chan struct{}), release: make(chan struct{})}
	forwarder := newTestForwarder(upstream, []netip.Addr{netip.MustParseAddr("192.0.2.1")})
	if !forwarder.startHandler(nil) {
		t.Fatal("resolver handler did not start")
	}
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		defer forwarder.finishHandler(nil)
		_, _ = forwarder.exchange(query)
	}()
	<-upstream.started
	file, err := resolverTestFile(t, []byte("nameserver 127.0.0.53\n"))
	if err != nil {
		t.Fatal(err)
	}
	id, _ := transfernumber.New(1)
	set, err := newAccessSet(file, []transfernumber.Number{id}, forwarder)
	if err != nil {
		t.Fatal(err)
	}
	access, err := set.Access(id)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	closeCtx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	stopped, err := set.Close(closeCtx)
	cancel()
	elapsed := time.Since(started)
	if stopped || !errors.Is(err, context.DeadlineExceeded) || elapsed < 40*time.Millisecond || elapsed > 250*time.Millisecond {
		t.Fatalf("unconfirmed close stopped=%v duration=%v error=%v", stopped, elapsed, err)
	}
	duplicate, openErr := access.Open()
	if openErr != nil {
		t.Fatalf("resolver capability closed before containment: %v", openErr)
	}
	_ = duplicate.Close()
	close(upstream.release)
	select {
	case <-workerDone:
	case <-time.After(time.Second):
		t.Fatal("released resolver worker did not stop")
	}
	finishCtx, finishCancel := context.WithTimeout(context.Background(), time.Second)
	defer finishCancel()
	stopped, err = set.Close(finishCtx)
	if !stopped || err != nil {
		t.Fatalf("confirmed close stopped=%v error=%v", stopped, err)
	}
	if _, err := set.Access(id); err == nil {
		t.Fatal("closed resolver set still grants access")
	}
}

func resolverTestFile(t *testing.T, content []byte) (*os.File, error) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "resolver")
	if err != nil {
		return nil, err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func TestOversizedDownstreamRequestsReceiveIdentifiableSERVFAIL(t *testing.T) {
	query := oversizedResolverQuestionMessage(t)
	for _, transport := range []string{"UDP", "TCP"} {
		t.Run(transport, func(t *testing.T) {
			upstream := &scriptedResolverUpstream{reply: func(resolverExchangeCall) ([]byte, error) {
				t.Fatal("oversized request reached upstream resolver")
				return nil, nil
			}}
			forwarder := newTestForwarder(upstream, []netip.Addr{netip.MustParseAddr("192.0.2.1")})
			var response []byte
			if transport == "UDP" {
				response = exchangeOversizedUDPRequest(t, forwarder, query)
			} else {
				response = exchangeOversizedTCPRequest(t, forwarder, query)
			}
			assertResolverSERVFAIL(t, query, response)
			if diagnosticErr := forwarder.diagnosticError(); !isDNSMessageLimit(diagnosticErr) {
				t.Fatalf("%s limit diagnostic=%v", transport, diagnosticErr)
			}
		})
	}
}

func TestResolverDiagnosticsHaveAFixedRetentionBound(t *testing.T) {
	diagnostics := &resolverDiagnostics{}
	for index := range 32 {
		diagnostics.record(fmt.Errorf("controlled resolver failure %d", index))
	}
	diagnostics.mu.Lock()
	retained, suppressed := len(diagnostics.failures), diagnostics.suppressed
	diagnostics.mu.Unlock()
	if retained != retainedResolverFailures || suppressed != 32-retainedResolverFailures {
		t.Fatalf("resolver retained=%d suppressed=%d", retained, suppressed)
	}
	if diagnosticErr := diagnostics.err(); diagnosticErr == nil ||
		!strings.Contains(diagnosticErr.Error(), "16 additional DNS resolver failures were suppressed") {
		t.Fatalf("bounded resolver diagnostic=%v", diagnosticErr)
	}
}

func TestUpstreamConnectionCompletionRacingOwnerCloseIsBenign(t *testing.T) {
	left, right := net.Pipe()
	defer right.Close()
	connection := &blockingCloseConn{Conn: left, entered: make(chan struct{}), release: make(chan struct{})}
	upstream := newUpstreamConnections()
	if !upstream.track(connection) {
		t.Fatal("upstream connection was not tracked")
	}
	ownerClosed := make(chan error, 1)
	go func() {
		ownerClosed <- upstream.close()
	}()
	<-connection.entered
	released := make(chan struct{})
	go func() {
		upstream.release(connection)
		close(released)
	}()
	<-released
	close(connection.release)
	if err := <-ownerClosed; err != nil {
		t.Fatalf("connection completion race became cleanup failure: %v", err)
	}
	if got := appendResolverCloseError(nil, net.ErrClosed); len(got) != 0 {
		t.Fatalf("net.ErrClosed retained as cleanup failure: %v", got)
	}
	want := errors.New("controlled close failure")
	if got := appendResolverCloseError(nil, want); len(got) != 1 || !errors.Is(got[0], want) {
		t.Fatalf("real close failure was discarded: %v", got)
	}
}

type resolverExchangeCall struct {
	server netip.Addr
	tcp    bool
}

type scriptedResolverUpstream struct {
	calls []resolverExchangeCall
	reply func(resolverExchangeCall) ([]byte, error)
}

type uncooperativeResolverUpstream struct {
	started chan struct{}
	release chan struct{}
}

type blockingCloseConn struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int32
}

func (connection *blockingCloseConn) Close() error {
	if connection.calls.Add(1) != 1 {
		return net.ErrClosed
	}
	close(connection.entered)
	<-connection.release
	return connection.Conn.Close()
}

func (upstream *uncooperativeResolverUpstream) exchange(context.Context, netip.Addr, []byte,
	bool, time.Duration) ([]byte, error) {
	close(upstream.started)
	<-upstream.release
	return nil, errors.New("released")
}
func (*uncooperativeResolverUpstream) close() error { return nil }

func (upstream *scriptedResolverUpstream) exchange(_ context.Context, server netip.Addr, _ []byte,
	tcp bool, _ time.Duration) ([]byte, error) {
	call := resolverExchangeCall{server: server, tcp: tcp}
	upstream.calls = append(upstream.calls, call)
	return upstream.reply(call)
}
func (upstream *scriptedResolverUpstream) close() error { return nil }
func (upstream *scriptedResolverUpstream) allCalls() []resolverExchangeCall {
	return slices.Clone(upstream.calls)
}
func (upstream *scriptedResolverUpstream) servers() []netip.Addr {
	result := make([]netip.Addr, len(upstream.calls))
	for index, call := range upstream.calls {
		result[index] = call.server
	}
	return result
}

func newTestForwarder(upstream resolverUpstream, servers []netip.Addr) *dnsForwarder {
	ctx, cancel := context.WithCancel(context.Background())
	return &dnsForwarder{context: ctx, cancel: cancel, upstream: upstream,
		config:      resolverConfig{nameservers: servers, attempts: 1, timeout: time.Second},
		diagnostics: &resolverDiagnostics{}, activeTCP: make(map[net.Conn]struct{}), workersDone: make(chan struct{})}
}

func exchangeOversizedUDPRequest(t *testing.T, forwarder *dnsForwarder, query []byte) []byte {
	t.Helper()
	listener, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	forwarder.udp = []net.PacketConn{listener}
	forwarder.servers.Add(1)
	go forwarder.serveUDP(listener)
	client, err := net.Dial("udp4", listener.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(time.Second))
	if _, err := client.Write(query); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, maximumDNSWireMessage)
	count, err := client.Read(response)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if stopped, err := forwarder.close(ctx); !stopped || err != nil {
		t.Fatalf("close UDP request forwarder stopped=%v error=%v", stopped, err)
	}
	return slices.Clone(response[:count])
}

func exchangeOversizedTCPRequest(t *testing.T, forwarder *dnsForwarder, query []byte) []byte {
	t.Helper()
	client, server := net.Pipe()
	deadline := time.Now().Add(time.Second)
	_ = client.SetDeadline(deadline)
	_ = server.SetDeadline(deadline)
	finished := make(chan error, 1)
	go func() { finished <- forwarder.serveTCPConnection(server) }()
	var length [2]byte
	binary.BigEndian.PutUint16(length[:], uint16(len(query)))
	go func() {
		_, _ = client.Write(append(length[:], query...))
	}()
	if _, err := io.ReadFull(client, length[:]); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, err := io.ReadFull(client, response); err != nil {
		t.Fatal(err)
	}
	_ = client.Close()
	_ = server.Close()
	<-finished
	return response
}

func assertResolverSERVFAIL(t *testing.T, query, response []byte) {
	t.Helper()
	var original, failed dnsmessage.Message
	if err := original.Unpack(query); err != nil {
		t.Fatal(err)
	}
	if err := failed.Unpack(response); err != nil || failed.ID != original.ID ||
		failed.RCode != dnsmessage.RCodeServerFailure || !slices.Equal(failed.Questions, original.Questions) {
		t.Fatalf("SERVFAIL response=%#v error=%v", failed, err)
	}
}

func resolverTestMessage(t *testing.T) []byte {
	t.Helper()
	name, err := dnsmessage.NewName("forward.transferlanes.test.")
	if err != nil {
		t.Fatal(err)
	}
	message := dnsmessage.Message{Header: dnsmessage.Header{ID: 42, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	packed, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return packed
}

func oversizedResolverQuestionMessage(t *testing.T) []byte {
	t.Helper()
	name, err := dnsmessage.NewName("question-section-crosses-forward-limit.transferlanes.test.")
	if err != nil {
		t.Fatal(err)
	}
	question := dnsmessage.Question{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}
	message := dnsmessage.Message{Header: dnsmessage.Header{ID: 77, RecursionDesired: true}}
	for {
		packed, packErr := message.Pack()
		if packErr != nil {
			t.Fatal(packErr)
		}
		if len(packed) > maximumDNSMessage {
			return packed
		}
		message.Questions = append(message.Questions, question)
	}
}

func resolverTestResponse(t *testing.T, query []byte, code dnsmessage.RCode, truncated bool) []byte {
	t.Helper()
	var message dnsmessage.Message
	if err := message.Unpack(query); err != nil {
		t.Fatal(err)
	}
	message.Response = true
	message.RCode = code
	message.Truncated = truncated
	packed, err := message.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return packed
}
