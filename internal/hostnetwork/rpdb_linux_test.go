//go:build linux

package hostnetwork

import (
	"net/netip"
	"testing"

	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
)

func TestRoutingRuleDecoderModelsDestinationSelector(t *testing.T) {
	message := nl.NewRtMsg()
	message.Family = unix.AF_INET
	message.Dst_len = 30
	message.Table = 100
	message.Type = uint8(nl.FR_ACT_TO_TBL)
	encoded := append([]byte(nil), message.Serialize()...)
	encoded = append(encoded, nl.NewRtAttr(nl.FRA_DST, []byte{198, 18, 0, 0}).Serialize()...)
	rule, err := routingRuleFromMessage(encoded)
	if err != nil {
		t.Fatal(err)
	}
	want := netip.MustParsePrefix("198.18.0.0/30")
	if rule.unknownSelector || rule.destination == nil || *rule.destination != want {
		t.Fatalf("destination rule = %#v, want %s", rule, want)
	}
}

func TestRoutingRuleDecoderPreservesProtocolForAllocationInventory(t *testing.T) {
	message := nl.NewRtMsg()
	message.Family = unix.AF_INET
	message.Table = 100
	message.Type = uint8(nl.FR_ACT_TO_TBL)
	encoded := append([]byte(nil), message.Serialize()...)
	encoded = append(encoded, nl.NewRtAttr(nl.FRA_PROTOCOL, []byte{211}).Serialize()...)
	rule, err := routingRuleFromMessage(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if rule.protocol != 211 {
		t.Fatalf("decoded protocol = %d, want 211", rule.protocol)
	}
}

func TestRoutingRuleDecoderRejectsUnsupportedHeaderSelectors(t *testing.T) {
	transfer := conntrackTestClaim(t).transfers[0]
	for name, header := range map[string]struct {
		tos   uint8
		flags uint32
	}{
		"nonzero TOS":          {tos: 0x10, flags: unix.FIB_RULE_PERMANENT},
		"inverted nonzero TOS": {tos: 0x10, flags: unix.FIB_RULE_PERMANENT | unix.FIB_RULE_INVERT},
		"inverted unresolved flag": {flags: unix.FIB_RULE_PERMANENT | unix.FIB_RULE_INVERT |
			unix.FIB_RULE_UNRESOLVED},
		"inverted detached flag": {flags: unix.FIB_RULE_PERMANENT | unix.FIB_RULE_INVERT |
			unix.FIB_RULE_IIF_DETACHED},
	} {
		t.Run(name, func(t *testing.T) {
			rule, err := routingRuleFromMessage(routingRuleMessage(header.tos, header.flags))
			if err != nil {
				t.Fatalf("decode rule: %v", err)
			}
			if !rule.unknownSelector {
				t.Fatal("unsupported rule header was treated as semantically neutral")
			}
			if !hasPreemptingRule(transfer, []routingRule{rule}) {
				t.Fatal("unsupported higher-priority rule header did not block allocation")
			}
		})
	}
}

func TestRoutingRuleDecoderModelsNeutralAndInvertFlags(t *testing.T) {
	transfer := conntrackTestClaim(t).transfers[0]
	for name, header := range map[string]struct {
		flags    uint32
		inverted bool
		blocks   bool
	}{
		"permanent": {flags: unix.FIB_RULE_PERMANENT, blocks: true},
		"invert": {flags: unix.FIB_RULE_PERMANENT | unix.FIB_RULE_INVERT,
			inverted: true, blocks: false},
	} {
		t.Run(name, func(t *testing.T) {
			rule, err := routingRuleFromMessage(routingRuleMessage(0, header.flags))
			if err != nil {
				t.Fatalf("decode rule: %v", err)
			}
			if rule.unknownSelector || rule.inverted != header.inverted {
				t.Fatalf("decoded header = unknown %t, inverted %t", rule.unknownSelector, rule.inverted)
			}
			if got := hasPreemptingRule(transfer, []routingRule{rule}); got != header.blocks {
				t.Fatalf("preflight block = %t, want %t", got, header.blocks)
			}
		})
	}
}

func routingRuleMessage(tos uint8, flags uint32) []byte {
	message := nl.NewRtMsg()
	message.Family = unix.AF_INET
	message.Src_len = 15
	message.Tos = tos
	message.Table = 100
	message.Type = uint8(nl.FR_ACT_TO_TBL)
	message.Flags = flags
	result := append([]byte(nil), message.Serialize()...)
	return append(result, nl.NewRtAttr(nl.FRA_SRC, []byte{198, 18, 0, 0}).Serialize()...)
}
