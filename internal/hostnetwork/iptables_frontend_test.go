package hostnetwork

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyForwardReaderReportsActiveAndInactiveSurfaces(t *testing.T) {
	for _, test := range []struct {
		name   string
		policy string
		active bool
	}{
		{name: "active", policy: "DROP", active: true},
		{name: "inactive", policy: "ACCEPT"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &scriptedArgvRunner{responses: []argvResponse{
				{output: []byte("iptables v1.8.9 (legacy)\n")},
				{output: []byte("iptables-save v1.8.9 (legacy)\n")},
				{output: []byte("*filter\n:FORWARD " + test.policy + " [0:0]\nCOMMIT\n")},
			}}
			facts, err := (fixedLegacyIPTablesReader{runner: runner,
				tables: staticLegacyIPTablesTables{"filter"}}).Forward(context.Background())
			if err != nil || !facts.exists || facts.active != test.active {
				t.Fatalf("legacy FORWARD facts = %#v, %v", facts, err)
			}
		})
	}
}

func TestLegacyIPTablesReaderReportsActiveAndInactivePostrouting(t *testing.T) {
	for _, test := range []struct {
		name   string
		rule   string
		active bool
	}{
		{name: "direct rule", rule: "-A POSTROUTING -j MASQUERADE\n", active: true},
		{name: "unreferenced user chain", rule: ":FOREIGN - [0:0]\n-A FOREIGN -j MASQUERADE\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &scriptedArgvRunner{responses: []argvResponse{
				{output: []byte("iptables v1.8.9 (legacy)\n")},
				{output: []byte("iptables-save v1.8.9 (legacy)\n")},
				{output: []byte("*nat\n:POSTROUTING ACCEPT [0:0]\n" + test.rule + "COMMIT\n")},
			}}
			facts, err := (fixedLegacyIPTablesReader{runner: runner,
				tables: staticLegacyIPTablesTables{"nat"}}).Postrouting(context.Background())
			if err != nil || !facts.exists || facts.active != test.active {
				t.Fatalf("legacy POSTROUTING facts = %#v, %v", facts, err)
			}
		})
	}
}

func TestLegacyIPTablesReaderSkipsCommandsWhenNATTableIsAbsent(t *testing.T) {
	runner := &scriptedArgvRunner{}
	facts, err := (fixedLegacyIPTablesReader{runner: runner,
		tables: staticLegacyIPTablesTables{"filter"}}).Postrouting(context.Background())
	if err != nil || facts != (legacyPostroutingFacts{}) {
		t.Fatalf("legacy POSTROUTING facts = %#v, %v", facts, err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("absent legacy nat table invoked commands: %q", runner.calls)
	}
}

func TestLegacyForwardReaderFailsWhenAnExistingSurfaceCannotBeConfirmed(t *testing.T) {
	for name, responses := range map[string][]argvResponse{
		"filter read": {
			{output: []byte("iptables v1.8.9 (legacy)\n")},
			{output: []byte("iptables-save v1.8.9 (legacy)\n")},
			{err: errors.New("legacy filter read failed")},
		},
		"commands missing": {{err: exec.ErrNotFound}, {err: exec.ErrNotFound}},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &scriptedArgvRunner{responses: responses}
			if _, err := (fixedLegacyIPTablesReader{runner: runner,
				tables: staticLegacyIPTablesTables{"filter"}}).Forward(context.Background()); err == nil {
				t.Fatal("unconfirmed legacy FORWARD surface was accepted")
			}
		})
	}
}

func TestLegacyFrontendInventoryDoesNotRequireNFTCommands(t *testing.T) {
	runner := &scriptedArgvRunner{responses: []argvResponse{
		{output: []byte("iptables v1.8.9 (legacy)\n")},
		{output: []byte("iptables-save v1.8.9 (legacy)\n")},
		{output: []byte("iptables v1.8.9 (legacy)\n")},
		{output: []byte("iptables-save v1.8.9 (legacy)\n")},
		{output: []byte("*filter\n:FORWARD DROP [0:0]\nCOMMIT\n")},
		{output: []byte("*nat\n:POSTROUTING ACCEPT [0:0]\nCOMMIT\n")},
		{}, {}, {},
	}}
	backend := testIPTablesFirewall(runner)
	backend.legacyTables = staticLegacyIPTablesTables{"filter", "nat"}
	if _, err := backend.Inspect(context.Background()); err != nil {
		t.Fatalf("inspect legacy frontend without nft commands: %v", err)
	}
	assertOnlyFixedFrontendCommands(t, runner.calls[2:], iptablesFrontendLegacy)
}

