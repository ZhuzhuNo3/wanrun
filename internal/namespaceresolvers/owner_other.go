//go:build !linux

package namespaceresolvers

import (
	"context"
	"errors"

	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func Open(context.Context, *hostnetwork.Session, []transfernumber.Number, Intent) (*NamespaceResolverSet, error) {
	return nil, errors.New("namespace resolvers require Linux")
}
