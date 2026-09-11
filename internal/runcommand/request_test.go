package runcommand

import (
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestRequestAcceptsCompleteManualAndAutomaticIntent(t *testing.T) {
	dns := []netip.Addr{netip.MustParseAddr("2001:4860:4860::8888")}
	argv := []string{"copy", "{}", "--literal"}
	display := NewDisplayOptions(true, true)
	request, err := NewRequest("/source", false, []NetworkArgument{
		validNetworkArgument("192.0.2.1", 3, true),
		validNetworkArgument("192.0.2.2", 2, true),
		validNetworkArgument("192.0.2.3", 1, true),
	}, false, throughput.Settings{}, dns, argv, textPointer("/logs"), display, true)
	if err != nil {
		t.Fatal(err)
	}
	if request.SourcePath() != "/source" || request.LogDirectory() != "/logs" ||
		request.Display() != display || !request.FollowSymlinks() {
		t.Fatalf("request lost scalar intent: %+v", request)
	}
	if !slices.Equal(request.DNS(), dns) || !slices.Equal(request.ChildArgv(), argv) {
		t.Fatalf("request lost slices: DNS=%v argv=%q", request.DNS(), request.ChildArgv())
	}
	if got := request.Networks().Manual(); len(got) != 3 || got[0].Weight() != 3 {
		t.Fatalf("manual networks=%v", got)
	}

	settings := mustSettings(t)
	request, err = NewRequest("/source", true, []NetworkArgument{
		validNetworkArgument("198.51.100.1", 1, false),
		validNetworkArgument("198.51.100.2", 1, false),
	}, false, settings, nil, []string{"copy", "{}"}, nil,
		NewDisplayOptions(false, false), false)
	if err != nil {
		t.Fatal(err)
	}
	if !request.Networks().AutomaticWeight() || request.Networks().Settings() != settings ||
		!slices.Equal(request.Networks().Automatic(), []netip.Addr{
			netip.MustParseAddr("198.51.100.1"), netip.MustParseAddr("198.51.100.2")}) {
		t.Fatalf("automatic networks=%v", request.Networks().Automatic())
	}
}

func TestRequestAcceptsManualBounds(t *testing.T) {
	if _, err := NewRequest("/source", false,
		[]NetworkArgument{validNetworkArgument("192.0.2.1", 1, false)}, false,
		throughput.Settings{}, nil, []string{"copy", "{}"}, nil, DisplayOptions{}, false); err != nil {
		t.Fatalf("single manual network rejected: %v", err)
	}
	maximum := make([]NetworkArgument, transfernumber.Maximum)
	for index := range maximum {
		address := netip.AddrFrom4([4]byte{192, 0, 2, byte(index + 1)})
		maximum[index] = NewNetworkArgument(address, uint64(index+1), false, false, false)
	}
	if _, err := NewRequest("/source", false, maximum, false, throughput.Settings{}, nil,
		[]string{"copy", "{}"}, nil, DisplayOptions{}, false); err != nil {
		t.Fatalf("maximum manual networks rejected: %v", err)
	}
}

func TestNewRequestAppliesBaselineValidationOrder(t *testing.T) {
	valid := validNetworkArgument("192.0.2.1", 1, false)
	invalidWeightAndAddress := NewNetworkArgument(netip.Addr{}, 0, true, true, true)
	invalidAddress := NewNetworkArgument(netip.Addr{}, 1, false, false, false)
	for _, test := range []struct {
		name             string
		source           string
		automatic        bool
		arguments        []NetworkArgument
		measureOverrides bool
		settings         throughput.Settings
		dns              []netip.Addr
		argv             []string
		logDirectory     *string
		want             RequestValidationReason
		wantIndex        int
	}{
		{name: "source before count", source: "relative", want: RequestSourcePathInvalid, wantIndex: -1},
		{name: "count before manual override", source: "/source", measureOverrides: true,
			want: RequestNetworkCountInvalid, wantIndex: -1},
		{name: "manual override before network syntax", source: "/source",
			arguments: []NetworkArgument{invalidWeightAndAddress}, measureOverrides: true,
			want: RequestManualMeasureOverride, wantIndex: -1},
		{name: "manual settings before network syntax", source: "/source",
			arguments: []NetworkArgument{invalidWeightAndAddress}, settings: mustSettings(t),
			want: RequestManualMeasureOverride, wantIndex: -1},
		{name: "weight syntax before address syntax", source: "/source",
			arguments: []NetworkArgument{invalidWeightAndAddress},
			want:      RequestNetworkWeightSyntaxInvalid, wantIndex: 0},
		{name: "address NUL before later weight syntax", source: "/source",
			arguments: []NetworkArgument{
				NewNetworkArgument(netip.Addr{}, 1, false, false, true), invalidWeightAndAddress},
			want: RequestNetworkAddressContainsNUL, wantIndex: 0},
		{name: "earlier IPv6 before later weight syntax", source: "/source",
			arguments: []NetworkArgument{validNetworkArgument("2001:db8::1", 1, false),
				invalidWeightAndAddress}, want: RequestNetworkAddressInvalid, wantIndex: 0},
		{name: "duplicate before later syntax", source: "/source",
			arguments: []NetworkArgument{valid, valid, invalidAddress},
			want:      RequestNetworkRepeated, wantIndex: 1},
		{name: "network item before automatic conflict", source: "/source", automatic: true,
			arguments: []NetworkArgument{validNetworkArgument("192.0.2.1", 1, true),
				invalidAddress}, settings: mustSettings(t),
			want: RequestNetworkAddressSyntaxInvalid, wantIndex: 1},
		{name: "automatic conflict before minimum", source: "/source", automatic: true,
			arguments: []NetworkArgument{validNetworkArgument("192.0.2.1", 1, true)},
			settings:  mustSettings(t), want: RequestAutomaticExplicitWeightInvalid, wantIndex: -1},
		{name: "automatic minimum before settings", source: "/source", automatic: true,
			arguments: []NetworkArgument{valid}, want: RequestAutomaticNetworkCountInvalid, wantIndex: -1},
		{name: "settings before DNS", source: "/source", automatic: true,
			arguments: []NetworkArgument{valid,
				validNetworkArgument("192.0.2.2", 1, false)}, dns: []netip.Addr{{}},
			argv: []string{"copy", "{}"}, want: RequestMeasureSettingsInvalid, wantIndex: -1},
		{name: "DNS count before syntax", source: "/source", arguments: []NetworkArgument{valid},
			dns: make([]netip.Addr, maximumDNSResolvers+1), argv: []string{"copy", "{}"},
			want: RequestDNSCountInvalid, wantIndex: -1},
		{name: "earlier unusable DNS before later syntax", source: "/source",
			arguments: []NetworkArgument{valid},
			dns:       []netip.Addr{netip.MustParseAddr("127.0.0.1"), {}},
			argv:      []string{"copy", "{}"}, want: RequestDNSAddressInvalid, wantIndex: -1},
		{name: "DNS syntax before later unusable", source: "/source",
			arguments: []NetworkArgument{valid},
			dns:       []netip.Addr{{}, netip.MustParseAddr("127.0.0.1")},
			argv:      []string{"copy", "{}"}, want: RequestDNSAddressSyntaxInvalid, wantIndex: -1},
		{name: "log before child", source: "/source", arguments: []NetworkArgument{valid},
			logDirectory: textPointer("relative"), want: RequestLogDirectoryInvalid, wantIndex: -1},
		{name: "child before normalized weight", source: "/source",
			arguments: []NetworkArgument{validNetworkArgument("192.0.2.1", 0, true)},
			want:      RequestChildArgumentCountInvalid, wantIndex: -1},
		{name: "child NUL before normalized weight", source: "/source",
			arguments: []NetworkArgument{validNetworkArgument("192.0.2.1", 0, true)},
			argv:      []string{"copy\x00", "{}"}, want: RequestChildArgumentContainsNUL, wantIndex: -1},
		{name: "child placeholder before normalized weight", source: "/source",
			arguments: []NetworkArgument{validNetworkArgument("192.0.2.1", 0, true)},
			argv:      []string{"copy"}, want: RequestChildPlaceholderCountInvalid, wantIndex: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRequest(test.source, test.automatic, test.arguments,
				test.measureOverrides, test.settings, test.dns, test.argv,
				test.logDirectory, DisplayOptions{}, false)
			failure := assertRequestReason(t, err, test.want)
			if test.wantIndex >= 0 {
				if index, exists := failure.NetworkIndex(); !exists || index != test.wantIndex {
					t.Fatalf("network index=(%d,%t), want (%d,true)", index, exists, test.wantIndex)
				}
			}
		})
	}
}

