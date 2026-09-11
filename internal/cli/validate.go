package cli

import (
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"strings"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/listnetworks"
	"github.com/ZhuzhuNo3/transferlanes/internal/probe"
	"github.com/ZhuzhuNo3/transferlanes/internal/runcommand"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
)

func buildList(options listOptions) (Result, error) {
	if options.probeURLSet && !options.probe {
		return nil, usageErrorf("--probe-url requires --probe")
	}
	if (options.measureURLSet || options.durationSet) && !options.measure {
		return nil, usageErrorf("measure overrides require --measure")
	}
	var probeEndpoint *probe.Endpoint
	if options.probe {
		endpoint := probe.DefaultEndpoint()
		if options.probeURLSet {
			parsed, err := probe.NewEndpoint(options.probeURL)
			if err != nil {
				return nil, usageError("parse --probe-url", err)
			}
			endpoint = parsed
		}
		probeEndpoint = &endpoint
	}
	var measureWindow *throughput.Settings
	if options.measure {
		duration, err := parseDuration(options.duration, options.durationSet)
		if err != nil {
			return nil, err
		}
		endpoint := throughput.DefaultEndpointURL
		if options.measureURLSet {
			endpoint = options.measureURL
		}
		window, err := throughput.NewSettings(endpoint, duration)
		if err != nil {
			return nil, usageError("construct --measure request", err)
		}
		measureWindow = &window
	}
	request, err := listnetworks.NewRequest(options.networks, options.all, probeEndpoint, measureWindow)
	if err != nil {
		return nil, usageError("construct list request", err)
	}
	return List{request: request}, nil
}

func buildRun(options runOptions) (Result, error) {
	if !options.sourceSet {
		return nil, usageErrorf("--source is required")
	}
	selections := parseNetworkSelections(options.networks)
	settings := throughput.Settings{}
	var settingsError error
	if options.auto {
		duration, durationErr := parseDuration(options.duration, options.durationSet)
		if durationErr != nil {
			settingsError = durationErr
		} else {
			endpoint := throughput.DefaultEndpointURL
			if options.measureURLSet {
				endpoint = options.measureURL
			}
			var err error
			settings, err = throughput.NewSettings(endpoint, duration)
			if err != nil {
				settingsError = usageError("construct automatic measure request", err)
			}
		}
	}
	resolvers := parseResolvers(options.dns)
	var logDirectory *string
	if options.logDirSet {
		logDirectory = &options.logDir
	}
	request, err := runcommand.NewRequest(options.source, options.auto,
		networkArguments(selections), options.measureURLSet || options.durationSet,
		settings, resolvers, options.argv, logDirectory,
		runcommand.NewDisplayOptions(options.noTUI, options.mouse), options.followSymlinks)
	if err != nil {
		return nil, runRequestUsageError(err, selections, options.dns, settingsError)
	}
	return Run{request: request}, nil
}

type parsedNetworkSelection struct {
	addressText            string
	localIP                netip.Addr
	weight                 *decimalWeight
	normalizedWeight       uint64
	explicitWeight         bool
	weightTextInvalid      bool
	addressTextContainsNUL bool
}

type decimalWeight struct {
	coefficient *big.Int
	scale       int
}

func parseNetworkSelections(values []string) []parsedNetworkSelection {
	result := make([]parsedNetworkSelection, len(values))
	for index, raw := range values {
		addressText, weightText, explicit := strings.Cut(raw, "@")
		var weight *decimalWeight
		weightTextInvalid := false
		if explicit {
			parsed, valid := parseWeight(weightText)
			if valid {
				weight = &parsed
			} else {
				weightTextInvalid = true
			}
		}
		address, _ := netip.ParseAddr(addressText)
		result[index] = parsedNetworkSelection{
			addressText:            addressText,
			localIP:                address,
			weight:                 weight,
			explicitWeight:         explicit,
			weightTextInvalid:      weightTextInvalid,
			addressTextContainsNUL: strings.IndexByte(addressText, 0) >= 0,
		}
	}
	weights := normalizeNetworkWeights(result)
	for index := range result {
		result[index].normalizedWeight = weights[index]
	}
	return result
}

func invalidNetworkWeight() error {
	return usageErrorf("--network has an invalid weight")
}

func parseWeight(raw string) (decimalWeight, bool) {
	whole, fraction, decimal := strings.Cut(raw, ".")
	if !asciiDigits(whole) || decimal && !asciiDigits(fraction) {
		return decimalWeight{}, false
	}
	coefficient, ok := new(big.Int).SetString(whole+fraction, 10)
	if !ok || coefficient.Sign() <= 0 {
		return decimalWeight{}, false
	}
	return decimalWeight{coefficient: coefficient, scale: len(fraction)}, true
}