func TestLegacyForwardReaderIgnoresAbsentOrUnrelatedLegacyTables(t *testing.T) {
	for name, tables := range map[string]staticLegacyIPTablesTables{
		"no legacy tables": nil,
		"legacy nat only":  {"nat"},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &scriptedArgvRunner{}
			facts, err := (fixedLegacyIPTablesReader{runner: runner, tables: tables}).Forward(context.Background())
			if err != nil || facts != (legacyForwardFacts{}) {
				t.Fatalf("legacy FORWARD facts = %#v, %v", facts, err)
			}
			if len(runner.calls) != 0 {
				t.Fatalf("unrelated legacy tables invoked commands: %q", runner.calls)
			}
		})
	}
}

func TestLegacyForwardReaderTreatsMissingProcInventoryAsNoLegacySurface(t *testing.T) {
	tables := procLegacyIPTablesTables{path: filepath.Join(t.TempDir(), "missing-ip_tables_names")}
	runner := &scriptedArgvRunner{}
	facts, err := (fixedLegacyIPTablesReader{runner: runner, tables: tables}).Forward(context.Background())
	if err != nil || facts != (legacyForwardFacts{}) {
		t.Fatalf("legacy FORWARD facts = %#v, %v", facts, err)
	}
}

func TestLegacyForwardReaderRejectsNonAbsenceProcErrors(t *testing.T) {
	reader := fixedLegacyIPTablesReader{runner: &scriptedArgvRunner{},
		tables: failingLegacyIPTablesTables{err: errors.New("proc inventory denied")}}
	if _, err := reader.Forward(context.Background()); err == nil {
		t.Fatal("unreadable legacy table inventory was treated as absent")
	}
}

func TestMissingGenericIPTablesCommandsDoNotHideActiveLegacyForward(t *testing.T) {
	runner := &scriptedArgvRunner{responses: []argvResponse{
		{output: []byte("iptables v1.8.9 (legacy)\n")},
		{output: []byte("iptables-save v1.8.9 (legacy)\n")},
		{output: []byte("*filter\n:FORWARD DROP [0:0]\nCOMMIT\n")},
		{err: exec.ErrNotFound}, {err: exec.ErrNotFound},
	}}
	backend := testIPTablesFirewall(runner)
	backend.legacyTables = staticLegacyIPTablesTables{"filter"}
	nft := &recordingFirewall{kind: firewallNFTables,
		inventory: firewallInventory{kind: firewallNFTables}}
	legacy := fixedLegacyIPTablesReader{runner: runner, tables: backend.legacyTables}
	if _, err := (firewallBackends{nftables: nft, iptables: backend,
		legacyIPTables: legacy}).Inspect(context.Background()); err == nil {
		t.Fatal("native nft selection ignored an active legacy FORWARD surface")
	}
	if len(runner.calls) != 5 || runner.calls[0].name != iptablesLegacyRulesCommand ||
		runner.calls[1].name != iptablesLegacySaveCommand || runner.calls[2].name != iptablesLegacySaveCommand ||
		runner.calls[3].name != "iptables" || runner.calls[4].name != "iptables-save" {
		t.Fatalf("legacy FORWARD observation calls = %q", runner.calls)
	}
}

func TestMissingGenericIPTablesCommandsAllowAbsentOrInactiveLegacyForward(t *testing.T) {
	for name, test := range map[string]struct {
		tables    staticLegacyIPTablesTables
		responses []argvResponse
	}{
		"absent": {responses: []argvResponse{{err: exec.ErrNotFound}, {err: exec.ErrNotFound}}},
		"inactive": {tables: staticLegacyIPTablesTables{"filter"}, responses: []argvResponse{
			{output: []byte("iptables v1.8.9 (legacy)\n")},
			{output: []byte("iptables-save v1.8.9 (legacy)\n")},
			{output: []byte("*filter\n:FORWARD ACCEPT [0:0]\nCOMMIT\n")},
			{err: exec.ErrNotFound}, {err: exec.ErrNotFound},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			runner := &scriptedArgvRunner{responses: test.responses}
			backend := testIPTablesFirewall(runner)
			backend.legacyTables = test.tables
			backends := firewallBackends{nftables: &recordingFirewall{kind: firewallNFTables,
				inventory: firewallInventory{kind: firewallNFTables}}, iptables: backend,
				legacyIPTables: fixedLegacyIPTablesReader{runner: runner, tables: test.tables}}
			inventory, err := backends.Inspect(context.Background())
			if err != nil || inventory.kind != firewallNFTables {
				t.Fatalf("native nft selection = %s, %v", inventory.kind, err)
			}
		})
	}
}

