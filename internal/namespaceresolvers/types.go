package namespaceresolvers

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const loopbackResolver = "127.0.0.53"

// Intent is the immutable DNS choice for one run. An empty explicit set means
// that namespace DNS must transparently use the host resolver environment.
type Intent struct {
	explicit []netip.Addr
}

func DefaultIntent() Intent { return Intent{} }

func DirectIntent(addresses []netip.Addr) (Intent, error) {
	if len(addresses) == 0 {
		return Intent{}, errors.New("direct resolver intent requires at least one address")
	}
	seen := make(map[netip.Addr]struct{}, len(addresses))
	for _, address := range addresses {
		if !address.IsValid() || address.IsUnspecified() || address.IsMulticast() || address.IsLoopback() {
			return Intent{}, fmt.Errorf("direct resolver %s is invalid", address)
		}
		if _, duplicate := seen[address]; duplicate {
			return Intent{}, fmt.Errorf("direct resolver %s is repeated", address)
		}
		seen[address] = struct{}{}
	}
	return Intent{explicit: slices.Clone(addresses)}, nil
}

func (intent Intent) Direct() []netip.Addr   { return slices.Clone(intent.explicit) }
func (intent Intent) UsesHostResolver() bool { return len(intent.explicit) == 0 }

// ResolverAccess is a non-owning child resolver capability. Its owner must
// remain alive until every consumer process has stopped.
type ResolverAccess interface {
	Open() (*os.File, error)
}

type resolverAccess struct {
	file *os.File
}

func newResolverAccess(file *os.File) ResolverAccess { return resolverAccess{file: file} }

// Open duplicates the immutable resolver file for one child-process handoff.
func (access resolverAccess) Open() (*os.File, error) {
	if access.file == nil {
		return nil, errors.New("resolver access is unavailable")
	}
	return duplicateResolverFile(access.file)
}

// NamespaceResolverSet owns every live resolver listener and the immutable
// resolver file used by one run.
type NamespaceResolverSet struct {
	mu        sync.Mutex
	file      *os.File
	accesses  map[transfernumber.Number]ResolverAccess
	forwarder *dnsForwarder
	closed    bool
}

// DiagnosticError returns bounded resolver failures for the run owner to
// deliver through its normal result boundary.
func (set *NamespaceResolverSet) DiagnosticError() error {
	if set == nil || set.forwarder == nil {
		return nil
	}
	return set.forwarder.diagnosticError()
}

func (set *NamespaceResolverSet) Access(id transfernumber.Number) (ResolverAccess, error) {
	if set == nil {
		return nil, errors.New("namespace resolver set is closed")
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return nil, errors.New("namespace resolver set is closed")
	}
	access, ok := set.accesses[id]
	if !ok {
		return nil, fmt.Errorf("transfer %d has no resolver access", id.Value())
	}
	return access, nil
}

func (set *NamespaceResolverSet) Close(ctx context.Context) (bool, error) {
	if set == nil {
		return true, nil
	}
	if ctx == nil {
		return false, errors.New("namespace resolver cleanup context is unavailable")
	}
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return true, nil
	}
	workersStopped := true
	var forwardErr error
	if set.forwarder != nil {
		workersStopped, forwardErr = set.forwarder.close(ctx)
	}
	if !workersStopped {
		return false, forwardErr
	}
	fileErr := set.file.Close()
	set.closed = true
	set.accesses = nil
	return true, errors.Join(forwardErr, fileErr)
}

func resolverText(nameservers []netip.Addr, suffix []string) []byte {
	var result strings.Builder
	for _, address := range nameservers {
		_, _ = fmt.Fprintf(&result, "nameserver %s\n", address)
	}
	for _, line := range suffix {
		result.WriteString(line)
		result.WriteByte('\n')
	}
	return []byte(result.String())
}
