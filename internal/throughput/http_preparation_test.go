package throughput

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEndpointValidationFailureRetainsStage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		http.Error(writer, "denied", http.StatusForbidden)
	}))
	defer server.Close()
	target, _ := NewSourceBoundTarget(netip.MustParseAddr("127.0.0.1"))
	settings, _ := NewSettings(server.URL, minimumDuration)
	result := New().Observe(context.Background(), []Target{target}, settings)[0]
	if result.Err() == nil || !strings.Contains(result.Err().Error(), "validate endpoint") ||
		strings.Contains(result.Err().Error(), "no upload completed") {
		t.Fatalf("endpoint failure stage = %v", result.Err())
	}
}

func TestTransportFailuresRetainTheirStage(t *testing.T) {
	target, _ := NewSourceBoundTarget(netip.MustParseAddr("127.0.0.1"))
	t.Run("resolve", func(t *testing.T) {
		tester := New()
		tester.resolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("controlled resolver failure")
		}}
		settings, _ := NewSettings("http://unresolved.transferlanes.test/upload", minimumDuration)
		assertFailureStage(t, tester.Observe(context.Background(), []Target{target}, settings)[0], ResolveEndpoint)
	})
	t.Run("connect", func(t *testing.T) {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		address := listener.Addr().String()
		_ = listener.Close()
		settings, _ := NewSettings("http://"+address+"/upload", minimumDuration)
		assertFailureStage(t, New().Observe(context.Background(), []Target{target}, settings)[0], ConnectEndpoint)
	})
	t.Run("TLS", func(t *testing.T) {
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		server.Config.ErrorLog = log.New(io.Discard, "", 0)
		server.StartTLS()
		defer server.Close()
		settings, _ := NewSettings(server.URL, minimumDuration)
		assertFailureStage(t, New().Observe(context.Background(), []Target{target}, settings)[0], NegotiateTLS)
	})
}

func TestPreparationTimeoutRetainsTheActiveStage(t *testing.T) {
	target, _ := NewSourceBoundTarget(netip.MustParseAddr("127.0.0.1"))
	for _, test := range []struct {
		name  string
		stage FailureStage
		setup func(*testing.T, *Tester) string
	}{
		{name: "DNS", stage: ResolveEndpoint, setup: func(_ *testing.T, tester *Tester) string {
			tester.resolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			return "http://timeout.transferlanes.test/upload"
		}},
		{name: "connect", stage: ConnectEndpoint, setup: func(_ *testing.T, tester *Tester) string {
			tester.connect = func(ctx context.Context, _ Target, _ []netip.Addr, _ string) (net.Conn, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return "http://192.0.2.1/upload"
		}},
		{name: "TLS", stage: NegotiateTLS, setup: func(t *testing.T, _ *Tester) string {
			return newTLSBlackhole(t)
		}},
		{name: "validation", stage: ValidateEndpoint, setup: func(t *testing.T, _ *Tester) string {
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
				select {
				case <-request.Context().Done():
				case <-release:
				}
			}))
			t.Cleanup(func() {
				close(release)
				server.Close()
			})
			return server.URL
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tester := New()
			endpoint := test.setup(t, tester)
			settings, err := NewSettings(endpoint, minimumDuration)
			if err != nil {
				t.Fatal(err)
			}
			settings.preparationTimeout = minimumDuration
			started := time.Now()
			observation := tester.Observe(context.Background(), []Target{target}, settings)[0]
			assertFailureStage(t, observation, test.stage)
			if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
				t.Fatalf("preparation timeout took %v", elapsed)
			}
		})
	}
}

func TestPreparationDeadlineIsPerTargetAndHealthyTargetGetsFullWindow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		_, _ = io.WriteString(writer, "ok")
	}))
	defer server.Close()
	stuck, _ := NewSourceBoundTarget(netip.MustParseAddr("127.0.0.1"))
	healthy, _ := NewCurrentNamespaceTarget(transferNumber(t, 1))
	tester := New()
	tester.connect = func(ctx context.Context, target Target, addresses []netip.Addr, port string) (net.Conn, error) {
		if target == stuck {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return dialAddresses(ctx, target, addresses, port)
	}
	settings, _ := NewSettings(server.URL, 150*time.Millisecond)
	settings.preparationTimeout = minimumDuration
	started := time.Now()
	results := tester.Observe(context.Background(), []Target{stuck, healthy}, settings)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("independent target measurement did not finish within deadlock ceiling: %v", elapsed)
	}
	assertFailureStage(t, results[0], ConnectEndpoint)
	if results[1].Err() != nil || results[1].Bytes() == 0 || results[1].Duration() != 150*time.Millisecond {
		t.Fatalf("healthy target result=%#v error=%v", results[1], results[1].Err())
	}
}

