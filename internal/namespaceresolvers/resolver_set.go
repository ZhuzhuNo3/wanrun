package namespaceresolvers

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func newAccessSet(file *os.File, transfers []transfernumber.Number,
	forwarder *dnsForwarder,
) (*NamespaceResolverSet, error) {
	access := newResolverAccess(file)
	seen := make(map[transfernumber.Number]struct{}, len(transfers))
	accesses := make(map[transfernumber.Number]ResolverAccess, len(transfers))
	for _, id := range transfers {
		if id.Value() == 0 {
			closeInvalidAccessSet(file, forwarder)
			return nil, errors.New("resolver transfer identity is invalid")
		}
		if _, duplicate := seen[id]; duplicate {
			closeInvalidAccessSet(file, forwarder)
			return nil, fmt.Errorf("resolver repeats transfer %d", id.Value())
		}
		seen[id] = struct{}{}
		accesses[id] = access
	}
	return &NamespaceResolverSet{file: file, accesses: accesses, forwarder: forwarder}, nil
}

func closeInvalidAccessSet(file *os.File, forwarder *dnsForwarder) {
	_ = file.Close()
	if forwarder != nil {
		_, _ = forwarder.close(context.Background())
	}
}
