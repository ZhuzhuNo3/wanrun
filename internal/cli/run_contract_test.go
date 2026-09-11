package cli

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/runcommand"
)

type runUsageContract struct {
	name    string
	argv    []string
	message string
	reason  runcommand.RequestValidationReason
}

func baselineRunUsageContracts() []runUsageContract {
	return []runUsageContract{
		{name: "source", argv: []string{"run", "--source", "relative", "--network", "192.0.2.1", "copy", "{}"},
			message: "--source must be a clean absolute directory other than /", reason: runcommand.RequestSourcePathInvalid},
		{name: "network count", argv: []string{"run", "--source", "/src", "copy", "{}"},
			message: "run requires 1..35 --network values", reason: runcommand.RequestNetworkCountInvalid},
		{name: "automatic count", argv: []string{"run", "--source", "/src", "--auto-weight", "--network", "192.0.2.1", "copy", "{}"},
			message: "--auto-weight requires at least two networks", reason: runcommand.RequestAutomaticNetworkCountInvalid},
		{name: "invalid IP syntax", argv: []string{"run", "--source", "/src", "--network", "invalid", "copy", "{}"},
			message: "--network \"invalid\" is not IPv4"},
		{name: "network NUL", argv: []string{"run", "--source", "/src", "--network", "192.0.2.1\x00", "copy", "{}"},
			message: "--network contains NUL"},
		{name: "manual IPv6", argv: []string{"run", "--source", "/src", "--network", "2001:DB8::1", "copy", "{}"},
			message: "--network \"2001:DB8::1\" is not IPv4", reason: runcommand.RequestNetworkAddressInvalid},
		{name: "automatic IPv6", argv: []string{"run", "--source", "/src", "--auto-weight", "--network", "2001:DB8::1", "--network", "192.0.2.1", "copy", "{}"},
			message: "--network \"2001:DB8::1\" is not IPv4", reason: runcommand.RequestNetworkAddressInvalid},
		{name: "manual duplicate", argv: []string{"run", "--source", "/src", "--network", "192.0.2.1", "--network", "192.0.2.1", "copy", "{}"},
			message: "--network repeats 192.0.2.1", reason: runcommand.RequestNetworkRepeated},
		{name: "automatic duplicate", argv: []string{"run", "--source", "/src", "--auto-weight", "--network", "192.0.2.1", "--network", "192.0.2.1", "copy", "{}"},
			message: "--network repeats 192.0.2.1", reason: runcommand.RequestNetworkRepeated},
		{name: "DNS syntax", argv: []string{"run", "--source", "/src", "--network", "192.0.2.1", "--dns", "invalid", "copy", "{}"},
			message: "--dns \"invalid\" is not a usable nameserver IP"},
		{name: "DNS usability", argv: []string{"run", "--source", "/src", "--network", "192.0.2.1", "--dns", "127.0.0.1", "copy", "{}"},
			message: "--dns \"127.0.0.1\" is not a usable nameserver IP", reason: runcommand.RequestDNSAddressInvalid},
		{name: "log directory", argv: []string{"run", "--source", "/src", "--network", "192.0.2.1", "--log-dir", "relative", "copy", "{}"},
			message: "--log-dir must be a clean absolute path", reason: runcommand.RequestLogDirectoryInvalid},
		{name: "empty log directory", argv: []string{"run", "--source", "/src", "--network", "192.0.2.1", "--log-dir", "", "copy", "{}"},
			message: "--log-dir must be a clean absolute path", reason: runcommand.RequestLogDirectoryInvalid},
		{name: "missing child", argv: []string{"run", "--source", "/src", "--network", "192.0.2.1"},
			message: "run child command exceeds its private argument-count representation", reason: runcommand.RequestChildArgumentCountInvalid},
		{name: "child NUL", argv: []string{"run", "--source", "/src", "--network", "192.0.2.1", "copy\x00", "{}"},
			message: "child argv contains NUL", reason: runcommand.RequestChildArgumentContainsNUL},
		{name: "child placeholder", argv: []string{"run", "--source", "/src", "--network", "192.0.2.1", "copy", "prefix{}"},
			message: "child argv must contain exactly one standalone {} argument", reason: runcommand.RequestChildPlaceholderCountInvalid},
	}
}

