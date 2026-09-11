package listnetworks

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/probe"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
)

type publicObservation struct {
	publicIP     netip.Addr
	asn          uint32
	organization string
}

type throughputObservation struct {
	localIP  netip.Addr
	bytes    uint64
	duration time.Duration
	mbps     float64
	weight   uint64
	err      error
}

type captureNetworksFunc func(context.Context) (capturedNetworks, error)
type probeSourceFunc func(context.Context, netip.Addr, probe.Endpoint) (publicObservation, error)
type measureSourcesFunc func(context.Context, []throughput.Target, throughput.Settings) []throughputObservation

// Listing composes one immutable catalog capture with explicitly authorized diagnostics.
type Listing struct {
	capture      captureNetworksFunc
	probeSource  probeSourceFunc
	measureGroup measureSourcesFunc
}

// New constructs the read-only list use case. It does not touch RunDirectory or HostNetwork.
func New() *Listing {
	observer := probe.New()
	tester := throughput.New()
	return newListing(captureSystemNetworks,
		func(ctx context.Context, source netip.Addr, endpoint probe.Endpoint) (publicObservation, error) {
			observed, err := observer.Observe(ctx, source, endpoint)
			if err != nil {
				return publicObservation{}, err
			}
			publicIP, _ := observed.PublicIP()
			organization, _ := observed.Organization()
			return publicObservation{publicIP: publicIP, asn: observed.ASN(), organization: organization}, nil
		},
		func(ctx context.Context, targets []throughput.Target, settings throughput.Settings) []throughputObservation {
			return observeThroughputs(tester.Observe(ctx, targets, settings))
		})
}

func newListing(capture captureNetworksFunc, probeSource probeSourceFunc,
	measureGroup measureSourcesFunc) *Listing {
	return &Listing{capture: capture, probeSource: probeSource, measureGroup: measureGroup}
}

// Run validates selection first, captures once, then annotates only eligible rows requested by the caller.
func (listing *Listing) Run(ctx context.Context, request Request) (Result, error) {
	validated, err := validateRequest(request)
	if err != nil {
		return Result{}, err
	}
	if listing == nil || listing.capture == nil || ctx == nil {
		return Result{}, errors.New("network listing inputs are incomplete")
	}
	captured, err := listing.capture(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("capture local networks: %w", err)
	}
	selected := selectLocalNetworks(captured, validated)
	rows, err := rowsFromSelection(captured, selected)
	if err != nil {
		return Result{}, err
	}
	if len(validated.explicit) == 0 && !validated.all {
		rows = runnableRows(rows)
	}
	result := Result{rows: rows, runtimeFailure: explicitSelectionFailed(validated, rows)}
	active := activeRows(rows)
	if validated.probe != nil {
		if err := listing.addProbeResults(ctx, &result, active, *validated.probe); err != nil {
			return Result{}, err
		}
	}
	if validated.measure != nil {
		if err := listing.addMeasureResults(ctx, &result, active, *validated.measure); err != nil {
			return Result{}, err
		}
	}
	return result, nil
}

func runnableRows(rows []Row) []Row {
	result := make([]Row, 0, len(rows))
	for _, row := range rows {
		if row.runnable {
			result = append(result, row)
		}
	}
	return result
}

type selectedNetwork struct {
	address localNetwork
	present bool
}

func selectLocalNetworks(captured capturedNetworks, request validatedRequest) []selectedNetwork {
	if len(request.explicit) != 0 {
		selected := make([]selectedNetwork, len(request.explicit))
		for index, source := range request.explicit {
			selected[index] = findLocalNetwork(captured.addresses, source)
			if !selected[index].present {
				selected[index].address.localIP = source
			}
		}
		return selected
	}
	addresses := captured.defaults
	if request.all {
		addresses = captured.addresses
	}
	return selectUniqueLocalNetworks(addresses)
}

