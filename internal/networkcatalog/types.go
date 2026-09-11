package networkcatalog

import (
	"context"
	"net"
	"net/netip"
)

type Catalog struct{}

type Snapshot struct {
	links     []linkObservation
	addresses []addressObservation
	rules     []policyRule
	routes    []fibRoute
}

type Interface struct {
	index            int
	name             string
	kind             string
	mtu              int
	flags            net.Flags
	masterIndex      int
	operationalState string
}

type LocalAddress struct {
	ip              netip.Addr
	prefixBits      int
	interfaceValue  Interface
	exclusionReason string
}

type Egress struct {
	localIP        netip.Addr
	interfaceValue Interface
	table          int
	gateway        netip.Addr
	hasGateway     bool
	runnable       bool
	reason         string
	diagnostics    []string
}

type linkObservation struct {
	index            int
	name             string
	kind             string
	mtu              int
	flags            net.Flags
	masterIndex      int
	operationalState string
}

type addressObservation struct {
	linkIndex int
	prefix    netip.Prefix
}

type ruleAction uint8

const (
	ruleLookup ruleAction = iota
	ruleBlackhole
	ruleUnreachable
	ruleProhibit
	ruleContinue
)

type inputInterfaceState uint8

const (
	inputInterfaceUnresolved inputInterfaceState = iota + 1
	inputInterfaceAttached
	inputInterfaceDetached
)

type inputInterfaceSelector struct {
	name       string
	state      inputInterfaceState
	index      int
	isL3Master bool
}

type outputInterfaceState uint8

const (
	outputInterfaceAttached outputInterfaceState = iota + 1
	outputInterfaceDetached
)

type outputInterfaceSelector struct {
	name  string
	state outputInterfaceState
}

type unresolvedRuleSelectorKind uint8

const (
	ruleSelectorTypeOfService unresolvedRuleSelectorKind = iota + 1
	ruleSelectorFirewallMark
	ruleSelectorUIDRange
	ruleSelectorTransport
	ruleSelectorL3Master
	ruleSelectorTunnelID
	ruleSelectorFlowRealm
	ruleSelectorUnknownAttribute
)

type unresolvedRuleSelector struct {
	kind      unresolvedRuleSelectorKind
	attribute uint16
	value     string
}

type unsupportedRuleEffectKind uint8

const (
	ruleEffectRouteSuppression unsupportedRuleEffectKind = iota + 1
	ruleEffectGoto
	ruleEffectHeaderFlags
)

type unsupportedRuleEffect struct {
	kind      unsupportedRuleEffectKind
	attribute uint16
	value     string
	flags     uint32
}

type policyRule struct {
	priority            int
	table               int
	source              netip.Prefix
	destination         netip.Prefix
	inputInterface      inputInterfaceSelector
	outputInterface     outputInterfaceSelector
	unresolvedSelectors []unresolvedRuleSelector
	unsupportedEffects  []unsupportedRuleEffect
	inverted            bool
	action              ruleAction
}

type routeKind uint8

const (
	routeUnicast routeKind = iota
	routeThrow
	routeUnreachable
	routeBlackhole
	routeProhibit
	routeUnsupported
)

type fibRoute struct {
	table       int
	destination netip.Prefix
	kind        routeKind
	linkIndex   int
	gateway     netip.Addr
	hasGateway  bool
	metric      int
	unsupported string
}

func New() Catalog { return Catalog{} }

func (Catalog) Capture(ctx context.Context) (Snapshot, error) {
	return capture(ctx)
}

func (value Interface) Index() int               { return value.index }
func (value Interface) Name() string             { return value.name }
func (value Interface) Kind() string             { return value.kind }
func (value Interface) MTU() int                 { return value.mtu }
func (value Interface) Up() bool                 { return value.flags&net.FlagUp != 0 }
func (value Interface) Loopback() bool           { return value.flags&net.FlagLoopback != 0 }
func (value Interface) MasterIndex() int         { return value.masterIndex }
func (value Interface) OperationalState() string { return value.operationalState }

func (value LocalAddress) IP() netip.Addr          { return value.ip }
func (value LocalAddress) PrefixBits() int         { return value.prefixBits }
func (value LocalAddress) Interface() Interface    { return value.interfaceValue }
func (value LocalAddress) IncludedByDefault() bool { return value.exclusionReason == "" }
func (value LocalAddress) ExclusionReason() string { return value.exclusionReason }

func (value Egress) LocalIP() netip.Addr         { return value.localIP }
func (value Egress) Interface() Interface        { return value.interfaceValue }
func (value Egress) Table() int                  { return value.table }
func (value Egress) Runnable() bool              { return value.runnable }
func (value Egress) Reason() string              { return value.reason }
func (value Egress) Diagnostics() []string       { return append([]string(nil), value.diagnostics...) }
func (value Egress) Gateway() (netip.Addr, bool) { return value.gateway, value.hasGateway }
