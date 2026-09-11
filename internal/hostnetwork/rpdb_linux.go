//go:build linux

package hostnetwork

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func listRoutingRules() ([]routingRule, error) {
	request := nl.NewNetlinkRequest(unix.RTM_GETRULE, unix.NLM_F_DUMP|unix.NLM_F_REQUEST)
	request.AddData(nl.NewIfInfomsg(netlink.FAMILY_V4))
	messages, err := request.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWRULE)
	if err != nil {
		return nil, fmt.Errorf("dump host IPv4 rules: %w", err)
	}
	result := make([]routingRule, 0, len(messages))
	for _, message := range messages {
		rule, err := routingRuleFromMessage(message)
		if err != nil {
			return nil, err
		}
		result = append(result, rule)
	}
	return result, nil
}

func routingRuleFromMessage(message []byte) (routingRule, error) {
	routeMessage := nl.DeserializeRtMsg(message)
	if routeMessage == nil || routeMessage.Family != unix.AF_INET {
		return routingRule{}, errors.New("IPv4 rule dump returned an invalid family")
	}
	attributes, err := nl.ParseRouteAttr(message[routeMessage.Len():])
	if err != nil {
		return routingRule{}, fmt.Errorf("parse host IPv4 rule: %w", err)
	}
	rule := routingRule{priority: 0, table: int(routeMessage.Table), action: ruleAction(routeMessage.Type)}
	applyRoutingRuleHeader(&rule, routeMessage.Tos, routeMessage.Flags)
	seen := make(map[uint16]struct{})
	for _, attribute := range attributes {
		attributeType := attribute.Attr.Type & nl.NLA_TYPE_MASK
		if attributeType != nl.FRA_PAD {
			if _, duplicate := seen[attributeType]; duplicate {
				rule.unknownSelector = true
				rule.unknownTypes = append(rule.unknownTypes, attributeType)
				continue
			}
			seen[attributeType] = struct{}{}
		}
		if err := applyRoutingRuleAttribute(&rule, attribute, routeMessage.Src_len, routeMessage.Dst_len); err != nil {
			return routingRule{}, err
		}
	}
	rule.builtinLocal = rule.priority == 0 && rule.table == unix.RT_TABLE_LOCAL &&
		rule.action == ruleActionLookup && rule.source == nil && rule.destination == nil && rule.inputInterface == "" &&
		!rule.inverted && !rule.unknownSelector
	return rule, nil
}

func applyRoutingRuleHeader(rule *routingRule, tos uint8, flags uint32) {
	const understoodFlags uint32 = unix.FIB_RULE_PERMANENT | unix.FIB_RULE_INVERT

	rule.inverted = flags&unix.FIB_RULE_INVERT != 0
	if tos != 0 || flags & ^understoodFlags != 0 {
		rule.unknownSelector = true
	}
}

func applyRoutingRuleAttribute(rule *routingRule, attribute syscall.NetlinkRouteAttr,
	sourceBits, destinationBits uint8,
) error {
	attributeType := attribute.Attr.Type & nl.NLA_TYPE_MASK
	switch attributeType {
	case nl.FRA_PRIORITY:
		value, valid := nativeUint32(attribute.Value)
		if !valid {
			return errors.New("host IPv4 rule has invalid priority")
		}
		rule.priority = int(value)
	case nl.FRA_TABLE:
		value, valid := nativeUint32(attribute.Value)
		if !valid {
			return errors.New("host IPv4 rule has invalid table")
		}
		rule.table = int(value)
	case nl.FRA_SRC:
		address, valid := netip.AddrFromSlice(attribute.Value)
		if !valid || !address.Unmap().Is4() || sourceBits > 32 {
			return errors.New("host IPv4 rule has invalid source selector")
		}
		prefix := netip.PrefixFrom(address.Unmap(), int(sourceBits)).Masked()
		rule.source = &prefix
	case nl.FRA_DST:
		address, valid := netip.AddrFromSlice(attribute.Value)
		if !valid || !address.Unmap().Is4() || destinationBits > 32 {
			return errors.New("host IPv4 rule has invalid destination selector")
		}
		prefix := netip.PrefixFrom(address.Unmap(), int(destinationBits)).Masked()
		rule.destination = &prefix
	case nl.FRA_IIFNAME:
		name, valid := nulTerminatedName(attribute.Value)
		if !valid {
			return errors.New("host IPv4 rule has invalid input interface selector")
		}
		rule.inputInterface = name
	case nl.FRA_PROTOCOL:
		if len(attribute.Value) != 1 {
			return errors.New("host IPv4 rule has invalid protocol")
		}
		rule.protocol = attribute.Value[0]
	case nl.FRA_PAD:
		// Alignment does not select traffic.
	case nl.FRA_SUPPRESS_IFGROUP, nl.FRA_SUPPRESS_PREFIXLEN:
		value, valid := nativeUint32(attribute.Value)
		if !valid {
			return errors.New("host IPv4 rule has invalid suppress selector")
		}
		if value != ^uint32(0) {
			rule.unknownSelector = true
			rule.unknownTypes = append(rule.unknownTypes, attributeType)
		}
	default:
		rule.unknownSelector = true
		rule.unknownTypes = append(rule.unknownTypes, attributeType)
	}
	return nil
}

func nativeUint32(value []byte) (uint32, bool) {
	if len(value) != 4 {
		return 0, false
	}
	return binary.NativeEndian.Uint32(value), true
}

func nulTerminatedName(value []byte) (string, bool) {
	if len(value) < 2 || value[len(value)-1] != 0 {
		return "", false
	}
	name := string(value[:len(value)-1])
	return name, name != "" && !strings.ContainsRune(name, 0)
}
