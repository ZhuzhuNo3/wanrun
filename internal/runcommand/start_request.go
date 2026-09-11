package runcommand

import (
	"fmt"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transferstart"
)

// NewStartRequest converts immutable application intent into the private worker-start value.
func NewStartRequest(request Request, commandIO transferstart.CommandIO) (transferstart.Request, error) {
	networks := request.Networks()
	if networks.AutomaticWeight() {
		return transferstart.NewAutomaticRequest(request.SourcePath(), networks.Automatic(),
			networks.Settings(), request.DNS(), request.ChildArgv(), commandIO, request.FollowSymlinks())
	}
	manual := networks.Manual()
	wireNetworks := make([]transferstart.ManualNetwork, len(manual))
	for index, network := range manual {
		value, err := transferstart.NewManualNetwork(network.LocalIP(), network.Weight())
		if err != nil {
			return transferstart.Request{}, fmt.Errorf("construct transfer start network: %w", err)
		}
		wireNetworks[index] = value
	}
	return transferstart.NewManualRequest(request.SourcePath(), wireNetworks, request.DNS(),
		request.ChildArgv(), commandIO, request.FollowSymlinks())
}

// CheckStartRequestCapacity validates every possible canonical I/O form without touching runtime resources.
func CheckStartRequestCapacity(request Request) error {
	maximumPTY, err := transferstart.NewPTY(int(^uint16(0)), int(^uint16(0)))
	if err != nil {
		return err
	}
	for _, commandIO := range []transferstart.CommandIO{transferstart.Pipes(), maximumPTY} {
		start, err := NewStartRequest(request, commandIO)
		if err != nil {
			return fmt.Errorf("construct transfer start request: %w", err)
		}
		size, err := transferstart.EncodedSize(start)
		if err != nil {
			return fmt.Errorf("size transfer start request: %w", err)
		}
		if err := runsupervisor.ValidateStartRequestSize(size); err != nil {
			return err
		}
	}
	return nil
}