func TestNewRequestChecksNormalizedWeightAfterChild(t *testing.T) {
	argument := validNetworkArgument("192.0.2.1", 0, true)
	_, err := NewRequest("/source", false, []NetworkArgument{argument}, false,
		throughput.Settings{}, nil, nil, nil, DisplayOptions{}, false)
	assertRequestReason(t, err, RequestChildArgumentCountInvalid)
	_, err = NewRequest("/source", false, []NetworkArgument{argument}, false,
		throughput.Settings{}, nil, []string{"copy", "{}"}, nil, DisplayOptions{}, false)
	failure := assertRequestReason(t, err, RequestManualNetworkWeightInvalid)
	if index, exists := failure.NetworkIndex(); !exists || index != 0 {
		t.Fatalf("network index=(%d,%t), want (0,true)", index, exists)
	}
}

func TestNewRequestOwnsDNSUsability(t *testing.T) {
	for _, address := range []string{"0.0.0.0", "127.0.0.1", "224.0.0.1", "::", "::1", "ff02::1"} {
		t.Run(address, func(t *testing.T) {
			_, err := NewRequest("/source", false,
				[]NetworkArgument{validNetworkArgument("192.0.2.1", 1, false)}, false,
				throughput.Settings{}, []netip.Addr{netip.MustParseAddr(address)},
				[]string{"copy", "{}"}, nil, DisplayOptions{}, false)
			failure := assertRequestReason(t, err, RequestDNSAddressInvalid)
			if index, exists := failure.ResolverIndex(); !exists || index != 0 {
				t.Fatalf("resolver index=(%d,%t), want (0,true)", index, exists)
			}
		})
	}
}

