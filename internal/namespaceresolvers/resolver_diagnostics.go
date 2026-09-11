package namespaceresolvers

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

const retainedResolverFailures = 16

type resolverDiagnostics struct {
	mu         sync.Mutex
	failures   []error
	suppressed uint64
}

type dnsMessageLimitError struct {
	transport string
	direction string
}

func (failure *dnsMessageLimitError) Error() string {
	return fmt.Sprintf("%s DNS %s exceeds %d bytes", failure.transport, failure.direction,
		maximumDNSMessage)
}

func (forwarder *dnsForwarder) diagnosticError() error {
	if forwarder == nil || forwarder.diagnostics == nil {
		return nil
	}
	return forwarder.diagnostics.err()
}

func (forwarder *dnsForwarder) recordLimitFailure(err error) {
	if isDNSMessageLimit(err) {
		forwarder.diagnostics.record(err)
	}
}

func (diagnostics *resolverDiagnostics) record(err error) {
	if diagnostics == nil || err == nil {
		return
	}
	diagnostics.mu.Lock()
	if len(diagnostics.failures) < retainedResolverFailures {
		diagnostics.failures = append(diagnostics.failures, err)
	} else {
		diagnostics.suppressed++
	}
	diagnostics.mu.Unlock()
}

func (diagnostics *resolverDiagnostics) err() error {
	if diagnostics == nil {
		return nil
	}
	diagnostics.mu.Lock()
	failures := slices.Clone(diagnostics.failures)
	suppressed := diagnostics.suppressed
	diagnostics.mu.Unlock()
	if suppressed != 0 {
		failures = append(failures,
			fmt.Errorf("%d additional DNS resolver failures were suppressed", suppressed))
	}
	return errors.Join(failures...)
}

func isDNSMessageLimit(err error) bool {
	var limit *dnsMessageLimitError
	return errors.As(err, &limit)
}
