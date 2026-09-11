package runcommand

import (
	"net/netip"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const maximumDNSResolvers = int(^uint8(0))

// ManualNetwork is one validated network choice and its normalized relative weight.
type ManualNetwork struct {
	localIP netip.Addr
	weight  uint64
}

func (network ManualNetwork) LocalIP() netip.Addr { return network.localIP }
func (network ManualNetwork) Weight() uint64      { return network.weight }

// NetworkSelections is exactly one validated manual or automatic network mode.
type NetworkSelections struct {
	manual    []ManualNetwork
	automatic []netip.Addr
	settings  throughput.Settings
}

func (selections NetworkSelections) Manual() []ManualNetwork {
	return slices.Clone(selections.manual)
}

func (selections NetworkSelections) Automatic() []netip.Addr {
	return slices.Clone(selections.automatic)
}

func (selections NetworkSelections) AutomaticWeight() bool { return len(selections.automatic) != 0 }
func (selections NetworkSelections) Settings() throughput.Settings {
	return selections.settings
}

func (selections NetworkSelections) clone() NetworkSelections {
	selections.manual = slices.Clone(selections.manual)
	selections.automatic = slices.Clone(selections.automatic)
	return selections
}

// DisplayOptions contains the display preferences parsed from the public CLI.
type DisplayOptions struct {
	noTUI bool
	mouse bool
}

func NewDisplayOptions(noTUI, mouse bool) DisplayOptions {
	return DisplayOptions{noTUI: noTUI, mouse: mouse}
}

func (options DisplayOptions) NoTUI() bool { return options.noTUI }
func (options DisplayOptions) Mouse() bool { return options.mouse }

// NetworkArgument is one parsed CLI network argument awaiting request validation.
type NetworkArgument struct {
	localIP                netip.Addr
	normalizedWeight       uint64
	explicitWeight         bool
	weightTextInvalid      bool
	addressTextContainsNUL bool
}

// NewNetworkArgument records lexical parsing and normalization without applying run invariants.
func NewNetworkArgument(localIP netip.Addr, normalizedWeight uint64,
	explicitWeight, weightTextInvalid, addressTextContainsNUL bool,
) NetworkArgument {
	return NetworkArgument{
		localIP:                localIP,
		normalizedWeight:       normalizedWeight,
		explicitWeight:         explicitWeight,
		weightTextInvalid:      weightTextInvalid,
		addressTextContainsNUL: addressTextContainsNUL,
	}
}

// Request is one immutable, fully parsed run intent.
type Request struct {
	sourcePath     string
	networks       NetworkSelections
	dns            []netip.Addr
	childArgv      []string
	logDirectory   string
	display        DisplayOptions
	followSymlinks bool
}

// NewRequest validates one complete parsed run intent in public CLI precedence order.
func NewRequest(sourcePath string, automatic bool, arguments []NetworkArgument,
	measureOverrides bool, settings throughput.Settings, dns []netip.Addr,
	childArgv []string, logDirectory *string, display DisplayOptions,
	followSymlinks bool,
) (Request, error) {
	if !cleanAbsolute(sourcePath) || sourcePath == string(filepath.Separator) {
		return Request{}, requestValidation(RequestSourcePathInvalid)
	}
	if len(arguments) < 1 || len(arguments) > transfernumber.Maximum {
		return Request{}, requestMaximumValidation(RequestNetworkCountInvalid,
			transfernumber.Maximum)
	}
	if !automatic && (measureOverrides || settings.Valid()) {
		return Request{}, requestValidation(RequestManualMeasureOverride)
	}

	seen := make(map[netip.Addr]struct{}, len(arguments))
	hasExplicitWeight := false
	for index, argument := range arguments {
		if argument.weightTextInvalid {
			return Request{}, requestNetworkValidation(RequestNetworkWeightSyntaxInvalid,
				index, argument.localIP)
		}
		if argument.addressTextContainsNUL {
			return Request{}, requestNetworkValidation(RequestNetworkAddressContainsNUL,
				index, argument.localIP)
		}
		if !argument.localIP.IsValid() {
			return Request{}, requestNetworkValidation(RequestNetworkAddressSyntaxInvalid,
				index, argument.localIP)
		}
		if !argument.localIP.Is4() {
			return Request{}, requestNetworkValidation(RequestNetworkAddressInvalid,
				index, argument.localIP)
		}
		if _, duplicate := seen[argument.localIP]; duplicate {
			return Request{}, requestNetworkValidation(RequestNetworkRepeated,
				index, argument.localIP)
		}
		seen[argument.localIP] = struct{}{}
		hasExplicitWeight = hasExplicitWeight || argument.explicitWeight
	}
	if automatic && hasExplicitWeight {
		return Request{}, requestValidation(RequestAutomaticExplicitWeightInvalid)
	}
	if automatic && len(arguments) < 2 {
		return Request{}, requestValidation(RequestAutomaticNetworkCountInvalid)
	}
	if automatic && !settings.Valid() {
		return Request{}, requestValidation(RequestMeasureSettingsInvalid)
	}

	if len(dns) > maximumDNSResolvers {
		return Request{}, requestMaximumValidation(RequestDNSCountInvalid, maximumDNSResolvers)
	}
	for index, address := range dns {
		if !address.IsValid() {
			return Request{}, requestResolverValidation(RequestDNSAddressSyntaxInvalid,
				index, address)
		}
		if address.IsUnspecified() || address.IsMulticast() || address.IsLoopback() {
			return Request{}, requestResolverValidation(RequestDNSAddressInvalid, index, address)
		}
	}
	if logDirectory != nil && !cleanAbsolute(*logDirectory) {
		return Request{}, requestValidation(RequestLogDirectoryInvalid)
	}
	if err := validateChildArgv(childArgv); err != nil {
		return Request{}, err
	}

	networks, err := newNetworkSelections(automatic, arguments, settings)
	if err != nil {
		return Request{}, err
	}
	request := Request{
		sourcePath:     strings.Clone(sourcePath),
		networks:       networks,
		dns:            slices.Clone(dns),
		childArgv:      cloneText(childArgv),
		display:        display,
		followSymlinks: followSymlinks,
	}
	if logDirectory != nil {
		request.logDirectory = strings.Clone(*logDirectory)
	}
	return request, nil
}

func newNetworkSelections(automatic bool, arguments []NetworkArgument,
	settings throughput.Settings,
) (NetworkSelections, error) {
	if automatic {
		addresses := make([]netip.Addr, len(arguments))
		for index, argument := range arguments {
			addresses[index] = argument.localIP
		}
		return NetworkSelections{automatic: addresses, settings: settings}, nil
	}
	manual := make([]ManualNetwork, len(arguments))
	for index, argument := range arguments {
		if argument.normalizedWeight == 0 {
			return NetworkSelections{}, requestNetworkValidation(RequestManualNetworkWeightInvalid,
				index, argument.localIP)
		}
		manual[index] = ManualNetwork{localIP: argument.localIP,
			weight: argument.normalizedWeight}
	}
	return NetworkSelections{manual: manual}, nil
}

func validateChildArgv(argv []string) error {
	if len(argv) == 0 || len(argv) > int(^uint16(0)) {
		return requestValidation(RequestChildArgumentCountInvalid)
	}
	placeholders := 0
	for _, argument := range argv {
		if strings.IndexByte(argument, 0) >= 0 {
			return requestValidation(RequestChildArgumentContainsNUL)
		}
		if argument == "{}" {
			placeholders++
		}
	}
	if placeholders != 1 {
		return requestValidation(RequestChildPlaceholderCountInvalid)
	}
	return nil
}

func cleanAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path &&
		strings.IndexByte(path, 0) < 0
}

func cloneText(values []string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = strings.Clone(value)
	}
	return result
}

func (request Request) SourcePath() string { return strings.Clone(request.sourcePath) }
func (request Request) Networks() NetworkSelections {
	return request.networks.clone()
}
func (request Request) DNS() []netip.Addr       { return slices.Clone(request.dns) }
func (request Request) ChildArgv() []string     { return cloneText(request.childArgv) }
func (request Request) LogDirectory() string    { return strings.Clone(request.logDirectory) }
func (request Request) Display() DisplayOptions { return request.display }
func (request Request) FollowSymlinks() bool    { return request.followSymlinks }
