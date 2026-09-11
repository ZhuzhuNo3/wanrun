package runcommand

import (
	"fmt"
	"net/netip"
)

// RequestValidationReason identifies which ordered validation rejected a run intent.
type RequestValidationReason uint8

const (
	RequestSourcePathInvalid RequestValidationReason = iota + 1
	RequestNetworkCountInvalid
	RequestManualMeasureOverride
	RequestNetworkWeightSyntaxInvalid
	RequestNetworkAddressContainsNUL
	RequestNetworkAddressSyntaxInvalid
	RequestAutomaticNetworkCountInvalid
	RequestAutomaticExplicitWeightInvalid
	RequestNetworkAddressInvalid
	RequestNetworkRepeated
	RequestManualNetworkWeightInvalid
	RequestMeasureSettingsInvalid
	RequestDNSCountInvalid
	RequestDNSAddressSyntaxInvalid
	RequestDNSAddressInvalid
	RequestChildArgumentCountInvalid
	RequestChildArgumentContainsNUL
	RequestChildPlaceholderCountInvalid
	RequestLogDirectoryInvalid
)

// RequestValidationFailure carries presentation-neutral evidence about a rejected run intent.
type RequestValidationFailure struct {
	reason        RequestValidationReason
	networkIndex  int
	resolverIndex int
	address       netip.Addr
	maximum       int
}

func (failure *RequestValidationFailure) Error() string {
	if failure == nil {
		return "run request validation failed"
	}
	switch failure.reason {
	case RequestSourcePathInvalid:
		return "run request source path is invalid"
	case RequestNetworkCountInvalid:
		return fmt.Sprintf("run request network count is outside 1..%d", failure.maximum)
	case RequestManualMeasureOverride:
		return "manual run does not accept measure overrides"
	case RequestNetworkWeightSyntaxInvalid:
		return fmt.Sprintf("run request network %d weight syntax is invalid",
			failure.networkIndex+1)
	case RequestNetworkAddressContainsNUL:
		return fmt.Sprintf("run request network %d address contains NUL",
			failure.networkIndex+1)
	case RequestNetworkAddressSyntaxInvalid:
		return fmt.Sprintf("run request network %d address syntax is invalid",
			failure.networkIndex+1)
	case RequestAutomaticNetworkCountInvalid:
		return "automatic run requires at least two networks"
	case RequestAutomaticExplicitWeightInvalid:
		return "automatic run does not accept explicit network weights"
	case RequestNetworkAddressInvalid:
		if failure.networkIndex < 0 {
			return fmt.Sprintf("manual run network address %q is not IPv4", failure.address)
		}
		return fmt.Sprintf("run request network %d address %q is not IPv4",
			failure.networkIndex+1, failure.address)
	case RequestNetworkRepeated:
		return fmt.Sprintf("run request repeats network %s", failure.address)
	case RequestManualNetworkWeightInvalid:
		if failure.networkIndex < 0 {
			return "manual run network weight is invalid"
		}
		return fmt.Sprintf("run request network %d weight is invalid", failure.networkIndex+1)
	case RequestMeasureSettingsInvalid:
		return "automatic run measure settings are invalid"
	case RequestDNSCountInvalid:
		return fmt.Sprintf("run request accepts at most %d DNS resolvers", failure.maximum)
	case RequestDNSAddressSyntaxInvalid:
		return fmt.Sprintf("run request DNS resolver %d syntax is invalid",
			failure.resolverIndex+1)
	case RequestDNSAddressInvalid:
		return fmt.Sprintf("run request DNS resolver %q is invalid", failure.address)
	case RequestChildArgumentCountInvalid:
		return "run request child argument count is invalid"
	case RequestChildArgumentContainsNUL:
		return "run request child argument contains NUL"
	case RequestChildPlaceholderCountInvalid:
		return "run request child placeholder count is invalid"
	case RequestLogDirectoryInvalid:
		return "run request log directory is invalid"
	default:
		return "run request validation failed"
	}
}

func (failure *RequestValidationFailure) Reason() RequestValidationReason {
	if failure == nil {
		return 0
	}
	return failure.reason
}

func (failure *RequestValidationFailure) NetworkIndex() (int, bool) {
	if failure == nil || failure.networkIndex < 0 {
		return 0, false
	}
	return failure.networkIndex, true
}

func (failure *RequestValidationFailure) Address() netip.Addr {
	if failure == nil {
		return netip.Addr{}
	}
	return failure.address
}

func (failure *RequestValidationFailure) ResolverIndex() (int, bool) {
	if failure == nil || failure.resolverIndex < 0 {
		return 0, false
	}
	return failure.resolverIndex, true
}

func (failure *RequestValidationFailure) Maximum() int {
	if failure == nil {
		return 0
	}
	return failure.maximum
}

func requestValidation(reason RequestValidationReason) *RequestValidationFailure {
	return &RequestValidationFailure{reason: reason, networkIndex: -1, resolverIndex: -1}
}

func requestNetworkValidation(reason RequestValidationReason, index int,
	address netip.Addr,
) *RequestValidationFailure {
	return &RequestValidationFailure{reason: reason, networkIndex: index,
		resolverIndex: -1, address: address}
}

func requestMaximumValidation(reason RequestValidationReason, maximum int) *RequestValidationFailure {
	return &RequestValidationFailure{reason: reason, networkIndex: -1,
		resolverIndex: -1, maximum: maximum}
}

func requestResolverValidation(reason RequestValidationReason, index int,
	address netip.Addr,
) *RequestValidationFailure {
	return &RequestValidationFailure{reason: reason, networkIndex: -1,
		resolverIndex: index, address: address}
}