func selectUniqueLocalNetworks(addresses []localNetwork) []selectedNetwork {
	selected := make([]selectedNetwork, 0, len(addresses))
	indexes := make(map[netip.Addr]int, len(addresses))
	for _, address := range addresses {
		index, exists := indexes[address.localIP]
		if !exists {
			indexes[address.localIP] = len(selected)
			selected = append(selected, selectedNetwork{address: address, present: true})
			continue
		}
		mergeLocalNetwork(&selected[index], address)
	}
	return selected
}

func findLocalNetwork(addresses []localNetwork, source netip.Addr) selectedNetwork {
	var selected selectedNetwork
	for _, address := range addresses {
		if address.localIP != source {
			continue
		}
		mergeLocalNetwork(&selected, address)
	}
	return selected
}

func mergeLocalNetwork(selected *selectedNetwork, address localNetwork) {
	if !selected.present {
		*selected = selectedNetwork{address: address, present: true}
		return
	}
	if !address.included {
		selected.address.included = false
		selected.address.exclusionReason = address.exclusionReason
	}
}

func rowsFromSelection(captured capturedNetworks, selected []selectedNetwork) ([]Row, error) {
	if captured.resolve == nil {
		return nil, errors.New("captured network resolver is unavailable")
	}
	sources := make([]netip.Addr, len(selected))
	for index := range selected {
		sources[index] = selected[index].address.localIP
	}
	resolved := captured.resolve(sources)
	if len(resolved) != len(selected) {
		return nil, errors.New("network catalog returned an incomplete egress set")
	}
	expected := make(map[netip.Addr]struct{}, len(selected))
	for _, network := range selected {
		localIP := network.address.localIP
		if !localIP.Is4() {
			return nil, errors.New("selected network has no local IPv4 identity")
		}
		if _, duplicate := expected[localIP]; duplicate {
			return nil, fmt.Errorf("selected network repeats local IP %s", localIP)
		}
		expected[localIP] = struct{}{}
	}
	byLocalIP := make(map[netip.Addr]resolvedNetwork, len(resolved))
	for _, egress := range resolved {
		if !egress.localIP.Is4() {
			return nil, errors.New("network catalog returned an egress without local IPv4 identity")
		}
		if _, exists := expected[egress.localIP]; !exists {
			return nil, fmt.Errorf("network catalog returned unknown egress %s", egress.localIP)
		}
		if _, duplicate := byLocalIP[egress.localIP]; duplicate {
			return nil, fmt.Errorf("network catalog repeated egress %s", egress.localIP)
		}
		byLocalIP[egress.localIP] = egress
	}
	rows := make([]Row, len(selected))
	for index := range selected {
		egress, exists := byLocalIP[selected[index].address.localIP]
		if !exists {
			return nil, fmt.Errorf("network catalog omitted egress %s", selected[index].address.localIP)
		}
		rows[index] = newRow(selected[index], egress)
	}
	return rows, nil
}

func newRow(selected selectedNetwork, egress resolvedNetwork) Row {
	address := selected.address
	row := Row{localIP: address.localIP, interfaceName: address.interfaceName,
		interfaceIndex: address.interfaceIndex, includedByDefault: selected.present && address.included,
		table: egress.table, gateway: egress.gateway, hasGateway: egress.hasGateway}
	if egress.interfaceName != "" {
		row.interfaceName, row.interfaceIndex = egress.interfaceName, egress.interfaceIndex
	}
	switch {
	case !selected.present:
		row.localReason = egress.reason
	case !address.included:
		row.localReason = address.exclusionReason
	case !egress.runnable:
		row.localReason = egress.reason
	default:
		row.runnable = true
	}
	if !row.runnable && row.localReason == "" {
		row.localReason = "selected network is not runnable"
	}
	return row
}

func explicitSelectionFailed(request validatedRequest, rows []Row) bool {
	if len(request.explicit) == 0 {
		return false
	}
	for _, row := range rows {
		if !row.runnable {
			return true
		}
	}
	return false
}

