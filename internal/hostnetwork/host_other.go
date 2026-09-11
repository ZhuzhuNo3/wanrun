//go:build !linux

package hostnetwork

import (
	"context"
	"errors"
)

type unsupportedHost struct{}

func defaultKernelHost() kernelHost { return unsupportedHost{} }

func (unsupportedHost) Inspect(context.Context, []egressSelection) (hostInventory, error) {
	return hostInventory{}, errors.New("host-network mutation requires Linux")
}

func (unsupportedHost) ObservePublishedFirewalls(context.Context,
	[]networkClaim,
) (publishedFirewallObservations, error) {
	return nil, errors.New("host-network mutation requires Linux")
}

func (unsupportedHost) Install(context.Context, networkClaim) (*hostInstallation, error) {
	return nil, errors.New("host-network mutation requires Linux")
}

func (unsupportedHost) Verify(context.Context, networkClaim, *hostInstallation) error {
	return errors.New("host-network mutation requires Linux")
}

func (unsupportedHost) ConfirmConntrackEmpty(context.Context, networkClaim) error {
	return errors.New("host-network mutation requires Linux")
}

func (unsupportedHost) Rollback(context.Context, networkClaim, *hostInstallation, bool) error {
	return errors.New("host-network mutation requires Linux")
}

func (unsupportedHost) Close(context.Context, networkClaim, *hostInstallation) error {
	return errors.New("host-network mutation requires Linux")
}

func (unsupportedHost) Cleanup(context.Context, networkClaim, bool) error {
	return errors.New("host-network mutation requires Linux")
}