func TestRunAcceptsThirtyFiveTransfersAndRejectsThirtySix(t *testing.T) {
	argv := []string{"run", "--source", "/src"}
	for number := 1; number <= 35; number++ {
		argv = append(argv, "--network", fmt.Sprintf("192.0.2.%d", number))
	}
	argv = append(argv, "copy", "{}")
	if _, err := Parse(argv); err != nil {
		t.Fatalf("35 transfers were rejected: %v", err)
	}
	argv = append(argv[:len(argv)-2], "--network", "198.51.100.1", "copy", "{}")
	_, err := Parse(argv)
	var usage *UsageError
	var failure *runcommand.RequestValidationFailure
	if !errors.As(err, &usage) || !errors.As(err, &failure) ||
		failure.Reason() != runcommand.RequestNetworkCountInvalid ||
		err.Error() != "run requires 1..35 --network values" {
		t.Fatalf("36 transfers failure=%T %v", err, err)
	}
}

func TestListRequestRetainsOnlyRequestedDiagnostics(t *testing.T) {
	if defaultMeasureDuration != 3*time.Second {
		t.Fatalf("default list measure duration=%s, want 3s", defaultMeasureDuration)
	}
	for _, test := range []struct {
		argv                   []string
		wantProbe, wantMeasure bool
	}{
		{argv: []string{"list"}},
		{argv: []string{"list", "--probe"}, wantProbe: true},
		{argv: []string{"list", "--measure"}, wantMeasure: true},
		{argv: []string{"list", "--probe", "--measure"}, wantProbe: true, wantMeasure: true},
	} {
		parsed, err := Parse(test.argv)
		if err != nil {
			t.Fatalf("Parse(%q): %v", test.argv, err)
		}
		request := parsed.(List).Request()
		if request.ProbeRequested() != test.wantProbe || request.MeasureRequested() != test.wantMeasure {
			t.Fatalf("Parse(%q) diagnostics=%t/%t, want %t/%t", test.argv,
				request.ProbeRequested(), request.MeasureRequested(), test.wantProbe, test.wantMeasure)
		}
	}
}

func TestRunNormalizesManualWeightsToSimplestIntegerRatio(t *testing.T) {
	tests := []struct {
		name     string
		weights  []string
		expected []uint64
	}{
		{name: "mixed decimal and omitted", weights: []string{"@1.5", "", "@0.5"}, expected: []uint64{3, 2, 1}},
		{name: "decimal reduction", weights: []string{"@2.50", "@1.25"}, expected: []uint64{2, 1}},
		{name: "leading and trailing zeros", weights: []string{"@02.500", "@001.250"}, expected: []uint64{2, 1}},
		{name: "integer reduction", weights: []string{"@6", "@3"}, expected: []uint64{2, 1}},
		{name: "all omitted", weights: []string{"", ""}, expected: []uint64{1, 1}},
		{name: "large values simplify before range check",
			weights:  []string{"@184467440737095516160", "@92233720368547758080"},
			expected: []uint64{2, 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			argv := []string{"run", "--source", "/src"}
			for index, weight := range test.weights {
				argv = append(argv, "--network", fmt.Sprintf("192.0.2.%d%s", index+8, weight))
			}
			argv = append(argv, "--dns", "2001:4860:4860::8888", "--", "copy", "{}")
			parsed, err := Parse(argv)
			if err != nil {
				t.Fatal(err)
			}
			run := parsed.(Run)
			networks := run.Request().Networks()
			transfers := networks.Manual()
			if len(transfers) != len(test.expected) || len(networks.Automatic()) != 0 || networks.AutomaticWeight() {
				t.Fatalf("transfers=%+v", transfers)
			}
			for index, weight := range test.expected {
				if transfers[index].Weight() != weight {
					t.Errorf("transfer %d=%+v, want weight %d", index+1, transfers[index], weight)
				}
			}
			if transfers[0].LocalIP() != netip.MustParseAddr("192.0.2.8") {
				t.Fatalf("first transfer=%+v", transfers[0])
			}
			if got := run.Request().DNS(); !slices.Equal(got, []netip.Addr{netip.MustParseAddr("2001:4860:4860::8888")}) {
				t.Fatalf("DNS=%v", got)
			}
		})
	}
}

