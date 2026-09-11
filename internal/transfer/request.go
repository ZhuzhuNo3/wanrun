package transfer

import (
	"fmt"
	"net/netip"
	"path/filepath"
	"strings"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/namespaceresolvers"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const viewPlaceholder = "{}"

// CommandIO selects the child descriptor model without selecting a presentation implementation.
type CommandIO uint8

const (
	InteractiveCommandIO CommandIO = iota + 1
	PlainCommandIO
)

func (mode CommandIO) valid() bool {
	return mode == InteractiveCommandIO || mode == PlainCommandIO
}

// ManualNetworkSelection is one immutable, run-local source choice and positive byte weight.
type ManualNetworkSelection struct {
	transfer transfernumber.Number
	localIP  netip.Addr
	weight   uint64
}

// AutomaticNetworkSelection is one immutable, run-local source choice measured after network setup.
type AutomaticNetworkSelection struct {
	transfer transfernumber.Number
	localIP  netip.Addr
}

type selectedNetwork struct {
	transfer transfernumber.Number
	localIP  netip.Addr
}

// Request is the complete immutable intent for one directory transfer.
type Request struct {
	sourceName     string
	manual         []ManualNetworkSelection
	automatic      []AutomaticNetworkSelection
	argv           []string
	environment    []string
	resolver       namespaceresolvers.Intent
	terminal       childprocesses.TerminalSize
	commandIO      CommandIO
	settings       throughput.Settings
	followSymlinks bool
}

// NewManualNetworkSelection validates one explicit source and allocation weight.
func NewManualNetworkSelection(number transfernumber.Number, localIP netip.Addr, weight uint64) (ManualNetworkSelection, error) {
	if number.Value() == 0 || !localIP.Is4() || weight == 0 {
		return ManualNetworkSelection{}, fmt.Errorf("manual network selection is invalid")
	}
	return ManualNetworkSelection{transfer: number, localIP: localIP, weight: weight}, nil
}

// NewAutomaticNetworkSelection validates one source whose weight will be measured inside its namespace.
func NewAutomaticNetworkSelection(number transfernumber.Number, localIP netip.Addr) (AutomaticNetworkSelection, error) {
	if number.Value() == 0 || !localIP.Is4() {
		return AutomaticNetworkSelection{}, fmt.Errorf("automatic network selection is invalid")
	}
	return AutomaticNetworkSelection{transfer: number, localIP: localIP}, nil
}

// NewManualRequest freezes a complete manually weighted transfer intent.
func NewManualRequest(source string, selected []ManualNetworkSelection, argv, environment []string,
	resolver namespaceresolvers.Intent, terminal childprocesses.TerminalSize, followSymlinks bool,
) (Request, error) {
	networks := manualSelectedNetworks(selected)
	request, err := newRequest(source, networks, argv, environment, resolver, terminal,
		InteractiveCommandIO, followSymlinks)
	if err != nil {
		return Request{}, err
	}
	if err := validateManualSelections(selected); err != nil {
		return Request{}, err
	}
	request.manual = append([]ManualNetworkSelection(nil), selected...)
	return request, nil
}

// NewPlainManualRequest freezes a manually weighted request with separated stdout/stderr pipes.
func NewPlainManualRequest(source string, selected []ManualNetworkSelection, argv, environment []string,
	resolver namespaceresolvers.Intent, followSymlinks bool,
) (Request, error) {
	networks := manualSelectedNetworks(selected)
	request, err := newRequest(source, networks, argv, environment, resolver,
		childprocesses.TerminalSize{}, PlainCommandIO, followSymlinks)
	if err != nil {
		return Request{}, err
	}
	if err := validateManualSelections(selected); err != nil {
		return Request{}, err
	}
	request.manual = append([]ManualNetworkSelection(nil), selected...)
	return request, nil
}

// NewAutomaticRequest freezes a complete transfer intent with one comparable measure window.
func NewAutomaticRequest(source string, selected []AutomaticNetworkSelection, argv, environment []string,
	resolver namespaceresolvers.Intent, terminal childprocesses.TerminalSize, settings throughput.Settings,
	followSymlinks bool,
) (Request, error) {
	networks := automaticSelectedNetworks(selected)
	if len(networks) < 2 || !settings.Valid() {
		return Request{}, fmt.Errorf("automatic transfer requires at least two networks and a valid window")
	}
	request, err := newRequest(source, networks, argv, environment, resolver, terminal,
		InteractiveCommandIO, followSymlinks)
	if err != nil {
		return Request{}, err
	}
	request.automatic = append([]AutomaticNetworkSelection(nil), selected...)
	request.settings = settings
	return request, nil
}

// NewPlainAutomaticRequest freezes an automatically weighted request with separated pipes.
func NewPlainAutomaticRequest(source string, selected []AutomaticNetworkSelection, argv, environment []string,
	resolver namespaceresolvers.Intent, settings throughput.Settings, followSymlinks bool,
) (Request, error) {
	networks := automaticSelectedNetworks(selected)
	if len(networks) < 2 || !settings.Valid() {
		return Request{}, fmt.Errorf("automatic transfer requires at least two networks and a valid window")
	}
	request, err := newRequest(source, networks, argv, environment, resolver,
		childprocesses.TerminalSize{}, PlainCommandIO, followSymlinks)
	if err != nil {
		return Request{}, err
	}
	request.automatic = append([]AutomaticNetworkSelection(nil), selected...)
	request.settings = settings
	return request, nil
}

func newRequest(source string, networks []selectedNetwork, argv, environment []string,
	resolver namespaceresolvers.Intent, terminal childprocesses.TerminalSize,
	commandIO CommandIO, followSymlinks bool,
) (Request, error) {
	cleanedSource := filepath.Clean(source)
	sourceName := filepath.Base(cleanedSource)
	if source == "" || strings.IndexByte(source, 0) >= 0 || sourceName == "." ||
		sourceName == ".." || sourceName == string(filepath.Separator) {
		return Request{}, fmt.Errorf("transfer source is invalid")
	}
	if err := validateSelectedNetworks(networks); err != nil {
		return Request{}, err
	}
	if err := validateCommandIntent(argv, environment, terminal, commandIO); err != nil {
		return Request{}, err
	}
	return Request{sourceName: strings.Clone(sourceName), argv: cloneText(argv),
		environment: cloneText(environment), resolver: resolver,
		terminal: terminal, commandIO: commandIO,
		followSymlinks: followSymlinks}, nil
}

func manualSelectedNetworks(values []ManualNetworkSelection) []selectedNetwork {
	result := make([]selectedNetwork, len(values))
	for index, value := range values {
		result[index] = selectedNetwork{transfer: value.transfer, localIP: value.localIP}
	}
	return result
}

func automaticSelectedNetworks(values []AutomaticNetworkSelection) []selectedNetwork {
	result := make([]selectedNetwork, len(values))
	for index, value := range values {
		result[index] = selectedNetwork{transfer: value.transfer, localIP: value.localIP}
	}
	return result
}

func (request Request) selectedNetworks() []selectedNetwork {
	if len(request.manual) != 0 {
		return manualSelectedNetworks(request.manual)
	}
	return automaticSelectedNetworks(request.automatic)
}

func validateSelectedNetworks(selections []selectedNetwork) error {
	if len(selections) == 0 || len(selections) > transfernumber.Maximum {
		return fmt.Errorf("run requires 1..%d transfers", transfernumber.Maximum)
	}
	seenIDs := make(map[transfernumber.Number]struct{}, len(selections))
	seenIPs := make(map[netip.Addr]struct{}, len(selections))
	for index, selected := range selections {
		if selected.transfer.Value() == 0 || !selected.localIP.Is4() {
			return fmt.Errorf("network selection %d is invalid", index+1)
		}
		if _, duplicate := seenIDs[selected.transfer]; duplicate {
			return fmt.Errorf("run repeats transfer %d", selected.transfer.Value())
		}
		if _, duplicate := seenIPs[selected.localIP]; duplicate {
			return fmt.Errorf("transfer repeats source %s", selected.localIP)
		}
		seenIDs[selected.transfer], seenIPs[selected.localIP] = struct{}{}, struct{}{}
	}
	for expected := 1; expected <= len(selections); expected++ {
		number, _ := transfernumber.New(expected)
		if _, exists := seenIDs[number]; !exists {
			return fmt.Errorf("run transfer set is missing %d", expected)
		}
	}
	return nil
}

func validateManualSelections(selections []ManualNetworkSelection) error {
	for index, selected := range selections {
		if selected.weight == 0 {
			return fmt.Errorf("manual network selection %d is invalid", index+1)
		}
	}
	return nil
}

func validateCommandIntent(argv, environment []string,
	terminal childprocesses.TerminalSize, commandIO CommandIO,
) error {
	if len(argv) == 0 || !commandIO.valid() {
		return fmt.Errorf("transfer command or resolver is invalid")
	}
	placeholderCount := 0
	for _, argument := range argv {
		if strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("transfer command contains NUL")
		}
		if argument == viewPlaceholder {
			placeholderCount++
		}
	}
	if placeholderCount != 1 {
		return fmt.Errorf("transfer command must contain exactly one standalone %q argument", viewPlaceholder)
	}
	seenEnvironment := make(map[string]struct{}, len(environment))
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if !found || name == "" || strings.IndexByte(entry, 0) >= 0 {
			return fmt.Errorf("transfer command environment is invalid")
		}
		if _, duplicate := seenEnvironment[name]; duplicate {
			return fmt.Errorf("transfer command environment repeats %q", name)
		}
		seenEnvironment[name] = struct{}{}
	}
	cols, rows := terminal.Values()
	if commandIO == InteractiveCommandIO && (cols == 0 || rows == 0) ||
		commandIO == PlainCommandIO && (cols != 0 || rows != 0) {
		return fmt.Errorf("transfer terminal size is invalid")
	}
	return nil
}

// CommandIO returns the immutable child descriptor model for this request.
func (request Request) CommandIO() CommandIO { return request.commandIO }

// FollowSymlinks reports whether source-tree symlink targets are explicitly authorized.
func (request Request) FollowSymlinks() bool { return request.followSymlinks }

func cleanAbsolutePath(path string) bool {
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
