package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// DefaultEndpointURL is the one public-egress observation service used when no override is supplied.
	DefaultEndpointURL         = "https://ipwho.is/?fields=ip,success,connection"
	maximumEgressResponseBytes = 64 * 1024
	maximumEndpointBytes       = 2048
	defaultEgressTimeout       = 10 * time.Second
)

// Endpoint is one validated probe URL. A probe never redirects or falls back to another endpoint.
type Endpoint struct{ url url.URL }

// NewEndpoint validates one explicit endpoint using the same protocol as the default endpoint.
func NewEndpoint(raw string) (Endpoint, error) {
	if len(raw) == 0 || len(raw) > maximumEndpointBytes || !utf8.ValidString(raw) || strings.IndexByte(raw, 0) >= 0 {
		return Endpoint{}, errors.New("probe endpoint length or encoding is invalid")
	}
	parsed, err := parseEndpointURL(raw)
	if err != nil {
		return Endpoint{}, fmt.Errorf("probe endpoint: %w", err)
	}
	return Endpoint{url: parsed}, nil
}

// DefaultEndpoint returns the documented default public-egress endpoint.
func DefaultEndpoint() Endpoint {
	endpoint, _ := NewEndpoint(DefaultEndpointURL)
	return endpoint
}

func (endpoint Endpoint) valid() bool { return validateEndpointURL(endpoint.url) == nil }

// Valid reports whether the endpoint can be used without normalization or fallback.
func (endpoint Endpoint) Valid() bool { return endpoint.valid() }
func (endpoint Endpoint) string() string {
	copyOfURL := endpoint.url
	return copyOfURL.String()
}

// Egress is the public address and provider metadata observed by one bound request.
type Egress struct {
	publicIP     netip.Addr
	asn          uint32
	organization string
}

func (egress Egress) PublicIP() (netip.Addr, bool) { return egress.publicIP, egress.publicIP.IsValid() }
func (egress Egress) ASN() uint32                  { return egress.asn }
func (egress Egress) Organization() (string, bool) {
	return egress.organization, egress.organization != ""
}

// EgressProbe observes public facts through a socket bound to the requested local IPv4 address.
type EgressProbe struct {
	transport func(netip.Addr) http.RoundTripper
	timeout   time.Duration
}

// New constructs the production source-bound probe.
func New() *EgressProbe {
	return newEgressProbe(func(source netip.Addr) http.RoundTripper {
		return newBoundTransport(source, nil)
	}, defaultEgressTimeout)
}

func newEgressProbe(transport func(netip.Addr) http.RoundTripper, timeout time.Duration) *EgressProbe {
	return &EgressProbe{transport: transport, timeout: timeout}
}

// Observe performs exactly one GET through source and returns only validated endpoint facts.
func (probe *EgressProbe) Observe(ctx context.Context, source netip.Addr, endpoint Endpoint) (Egress, error) {
	if probe == nil || ctx == nil || !source.Is4() || !endpoint.valid() || probe.transport == nil ||
		probe.timeout <= 0 {
		return Egress{}, errors.New("probe inputs are invalid")
	}
	transport := probe.transport(source)
	if transport == nil {
		return Egress{}, errors.New("probe transport is unavailable")
	}
	requestContext, cancel := context.WithTimeout(ctx, probe.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, endpoint.string(), nil)
	if err != nil {
		return Egress{}, fmt.Errorf("create probe request: %w", err)
	}
	client := &http.Client{Transport: transport, CheckRedirect: noRedirect}
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return Egress{}, fmt.Errorf("perform probe request: %w", err)
	}
	return decodeEgress(response)
}

type egressResponse struct {
	IP         json.RawMessage `json:"ip"`
	Success    json.RawMessage `json:"success"`
	Connection json.RawMessage `json:"connection"`
}

type egressConnection struct {
	ASN          json.RawMessage `json:"asn"`
	Organization json.RawMessage `json:"org"`
}

func decodeEgress(response *http.Response) (Egress, error) {
	if response == nil || response.Body == nil {
		return Egress{}, errors.New("probe response is absent")
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return Egress{}, fmt.Errorf("probe endpoint returned status %d", response.StatusCode)
	}
	content, err := readBoundedBody(response.Body, maximumEgressResponseBytes)
	if err != nil {
		return Egress{}, fmt.Errorf("read probe response: %w", err)
	}
	if !utf8.Valid(content) {
		return Egress{}, errors.New("probe response is not valid UTF-8")
	}
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return Egress{}, errors.New("probe response must be one JSON object")
	}
	var wire egressResponse
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if err := decoder.Decode(&wire); err != nil {
		return Egress{}, fmt.Errorf("decode probe response: %w", err)
	}
	if err := ensureJSONEnd(decoder); err != nil {
		return Egress{}, err
	}
	return validateEgressResponse(wire)
}

func ensureJSONEnd(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("probe response contains trailing JSON")
	}
	return nil
}

func validateEgressResponse(wire egressResponse) (Egress, error) {
	var success bool
	if len(wire.Success) == 0 || json.Unmarshal(wire.Success, &success) != nil || !success {
		return Egress{}, errors.New("probe response does not report success")
	}
	var rawAddress string
	if len(wire.IP) == 0 || json.Unmarshal(wire.IP, &rawAddress) != nil {
		return Egress{}, errors.New("probe public IP is missing or invalid")
	}
	address, err := netip.ParseAddr(rawAddress)
	if err != nil || !address.IsValid() || address.IsUnspecified() {
		return Egress{}, errors.New("probe public IP is invalid")
	}
	var connection egressConnection
	if len(wire.Connection) == 0 || json.Unmarshal(wire.Connection, &connection) != nil {
		return Egress{}, errors.New("probe connection is missing or invalid")
	}
	var asn uint32
	if len(connection.ASN) == 0 || json.Unmarshal(connection.ASN, &asn) != nil || asn == 0 {
		return Egress{}, errors.New("probe ASN is missing or invalid")
	}
	var organization string
	if len(connection.Organization) == 0 || json.Unmarshal(connection.Organization, &organization) != nil ||
		len(organization) > 256 || !utf8.ValidString(organization) {
		return Egress{}, errors.New("probe organization is missing or invalid")
	}
	organization = strings.TrimSpace(organization)
	if organization == "" {
		return Egress{}, errors.New("probe organization is missing or invalid")
	}
	return Egress{publicIP: address, asn: asn, organization: organization}, nil
}
