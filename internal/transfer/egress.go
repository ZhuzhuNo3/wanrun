package transfer

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/ZhuzhuNo3/transferlanes/internal/networkcatalog"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

// egressResolver is the replaceable read-only host observation boundary.
type egressResolver interface {
	resolve(context.Context, []selectedNetwork) (map[transfernumber.Number]networkcatalog.Egress, error)
}

type catalogEgressResolver struct{ catalog networkcatalog.Catalog }

func (reader catalogEgressResolver) resolve(ctx context.Context,
	selected []selectedNetwork,
) (map[transfernumber.Number]networkcatalog.Egress, error) {
	sources := make([]netip.Addr, len(selected))
	for index, value := range selected {
		sources[index] = value.localIP
	}
	snapshot, err := reader.catalog.Capture(ctx)
	if err != nil {
		return nil, err
	}
	resolved := snapshot.Resolve(sources)
	if len(resolved) != len(selected) {
		return nil, errors.New("network catalog returned an incomplete selected egress set")
	}
	result := make(map[transfernumber.Number]networkcatalog.Egress, len(resolved))
	for index, egress := range resolved {
		selection := selected[index]
		if egress.LocalIP() != selection.localIP {
			return nil, fmt.Errorf("transfer %d egress source identity changed", selection.transfer.Value())
		}
		if !egress.Runnable() {
			return nil, fmt.Errorf("transfer %d egress is not runnable: %s",
				selection.transfer.Value(), egress.Reason())
		}
		result[selection.transfer] = egress
	}
	return result, nil
}

func validateEgressSet(selected []selectedNetwork,
	egresses map[transfernumber.Number]networkcatalog.Egress,
) error {
	if len(egresses) != len(selected) {
		return errors.New("selected egress set is incomplete or contains extras")
	}
	for _, value := range selected {
		if _, exists := egresses[value.transfer]; !exists {
			return fmt.Errorf("transfer %d selected egress is absent", value.transfer.Value())
		}
	}
	return nil
}