func asciiDigits(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

func normalizeNetworkWeights(parsed []parsedNetworkSelection) []uint64 {
	result := make([]uint64, len(parsed))
	for _, selection := range parsed {
		if selection.weightTextInvalid {
			return result
		}
	}
	maxScale := 0
	for _, selection := range parsed {
		if selection.weight != nil && selection.weight.scale > maxScale {
			maxScale = selection.weight.scale
		}
	}
	values := make([]*big.Int, len(parsed))
	divisor := new(big.Int)
	for index, selection := range parsed {
		coefficient, scale := big.NewInt(1), 0
		if selection.weight != nil {
			coefficient, scale = selection.weight.coefficient, selection.weight.scale
		}
		factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(maxScale-scale)), nil)
		values[index] = new(big.Int).Mul(coefficient, factor)
		divisor.GCD(nil, nil, divisor, values[index])
	}
	for index, value := range values {
		weight := new(big.Int).Quo(value, divisor)
		if weight.IsUint64() && weight.Sign() > 0 {
			result[index] = weight.Uint64()
		}
	}
	return result
}

func networkArguments(parsed []parsedNetworkSelection) []runcommand.NetworkArgument {
	result := make([]runcommand.NetworkArgument, len(parsed))
	for index, selection := range parsed {
		result[index] = runcommand.NewNetworkArgument(selection.localIP,
			selection.normalizedWeight, selection.explicitWeight,
			selection.weightTextInvalid, selection.addressTextContainsNUL)
	}
	return result
}

func runRequestUsageError(err error, selections []parsedNetworkSelection,
	resolverTexts []string, settingsError error,
) error {
	var usage *UsageError
	if errors.As(err, &usage) {
		return err
	}
	var failure *runcommand.RequestValidationFailure
	if !errors.As(err, &failure) {
		return usageError("construct run request", err)
	}
	var message string
	switch failure.Reason() {
	case runcommand.RequestSourcePathInvalid:
		message = "--source must be a clean absolute directory other than /"
	case runcommand.RequestNetworkCountInvalid:
		message = fmt.Sprintf("run requires 1..%d --network values", failure.Maximum())
	case runcommand.RequestManualMeasureOverride:
		message = "measure overrides require --auto-weight"
	case runcommand.RequestNetworkWeightSyntaxInvalid,
		runcommand.RequestManualNetworkWeightInvalid:
		return invalidNetworkWeight()
	case runcommand.RequestNetworkAddressContainsNUL:
		return usageErrorf("--network contains NUL")
	case runcommand.RequestNetworkAddressSyntaxInvalid:
		return usageErrorf("--network %q is not IPv4",
			networkAddressText(failure, selections))
	case runcommand.RequestAutomaticNetworkCountInvalid:
		message = "--auto-weight requires at least two networks"
	case runcommand.RequestAutomaticExplicitWeightInvalid:
		message = "--auto-weight cannot be combined with an explicit @weight"
	case runcommand.RequestNetworkAddressInvalid:
		message = fmt.Sprintf("--network %q is not IPv4", networkAddressText(failure, selections))
	case runcommand.RequestNetworkRepeated:
		message = fmt.Sprintf("--network repeats %s", failure.Address())
	case runcommand.RequestMeasureSettingsInvalid:
		if settingsError != nil {
			return settingsError
		}
		message = "automatic measure settings are invalid"
	case runcommand.RequestDNSCountInvalid:
		message = fmt.Sprintf("run accepts at most %d --dns values", failure.Maximum())
	case runcommand.RequestDNSAddressSyntaxInvalid:
		return usageErrorf("--dns %q is not a usable nameserver IP",
			resolverAddressText(failure, resolverTexts))
	case runcommand.RequestDNSAddressInvalid:
		message = fmt.Sprintf("--dns %q is not a usable nameserver IP",
			resolverAddressText(failure, resolverTexts))
	case runcommand.RequestLogDirectoryInvalid:
		message = "--log-dir must be a clean absolute path"
	case runcommand.RequestChildArgumentCountInvalid:
		message = "run child command exceeds its private argument-count representation"
	case runcommand.RequestChildArgumentContainsNUL:
		message = "child argv contains NUL"
	case runcommand.RequestChildPlaceholderCountInvalid:
		message = "child argv must contain exactly one standalone {} argument"
	default:
		return usageError("construct run request", err)
	}
	return usageErrorWithCause(message, err)
}

func resolverAddressText(failure *runcommand.RequestValidationFailure, values []string) string {
	if index, exists := failure.ResolverIndex(); exists && index < len(values) {
		return values[index]
	}
	return failure.Address().String()
}

func networkAddressText(failure *runcommand.RequestValidationFailure,
	selections []parsedNetworkSelection,
) string {
	if index, exists := failure.NetworkIndex(); exists && index < len(selections) {
		return selections[index].addressText
	}
	for _, selection := range selections {
		if selection.localIP == failure.Address() {
			return selection.addressText
		}
	}
	return failure.Address().String()
}

func parseResolvers(values []string) []netip.Addr {
	result := make([]netip.Addr, len(values))
	for index, raw := range values {
		result[index], _ = netip.ParseAddr(raw)
	}
	return result
}

func parseDuration(raw string, set bool) (time.Duration, error) {
	if !set {
		return defaultMeasureDuration, nil
	}
	duration, err := time.ParseDuration(raw)
	if err != nil {
		return 0, usageError("parse --measure-duration", err)
	}
	return duration, nil
}
