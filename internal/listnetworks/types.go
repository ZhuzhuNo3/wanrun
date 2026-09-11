package listnetworks

import (
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/probe"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
)

// UsageError identifies list selection syntax that must fail before capture or active diagnostics.
type UsageError struct{ cause error }

func (failure *UsageError) Error() string { return failure.cause.Error() }
func (failure *UsageError) Unwrap() error { return failure.cause }

// Request is one already-separated list use-case input; it owns no host resources.
type Request struct {
	explicitRaw []string
	all         bool
	probe       *probe.Endpoint
	measure     *throughput.Settings
}

func (request Request) ProbeRequested() bool   { return request.probe != nil }
func (request Request) MeasureRequested() bool { return request.measure != nil }

// NewRequest validates selection before any catalog or HTTP work can begin.
func NewRequest(explicit []string, all bool, probeEndpoint *probe.Endpoint,
	measureWindow *throughput.Settings) (Request, error) {
	request := Request{explicitRaw: append([]string(nil), explicit...), all: all}
	if probeEndpoint != nil {
		copyOfEndpoint := *probeEndpoint
		request.probe = &copyOfEndpoint
	}
	if measureWindow != nil {
		copyOfWindow := *measureWindow
		request.measure = &copyOfWindow
	}
	if _, err := validateRequest(request); err != nil {
		return Request{}, err
	}
	return request, nil
}

type validatedRequest struct {
	explicit []netip.Addr
	all      bool
	probe    *probe.Endpoint
	measure  *throughput.Settings
}

func validateRequest(request Request) (validatedRequest, error) {
	if request.all && len(request.explicitRaw) != 0 {
		return validatedRequest{}, usage(errors.New("--all and explicit network selection are mutually exclusive"))
	}
	result := validatedRequest{all: request.all, probe: request.probe, measure: request.measure,
		explicit: make([]netip.Addr, len(request.explicitRaw))}
	seen := make(map[netip.Addr]struct{}, len(request.explicitRaw))
	for index, raw := range request.explicitRaw {
		address, err := netip.ParseAddr(raw)
		if err != nil || !address.Is4() {
			return validatedRequest{}, usage(fmt.Errorf("network %q is not a valid IPv4 address", raw))
		}
		if _, duplicate := seen[address]; duplicate {
			return validatedRequest{}, usage(fmt.Errorf("network %s is repeated", address))
		}
		seen[address] = struct{}{}
		result.explicit[index] = address
	}
	if request.probe != nil && !request.probe.Valid() {
		return validatedRequest{}, usage(errors.New("probe endpoint is invalid"))
	}
	if request.measure != nil && !request.measure.Valid() {
		return validatedRequest{}, usage(errors.New("measure window is invalid"))
	}
	return result, nil
}

func usage(err error) error { return &UsageError{cause: err} }

// Result preserves stable row order and the future CLI's aggregate runtime-exit semantic.
type Result struct {
	rows           []Row
	runtimeFailure bool
}

func (result Result) Rows() []Row          { return append([]Row(nil), result.rows...) }
func (result Result) RuntimeFailure() bool { return result.runtimeFailure }

// Row contains only local facts unless an active diagnostic was explicitly requested.
type Row struct {
	localIP           netip.Addr
	interfaceName     string
	interfaceIndex    int
	table             int
	gateway           netip.Addr
	hasGateway        bool
	includedByDefault bool
	runnable          bool
	localReason       string
	probe             *ProbeStatus
	measure           *MeasureStatus
}

func (row Row) LocalIP() netip.Addr         { return row.localIP }
func (row Row) InterfaceName() string       { return row.interfaceName }
func (row Row) InterfaceIndex() int         { return row.interfaceIndex }
func (row Row) RouteTable() int             { return row.table }
func (row Row) Gateway() (netip.Addr, bool) { return row.gateway, row.hasGateway }
func (row Row) IncludedByDefault() bool     { return row.includedByDefault }
func (row Row) Runnable() bool              { return row.runnable }
func (row Row) LocalReason() string         { return row.localReason }
func (row Row) Probe() (ProbeStatus, bool) {
	if row.probe == nil {
		return ProbeStatus{}, false
	}
	return *row.probe, true
}
func (row Row) Measure() (MeasureStatus, bool) {
	if row.measure == nil {
		return MeasureStatus{}, false
	}
	return *row.measure, true
}

// ProbeStatus exists only when probing was requested. Unattempted rows have no synthetic error.
type ProbeStatus struct {
	attempted    bool
	publicIP     netip.Addr
	asn          uint32
	organization string
	err          error
}

func (status ProbeStatus) Attempted() bool { return status.attempted }
func (status ProbeStatus) PublicIP() (netip.Addr, bool) {
	return status.publicIP, status.publicIP.IsValid()
}
func (status ProbeStatus) ASN() uint32 { return status.asn }
func (status ProbeStatus) Organization() (string, bool) {
	return status.organization, status.organization != ""
}
func (status ProbeStatus) Err() error { return status.err }

// MeasureStatus exists only when measurement was requested. A failed row has no weight; successful
// rows have relative weights only when at least two rows completed measurement.
type MeasureStatus struct {
	attempted bool
	bytes     uint64
	duration  time.Duration
	mbps      float64
	weight    uint64
	err       error
}

func (status MeasureStatus) Attempted() bool         { return status.attempted }
func (status MeasureStatus) Bytes() uint64           { return status.bytes }
func (status MeasureStatus) Duration() time.Duration { return status.duration }
func (status MeasureStatus) Mbps() float64           { return status.mbps }
func (status MeasureStatus) Err() error              { return status.err }
func (status MeasureStatus) HasWeight() bool         { return status.weight > 0 }
func (status MeasureStatus) Weight() (uint64, bool) {
	return status.weight, status.HasWeight()
}
