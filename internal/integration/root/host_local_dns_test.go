//go:build linux && rootintegration

package root_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vishvananda/netns"
	"golang.org/x/net/dns/dnsmessage"
)

func TestPublicRunHostLocalDNSAutoWeight(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	drainSourceObservations(fixture.measured)

	hostname := fmt.Sprintf("%s-throughput.transferlanes.test.", fixture.prefix)
	resolver := startHostLocalResolver(t, hostname, fixture.remote)
	resolver.installHostConfiguration(t)
	scenario := fixture.newRunSupervisorScenario(t, "host-local-dns-auto-weight")
	destination := filepath.Join(scenario.markers, "rsync-destination")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"run", "--no-tui", "--source", os.Getenv(supervisorTransferEnv)}
	for _, source := range fixture.sources {
		arguments = append(arguments, "--network", source.String())
	}
	arguments = append(arguments, "--auto-weight", "--measure-url",
		"http://"+net.JoinHostPort(strings.TrimSuffix(hostname, "."), "18081")+"/measure",
		"--measure-duration", "500ms", "--", "/usr/bin/rsync", "-a", "{}", destination+"/")

	stdout, stderr, runErr := runPublicTransferLanesPipes(t, arguments)
	resolver.restoreHostConfiguration(t)
	if runErr != nil {
		t.Fatalf("hostname auto-weight run failed: %v\nstdout=%s\nstderr=%s", runErr, stdout, stderr)
	}
	for number := 1; number <= len(fixture.sources); number++ {
		if !strings.Contains(stdout, "transfer "+strconv.Itoa(number)+" completed: exit=0 signal=0") {
			t.Fatalf("auto-weight summary omits transfer %d: %q", number, stdout)
		}
	}
	if resolver.udpQueries.Load() < uint64(len(fixture.sources)) ||
		resolver.tcpQueries.Load() < uint64(len(fixture.sources)) {
		t.Fatalf("host resolver traffic udp=%d tcp=%d, want both transports for every transfer",
			resolver.udpQueries.Load(), resolver.tcpQueries.Load())
	}
	assertPublicRsyncTree(t, destination, os.Getenv(supervisorTransferEnv))
	assertOverlappingMeasureWindow(t, drainSourceObservations(fixture.measured), fixture.sources)
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("hostname auto-weight run left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicRunExplicitDNSQueriesDirectlyFromEveryTransferNamespace(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	drainSourceObservations(fixture.measured)

	hostname := fmt.Sprintf("%s-direct-dns.transferlanes.test.", fixture.prefix)
	resolver := startNamespaceResolver(t, fixture.router, fixture.remote, hostname, fixture.remote)
	scenario := fixture.newRunSupervisorScenario(t, "explicit-direct-dns")
	destination := filepath.Join(scenario.markers, "rsync-destination")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	arguments := []string{"run", "--no-tui", "--source", os.Getenv(supervisorTransferEnv),
		"--dns", fixture.remote.String()}
	for _, source := range fixture.sources {
		arguments = append(arguments, "--network", source.String())
	}
	arguments = append(arguments, "--auto-weight", "--measure-url",
		"http://"+net.JoinHostPort(strings.TrimSuffix(hostname, "."), "18081")+"/measure",
		"--measure-duration", "500ms", "--", "/usr/bin/rsync", "-a", "{}", destination+"/")

	stdout, stderr, runErr := runPublicTransferLanesPipes(t, arguments)
	if runErr != nil {
		t.Fatalf("explicit DNS auto-weight run failed: %v\nstdout=%s\nstderr=%s", runErr, stdout, stderr)
	}
	seen := make(map[netip.Addr]struct{})
	for _, source := range resolver.querySources.snapshot() {
		seen[source] = struct{}{}
	}
	for _, source := range fixture.sources {
		if _, found := seen[source]; !found {
			t.Fatalf("explicit DNS did not query directly from %s; observed=%v", source, seen)
		}
	}
	assertPublicRsyncTree(t, destination, os.Getenv(supervisorTransferEnv))
	assertOverlappingMeasureWindow(t, drainSourceObservations(fixture.measured), fixture.sources)
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("explicit DNS run left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicListAndRunKeepHostnameFailureStagesConsistent(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	fixtureBaseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	listSource, listEndpoint, diagnosticPort, closeMeasureEndpoint := startSharedFailureEndpoint(t, fixture)
	activeBaseline := fixture.hostSurface()
	hostname := fmt.Sprintf("%s-stage.transferlanes.test.", fixture.prefix)
	resolver := startHostLocalResolverAnswers(t, hostname, []netip.Addr{fixture.remote, listEndpoint})
	resolver.installHostConfiguration(t)
	baseHost := strings.TrimSuffix(hostname, ".")
	assertResolverNXDOMAIN(t, resolver, "missing-"+hostname)
	for _, test := range []struct {
		name     string
		endpoint string
		stage    string
	}{
		{name: "NXDOMAIN", endpoint: "http://missing-" + baseHost + ":" + diagnosticPort + "/measure", stage: "resolve endpoint"},
		{name: "connection-refused", endpoint: "http://" + baseHost + ":18089/measure", stage: "connect endpoint"},
		{name: "TLS", endpoint: "https://" + baseHost + ":" + diagnosticPort + "/measure", stage: "negotiate TLS"},
		{name: "HTTP-503", endpoint: "http://" + baseHost + ":" + diagnosticPort + "/measure-fail", stage: "validate endpoint"},
	} {
		t.Run(test.name, func(t *testing.T) {
			listArguments := []string{"list", "--network", listSource.String(), "--measure",
				"--measure-url", test.endpoint, "--measure-duration", rootMeasureDuration.String()}
			listStdout, listStderr, listErr := runPublicTransferLanesPipes(t, listArguments)
			if listErr == nil || !strings.Contains(listStdout+listStderr, test.stage) {
				t.Fatalf("list failure=%v did not retain %q:\nstdout=%s\nstderr=%s",
					listErr, test.stage, listStdout, listStderr)
			}

			fixture.newRunSupervisorScenario(t, "hostname-stage-"+test.name)
			runArguments := []string{"run", "--no-tui", "--source", os.Getenv(supervisorTransferEnv),
				"--network", fixture.sources[0].String(), "--network", fixture.sources[1].String(),
				"--auto-weight", "--measure-url", test.endpoint,
				"--measure-duration", rootMeasureDuration.String(), "--", "/bin/true", "{}"}
			runStdout, runStderr, runErr := runPublicTransferLanesPipes(t, runArguments)
			if runErr == nil || !strings.Contains(runStdout+runStderr, test.stage) {
				t.Fatalf("run failure=%v did not retain %q:\nstdout=%s\nstderr=%s",
					runErr, test.stage, runStdout, runStderr)
			}
			if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
				t.Fatalf("failed run left run roots: before=%v after=%v", runsBefore, after)
			}
			fixture.assertSurface(activeBaseline)
		})
	}
	resolver.restoreHostConfiguration(t)
	closeMeasureEndpoint()
	fixture.assertSurface(fixtureBaseline)
}