func activeRows(rows []Row) []int {
	result := make([]int, 0, len(rows))
	for index, row := range rows {
		if row.runnable {
			result = append(result, index)
		}
	}
	return result
}

func (listing *Listing) addProbeResults(ctx context.Context, result *Result, active []int,
	endpoint probe.Endpoint) error {
	for index := range result.rows {
		result.rows[index].probe = &ProbeStatus{}
	}
	if len(active) == 0 {
		return nil
	}
	if listing.probeSource == nil {
		return errors.New("probe capability is unavailable")
	}
	var workers sync.WaitGroup
	workers.Add(len(active))
	for _, rowIndex := range active {
		go func(rowIndex int) {
			defer workers.Done()
			observed, err := listing.probeSource(ctx, result.rows[rowIndex].localIP, endpoint)
			result.rows[rowIndex].probe = &ProbeStatus{attempted: true, publicIP: observed.publicIP,
				asn: observed.asn, organization: observed.organization, err: err}
		}(rowIndex)
	}
	workers.Wait()
	for _, rowIndex := range active {
		if result.rows[rowIndex].probe.err != nil {
			result.runtimeFailure = true
		}
	}
	return nil
}

func (listing *Listing) addMeasureResults(ctx context.Context, result *Result, active []int,
	window throughput.Settings) error {
	for index := range result.rows {
		result.rows[index].measure = &MeasureStatus{}
	}
	if len(active) == 0 {
		return nil
	}
	if listing.measureGroup == nil {
		return errors.New("measure capability is unavailable")
	}
	targets := make([]throughput.Target, len(active))
	for index, rowIndex := range active {
		var err error
		targets[index], err = throughput.NewSourceBoundTarget(result.rows[rowIndex].localIP)
		if err != nil {
			return err
		}
	}
	observed := listing.measureGroup(ctx, targets, window)
	if len(observed) != len(targets) {
		return errors.New("measure capability returned an incomplete result set")
	}
	expected := make(map[netip.Addr]struct{}, len(targets))
	for _, target := range targets {
		expected[target.LocalIP()] = struct{}{}
	}
	byNetwork := make(map[netip.Addr]throughputObservation, len(observed))
	for _, value := range observed {
		if _, exists := expected[value.localIP]; !exists {
			return errors.New("measure result identifies an unknown network")
		}
		if _, duplicate := byNetwork[value.localIP]; duplicate {
			return errors.New("measure result repeats a network")
		}
		byNetwork[value.localIP] = value
	}
	for _, rowIndex := range active {
		value, exists := byNetwork[result.rows[rowIndex].localIP]
		if !exists {
			return errors.New("measure result set is missing a network")
		}
		result.rows[rowIndex].measure = &MeasureStatus{attempted: true, bytes: value.bytes,
			duration: value.duration, mbps: value.mbps, weight: value.weight, err: value.err}
		if value.err != nil {
			result.runtimeFailure = true
		}
	}
	return nil
}

func observeThroughputs(results []throughput.Observation) []throughputObservation {
	observed := make([]throughputObservation, len(results))
	indexes := make(map[netip.Addr]int, len(results))
	for index, result := range results {
		localIP := result.Target().LocalIP()
		observed[index] = throughputObservation{localIP: localIP, bytes: result.Bytes(),
			duration: result.Duration(), mbps: result.Mbps(), err: result.Err()}
		indexes[localIP] = index
	}
	weights, err := throughput.RelativeWeights(results, true)
	if err != nil {
		for index := range observed {
			if observed[index].err == nil {
				observed[index].err = fmt.Errorf("normalize measure group: %w", err)
			}
		}
		return observed
	}
	for _, weight := range weights {
		localIP := weight.Target().LocalIP()
		observedIndex := indexes[localIP]
		observed[observedIndex].weight = weight.Value()
	}
	return observed
}
