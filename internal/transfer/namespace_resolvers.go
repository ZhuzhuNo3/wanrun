package transfer

import (
	"context"

	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/namespaceresolvers"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type resolverGroup interface {
	Access(transfernumber.Number) (namespaceresolvers.ResolverAccess, error)
	DiagnosticError() error
	Close(context.Context) (bool, error)
}

type namespaceResolvers interface {
	open(context.Context, *hostnetwork.Session, []selectedNetwork, namespaceresolvers.Intent) (resolverGroup, error)
}

type networkNamespaceResolvers struct{}

func (networkNamespaceResolvers) open(ctx context.Context, network *hostnetwork.Session,
	selected []selectedNetwork, intent namespaceresolvers.Intent,
) (resolverGroup, error) {
	transfers := make([]transfernumber.Number, len(selected))
	for index, selection := range selected {
		transfers[index] = selection.transfer
	}
	return namespaceresolvers.Open(ctx, network, transfers, intent)
}