func TestIPTablesFrontendRequiresMatchingExplicitIdentity(t *testing.T) {
	for _, test := range []struct {
		name, iptables, save string
		want                 iptablesFrontend
		wantErr              bool
	}{
		{name: "nft", iptables: "iptables v1.8.9 (nf_tables)\n",
			save: "iptables-save v1.8.9 (nf_tables)\n", want: iptablesFrontendNFT},
		{name: "legacy", iptables: "iptables v1.8.9 (legacy)\n",
			save: "iptables-save v1.8.9 (legacy)\n", want: iptablesFrontendLegacy},
		{name: "different frontends", iptables: "iptables v1.8.9 (nf_tables)\n",
			save: "iptables-save v1.8.9 (legacy)\n", wantErr: true},
		{name: "unidentified old output", iptables: "iptables v1.6.0\n",
			save: "iptables-save v1.6.0\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			frontend, err := matchingIPTablesFrontend([]byte(test.iptables), []byte(test.save))
			if (err != nil) != test.wantErr || frontend != test.want {
				t.Fatalf("matchingIPTablesFrontend = %q, %v; want %q error=%v",
					frontend, err, test.want, test.wantErr)
			}
		})
	}
}

func TestFixedIPTablesCommandsAcceptTheirReportedProgramNames(t *testing.T) {
	for _, test := range []struct {
		command string
		output  string
		want    iptablesFrontend
	}{
		{command: iptablesNFTRulesCommand, output: "iptables v1.8.9 (nf_tables)\n", want: iptablesFrontendNFT},
		{command: iptablesNFTSaveCommand, output: "iptables-nft-save v1.8.9 (nf_tables)\n", want: iptablesFrontendNFT},
		{command: iptablesLegacyRulesCommand, output: "iptables v1.8.9 (legacy)\n", want: iptablesFrontendLegacy},
		{command: iptablesLegacySaveCommand, output: "iptables-save v1.8.9 (legacy)\n", want: iptablesFrontendLegacy},
	} {
		t.Run(test.command, func(t *testing.T) {
			actual, err := parseIPTablesRulePlaneFrontend(test.command, []byte(test.output))
			if err != nil || actual != test.want {
				t.Fatalf("parse fixed command identity = %q, %v; want %q", actual, err, test.want)
			}
		})
	}
}

func TestIPTablesInventoryDistinguishesAbsenceFromIncompleteObservation(t *testing.T) {
	missing := exec.ErrNotFound
	for _, test := range []struct {
		name      string
		responses []argvResponse
		absent    bool
	}{
		{name: "both executables absent", responses: []argvResponse{{err: missing}, {err: missing}}, absent: true},
		{name: "only save absent", responses: []argvResponse{
			{output: []byte("iptables v1.8.9 (nf_tables)\n")}, {err: missing}}},
		{name: "frontend mismatch", responses: []argvResponse{
			{output: []byte("iptables v1.8.9 (nf_tables)\n")},
			{output: []byte("iptables-save v1.8.9 (legacy)\n")}}},
		{name: "selected command family absent", responses: []argvResponse{
			{output: []byte("iptables v1.8.9 (nf_tables)\n")},
			{output: []byte("iptables-save v1.8.9 (nf_tables)\n")},
			{err: missing}, {err: missing}}},
		{name: "selected command family has wrong identity", responses: []argvResponse{
			{output: []byte("iptables v1.8.9 (nf_tables)\n")},
			{output: []byte("iptables-save v1.8.9 (nf_tables)\n")},
			{output: []byte("iptables v1.8.9 (legacy)\n")},
			{output: []byte("iptables-save v1.8.9 (legacy)\n")}}},
		{name: "filter read failed", responses: []argvResponse{
			{output: []byte("iptables v1.8.9 (nf_tables)\n")},
			{output: []byte("iptables-save v1.8.9 (nf_tables)\n")},
			{output: []byte("iptables v1.8.9 (nf_tables)\n")},
			{output: []byte("iptables-save v1.8.9 (nf_tables)\n")},
			{err: errors.New("read failed")}}},
		{name: "capability probe failed", responses: []argvResponse{
			{output: []byte("iptables v1.8.9 (nf_tables)\n")},
			{output: []byte("iptables-save v1.8.9 (nf_tables)\n")},
			{output: []byte("iptables v1.8.9 (nf_tables)\n")},
			{output: []byte("iptables-save v1.8.9 (nf_tables)\n")},
			{output: []byte("*filter\n:FORWARD ACCEPT [0:0]\nCOMMIT\n")},
			{output: []byte("*nat\n:POSTROUTING ACCEPT [0:0]\nCOMMIT\n")},
			{err: errors.New("probe failed")}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &scriptedArgvRunner{responses: test.responses}
			_, err := testIPTablesFirewall(runner).Inspect(
				context.Background())
			if err == nil || errors.Is(err, errFirewallUnavailable) != test.absent {
				t.Fatalf("Inspect error = %v, want unavailable=%v", err, test.absent)
			}
		})
	}
}

