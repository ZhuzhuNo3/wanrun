package transferstart

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const maximumDNSResolvers = int(^uint8(0))

type commandIOMode uint8

const (
	pipesMode commandIOMode = iota + 1
	ptyMode
)

// CommandIO is either separated pipes or one PTY with a valid initial viewport.
type CommandIO struct {
	mode     commandIOMode
	terminal childprocesses.TerminalSize
}

func Pipes() CommandIO { return CommandIO{mode: pipesMode} }

func NewPTY(columns, rows int) (CommandIO, error) {
	size, err := childprocesses.NewTerminalSize(columns, rows)
	if err != nil {
		return CommandIO{}, err
	}
	return CommandIO{mode: ptyMode, terminal: size}, nil
}

func (value CommandIO) IsPipes() bool { return value.mode == pipesMode }
func (value CommandIO) PTYSize() (childprocesses.TerminalSize, bool) {
	return value.terminal, value.mode == ptyMode
}

func (value CommandIO) valid() bool {
	columns, rows := value.terminal.Values()
	return value.mode == pipesMode && columns == 0 && rows == 0 ||
		value.mode == ptyMode && columns != 0 && rows != 0
}

// ManualNetwork is one private-wire network record with a positive normalized weight.
type ManualNetwork struct {
	localIP netip.Addr
	weight  uint64
}

func NewManualNetwork(localIP netip.Addr, weight uint64) (ManualNetwork, error) {
	if !localIP.Is4() || weight == 0 {
		return ManualNetwork{}, errors.New("transfer start manual network is invalid")
	}
	return ManualNetwork{localIP: localIP, weight: weight}, nil
}

func (network ManualNetwork) LocalIP() netip.Addr { return network.localIP }
func (network ManualNetwork) Weight() uint64      { return network.weight }

// Request is the immutable parent-to-worker transfer start value.
type Request struct {
	sourceLabel    string
	manual         []ManualNetwork
	automatic      []netip.Addr
	settings       throughput.Settings
	dns            []netip.Addr
	childArgv      []string
	commandIO      CommandIO
	followSymlinks bool
}

func NewManualRequest(sourceLabel string, networks []ManualNetwork, dns []netip.Addr,
	childArgv []string, commandIO CommandIO, followSymlinks bool,
) (Request, error) {
	request := Request{sourceLabel: strings.Clone(sourceLabel), manual: slices.Clone(networks),
		dns: slices.Clone(dns), childArgv: cloneText(childArgv), commandIO: commandIO,
		followSymlinks: followSymlinks}
	if err := request.validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}

func NewAutomaticRequest(sourceLabel string, networks []netip.Addr, settings throughput.Settings,
	dns []netip.Addr, childArgv []string, commandIO CommandIO, followSymlinks bool,
) (Request, error) {
	request := Request{sourceLabel: strings.Clone(sourceLabel), automatic: slices.Clone(networks),
		settings: settings, dns: slices.Clone(dns), childArgv: cloneText(childArgv),
		commandIO: commandIO, followSymlinks: followSymlinks}
	if err := request.validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}

func (request Request) validate() error {
	if !validText(request.sourceLabel, true) || !filepath.IsAbs(request.sourceLabel) ||
		filepath.Clean(request.sourceLabel) != request.sourceLabel || request.sourceLabel == "/" ||
		!request.commandIO.valid() || (len(request.manual) == 0) == (len(request.automatic) == 0) ||
		len(request.dns) > maximumDNSResolvers || len(request.childArgv) == 0 ||
		len(request.childArgv) > int(^uint16(0)) {
		return errors.New("transfer start request shape is invalid")
	}
	seen := make(map[netip.Addr]struct{}, len(request.manual)+len(request.automatic))
	if len(request.manual) != 0 {
		if len(request.manual) > transfernumber.Maximum || request.settings.Valid() {
			return errors.New("transfer start manual mode is invalid")
		}
		for _, network := range request.manual {
			if !network.localIP.Is4() || network.weight == 0 {
				return errors.New("transfer start manual network is invalid")
			}
			if _, duplicate := seen[network.localIP]; duplicate {
				return errors.New("transfer start repeats a source address")
			}
			seen[network.localIP] = struct{}{}
		}
	} else {
		if len(request.automatic) < 2 || len(request.automatic) > transfernumber.Maximum ||
			!request.settings.Valid() {
			return errors.New("transfer start automatic mode is invalid")
		}
		for _, localIP := range request.automatic {
			if !localIP.Is4() {
				return errors.New("transfer start automatic network is invalid")
			}
			if _, duplicate := seen[localIP]; duplicate {
				return errors.New("transfer start repeats a source address")
			}
			seen[localIP] = struct{}{}
		}
	}
	for _, resolver := range request.dns {
		if !resolver.IsValid() || resolver.IsUnspecified() || resolver.IsLoopback() || resolver.IsMulticast() {
			return errors.New("transfer start DNS is invalid")
		}
	}
	placeholders := 0
	for _, argument := range request.childArgv {
		if !validText(argument, false) {
			return errors.New("transfer start argv is invalid")
		}
		if argument == "{}" {
			placeholders++
		}
	}
	if placeholders != 1 {
		return errors.New("transfer start argv needs one standalone placeholder")
	}
	return nil
}

func (request Request) SourceLabel() string { return strings.Clone(request.sourceLabel) }
func (request Request) ManualNetworks() []ManualNetwork {
	return slices.Clone(request.manual)
}
func (request Request) AutomaticNetworks() []netip.Addr { return slices.Clone(request.automatic) }
func (request Request) AutomaticWeight() bool           { return len(request.automatic) != 0 }
func (request Request) ThroughputSettings() throughput.Settings {
	return request.settings
}
func (request Request) DNS() []netip.Addr    { return slices.Clone(request.dns) }
func (request Request) ChildArgv() []string  { return cloneText(request.childArgv) }
func (request Request) CommandIO() CommandIO { return request.commandIO }
func (request Request) FollowSymlinks() bool { return request.followSymlinks }
func (request Request) networkCount() int    { return len(request.manual) + len(request.automatic) }

func validText(value string, required bool) bool {
	return (!required || value != "") && len(value) <= int(^uint16(0)) && strings.IndexByte(value, 0) < 0
}

func cloneText(values []string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = strings.Clone(value)
	}
	return result
}

func invalidField(name string, err error) error {
	if err == nil {
		return fmt.Errorf("transfer start %s is invalid", name)
	}
	return fmt.Errorf("transfer start %s is invalid: %w", name, err)
}