func TestRequestDefensivelyCopiesInputsAndOutputs(t *testing.T) {
	arguments := []NetworkArgument{validNetworkArgument("192.0.2.1", 1, false)}
	dns := []netip.Addr{netip.MustParseAddr("2001:4860:4860::8888")}
	argv := []string{"copy", "{}"}
	logDirectory := "/logs"
	request, err := NewRequest("/source", false, arguments, false,
		throughput.Settings{}, dns, argv, &logDirectory, DisplayOptions{}, false)
	if err != nil {
		t.Fatal(err)
	}
	arguments[0] = validNetworkArgument("192.0.2.9", 9, false)
	dns[0] = netip.MustParseAddr("2001:4860:4860::8844")
	argv[0] = "changed"
	logDirectory = "/changed"
	if request.Networks().Manual()[0].LocalIP() != netip.MustParseAddr("192.0.2.1") ||
		request.Networks().Manual()[0].Weight() != 1 || request.LogDirectory() != "/logs" ||
		request.DNS()[0] != netip.MustParseAddr("2001:4860:4860::8888") ||
		request.ChildArgv()[0] != "copy" {
		t.Fatal("constructor input mutated request")
	}
	manualCopy := request.Networks().Manual()
	dnsCopy := request.DNS()
	argvCopy := request.ChildArgv()
	manualCopy[0] = ManualNetwork{localIP: netip.MustParseAddr("192.0.2.8"), weight: 8}
	dnsCopy[0] = netip.MustParseAddr("2001:db8::1")
	argvCopy[0] = "changed"
	if request.Networks().Manual()[0].LocalIP() != netip.MustParseAddr("192.0.2.1") ||
		request.DNS()[0] != netip.MustParseAddr("2001:4860:4860::8888") ||
		request.ChildArgv()[0] != "copy" {
		t.Fatal("getter output mutated request")
	}
}

func validNetworkArgument(address string, weight uint64, explicit bool) NetworkArgument {
	return NewNetworkArgument(netip.MustParseAddr(address), weight, explicit, false, false)
}

func textPointer(value string) *string { return &value }

func mustSettings(t *testing.T) throughput.Settings {
	t.Helper()
	settings, err := throughput.NewSettings("https://example.invalid/upload", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return settings
}

func assertRequestReason(t *testing.T, err error,
	want RequestValidationReason,
) *RequestValidationFailure {
	t.Helper()
	var failure *RequestValidationFailure
	if !errors.As(err, &failure) || failure.Reason() != want {
		t.Fatalf("validation failure=%T %v reason=%v, want %v", err, err,
			failure.Reason(), want)
	}
	return failure
}