func TestIPTablesForwardActivityUsesPolicyAndRules(t *testing.T) {
	for _, test := range []struct {
		name    string
		filter  string
		active  bool
		wantErr bool
	}{
		{name: "empty accept", filter: "*filter\n:FORWARD ACCEPT [0:0]\nCOMMIT\n"},
		{name: "drop policy", filter: "*filter\n:FORWARD DROP [0:0]\nCOMMIT\n", active: true},
		{name: "forward rule", filter: "*filter\n:FORWARD ACCEPT [0:0]\n-A FORWARD -j ACCEPT\nCOMMIT\n", active: true},
		{name: "missing declaration", filter: "*filter\n:INPUT ACCEPT [0:0]\nCOMMIT\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			active, err := iptablesForwardActive([]byte(test.filter))
			if (err != nil) != test.wantErr || active != test.active {
				t.Fatalf("iptablesForwardActive = %v, %v; want %v, error=%v",
					active, err, test.active, test.wantErr)
			}
		})
	}
}

func TestIPTablesPostroutingActivityUsesOnlyRules(t *testing.T) {
	for _, test := range []struct {
		name    string
		nat     string
		active  bool
		wantErr bool
	}{
		{name: "empty", nat: "*nat\n:POSTROUTING ACCEPT [0:0]\nCOMMIT\n"},
		{name: "rule", nat: "*nat\n:POSTROUTING ACCEPT [0:0]\n-A POSTROUTING -j MASQUERADE\nCOMMIT\n", active: true},
		{name: "unrelated chain", nat: "*nat\n:POSTROUTING ACCEPT [0:0]\n:CUSTOM - [0:0]\n-A CUSTOM -j RETURN\nCOMMIT\n"},
		{name: "missing declaration", nat: "*nat\n:CUSTOM - [0:0]\nCOMMIT\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			active, err := iptablesPostroutingActive([]byte(test.nat))
			if (err != nil) != test.wantErr || active != test.active {
				t.Fatalf("iptablesPostroutingActive = %v, %v; want %v, error=%v",
					active, err, test.active, test.wantErr)
			}
		})
	}
}

func TestIPTablesSaveChainDeclarationsPreserveOnlyUserChains(t *testing.T) {
	lines, err := parseIPTablesLines([]byte(strings.Join([]string{
		"*filter",
		":FORWARD DROP [0:0]",
		":WRF_example - [0:0]",
		"-A WRF_example -j ACCEPT",
		"COMMIT",
	}, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"-N", "WRF_example"}, {"-A", "WRF_example", "-j", "ACCEPT"}}
	if !reflect.DeepEqual(lines, want) {
		t.Fatalf("parsed iptables-save lines = %q, want %q", lines, want)
	}
}

func TestIPTablesOwnerIdentityIncludesRunAndFullRandomToken(t *testing.T) {
	left := iptablesFixtureClaim(t, "2a112233445566778899aabbccddeeff")
	right := iptablesFixtureClaim(t, "2b112233445566778899aabbccddeeff")
	leftProgram, err := buildIPTablesProgram(left)
	if err != nil {
		t.Fatal(err)
	}
	rightProgram, err := buildIPTablesProgram(right)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(leftProgram.ownerPrefix, left.runID.String()+":"+left.ownerToken) ||
		leftProgram.ownerPrefix == rightProgram.ownerPrefix {
		t.Fatalf("iptables owner identities collide or omit token: %q / %q",
			leftProgram.ownerPrefix, rightProgram.ownerPrefix)
	}
}
