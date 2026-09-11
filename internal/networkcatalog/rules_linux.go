//go:build linux

package networkcatalog

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"syscall"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func observePolicyRules(ctx context.Context) ([]policyRule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	request := nl.NewNetlinkRequest(unix.RTM_GETRULE, unix.NLM_F_DUMP|unix.NLM_F_REQUEST)
	request.AddData(nl.NewIfInfomsg(unix.AF_INET))
	messages, err := request.Execute(unix.NETLINK_ROUTE, unix.RTM_NEWRULE)
	if err != nil {
		return nil, fmt.Errorf("list Linux IPv4 policy rules: %w", err)
	}
	result := make([]policyRule, 0, len(messages))
	for _, message := range messages {
		rule, err := decodePolicyRule(message)
		if err != nil {
			return nil, err
		}
		result = append(result, rule)
	}
	return result, ctx.Err()
}

func decodePolicyRule(message []byte) (policyRule, error) {
	if len(message) < unix.SizeofRtMsg {
		return policyRule{}, fmt.Errorf("policy rule message is truncated")
	}
	header := nl.DeserializeRtMsg(message)
	if header.Family != unix.AF_INET || header.Src_len > 32 || header.Dst_len > 32 {
		return policyRule{}, fmt.Errorf("policy rule has invalid IPv4 header")
	}
	defaultPrefix := netip.MustParsePrefix("0.0.0.0/0")
	rule := policyRule{table: int(header.Table), source: defaultPrefix, destination: defaultPrefix,
		inverted: header.Flags&unix.FIB_RULE_INVERT != 0, action: linuxRuleAction(header.Type)}
	decodeRuleHeader(&rule, header)
	attributes, err := nl.ParseRouteAttr(message[header.Len():])
	if err != nil {
		return policyRule{}, fmt.Errorf("parse policy rule attributes: %w", err)
	}
	if err := decodeRuleAttributes(&rule, header, attributes); err != nil {
		return policyRule{}, err
	}
	return rule, nil
}

func decodeRuleHeader(rule *policyRule, header *nl.RtMsg) {
	if header.Tos != 0 {
		appendRuleSelector(rule, ruleSelectorTypeOfService, 0, []byte{header.Tos})
	}
	const understood = unix.FIB_RULE_PERMANENT | unix.FIB_RULE_INVERT |
		unix.FIB_RULE_IIF_DETACHED | unix.FIB_RULE_OIF_DETACHED
	if flags := header.Flags &^ understood; flags != 0 {
		rule.unsupportedEffects = append(rule.unsupportedEffects,
			unsupportedRuleEffect{kind: ruleEffectHeaderFlags, flags: flags})
	}
}

func decodeRuleAttributes(rule *policyRule, header *nl.RtMsg,
	attributes []syscall.NetlinkRouteAttr,
) error {
	seen := make(map[uint16]struct{}, len(attributes))
	for _, attribute := range attributes {
		kind := attribute.Attr.Type & nl.NLA_TYPE_MASK
		if _, duplicate := seen[kind]; duplicate && kind != nl.FRA_PAD {
			return fmt.Errorf("policy rule repeats attribute %d", kind)
		}
		seen[kind] = struct{}{}
		if err := decodeRuleAttribute(rule, header, kind, attribute.Value); err != nil {
			return err
		}
	}
	return validateRuleAttributes(header, seen)
}

func validateRuleAttributes(header *nl.RtMsg, seen map[uint16]struct{}) error {
	if header.Src_len > 0 && !hasRuleAttribute(seen, nl.FRA_SRC) {
		return fmt.Errorf("policy rule source prefix is missing")
	}
	if header.Dst_len > 0 && !hasRuleAttribute(seen, nl.FRA_DST) {
		return fmt.Errorf("policy rule destination prefix is missing")
	}
	if header.Flags&unix.FIB_RULE_IIF_DETACHED != 0 && !hasRuleAttribute(seen, nl.FRA_IIFNAME) {
		return fmt.Errorf("policy rule marks a missing input interface as detached")
	}
	if header.Flags&unix.FIB_RULE_OIF_DETACHED != 0 && !hasRuleAttribute(seen, nl.FRA_OIFNAME) {
		return fmt.Errorf("policy rule marks a missing output interface as detached")
	}
	if header.Table == unix.RT_TABLE_COMPAT && !hasRuleAttribute(seen, nl.FRA_TABLE) {
		return fmt.Errorf("policy rule compatibility table marker has no table attribute")
	}
	return nil
}