func TestRunKeepsAutomaticSelectionsFreeOfManualWeights(t *testing.T) {
	parsed, err := Parse([]string{"run", "--source", "/src", "--auto-weight",
		"--network", "192.0.2.8", "--network", "192.0.2.9", "copy", "{}"})
	if err != nil {
		t.Fatal(err)
	}
	run := parsed.(Run)
	networks := run.Request().Networks()
	transfers := networks.Automatic()
	if len(networks.Manual()) != 0 || !networks.AutomaticWeight() || len(transfers) != 2 ||
		transfers[0] != netip.MustParseAddr("192.0.2.8") {
		t.Fatalf("automatic transfers=%+v", transfers)
	}
}

func TestRunRejectsWeightsOutsidePositiveIntegerOrDecimalGrammar(t *testing.T) {
	for _, weight := range []string{
		"", "0", "0.0", "-1", "+1", "1/2", "1e2", ".5", "1.", "1.2.3", " 1", "1 ", "\u00a01", "1@2",
		"18446744073709551616",
	} {
		t.Run(weight, func(t *testing.T) {
			argv := []string{"run", "--source", "/src", "--network", "192.0.2.8@" + weight}
			if weight == "18446744073709551616" {
				argv = append(argv, "--network", "192.0.2.9")
			}
			_, err := Parse(append(argv, "copy", "{}"))
			var usage *UsageError
			if !errors.As(err, &usage) {
				t.Fatalf("Parse(weight=%q) error=%T %v, want UsageError", weight, err, err)
			}
			if got, want := err.Error(), "--network has an invalid weight"; got != want {
				t.Fatalf("Parse(weight=%q) error=%q, want %q", weight, got, want)
			}
			var failure *runcommand.RequestValidationFailure
			if errors.As(err, &failure) {
				t.Fatalf("CLI-owned weight failure unexpectedly wraps runcommand cause %v", failure)
			}
		})
	}
}

func TestRunPreservesDNSValidationOwnershipAndOrder(t *testing.T) {
	for _, test := range []struct {
		name      string
		resolvers []string
		message   string
		reason    runcommand.RequestValidationReason
		index     int
	}{
		{name: "earlier loopback before later syntax", resolvers: []string{"127.0.0.1", "invalid"},
			message: "--dns \"127.0.0.1\" is not a usable nameserver IP",
			reason:  runcommand.RequestDNSAddressInvalid, index: 0},
		{name: "earlier multicast after usable before later syntax",
			resolvers: []string{"8.8.8.8", "FF02::1", "invalid"},
			message:   "--dns \"FF02::1\" is not a usable nameserver IP",
			reason:    runcommand.RequestDNSAddressInvalid, index: 1},
		{name: "earlier syntax before later unusable", resolvers: []string{"invalid", "127.0.0.1"},
			message: "--dns \"invalid\" is not a usable nameserver IP", index: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			argv := []string{"run", "--source", "/src", "--network", "192.0.2.1"}
			for _, resolver := range test.resolvers {
				argv = append(argv, "--dns", resolver)
			}
			_, err := Parse(append(argv, "copy", "{}"))
			var usage *UsageError
			if !errors.As(err, &usage) || err.Error() != test.message {
				t.Fatalf("validation failure=%T %q, want UsageError %q", err, err, test.message)
			}
			var failure *runcommand.RequestValidationFailure
			if test.reason == 0 {
				if errors.As(err, &failure) {
					t.Fatalf("CLI-owned syntax failure unexpectedly wraps runcommand cause %v", failure)
				}
				return
			}
			if !errors.As(err, &failure) || failure.Reason() != test.reason {
				t.Fatalf("structured failure=%T %v reason=%v, want %v", err, err,
					failure.Reason(), test.reason)
			}
			if index, exists := failure.ResolverIndex(); !exists || index != test.index {
				t.Fatalf("resolver index=(%d,%t), want (%d,true)", index, exists, test.index)
			}
			if failure.Address() != netip.MustParseAddr(test.resolvers[test.index]) {
				t.Fatalf("resolver address=%v, want %s", failure.Address(), test.resolvers[test.index])
			}
		})
	}
}