func assertResolverNXDOMAIN(t *testing.T, resolver *hostLocalResolver, hostname string) {
	t.Helper()
	name, err := dnsmessage.NewName(hostname)
	if err != nil {
		t.Fatal(err)
	}
	queryMessage := dnsmessage.Message{Header: dnsmessage.Header{ID: 91, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	query, err := queryMessage.Pack()
	if err != nil {
		t.Fatal(err)
	}
	response, err := resolver.response(query, false)
	var message dnsmessage.Message
	unpackErr := message.Unpack(response)
	if err != nil || unpackErr != nil || message.RCode != dnsmessage.RCodeNameError {
		t.Fatalf("fixture response code=%v responseError=%v unpackError=%v, want NXDOMAIN",
			message.RCode, err, unpackErr)
	}
}

func startSharedFailureEndpoint(t *testing.T, fixture *hostNetworkFixture) (netip.Addr, netip.Addr, string, func()) {
	t.Helper()
	sequence := int(labSequence.Add(1) & 0xff)
	name := fmt.Sprintf("sf%04x", sequence)
	table := 42000 + sequence
	priority := 15000 + sequence
	source := netip.MustParseAddr(fmt.Sprintf("10.220.%d.2", sequence))
	endpoint := netip.MustParseAddr(fmt.Sprintf("10.220.%d.200", sequence))
	port := "18081"
	fixture.run("ip", "link", "add", name, "type", "dummy")
	fixture.run("ip", "link", "set", name, "up")
	fixture.run("ip", "addr", "add", source.String()+"/32", "dev", name)
	fixture.run("ip", "addr", "add", endpoint.String()+"/32", "dev", name)
	fixture.run("ip", "route", "add", "table", strconv.Itoa(table), "default", "dev", name)
	fixture.run("ip", "route", "add", "table", strconv.Itoa(table), "unreachable", fixture.remote.String()+"/32")
	fixture.run("ip", "rule", "add", "priority", strconv.Itoa(priority),
		"from", source.String()+"/32", "table", strconv.Itoa(table))
	listener, err := net.Listen("tcp4", net.JoinHostPort(endpoint.String(), port))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		if request.URL.Path == "/measure-fail" {
			http.Error(writer, "controlled measure failure", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(writer, "ok")
	})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- server.Serve(listener) }()
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			_ = server.Close()
			if err := <-serveDone; err != nil && !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("join shared failure endpoint: %v", err)
			}
			fixture.runCleanup("ip", "rule", "del", "priority", strconv.Itoa(priority))
			fixture.runCleanup("ip", "route", "flush", "table", strconv.Itoa(table))
			fixture.runCleanup("ip", "link", "del", name)
		})
	}
	t.Cleanup(cleanup)
	return source, endpoint, port, cleanup
}