func hasRuleAttribute(seen map[uint16]struct{}, kind uint16) bool {
	_, present := seen[kind]
	return present
}

func decodeRuleAttribute(rule *policyRule, header *nl.RtMsg, kind uint16, value []byte) error {
	switch kind {
	case nl.FRA_SRC:
		return decodeRuleSource(rule, header.Src_len, value)
	case nl.FRA_DST:
		return decodeRuleDestination(rule, header.Dst_len, value)
	case nl.FRA_TABLE:
		return decodeRuleTable(rule, header, value)
	case nl.FRA_PRIORITY:
		return assignRuleUint32(value, func(decoded uint32) { rule.priority = int(decoded) })
	case nl.FRA_PROTOCOL:
		return requireRuleBytes(value, 1)
	case nl.FRA_IIFNAME:
		return decodeRuleInputInterface(rule, header.Flags, value)
	case nl.FRA_L3MDEV:
		return decodeRuleL3Master(rule, kind, value)
	case nl.FRA_SUPPRESS_IFGROUP, nl.FRA_SUPPRESS_PREFIXLEN:
		return decodeRuleSuppression(rule, kind, value)
	case nl.FRA_GOTO:
		return decodeRuleGoto(rule, kind, value)
	case nl.FRA_FWMARK, nl.FRA_FWMASK:
		return decodeRuleFixedSelector(rule, ruleSelectorFirewallMark, kind, value, 4)
	case nl.FRA_OIFNAME:
		return decodeRuleOutputInterface(rule, header.Flags, value)
	case nl.FRA_UID_RANGE:
		return decodeRuleFixedSelector(rule, ruleSelectorUIDRange, kind, value, 8)
	case nl.FRA_IP_PROTO:
		return decodeRuleFixedSelector(rule, ruleSelectorTransport, kind, value, 1)
	case nl.FRA_SPORT_RANGE, nl.FRA_DPORT_RANGE:
		return decodeRuleFixedSelector(rule, ruleSelectorTransport, kind, value, 4)
	case nl.FRA_TUN_ID:
		return decodeRuleFixedSelector(rule, ruleSelectorTunnelID, kind, value, 8)
	case nl.FRA_FLOW:
		return decodeRuleFixedSelector(rule, ruleSelectorFlowRealm, kind, value, 4)
	case nl.FRA_PAD:
		if hasNonzeroByte(value) {
			return fmt.Errorf("policy rule padding is not zero")
		}
		return nil
	case nl.FRA_UNSPEC, nl.FRA_UNUSED2, nl.FRA_UNUSED3, nl.FRA_UNUSED4, nl.FRA_UNUSED5:
		if hasNonzeroByte(value) {
			appendRuleSelector(rule, ruleSelectorUnknownAttribute, kind, value)
		}
		return nil
	default:
		appendRuleSelector(rule, ruleSelectorUnknownAttribute, kind, value)
		return nil
	}
}

func decodeRuleSource(rule *policyRule, bits uint8, value []byte) error {
	prefix, err := rulePrefix(bits, value)
	if err != nil {
		return fmt.Errorf("decode policy rule source: %w", err)
	}
	rule.source = prefix
	return nil
}

func decodeRuleDestination(rule *policyRule, bits uint8, value []byte) error {
	prefix, err := rulePrefix(bits, value)
	if err != nil {
		return fmt.Errorf("decode policy rule destination: %w", err)
	}
	rule.destination = prefix
	return nil
}

func decodeRuleTable(rule *policyRule, header *nl.RtMsg, value []byte) error {
	table, err := ruleUint32(value)
	if err != nil {
		return err
	}
	if header.Table != unix.RT_TABLE_UNSPEC && header.Table != unix.RT_TABLE_COMPAT &&
		uint32(header.Table) != table {
		return fmt.Errorf("policy rule table attribute %d conflicts with header table %d", table, header.Table)
	}
	rule.table = int(table)
	return nil
}

func decodeRuleInputInterface(rule *policyRule, flags uint32, value []byte) error {
	name, err := ruleInterfaceName(value)
	if err != nil {
		return fmt.Errorf("decode policy rule input interface: %w", err)
	}
	state, index := inputInterfaceUnresolved, 0
	if flags&unix.FIB_RULE_IIF_DETACHED != 0 {
		state, index = inputInterfaceDetached, -1
	}
	rule.inputInterface = inputInterfaceSelector{name: name, state: state, index: index}
	return nil
}