func TestHealthyTargetSamplesBeforeAnotherTargetPreparationTimesOut(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		_, _ = io.WriteString(writer, "ok")
	}))
	server.Config.IdleTimeout = 40 * time.Millisecond
	server.Start()
	defer server.Close()
	stuck, _ := NewSourceBoundTarget(netip.MustParseAddr("127.0.0.1"))
	healthy, _ := NewCurrentNamespaceTarget(transferNumber(t, 1))
	tester := New()
	tester.connect = func(ctx context.Context, target Target, addresses []netip.Addr, port string) (net.Conn, error) {
		if target == stuck {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return dialAddresses(ctx, target, addresses, port)
	}
	settings, _ := NewSettings(server.URL, minimumDuration)
	settings.preparationTimeout = 180 * time.Millisecond
	results := tester.Observe(context.Background(), []Target{stuck, healthy}, settings)
	assertFailureStage(t, results[0], ConnectEndpoint)
	if results[1].Err() != nil || results[1].Bytes() == 0 {
		t.Fatalf("healthy prepared connections idled behind another target: %#v, %v", results[1], results[1].Err())
	}
}

func TestHTTPSMeasurementUsesHTTP1EvenWhenServerOffersHTTP2(t *testing.T) {
	var protocolsMu sync.Mutex
	var protocols []int
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		protocolsMu.Lock()
		protocols = append(protocols, request.ProtoMajor)
		protocolsMu.Unlock()
		_, _ = io.WriteString(writer, "ok")
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	tester := New()
	tester.tlsRoots = roots
	target, _ := NewSourceBoundTarget(netip.MustParseAddr("127.0.0.1"))
	settings, _ := NewSettings(server.URL, minimumDuration)
	result := tester.Observe(context.Background(), []Target{target}, settings)[0]
	if result.Err() != nil || result.Bytes() == 0 {
		t.Fatalf("HTTP/1 measurement failed: %#v, %v", result, result.Err())
	}
	protocolsMu.Lock()
	defer protocolsMu.Unlock()
	if len(protocols) == 0 {
		t.Fatal("measurement server received no requests")
	}
	for _, protocol := range protocols {
		if protocol != 1 {
			t.Fatalf("measurement negotiated HTTP/%d with HTTP/2-capable server", protocol)
		}
	}
}

func TestPreparationDoesNotConsumeSampleWindow(t *testing.T) {
	var validations atomic.Int32
	var connectionsMu sync.Mutex
	validationConnections := make(map[string]struct{})
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		if request.ContentLength == 1 {
			validations.Add(1)
			connectionsMu.Lock()
			validationConnections[request.RemoteAddr] = struct{}{}
			connectionsMu.Unlock()
			time.Sleep(150 * time.Millisecond)
		}
		_, _ = io.WriteString(writer, "ok")
	}))
	server.EnableHTTP2 = false
	server.Start()
	defer server.Close()
	endpoint := server.URL + "/upload"
	target, _ := NewSourceBoundTarget(netip.MustParseAddr("127.0.0.1"))
	settings, _ := NewSettings(endpoint, minimumDuration)
	started := time.Now()
	result := New().Observe(context.Background(), []Target{target}, settings)[0]
	if result.Err() != nil || result.Bytes() == 0 || validations.Load() != int32(len(uploadRequestBytes)) {
		t.Fatalf("prepared observation = bytes %d validations %d error %v", result.Bytes(), validations.Load(), result.Err())
	}
	connectionsMu.Lock()
	connectionCount := len(validationConnections)
	connectionsMu.Unlock()
	if connectionCount != len(uploadRequestBytes) {
		t.Fatalf("validation used %d connections, want %d", connectionCount, len(uploadRequestBytes))
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Fatalf("preparation appears inside the sample window: %v", elapsed)
	}
}
