package transfer

import (
	"context"
	"errors"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestFinalWeightsUseManualSelectionWithoutMeasuring(t *testing.T) {
	source := sourceTree(t)
	request := validManualRequest(t, source)
	measurer := &recordingWeightMeasurer{}
	directory := &TransferDirectory{weights: measurer}

	weights, owner, err := directory.finalWeights(context.Background(), nil, request, nil, source)
	if err != nil || owner != nil || measurer.calls != 0 {
		t.Fatalf("manual weights err=%v owner=%v measure calls=%d", err, owner, measurer.calls)
	}
	allocation := allocateTestSource(t, source, weights)
	selected := request.selectedNetworks()
	firstBytes, firstPresent := allocation.TotalBytes(selected[0].transfer)
	secondBytes, secondPresent := allocation.TotalBytes(selected[1].transfer)
	if !firstPresent || !secondPresent || firstBytes != 8 || secondBytes != 4 {
		t.Fatalf("manual weighted allocation=%d/%d present=%t/%t",
			firstBytes, secondBytes, firstPresent, secondPresent)
	}
}

func TestFinalWeightsPreserveAutomaticMeasurementValues(t *testing.T) {
	source := sourceTree(t)
	request := validAutomaticRequest(t, source)
	selected := request.selectedNetworks()
	first := selected[0].transfer
	second := selected[1].transfer
	measurer := &recordingWeightMeasurer{weights: []sourcefiles.TransferWeight{
		newTestTransferWeight(t, second, 1),
		newTestTransferWeight(t, first, 9),
	}}
	directory := &TransferDirectory{weights: measurer}

	weights, owner, err := directory.finalWeights(context.Background(), nil, request, nil, source)
	if err != nil || owner != nil || measurer.calls != 1 {
		t.Fatalf("automatic weights err=%v owner=%v measure calls=%d", err, owner, measurer.calls)
	}
	allocation := allocateTestSource(t, source, weights)
	firstBytes, firstPresent := allocation.TotalBytes(first)
	secondBytes, secondPresent := allocation.TotalBytes(second)
	if !firstPresent || !secondPresent || firstBytes != 9 || secondBytes != 3 {
		t.Fatalf("automatic weighted allocation=%d/%d present=%t/%t",
			firstBytes, secondBytes, firstPresent, secondPresent)
	}
}

func TestFinalWeightsRejectInvalidAutomaticMeasurementSet(t *testing.T) {
	source := sourceTree(t)
	request := validAutomaticRequest(t, source)
	selected := request.selectedNetworks()
	first := selected[0].transfer
	second := selected[1].transfer
	third, err := transfernumber.New(3)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name    string
		weights []sourcefiles.TransferWeight
	}{
		{name: "incomplete", weights: transferWeights(t, first)},
		{name: "extra", weights: transferWeights(t, first, second, third)},
		{name: "unknown", weights: transferWeights(t, first, third)},
		{name: "duplicate", weights: transferWeights(t, first, first)},
	} {
		t.Run(test.name, func(t *testing.T) {
			measurer := &recordingWeightMeasurer{weights: test.weights}
			directory := &TransferDirectory{weights: measurer}
			_, owner, err := directory.finalWeights(context.Background(), nil, request, nil, source)
			if err == nil || owner != nil || measurer.calls != 1 {
				t.Fatalf("automatic weights err=%v owner=%v measure calls=%d", err, owner, measurer.calls)
			}
		})
	}
}

func TestFinalWeightsPreserveAutomaticMeasurementOwnerOnFailure(t *testing.T) {
	wantErr := errors.New("measurement helper failed")
	done := make(chan struct{})
	owner := &observedSupervisedProcesses{done: done}
	measurer := &recordingWeightMeasurer{err: wantErr, owner: owner}
	source := sourceTree(t)
	directory := &TransferDirectory{weights: measurer}

	_, gotOwner, err := directory.finalWeights(context.Background(), nil,
		validAutomaticRequest(t, source), nil, source)
	if !errors.Is(err, wantErr) || gotOwner != owner || measurer.calls != 1 {
		t.Fatalf("automatic failure err=%v owner=%v measure calls=%d", err, gotOwner, measurer.calls)
	}
}

func allocateTestSource(t *testing.T, source string,
	weights sourcefiles.TransferWeights,
) sourcefiles.TransferAllocation {
	t.Helper()
	sourceRoot, err := sourcefiles.OpenSourceRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer sourceRoot.Close()
	snapshot, err := sourceRoot.Scan(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	allocation, err := sourcefiles.AllocateTransferItems(snapshot, weights)
	if err != nil {
		t.Fatal(err)
	}
	return allocation
}

func newTestTransferWeight(t *testing.T, transfer transfernumber.Number,
	weight uint64,
) sourcefiles.TransferWeight {
	t.Helper()
	result, err := sourcefiles.NewTransferWeight(transfer, weight)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func transferWeights(t *testing.T,
	transfers ...transfernumber.Number,
) []sourcefiles.TransferWeight {
	t.Helper()
	result := make([]sourcefiles.TransferWeight, len(transfers))
	for index, transfer := range transfers {
		weight, err := sourcefiles.NewTransferWeight(transfer, uint64(index+1))
		if err != nil {
			t.Fatal(err)
		}
		result[index] = weight
	}
	return result
}

type recordingWeightMeasurer struct {
	calls   int
	weights []sourcefiles.TransferWeight
	owner   supervisedProcesses
	err     error
}

func (measurer *recordingWeightMeasurer) measure(context.Context, *hostnetwork.Session, Request,
	resolverGroup, string,
) ([]sourcefiles.TransferWeight, supervisedProcesses, error) {
	measurer.calls++
	return measurer.weights, measurer.owner, measurer.err
}