func decodeRuleOutputInterface(rule *policyRule, flags uint32, value []byte) error {
	name, err := ruleInterfaceName(value)
	if err != nil {
		return fmt.Errorf("decode policy rule output interface: %w", err)
	}
	state := outputInterfaceAttached
	if flags&unix.FIB_RULE_OIF_DETACHED != 0 {
		state = outputInterfaceDetached
	}
	rule.outputInterface = outputInterfaceSelector{name: name, state: state}
	return nil
}

func decodeRuleL3Master(rule *policyRule, kind uint16, value []byte) error {
	if err := requireRuleBytes(value, 1); err != nil {
		return err
	}
	if value[0] != 0 {
		appendRuleSelector(rule, ruleSelectorL3Master, kind, value)
	}
	return nil
}

func decodeRuleSuppression(rule *policyRule, kind uint16, value []byte) error {
	decoded, err := ruleUint32(value)
	if err != nil {
		return err
	}
	if decoded != ^uint32(0) {
		rule.unsupportedEffects = append(rule.unsupportedEffects,
			unsupportedRuleEffect{kind: ruleEffectRouteSuppression, attribute: kind, value: string(value)})
	}
	return nil
}

func decodeRuleGoto(rule *policyRule, kind uint16, value []byte) error {
	if err := requireRuleBytes(value, 4); err != nil {
		return err
	}
	rule.unsupportedEffects = append(rule.unsupportedEffects,
		unsupportedRuleEffect{kind: ruleEffectGoto, attribute: kind, value: string(value)})
	return nil
}

func decodeRuleFixedSelector(rule *policyRule, selector unresolvedRuleSelectorKind,
	kind uint16, value []byte, size int,
) error {
	if err := requireRuleBytes(value, size); err != nil {
		return err
	}
	appendRuleSelector(rule, selector, kind, value)
	return nil
}

func appendRuleSelector(rule *policyRule, selector unresolvedRuleSelectorKind,
	attribute uint16, value []byte,
) {
	rule.unresolvedSelectors = append(rule.unresolvedSelectors,
		unresolvedRuleSelector{kind: selector, attribute: attribute, value: string(value)})
}

func ruleInterfaceName(value []byte) (string, error) {
	if len(value) < 2 || len(value) > unix.IFNAMSIZ || value[len(value)-1] != 0 {
		return "", fmt.Errorf("invalid NUL-terminated interface name")
	}
	name := string(value[:len(value)-1])
	if name == "" || strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("invalid NUL-terminated interface name")
	}
	return name, nil
}

func rulePrefix(bits uint8, value []byte) (netip.Prefix, error) {
	if len(value) != 4 || bits > 32 {
		return netip.Prefix{}, fmt.Errorf("invalid IPv4 prefix")
	}
	var raw [4]byte
	copy(raw[:], value)
	prefix := netip.PrefixFrom(netip.AddrFrom4(raw), int(bits)).Masked()
	return prefix, nil
}

func assignRuleUint32(value []byte, assign func(uint32)) error {
	decoded, err := ruleUint32(value)
	if err != nil {
		return err
	}
	assign(decoded)
	return nil
}

func ruleUint32(value []byte) (uint32, error) {
	if err := requireRuleBytes(value, 4); err != nil {
		return 0, err
	}
	return nl.NativeEndian().Uint32(value), nil
}

func requireRuleBytes(value []byte, count int) error {
	if len(value) != count {
		return fmt.Errorf("policy rule attribute length is %d, want %d", len(value), count)
	}
	return nil
}

func hasNonzeroByte(value []byte) bool {
	for _, current := range value {
		if current != 0 {
			return true
		}
	}
	return false
}

func linuxRuleAction(value uint8) ruleAction {
	switch value {
	case unix.FR_ACT_TO_TBL:
		return ruleLookup
	case unix.FR_ACT_BLACKHOLE:
		return ruleBlackhole
	case unix.FR_ACT_UNREACHABLE:
		return ruleUnreachable
	case unix.FR_ACT_PROHIBIT:
		return ruleProhibit
	case unix.FR_ACT_NOP:
		return ruleContinue
	default:
		return ruleAction(255)
	}
}
