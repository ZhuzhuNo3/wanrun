//go:build linux

package networkcatalog

import (
	"strings"
	"testing"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func TestPolicyRuleDecoderPreservesDestinationAndInputInterface(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		flags uint32
		state inputInterfaceState
		index int
	}{
		{name: "attached", state: inputInterfaceUnresolved},
		{name: "detached", flags: unix.FIB_RULE_IIF_DETACHED,
			state: inputInterfaceDetached, index: -1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			rule, err := decodePolicyRule(policyRuleMessage(0, 30, testCase.flags,
				nl.NewRtAttr(nl.FRA_DST, []byte{198, 18, 0, 0}),
				nl.NewRtAttr(nl.FRA_IIFNAME, []byte("provider0\x00")),
			))
			if err != nil {
				t.Fatal(err)
			}
			if rule.destination != mustPrefix("198.18.0.0/30") ||
				rule.inputInterface.name != "provider0" ||
				rule.inputInterface.state != testCase.state ||
				rule.inputInterface.index != testCase.index ||
				len(rule.unresolvedSelectors) != 0 || len(rule.unsupportedEffects) != 0 {
				t.Fatalf("decoded rule = %#v", rule)
			}
		})
	}
}

func TestPolicyRuleDecoderSeparatesUnresolvedSelectorsAndEffects(t *testing.T) {
	flags := uint32(unix.FIB_RULE_UNRESOLVED)
	rule, err := decodePolicyRule(policyRuleMessage(0, 0, flags,
		nl.NewRtAttr(nl.FRA_FWMARK, nativeRuleUint32(7)),
		nl.NewRtAttr(nl.FRA_L3MDEV, []byte{1}),
		nl.NewRtAttr(nl.FRA_SUPPRESS_PREFIXLEN, nativeRuleUint32(24)),
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(rule.unresolvedSelectors) != 2 ||
		rule.unresolvedSelectors[0].kind != ruleSelectorFirewallMark ||
		rule.unresolvedSelectors[1].kind != ruleSelectorL3Master {
		t.Fatalf("unresolved selectors = %#v", rule.unresolvedSelectors)
	}
	if len(rule.unsupportedEffects) != 2 ||
		rule.unsupportedEffects[0].kind != ruleEffectHeaderFlags ||
		rule.unsupportedEffects[0].flags != flags ||
		rule.unsupportedEffects[1].kind != ruleEffectRouteSuppression {
		t.Fatalf("unsupported effects = %#v", rule.unsupportedEffects)
	}
}

func TestPolicyRuleDecoderPreservesOutputInterfaceAttachment(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		flags uint32
		state outputInterfaceState
	}{
		{name: "attached", state: outputInterfaceAttached},
		{name: "detached", flags: unix.FIB_RULE_OIF_DETACHED, state: outputInterfaceDetached},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			rule, err := decodePolicyRule(policyRuleMessage(0, 0, testCase.flags,
				nl.NewRtAttr(nl.FRA_OIFNAME, []byte("target0\x00"))))
			if err != nil {
				t.Fatal(err)
			}
			if rule.outputInterface.name != "target0" ||
				rule.outputInterface.state != testCase.state ||
				len(rule.unresolvedSelectors) != 0 || len(rule.unsupportedEffects) != 0 {
				t.Fatalf("decoded output interface = %#v", rule)
			}
		})
	}
}

func TestPolicyRuleDecoderRejectsInvalidInputInterfaceIdentity(t *testing.T) {
	longName := strings.Repeat("x", unix.IFNAMSIZ)
	for _, testCase := range []struct {
		name    string
		flags   uint32
		attrs   []*nl.RtAttr
		message []byte
	}{
		{name: "detached without name", flags: unix.FIB_RULE_IIF_DETACHED},
		{name: "detached output without name", flags: unix.FIB_RULE_OIF_DETACHED},
		{name: "empty name", attrs: []*nl.RtAttr{nl.NewRtAttr(nl.FRA_IIFNAME, []byte{0})}},
		{name: "missing terminator", attrs: []*nl.RtAttr{nl.NewRtAttr(nl.FRA_IIFNAME, []byte("wan0"))}},
		{name: "embedded terminator", attrs: []*nl.RtAttr{
			nl.NewRtAttr(nl.FRA_IIFNAME, []byte("wan\x000\x00"))}},
		{name: "name too long", attrs: []*nl.RtAttr{
			nl.NewRtAttr(nl.FRA_IIFNAME, []byte(longName+"\x00"))}},
		{name: "duplicate name", attrs: []*nl.RtAttr{
			nl.NewRtAttr(nl.FRA_IIFNAME, []byte("wan0\x00")),
			nl.NewRtAttr(nl.FRA_IIFNAME, []byte("wan1\x00"))}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			message := testCase.message
			if message == nil {
				message = policyRuleMessage(0, 0, testCase.flags, testCase.attrs...)
			}
			if _, err := decodePolicyRule(message); err == nil {
				t.Fatal("invalid policy rule input interface was accepted")
			}
		})
	}
}

func TestPolicyRuleDecoderRequiresDeclaredPrefixes(t *testing.T) {
	if _, err := decodePolicyRule(policyRuleMessage(24, 0, 0)); err == nil {
		t.Fatal("policy rule with missing source prefix was accepted")
	}
	if _, err := decodePolicyRule(policyRuleMessage(0, 24, 0)); err == nil {
		t.Fatal("policy rule with missing destination prefix was accepted")
	}
}

func TestPolicyRuleDecoderResolvesExtendedTableEncoding(t *testing.T) {
	rule, err := decodePolicyRule(policyRuleMessageWithTable(unix.RT_TABLE_COMPAT, 0, 0, 0,
		nl.NewRtAttr(nl.FRA_TABLE, nativeRuleUint32(30_000))))
	if err != nil {
		t.Fatal(err)
	}
	if rule.table != 30_000 {
		t.Fatalf("decoded table = %d, want 30000", rule.table)
	}
	if _, err := decodePolicyRule(policyRuleMessageWithTable(unix.RT_TABLE_COMPAT, 0, 0, 0)); err == nil {
		t.Fatal("compatibility table marker without table attribute was accepted")
	}
	if _, err := decodePolicyRule(policyRuleMessageWithTable(100, 0, 0, 0,
		nl.NewRtAttr(nl.FRA_TABLE, nativeRuleUint32(200)))); err == nil {
		t.Fatal("conflicting table header and attribute were accepted")
	}
}

func policyRuleMessage(sourceBits, destinationBits uint8, flags uint32,
	attributes ...*nl.RtAttr,
) []byte {
	return policyRuleMessageWithTable(100, sourceBits, destinationBits, flags, attributes...)
}

func policyRuleMessageWithTable(table uint8, sourceBits, destinationBits uint8, flags uint32,
	attributes ...*nl.RtAttr,
) []byte {
	header := nl.NewRtMsg()
	header.Family = unix.AF_INET
	header.Src_len = sourceBits
	header.Dst_len = destinationBits
	header.Table = table
	header.Type = uint8(nl.FR_ACT_TO_TBL)
	header.Flags = flags
	result := append([]byte(nil), header.Serialize()...)
	for _, attribute := range attributes {
		result = append(result, attribute.Serialize()...)
	}
	return result
}

func nativeRuleUint32(value uint32) []byte {
	result := make([]byte, 4)
	nl.NativeEndian().PutUint32(result, value)
	return result
}