type hostLocalResolver struct {
	udp          net.PacketConn
	tcp          net.Listener
	hostname     string
	notFoundName string
	answers      []netip.Addr
	udpQueries   atomic.Uint64
	tcpQueries   atomic.Uint64
	contextDone  chan struct{}
	querySources receiverSourceLog
	wait         sync.WaitGroup
	resolverInfo os.FileInfo
	resolverText []byte
	restored     bool
	answerDelay  atomic.Int64
}

func startHostLocalResolver(t *testing.T, hostname string, answer netip.Addr) *hostLocalResolver {
	return startHostLocalResolverAnswers(t, hostname, []netip.Addr{answer})
}

func startDelayedHostLocalResolver(t *testing.T, hostname string, answer netip.Addr,
	delay time.Duration,
) *hostLocalResolver {
	resolver := startHostLocalResolver(t, hostname, answer)
	resolver.answerDelay.Store(int64(delay))
	return resolver
}

func startHostLocalResolverAnswers(t *testing.T, hostname string, answers []netip.Addr) *hostLocalResolver {
	t.Helper()
	address := "127.0.0.54:53"
	udp, err := net.ListenPacket("udp4", address)
	if err != nil {
		t.Fatalf("listen host-local UDP DNS: %v", err)
	}
	tcp, err := net.Listen("tcp4", address)
	if err != nil {
		_ = udp.Close()
		t.Fatalf("listen host-local TCP DNS: %v", err)
	}
	return startResolver(t, udp, tcp, hostname, answers)
}

func startNamespaceResolver(t *testing.T, namespace string, address netip.Addr,
	hostname string, answer netip.Addr) *hostLocalResolver {
	t.Helper()
	type listeners struct {
		udp net.PacketConn
		tcp net.Listener
		err error
	}
	result := make(chan listeners, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		host, err := netns.Get()
		if err != nil {
			result <- listeners{err: err}
			return
		}
		defer host.Close()
		target, err := netns.GetFromName(namespace)
		if err == nil {
			defer target.Close()
			err = netns.Set(target)
		}
		var opened listeners
		if err == nil {
			opened.udp, err = net.ListenPacket("udp4", net.JoinHostPort(address.String(), "53"))
		}
		if err == nil {
			opened.tcp, err = net.Listen("tcp4", net.JoinHostPort(address.String(), "53"))
		}
		if err != nil && opened.udp != nil {
			_ = opened.udp.Close()
		}
		opened.err = errors.Join(err, netns.Set(host))
		result <- opened
	}()
	opened := <-result
	if opened.err != nil {
		t.Fatalf("listen namespace DNS: %v", opened.err)
	}
	return startResolver(t, opened.udp, opened.tcp, hostname, []netip.Addr{answer})
}

func startResolver(t *testing.T, udp net.PacketConn, tcp net.Listener,
	hostname string, answers []netip.Addr) *hostLocalResolver {
	t.Helper()
	resolver := &hostLocalResolver{udp: udp, tcp: tcp, hostname: hostname,
		notFoundName: "missing-" + hostname, answers: slices.Clone(answers),
		contextDone: make(chan struct{})}
	resolver.wait.Add(2)
	go resolver.serveUDP(t)
	go resolver.serveTCP(t)
	t.Cleanup(func() {
		resolver.restoreHostConfiguration(t)
		close(resolver.contextDone)
		_ = resolver.udp.Close()
		_ = resolver.tcp.Close()
		resolver.wait.Wait()
	})
	return resolver
}

func (resolver *hostLocalResolver) installHostConfiguration(t *testing.T) {
	t.Helper()
	content, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat("/etc/resolv.conf")
	if err != nil {
		t.Fatal(err)
	}
	resolver.resolverText, resolver.resolverInfo = content, info
	resolver.writeHostConfiguration(t, []byte(
		"nameserver 127.0.0.54\n"+
			"nameserver 192.0.2.1\n"+
			"nameserver 192.0.2.2\n"+
			"nameserver 192.0.2.3\n"+
			"options timeout:1 attempts:1\n",
	))
}