func TestRunRejectsTooManyResolversBeforeIndividualSyntax(t *testing.T) {
	argv := []string{"run", "--source", "/src", "--network", "192.0.2.1"}
	for range 256 {
		argv = append(argv, "--dns", "invalid")
	}
	_, err := Parse(append(argv, "copy", "{}"))
	var usage *UsageError
	var failure *runcommand.RequestValidationFailure
	if !errors.As(err, &usage) || !errors.As(err, &failure) ||
		failure.Reason() != runcommand.RequestDNSCountInvalid || failure.Maximum() != 255 ||
		err.Error() != "run accepts at most 255 --dns values" {
		t.Fatalf("resolver count failure=%T %v", err, err)
	}
}

func TestRunPreservesBaselineRequestValidationDiagnostics(t *testing.T) {
	for _, test := range baselineRunUsageContracts() {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(test.argv)
			var usage *UsageError
			if !errors.As(err, &usage) || err.Error() != test.message {
				t.Fatalf("validation failure=%T %q, want UsageError %q", err, err, test.message)
			}
			var failure *runcommand.RequestValidationFailure
			if test.reason == 0 {
				if errors.As(err, &failure) {
					t.Fatalf("lexical failure unexpectedly exposes runcommand cause %v", failure)
				}
			} else if !errors.As(err, &failure) || failure.Reason() != test.reason {
				t.Fatalf("structured failure=%T %v reason=%v, want %v", err, err,
					failure.Reason(), test.reason)
			}
		})
	}
}

