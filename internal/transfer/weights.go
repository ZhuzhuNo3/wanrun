package transfer

import (
	"context"
	"errors"
	"fmt"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type weightMeasurer interface {
	measure(context.Context, *hostnetwork.Session, Request, resolverGroup, string) (
		[]sourcefiles.TransferWeight, supervisedProcesses, error)
}

type throughputWeightMeasurer struct {
	tester    *throughput.Tester
	processes *childprocesses.ChildProcesses
}

func (measurer throughputWeightMeasurer) measure(ctx context.Context, network *hostnetwork.Session,
	request Request, resolvers resolverGroup, workingDirectory string,
) ([]sourcefiles.TransferWeight, supervisedProcesses, error) {
	selectedNetworks := request.selectedNetworks()
	targets := make([]throughput.NamespaceExecution, len(selectedNetworks))
	for index, selected := range selectedNetworks {
		resolver, err := resolvers.Access(selected.transfer)
		if err != nil {
			return nil, nil, err
		}
		target, err := throughput.NewNamespaceExecution(selected.transfer, workingDirectory,
			resolver, request.environment)
		if err != nil {
			return nil, nil, err
		}
		targets[index] = target
	}
	results, uncontained, err := measurer.tester.ObserveNamespaces(ctx, measurer.processes, network,
		targets, request.settings)
	if err != nil {
		if uncontained != nil {
			return nil, uncontained, err
		}
		return nil, nil, err
	}
	measured, err := throughput.RelativeWeights(results, false)
	if err != nil {
		return nil, nil, err
	}
	result := make([]sourcefiles.TransferWeight, len(measured))
	for index, weight := range measured {
		transfer := weight.Target().Transfer()
		if !weight.Target().HasTransfer() {
			return nil, nil, errors.New("automatic throughput weight has no transfer identity")
		}
		result[index], err = sourcefiles.NewTransferWeight(transfer, weight.Value())
		if err != nil {
			return nil, nil, err
		}
	}
	return result, nil, nil
}

func (transfer *TransferDirectory) finalWeights(ctx context.Context, network *hostnetwork.Session,
	request Request, resolvers resolverGroup, sourceRoot string,
) (sourcefiles.TransferWeights, supervisedProcesses, error) {
	if len(request.automatic) != 0 {
		weighted, uncontained, err := transfer.weights.measure(ctx, network, request, resolvers, sourceRoot)
		if err != nil {
			return sourcefiles.TransferWeights{}, uncontained,
				fmt.Errorf("measure automatic transfer weights: %w", err)
		}
		if err := validateWeightSet(request.selectedNetworks(), weighted); err != nil {
			return sourcefiles.TransferWeights{}, nil, err
		}
		weights, err := sourcefiles.NewTransferWeights(weighted)
		return weights, nil, err
	}
	weighted := make([]sourcefiles.TransferWeight, len(request.manual))
	for index, selected := range request.manual {
		weight, err := sourcefiles.NewTransferWeight(selected.transfer, selected.weight)
		if err != nil {
			return sourcefiles.TransferWeights{}, nil, err
		}
		weighted[index] = weight
	}
	weights, err := sourcefiles.NewTransferWeights(weighted)
	return weights, nil, err
}

func validateWeightSet(selected []selectedNetwork, weights []sourcefiles.TransferWeight) error {
	if len(selected) != len(weights) {
		return errors.New("automatic weight set is incomplete or contains extras")
	}
	expected := make(map[transfernumber.Number]struct{}, len(selected))
	for _, value := range selected {
		expected[value.transfer] = struct{}{}
	}
	seen := make(map[transfernumber.Number]struct{}, len(weights))
	for _, weighted := range weights {
		id := weighted.Transfer()
		if _, exists := expected[id]; !exists {
			return fmt.Errorf("automatic weight contains unknown transfer %d", id.Value())
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("automatic weight repeats transfer %d", id.Value())
		}
		seen[id] = struct{}{}
	}
	return nil
}