func (resolver *hostLocalResolver) restoreHostConfiguration(t *testing.T) {
	t.Helper()
	if resolver == nil || resolver.resolverInfo == nil || resolver.restored {
		return
	}
	resolver.writeHostConfiguration(t, resolver.resolverText)
	resolver.restored = true
	content, err := os.ReadFile("/etc/resolv.conf")
	info, statErr := os.Stat("/etc/resolv.conf")
	if err != nil || statErr != nil || !bytes.Equal(content, resolver.resolverText) ||
		!os.SameFile(info, resolver.resolverInfo) {
		t.Fatalf("host resolver was not restored in place: read=%v stat=%v", err, statErr)
	}
}

func (resolver *hostLocalResolver) writeHostConfiguration(t *testing.T, content []byte) {
	t.Helper()
	file, err := os.OpenFile("/etc/resolv.conf", os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(content)
	closeErr := file.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		t.Fatal(err)
	}
}

func (resolver *hostLocalResolver) serveUDP(t *testing.T) {
	defer resolver.wait.Done()
	buffer := make([]byte, 4096)
	for {
		count, peer, err := resolver.udp.ReadFrom(buffer)
		if err != nil {
			select {
			case <-resolver.contextDone:
				return
			default:
				t.Errorf("host-local UDP DNS read: %v", err)
				return
			}
		}
		resolver.udpQueries.Add(1)
		resolver.observeQuerySource(peer.String())
		response, err := resolver.response(buffer[:count], true)
		if err != nil {
			t.Errorf("host-local UDP DNS response: %v", err)
			continue
		}
		if _, err := resolver.udp.WriteTo(response, peer); err != nil {
			t.Errorf("host-local UDP DNS write: %v", err)
		}
	}
}

func (resolver *hostLocalResolver) serveTCP(t *testing.T) {
	defer resolver.wait.Done()
	for {
		connection, err := resolver.tcp.Accept()
		if err != nil {
			select {
			case <-resolver.contextDone:
				return
			default:
				t.Errorf("host-local TCP DNS accept: %v", err)
				return
			}
		}
		resolver.wait.Add(1)
		go func() {
			defer resolver.wait.Done()
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
			resolver.observeQuerySource(connection.RemoteAddr().String())
			if err := resolver.serveTCPConnection(connection); err != nil {
				t.Errorf("host-local TCP DNS: %v", err)
			}
		}()
	}
}

func (resolver *hostLocalResolver) observeQuerySource(peer string) {
	host, _, err := net.SplitHostPort(peer)
	if err != nil {
		return
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return
	}
	resolver.querySources.record(address)
}

func (resolver *hostLocalResolver) serveTCPConnection(connection net.Conn) error {
	var length [2]byte
	if _, err := io.ReadFull(connection, length[:]); err != nil {
		return err
	}
	size := int(length[0])<<8 | int(length[1])
	if size < 12 || size > 4096 {
		return errors.New("host-local DNS query length is invalid")
	}
	query := make([]byte, size)
	if _, err := io.ReadFull(connection, query); err != nil {
		return err
	}
	resolver.tcpQueries.Add(1)
	response, err := resolver.response(query, false)
	if err != nil {
		return err
	}
	length[0], length[1] = byte(len(response)>>8), byte(len(response))
	_, err = connection.Write(append(length[:], response...))
	return err
}

func (resolver *hostLocalResolver) response(query []byte, truncated bool) ([]byte, error) {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil || header.Response {
		return nil, errors.Join(errors.New("host-local DNS query is invalid"), err)
	}
	question, err := parser.Question()
	if err != nil {
		return nil, err
	}
	responseCode := dnsmessage.RCodeSuccess
	if strings.EqualFold(question.Name.String(), resolver.notFoundName) {
		responseCode = dnsmessage.RCodeNameError
	}
	builder := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: header.ID, Response: true,
		RecursionDesired: header.RecursionDesired, RecursionAvailable: true,
		Truncated: truncated, RCode: responseCode})
	builder.EnableCompression()
	if err := builder.StartQuestions(); err != nil {
		return nil, err
	}
	if err := builder.Question(question); err != nil {
		return nil, err
	}
	if truncated || responseCode == dnsmessage.RCodeNameError {
		return builder.Finish()
	}
	if question.Type != dnsmessage.TypeA || !strings.EqualFold(question.Name.String(), resolver.hostname) {
		return builder.Finish()
	}
	if delay := time.Duration(resolver.answerDelay.Load()); delay > 0 {
		time.Sleep(delay)
	}
	if err := builder.StartAnswers(); err != nil {
		return nil, err
	}
	for _, answer := range resolver.answers {
		if err := builder.AResource(dnsmessage.ResourceHeader{
			Name: question.Name, Type: dnsmessage.TypeA, Class: question.Class, TTL: 0},
			dnsmessage.AResource{A: answer.As4()}); err != nil {
			return nil, err
		}
	}
	return builder.Finish()
}