func TestRunPreservesBaselineValidationPipeline(t *testing.T) {
	for _, test := range []struct {
		name    string
		argv    []string
		message string
		reason  runcommand.RequestValidationReason
	}{
		{name: "source before every later stage",
			argv: []string{"run", "--source", "relative", "--auto-weight",
				"--network", "invalid", "--measure-duration", "invalid", "--dns", "127.0.0.1", "copy", "{}"},
			message: "--source must be a clean absolute directory other than /",
			reason:  runcommand.RequestSourcePathInvalid},
		{name: "network count before manual measure override",
			argv: []string{"run", "--source", "/src", "--measure-duration", "invalid",
				"--dns", "127.0.0.1", "copy", "{}"},
			message: "run requires 1..35 --network values",
			reason:  runcommand.RequestNetworkCountInvalid},
		{name: "manual measure override before network syntax",
			argv: []string{"run", "--source", "/src", "--network", "invalid",
				"--measure-duration", "invalid", "--dns", "127.0.0.1", "copy", "{}"},
			message: "measure overrides require --auto-weight",
			reason:  runcommand.RequestManualMeasureOverride},
		{name: "network weight syntax before address syntax",
			argv:    []string{"run", "--source", "/src", "--network", "invalid@0", "copy", "{}"},
			message: "--network has an invalid weight"},
		{name: "single automatic network syntax before minimum",
			argv: []string{"run", "--source", "/src", "--auto-weight",
				"--network", "invalid", "copy", "{}"},
			message: "--network \"invalid\" is not IPv4"},
		{name: "single automatic IPv6 before minimum",
			argv: []string{"run", "--source", "/src", "--auto-weight",
				"--network", "2001:DB8::1", "copy", "{}"},
			message: "--network \"2001:DB8::1\" is not IPv4",
			reason:  runcommand.RequestNetworkAddressInvalid},
		{name: "single automatic explicit weight before minimum",
			argv: []string{"run", "--source", "/src", "--auto-weight",
				"--network", "192.0.2.1@2", "copy", "{}"},
			message: "--auto-weight cannot be combined with an explicit @weight",
			reason:  runcommand.RequestAutomaticExplicitWeightInvalid},
		{name: "IPv6 before measure and DNS",
			argv: []string{"run", "--source", "/src", "--auto-weight",
				"--network", "2001:DB8::1", "--network", "192.0.2.2",
				"--measure-duration", "invalid", "--dns", "127.0.0.1", "copy", "{}"},
			message: "--network \"2001:DB8::1\" is not IPv4",
			reason:  runcommand.RequestNetworkAddressInvalid},
		{name: "duplicate before measure and DNS",
			argv: []string{"run", "--source", "/src", "--auto-weight",
				"--network", "192.0.2.1", "--network", "192.0.2.1",
				"--measure-duration", "invalid", "--dns", "127.0.0.1", "copy", "{}"},
			message: "--network repeats 192.0.2.1",
			reason:  runcommand.RequestNetworkRepeated},
		{name: "earlier duplicate before later syntax",
			argv: []string{"run", "--source", "/src", "--network", "192.0.2.1",
				"--network", "192.0.2.1", "--network", "invalid", "copy", "{}"},
			message: "--network repeats 192.0.2.1",
			reason:  runcommand.RequestNetworkRepeated},
		{name: "earlier duplicate before later weight syntax",
			argv: []string{"run", "--source", "/src", "--network", "192.0.2.1",
				"--network", "192.0.2.1", "--network", "192.0.2.2@0", "copy", "{}"},
			message: "--network repeats 192.0.2.1",
			reason:  runcommand.RequestNetworkRepeated},
		{name: "later syntax before automatic explicit weight conflict",
			argv: []string{"run", "--source", "/src", "--auto-weight",
				"--network", "192.0.2.1@2", "--network", "invalid", "copy", "{}"},
			message: "--network \"invalid\" is not IPv4"},
		{name: "automatic explicit weight conflict before measure and DNS",
			argv: []string{"run", "--source", "/src", "--auto-weight",
				"--network", "192.0.2.1@2", "--network", "192.0.2.2",
				"--measure-duration", "invalid", "--dns", "127.0.0.1", "copy", "{}"},
			message: "--auto-weight cannot be combined with an explicit @weight",
			reason:  runcommand.RequestAutomaticExplicitWeightInvalid},
		{name: "automatic minimum before measure and DNS",
			argv: []string{"run", "--source", "/src", "--auto-weight",
				"--network", "192.0.2.1", "--measure-duration", "invalid",
				"--dns", "127.0.0.1", "copy", "{}"},
			message: "--auto-weight requires at least two networks",
			reason:  runcommand.RequestAutomaticNetworkCountInvalid},
		{name: "measure settings before DNS",
			argv: []string{"run", "--source", "/src", "--auto-weight",
				"--network", "192.0.2.1", "--network", "192.0.2.2",
				"--measure-duration", "invalid", "--dns", "127.0.0.1", "copy", "{}"},
			message: `parse --measure-duration: time: invalid duration "invalid"`},
		{name: "DNS before log directory and child argv",
			argv: []string{"run", "--source", "/src", "--network", "192.0.2.1",
				"--dns", "127.0.0.1", "--log-dir", "relative"},
			message: "--dns \"127.0.0.1\" is not a usable nameserver IP",
			reason:  runcommand.RequestDNSAddressInvalid},
		{name: "log directory before child argv",
			argv: []string{"run", "--source", "/src", "--network", "192.0.2.1",
				"--log-dir", "relative"},
			message: "--log-dir must be a clean absolute path",
			reason:  runcommand.RequestLogDirectoryInvalid},
		{name: "DNS before manual weight normalization",
			argv: []string{"run", "--source", "/src",
				"--network", "192.0.2.1@18446744073709551616",
				"--network", "192.0.2.2", "--dns", "127.0.0.1", "copy", "{}"},
			message: "--dns \"127.0.0.1\" is not a usable nameserver IP",
			reason:  runcommand.RequestDNSAddressInvalid},
		{name: "child argv before manual weight normalization",
			argv: []string{"run", "--source", "/src",
				"--network", "192.0.2.1@18446744073709551616",
				"--network", "192.0.2.2"},
			message: "run child command exceeds its private argument-count representation",
			reason:  runcommand.RequestChildArgumentCountInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Parse(test.argv)
			var usage *UsageError
			if !errors.As(err, &usage) || err.Error() != test.message {
				t.Fatalf("validation failure=%T %q, want UsageError %q", err, err, test.message)
			}
			var failure *runcommand.RequestValidationFailure
			if test.reason == 0 {
				if errors.As(err, &failure) {
					t.Fatalf("lexical failure unexpectedly exposes runcommand cause %v", failure)
				}
			} else if !errors.As(err, &failure) || failure.Reason() != test.reason {
				t.Fatalf("structured failure=%T %v reason=%v, want %v", err, err,
					failure.Reason(), test.reason)
			}
		})
	}
}

