package probe

import (
	"context"
	"errors"
	"io"
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

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestEgressProbeUsesExactlyOneBoundGET(t *testing.T) {
	local := netip.MustParseAddr("192.0.2.41")
	var calls atomic.Int32
	var bound netip.Addr
	observer := newEgressProbe(func(source netip.Addr) http.RoundTripper {
		bound = source
		return roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			calls.Add(1)
			if request.Method != http.MethodGet {
				t.Fatalf("method = %s, want GET", request.Method)
			}
			return response(http.StatusOK,
				`{"ip":"203.0.113.9","success":true,"connection":{"asn":64512,"org":"Example ISP"}}`), nil
		})
	}, time.Second)
	endpoint := mustEgressEndpoint(t, "https://diagnostic.example/meta?format=json")

	result, err := observer.Observe(context.Background(), local, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || bound != local {
		t.Fatalf("requests=%d bound=%s, want one bound to %s", calls.Load(), bound, local)
	}
	publicIP, present := result.PublicIP()
	organization, hasOrganization := result.Organization()
	if publicIP != netip.MustParseAddr("203.0.113.9") || !present || result.ASN() != 64512 ||
		organization != "Example ISP" || !hasOrganization {
		t.Fatalf("result = %#v", result)
	}
}

func TestEgressProbeRejectsRedirectStatusAndResponseViolationsWithoutRetry(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "redirect", status: http.StatusFound, body: `{}`},
		{name: "other success", status: http.StatusNoContent, body: `{}`},
		{name: "oversize", status: http.StatusOK, body: strings.Repeat("x", maximumEgressResponseBytes+1)},
		{name: "invalid json", status: http.StatusOK, body: `{`},
		{name: "missing success", status: http.StatusOK, body: `{"ip":"203.0.113.9","connection":{"asn":64512,"org":"ISP"}}`},
		{name: "unsuccessful", status: http.StatusOK, body: `{"ip":"203.0.113.9","success":false,"connection":{"asn":64512,"org":"ISP"}}`},
		{name: "missing public ip", status: http.StatusOK, body: `{"success":true,"connection":{"asn":64512,"org":"ISP"}}`},
		{name: "invalid public ip", status: http.StatusOK, body: `{"ip":"bad","success":true,"connection":{"asn":64512,"org":"ISP"}}`},
		{name: "missing connection", status: http.StatusOK, body: `{"ip":"203.0.113.9","success":true}`},
		{name: "invalid asn", status: http.StatusOK, body: `{"ip":"203.0.113.9","success":true,"connection":{"asn":0,"org":"ISP"}}`},
		{name: "invalid organization", status: http.StatusOK, body: `{"ip":"203.0.113.9","success":true,"connection":{"asn":64512,"org":""}}`},
		{name: "oversize organization", status: http.StatusOK, body: `{"ip":"203.0.113.9","success":true,"connection":{"asn":64512,"org":"` + strings.Repeat("x", 257) + `"}}`},
		{name: "trailing json", status: http.StatusOK, body: `{"ip":"203.0.113.9","success":true,"connection":{"asn":64512,"org":"ISP"}}{}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			observer := newEgressProbe(func(netip.Addr) http.RoundTripper {
				return roundTripperFunc(func(*http.Request) (*http.Response, error) {
					calls.Add(1)
					return response(test.status, test.body), nil
				})
			}, time.Second)
			_, err := observer.Observe(context.Background(), netip.MustParseAddr("192.0.2.41"),
				mustEgressEndpoint(t, "https://diagnostic.example/meta"))
			if err == nil {
				t.Fatal("Observe succeeded, want error")
			}
			if calls.Load() != 1 {
				t.Fatalf("requests = %d, want exactly one", calls.Load())
			}
		})
	}
}

func TestEgressProbeEndpointAndDeadlineAreStrict(t *testing.T) {
	if DefaultEndpointURL != "https://ipwho.is/?fields=ip,success,connection" {
		t.Fatalf("default probe endpoint = %q", DefaultEndpointURL)
	}
	if defaultEgressTimeout != 10*time.Second {
		t.Fatalf("default probe timeout = %s, want 10s", defaultEgressTimeout)
	}
	for _, raw := range []string{"", "ftp://example.test/meta", "https://user@example.test/meta",
		"https://example.test/meta#fragment", "//example.test/meta", strings.Repeat("x", maximumEndpointBytes+1),
		"https://example.test/" + string([]byte{0xff})} {
		if _, err := NewEndpoint(raw); err == nil {
			t.Errorf("NewEndpoint(%q) succeeded", raw)
		}
	}
	endpoint := mustEgressEndpoint(t, "https://diagnostic.example/meta")
	observer := newEgressProbe(func(netip.Addr) http.RoundTripper {
		return roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
	}, 10*time.Millisecond)
	_, err := observer.Observe(context.Background(), netip.MustParseAddr("192.0.2.41"), endpoint)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline error = %v", err)
	}
}

func TestEgressProbeTrimsNonemptyOrganization(t *testing.T) {
	observer := newEgressProbe(func(netip.Addr) http.RoundTripper {
		return roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return response(http.StatusOK,
				`{"ip":"203.0.113.9","success":true,"connection":{"asn":64512,"org":"  Example ISP  "}}`), nil
		})
	}, time.Second)
	result, err := observer.Observe(context.Background(), netip.MustParseAddr("192.0.2.41"),
		mustEgressEndpoint(t, "https://diagnostic.example/meta"))
	if err != nil {
		t.Fatal(err)
	}
	if organization, present := result.Organization(); !present || organization != "Example ISP" {
		t.Fatalf("organization=%q present=%t", organization, present)
	}
}

func TestEgressProbeProductionSocketBindsSourceAndDoesNotFollowRedirect(t *testing.T) {
	var requests, redirected atomic.Int32
	var sourceMu sync.Mutex
	var observedSource string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path == "/redirected" {
			redirected.Add(1)
		}
		if request.URL.Path == "/start" {
			http.Redirect(writer, request, "/redirected", http.StatusFound)
			return
		}
		sourceMu.Lock()
		observedSource, _, _ = net.SplitHostPort(request.RemoteAddr)
		sourceMu.Unlock()
		_, _ = io.WriteString(writer,
			`{"ip":"203.0.113.9","success":true,"connection":{"asn":64512,"org":"Example ISP"}}`)
	}))
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	defer server.Close()

	source := netip.MustParseAddr("127.0.0.1")
	endpoint := mustEgressEndpoint(t, server.URL+"/meta")
	if _, err := New().Observe(context.Background(), source, endpoint); err != nil {
		t.Fatal(err)
	}
	sourceMu.Lock()
	gotSource := observedSource
	sourceMu.Unlock()
	if gotSource != source.String() {
		t.Fatalf("server observed source %s, want %s", gotSource, source)
	}
	redirect := mustEgressEndpoint(t, server.URL+"/start")
	if _, err := New().Observe(context.Background(), source, redirect); err == nil {
		t.Fatal("redirecting endpoint succeeded")
	}
	if redirected.Load() != 0 || requests.Load() != 2 {
		t.Fatalf("requests=%d redirected=%d, want 2/0", requests.Load(), redirected.Load())
	}
}

func mustEgressEndpoint(t *testing.T, raw string) Endpoint {
	t.Helper()
	endpoint, err := NewEndpoint(raw)
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)),
		Header: make(http.Header)}
}
