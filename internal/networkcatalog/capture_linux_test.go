//go:build linux

package networkcatalog

import (
	"context"
	"fmt"
	"net"
	"testing"
)

type linkRead struct {
	link    linkObservation
	present bool
	err     error
}

type changingNetworkReader struct {
	addresses  []addressObservation
	routes     []fibRoute
	rules      [][]policyRule
	byName     map[string][]linkRead
	byIndex    map[int][]linkRead
	ruleRead   int
	nameReads  map[string]int
	indexReads map[int]int
}

func TestCaptureIncludesInterfaceReferencedOnlyByPolicyRule(t *testing.T) {
	reader := stableRuleOnlyReader()
	snapshot, err := captureLinuxNetwork(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.links) != 1 || snapshot.links[0].name != "return0" {
		t.Fatalf("captured links = %#v", snapshot.links)
	}
	if len(snapshot.rules) != 1 {
		t.Fatalf("captured rules = %#v", snapshot.rules)
	}
	selector := snapshot.rules[0].inputInterface
	if selector.name != "return0" || selector.state != inputInterfaceAttached ||
		selector.index != 9 || selector.isL3Master {
		t.Fatalf("captured input interface = %#v", selector)
	}
}

func TestCaptureRejectsChangingRuleAndInterfaceFacts(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		change func(*changingNetworkReader)
	}{
		{name: "rule dump changes", change: func(reader *changingNetworkReader) {
			reader.rules[1][0].priority++
		}},
		{name: "attached rule becomes unavailable", change: func(reader *changingNetworkReader) {
			reader.byName["return0"][1] = linkRead{}
		}},
		{name: "name resolves to another index", change: func(reader *changingNetworkReader) {
			changed := ruleOnlyLink()
			changed.index = 10
			reader.byName["return0"][1] = presentLink(changed)
			reader.byIndex[10] = []linkRead{presentLink(changed)}
		}},
		{name: "link is renamed during index observation", change: func(reader *changingNetworkReader) {
			changed := ruleOnlyLink()
			changed.name = "renamed0"
			reader.byIndex[9][0] = presentLink(changed)
		}},
		{name: "L3 master identity changes", change: func(reader *changingNetworkReader) {
			changed := ruleOnlyLink()
			changed.kind = "vrf"
			reader.byName["return0"][1] = presentLink(changed)
			reader.byIndex[9][1] = presentLink(changed)
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			reader := stableRuleOnlyReader()
			testCase.change(reader)
			if _, err := captureLinuxNetwork(context.Background(), reader); err == nil {
				t.Fatal("unstable Linux network facts were accepted")
			}
		})
	}
}

func TestCaptureRejectsChangingOutputInterfaceAttachment(t *testing.T) {
	reader := stableRuleOnlyReader()
	reader.rules[0][0].outputInterface = outputInterfaceSelector{
		name: "target0", state: outputInterfaceAttached,
	}
	reader.rules[1][0].outputInterface = outputInterfaceSelector{
		name: "target0", state: outputInterfaceDetached,
	}
	if _, err := captureLinuxNetwork(context.Background(), reader); err == nil {
		t.Fatal("changing output interface attachment was accepted")
	}
}

func TestCaptureRejectsDetachedRuleThatStillNamesAnInterface(t *testing.T) {
	reader := stableRuleOnlyReader()
	for observation := range reader.rules {
		reader.rules[observation][0].inputInterface.state = inputInterfaceDetached
		reader.rules[observation][0].inputInterface.index = -1
	}
	if _, err := captureLinuxNetwork(context.Background(), reader); err == nil {
		t.Fatal("detached policy rule with an attached link was accepted")
	}
}

func TestCaptureDropsUnstableAddressInterfaceWithoutRejectingOtherFacts(t *testing.T) {
	reader := stableRuleOnlyReader()
	reader.rules = [][]policyRule{{}, {}}
	reader.byName = nil
	reader.addresses = []addressObservation{fixtureAddress(2, "192.0.2.10/24")}
	reader.byIndex = map[int][]linkRead{2: {{}, {}}}
	snapshot, err := captureLinuxNetwork(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.links) != 0 || len(snapshot.addresses) != 0 {
		t.Fatalf("unstable optional link facts were retained: links=%#v addresses=%#v",
			snapshot.links, snapshot.addresses)
	}
}

func stableRuleOnlyReader() *changingNetworkReader {
	rule := fixtureRule(50, 300, "0.0.0.0/0")
	rule.destination = mustPrefix("198.18.0.0/30")
	rule.inputInterface = inputInterfaceSelector{name: "return0", state: inputInterfaceUnresolved}
	link := ruleOnlyLink()
	return &changingNetworkReader{
		rules:      [][]policyRule{{rule}, {rule}},
		byName:     map[string][]linkRead{"return0": {presentLink(link), presentLink(link)}},
		byIndex:    map[int][]linkRead{9: {presentLink(link), presentLink(link)}},
		nameReads:  make(map[string]int),
		indexReads: make(map[int]int),
	}
}

func ruleOnlyLink() linkObservation {
	return linkObservation{index: 9, name: "return0", kind: "device", mtu: 1500,
		flags: net.FlagUp, operationalState: "up"}
}

func presentLink(link linkObservation) linkRead {
	return linkRead{link: link, present: true}
}

func (reader *changingNetworkReader) readAddresses(ctx context.Context) ([]addressObservation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]addressObservation(nil), reader.addresses...), nil
}

func (reader *changingNetworkReader) readPolicyRules(ctx context.Context) ([]policyRule, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reader.ruleRead >= len(reader.rules) {
		return nil, fmt.Errorf("unexpected policy rule read %d", reader.ruleRead+1)
	}
	result := cloneRules(reader.rules[reader.ruleRead])
	reader.ruleRead++
	return result, nil
}

func (reader *changingNetworkReader) readRoutes(ctx context.Context) ([]fibRoute, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return append([]fibRoute(nil), reader.routes...), nil
}

func (reader *changingNetworkReader) readLinkByIndex(ctx context.Context,
	index int,
) (linkObservation, bool, error) {
	if err := ctx.Err(); err != nil {
		return linkObservation{}, false, err
	}
	read := reader.indexReads[index]
	reader.indexReads[index] = read + 1
	return nextLinkRead(reader.byIndex[index], read, fmt.Sprintf("interface index %d", index))
}

func (reader *changingNetworkReader) readLinkByName(ctx context.Context,
	name string,
) (linkObservation, bool, error) {
	if err := ctx.Err(); err != nil {
		return linkObservation{}, false, err
	}
	read := reader.nameReads[name]
	reader.nameReads[name] = read + 1
	return nextLinkRead(reader.byName[name], read, "interface "+name)
}

func nextLinkRead(values []linkRead, index int,
	description string,
) (linkObservation, bool, error) {
	if index >= len(values) {
		return linkObservation{}, false, fmt.Errorf("unexpected %s read %d", description, index+1)
	}
	return values[index].link, values[index].present, values[index].err
}
