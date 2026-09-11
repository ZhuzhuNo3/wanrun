package transfer

import (
	"net/netip"
	"reflect"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/namespaceresolvers"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestRequestFreezesCompleteManualIntent(t *testing.T) {
	first, _ := transfernumber.New(1)
	second, _ := transfernumber.New(2)
	firstSelection, err := NewManualNetworkSelection(first, netip.MustParseAddr("192.0.2.10"), 3)
	if err != nil {
		t.Fatal(err)
	}
	secondSelection, err := NewManualNetworkSelection(second, netip.MustParseAddr("192.0.2.11"), 1)
	if err != nil {
		t.Fatal(err)
	}
	size, _ := childprocesses.NewTerminalSize(100, 30)
	argv := []string{"copy", "--source", "{}"}
	environment := []string{"PATH=/usr/bin", "TRANSFERLANES_VALUE=exact"}
	request, err := NewManualRequest("/source", []ManualNetworkSelection{firstSelection, secondSelection}, argv,
		environment, namespaceresolvers.DefaultIntent(), size, true)
	if err != nil {
		t.Fatal(err)
	}
	argv[0], environment[0] = "changed", "PATH=/changed"

	if !reflect.DeepEqual(request.argv, []string{"copy", "--source", "{}"}) {
		t.Fatalf("command template = %#v", request.argv)
	}
	if !reflect.DeepEqual(request.environment,
		[]string{"PATH=/usr/bin", "TRANSFERLANES_VALUE=exact"}) {
		t.Fatalf("environment = %#v", request.environment)
	}
	if len(request.automatic) != 0 || len(request.manual) != 2 {
		t.Fatal("manual request became automatic")
	}
	if !request.FollowSymlinks() {
		t.Fatal("manual request lost explicit symlink policy")
	}
}

func TestRequestRejectsAmbiguousTransfersAndCommandBeforeExecution(t *testing.T) {
	first, _ := transfernumber.New(1)
	second, _ := transfernumber.New(2)
	third, _ := transfernumber.New(3)
	size, _ := childprocesses.NewTerminalSize(80, 24)
	valid, _ := NewManualNetworkSelection(first, netip.MustParseAddr("192.0.2.10"), 1)
	duplicateID, _ := NewManualNetworkSelection(first, netip.MustParseAddr("192.0.2.11"), 1)
	duplicateIP, _ := NewManualNetworkSelection(second, netip.MustParseAddr("192.0.2.10"), 1)
	missingSecond, _ := NewManualNetworkSelection(third, netip.MustParseAddr("192.0.2.12"), 1)

	tests := []struct {
		name       string
		selections []ManualNetworkSelection
		argv       []string
		env        []string
	}{
		{name: "duplicate transfer identity", selections: []ManualNetworkSelection{valid, duplicateID}, argv: []string{"copy", "{}"}},
		{name: "incomplete transfer set", selections: []ManualNetworkSelection{valid, missingSecond}, argv: []string{"copy", "{}"}},
		{name: "duplicate source", selections: []ManualNetworkSelection{valid, duplicateIP}, argv: []string{"copy", "{}"}},
		{name: "missing placeholder", selections: []ManualNetworkSelection{valid}, argv: []string{"copy"}},
		{name: "repeated placeholder", selections: []ManualNetworkSelection{valid}, argv: []string{"copy", "{}", "{}"}},
		{name: "embedded placeholder", selections: []ManualNetworkSelection{valid}, argv: []string{"copy", "prefix{}"}},
		{name: "duplicate environment", selections: []ManualNetworkSelection{valid}, argv: []string{"copy", "{}"},
			env: []string{"A=one", "A=two"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewManualRequest("/source", test.selections, test.argv, test.env,
				namespaceresolvers.DefaultIntent(), size, false); err == nil {
				t.Fatal("invalid transfer request was accepted")
			}
		})
	}
}

func TestManualNetworkSelectionRequiresPositiveWeight(t *testing.T) {
	number, _ := transfernumber.New(1)
	address := netip.MustParseAddr("192.0.2.10")
	if _, err := NewManualNetworkSelection(number, address, 0); err == nil {
		t.Fatal("zero weight was accepted")
	}
}

func TestAutomaticRequestRequiresAComparableGroup(t *testing.T) {
	first, _ := transfernumber.New(1)
	second, _ := transfernumber.New(2)
	size, _ := childprocesses.NewTerminalSize(80, 24)
	window, _ := throughput.NewSettings("https://measure.example/upload", time.Second)
	firstSelection, _ := NewAutomaticNetworkSelection(first, netip.MustParseAddr("192.0.2.10"))
	secondSelection, _ := NewAutomaticNetworkSelection(second, netip.MustParseAddr("192.0.2.11"))

	if _, err := NewAutomaticRequest("/source", []AutomaticNetworkSelection{firstSelection}, []string{"copy", "{}"},
		nil, namespaceresolvers.DefaultIntent(), size, window, false); err == nil {
		t.Fatal("single-transfer automatic weighting was accepted")
	}
	request, err := NewAutomaticRequest("/source", []AutomaticNetworkSelection{secondSelection, firstSelection},
		[]string{"copy", "{}"}, nil, namespaceresolvers.DefaultIntent(), size, window, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.manual) != 0 || len(request.automatic) != 2 ||
		request.automatic[0].transfer != second || request.automatic[1].transfer != first {
		t.Fatalf("automatic request order changed: %#v", request.automatic)
	}
}

func TestPlainRequestCarriesNoSyntheticTerminalRequirement(t *testing.T) {
	first, _ := transfernumber.New(1)
	selection, err := NewManualNetworkSelection(first, netip.MustParseAddr("192.0.2.10"), 1)
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewPlainManualRequest("/srv/source", []ManualNetworkSelection{selection},
		[]string{"copy", "{}"}, []string{"LANG=C"}, namespaceresolvers.DefaultIntent(), false)
	if err != nil {
		t.Fatal(err)
	}
	if request.CommandIO() != PlainCommandIO {
		t.Fatalf("plain request I/O = %v", request.CommandIO())
	}
}