func TestRunAcceptsExplicitPlainDisplayWithoutChangingChildArgv(t *testing.T) {
	parsed, err := Parse([]string{"run", "--source", "/src", "--network", "192.0.2.8",
		"--no-tui", "--mouse", "--follow-symlinks", "--", "copy", "{}"})
	if err != nil {
		t.Fatal(err)
	}
	run := parsed.(Run)
	request := run.Request()
	if !request.Display().NoTUI() || !request.Display().Mouse() || !request.FollowSymlinks() ||
		!slices.Equal(request.ChildArgv(), []string{"copy", "{}"}) {
		t.Fatalf("no-tui=%v mouse=%v follow=%v argv=%q", request.Display().NoTUI(), request.Display().Mouse(),
			request.FollowSymlinks(), request.ChildArgv())
	}
	defaultPolicy, err := Parse([]string{"run", "--source", "/src", "--network", "192.0.2.8",
		"copy", "{}"})
	if err != nil || defaultPolicy.(Run).Request().FollowSymlinks() {
		t.Fatalf("default symlink policy=%v error=%v", defaultPolicy, err)
	}
	if _, err := Parse([]string{"run", "--source", "/src", "--network", "192.0.2.8",
		"--no-tui", "--no-tui", "copy", "{}"}); err == nil {
		t.Fatal("duplicate --no-tui was accepted")
	}
	if _, err := Parse([]string{"run", "--source", "/src", "--network", "192.0.2.8",
		"--mouse", "--mouse", "copy", "{}"}); err == nil {
		t.Fatal("duplicate --mouse was accepted")
	}
	if _, err := Parse([]string{"run", "--source", "/src", "--network", "192.0.2.8",
		"--follow-symlinks", "--follow-symlinks", "copy", "{}"}); err == nil {
		t.Fatal("duplicate --follow-symlinks was accepted")
	}
}

func TestRejectsRunInputBeforeSideEffects(t *testing.T) {
	tests := [][]string{
		{"run", "--network", "192.0.2.1", "copy", "{}"},
		{"run", "--source", "relative", "--network", "192.0.2.1", "copy", "{}"},
		{"run", "--source", "/", "--network", "192.0.2.1", "copy", "{}"},
		{"run", "--source", "/src", "--network", "192.0.2.1", "--network", "192.0.2.1", "copy", "{}"},
		{"run", "--source", "/src", "--network", "2001:db8::1", "copy", "{}"},
		{"run", "--source", "/src", "--network", "192.0.2.1@0", "copy", "{}"},
		{"run", "--source", "/src", "--network", "192.0.2.1@1", "--auto-weight", "copy", "{}"},
		{"run", "--source", "/src", "--network", "192.0.2.1", "--auto-weight", "copy", "{}"},
		{"run", "--source", "/src", "--network", "192.0.2.1", "--measure-duration", "1s", "copy", "{}"},
		{"run", "--source", "/src", "--network", "192.0.2.1", "--dns", "127.0.0.1", "copy", "{}"},
		{"run", "--source", "/src", "--network", "192.0.2.1", "copy", "prefix{}"},
		{"run", "--source", "/src", "--network", "192.0.2.1", "copy", "{}", "{}"},
	}
	for _, argv := range tests {
		if _, err := Parse(argv); err == nil {
			t.Errorf("Parse(%q) unexpectedly succeeded", argv)
		}
	}
}

func TestListRejectsContradictoryOrUnrequestedDiagnostics(t *testing.T) {
	for _, argv := range [][]string{
		{"list", "--all", "--network", "192.0.2.1"},
		{"list", "--probe-url", "https://example.invalid"},
		{"list", "--measure-url", "https://example.invalid"},
		{"list", "--measure-duration", "1s"},
		{"list", "--network", "192.0.2.1", "--network", "192.0.2.1"},
	} {
		if _, err := Parse(argv); err == nil {
			t.Errorf("Parse(%q) unexpectedly succeeded", argv)
		}
	}
}

func TestRejectsLegacyPublicEntrypoints(t *testing.T) {
	for _, argv := range [][]string{
		{"--list-networks"},
		{"--source", "/src", "--network", "192.0.2.2", "copy", "{}"},
		{"probe"},
		{"measure"},
	} {
		if _, err := Parse(argv); err == nil {
			t.Errorf("Parse(%q) unexpectedly succeeded", argv)
		}
	}
}
