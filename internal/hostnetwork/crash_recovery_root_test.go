//go:build linux && rootintegration

package hostnetwork

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const crashInstructionEnvironment = "TRANSFERLANES_HOSTNETWORK_CRASH_INSTRUCTION"

type crashBoundary string

const (
	crashAfterClaim      crashBoundary = "after-claim"
	crashAfterStrongLink crashBoundary = "after-strong-link"
	crashAfterWeakFIB    crashBoundary = "after-weak-fib"
	crashAfterActivation crashBoundary = "after-activation"
	crashAfterFirewall   crashBoundary = "after-firewall"
)

type crashInstruction struct {
	Boundary   crashBoundary    `json:"boundary"`
	Backend    firewallKind     `json:"backend"`
	Frontend   iptablesFrontend `json:"frontend,omitempty"`
	RunID      string           `json:"run_id"`
	LockPath   string           `json:"lock_path"`
	ReadyPath  string           `json:"ready_path"`
	Selections []crashSelection `json:"selections"`
}

type crashSelection struct {
	Number        int    `json:"number"`
	Source        string `json:"source"`
	ProviderName  string `json:"provider_name"`
	ProviderIndex int    `json:"provider_index"`
	RouteTable    int    `json:"route_table"`
	Gateway       string `json:"gateway"`
}

func TestHostNetworkRecoversAfterRealOwnerDeath(t *testing.T) {
	if encoded := os.Getenv(crashInstructionEnvironment); encoded != "" {
		runCrashChild(t, encoded)
		return
	}
	tests := []struct {
		name       string
		boundary   crashBoundary
		backend    firewallKind
		leavesWeak bool
	}{
		{name: "claim", boundary: crashAfterClaim, backend: firewallIPTables},
		{name: "strong-link", boundary: crashAfterStrongLink, backend: firewallIPTables},
		{name: "unactivated-weak-fib", boundary: crashAfterWeakFIB,
			backend: firewallIPTables, leavesWeak: true},
		{name: "activated", boundary: crashAfterActivation, backend: firewallIPTables},
		{name: "iptables-firewall", boundary: crashAfterFirewall,
			backend: firewallIPTables, leavesWeak: true},
		{name: "nftables-firewall", boundary: crashAfterFirewall,
			backend: firewallNFTables, leavesWeak: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newIPTablesRootFixture(t, iptablesFrontendNFT)
			fixture.install()
			baseline := fixture.surface()
			instruction := newCrashInstruction(t, fixture, test.boundary, test.backend)
			claim := startObserveAndKillCrashChild(t, instruction)
			registerCrashCleanup(t, instruction, claim)
			owner := crashNetworkOwner(instruction)
			directory := rundirectory.New()
			recoverErr := recoverCrashNetworks(directory, owner)
			if test.leavesWeak {
				if recoverErr == nil {
					t.Fatal("unactivated recovery removed receipt-less weak FIB")
				}
				assertCrashFirewallAbsent(t, instruction, claim)
				if err := removeActivatedWeakFIB(netlinkRoutingKernel{}, claim); err != nil {
					t.Fatalf("remove test-owned weak FIB after safe retention: %v", err)
				}
				if err := recoverCrashNetworks(directory, owner); err != nil {
					t.Fatalf("retry stale recovery after test-owned weak FIB removal: %v", err)
				}
			} else if recoverErr != nil {
				t.Fatalf("stale recovery: %v", recoverErr)
			}
			assertCrashRunRemoved(t, instruction, claim)
			fixture.assertSurface(baseline)
		})
	}
}

func newCrashInstruction(t *testing.T, fixture *iptablesRootFixture, boundary crashBoundary,
	backend firewallKind,
) crashInstruction {
	t.Helper()
	id, err := runid.New()
	if err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	instruction := crashInstruction{Boundary: boundary, Backend: backend,
		Frontend: fixture.frontend, RunID: id.String(), LockPath: fixture.lockPath, ReadyPath: ready}
	for _, selection := range fixture.selections[:1] {
		instruction.Selections = append(instruction.Selections, crashSelection{
			Number: int(selection.transfer.Value()), Source: selection.source.String(),
			ProviderName: selection.providerName, ProviderIndex: selection.providerIndex,
			RouteTable: selection.routeTable, Gateway: selection.gateway.String(),
		})
	}
	return instruction
}

func startObserveAndKillCrashChild(t *testing.T, instruction crashInstruction) networkClaim {
	t.Helper()
	encoded, err := json.Marshal(instruction)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestHostNetworkRecoversAfterRealOwnerDeath$")
	command.Env = append(os.Environ(), crashInstructionEnvironment+"="+string(encoded))
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	waited := false
	t.Cleanup(func() {
		if waited {
			return
		}
		_ = command.Process.Kill()
		<-exited
	})
	waitForCrashBoundary(t, instruction.ReadyPath, exited, &output, &waited)
	claim := observeCrashBoundary(t, instruction)
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL crash helper: %v", err)
	}
	if err := <-exited; err == nil {
		t.Fatal("crash helper exited successfully instead of being killed")
	}
	waited = true
	return claim
}

func waitForCrashBoundary(t *testing.T, ready string, exited <-chan error, output *bytes.Buffer,
	waited *bool,
) {
	t.Helper()
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-exited:
			*waited = true
			t.Fatalf("crash helper exited before boundary: %v\n%s", err, output.String())
		case <-ticker.C:
			if contents, err := os.ReadFile(ready); err == nil && string(contents) == "ready\n" {
				return
			}
		case <-deadline.C:
			t.Fatalf("crash helper did not reach boundary\n%s", output.String())
		}
	}
}

func observeCrashBoundary(t *testing.T, instruction crashInstruction) networkClaim {
	t.Helper()
	root, err := os.Open(filepath.Join(rundirectory.AuthorityRoot, instruction.RunID))
	if err != nil {
		t.Fatalf("open crash helper run root: %v", err)
	}
	defer root.Close()
	claim, exists, err := readPublishedClaim(root, mustRunID(t, instruction.RunID))
	if err != nil || !exists {
		t.Fatalf("observe crash helper claim: exists=%t error=%v", exists, err)
	}
	activated, err := observeCrashActivation(root, claim)
	if err != nil {
		t.Fatalf("observe crash helper activation: %v", err)
	}
	if activated != (instruction.Boundary == crashAfterActivation) {
		t.Fatalf("activation at %s = %t", instruction.Boundary, activated)
	}
	switch instruction.Boundary {
	case crashAfterClaim:
		if err := verifyClaimedWeakFIBAbsent(claim); err != nil {
			t.Fatalf("claim boundary has weak FIB: %v", err)
		}
	case crashAfterStrongLink:
		if _, err := ownedHostVeth(claim.transfers[0]); err != nil {
			t.Fatalf("strong-link boundary: %v", err)
		}
		if err := verifyClaimedWeakFIBAbsent(claim); err != nil {
			t.Fatalf("strong-link boundary has weak FIB: %v", err)
		}
	case crashAfterWeakFIB:
		if exists, err := claimedRuleExists(claimedOutboundRule(claim, claim.transfers[0])); err != nil || !exists {
			t.Fatalf("weak-FIB boundary outbound rule: exists=%t error=%v", exists, err)
		}
	case crashAfterFirewall:
		assertCrashFirewallPresent(t, instruction, claim)
	case crashAfterActivation:
		if err := verifyClaimedWeakFIBAbsent(claim); err == nil {
			t.Fatal("activated boundary has no weak FIB")
		}
	}
	return claim
}

func runCrashChild(t *testing.T, encoded string) {
	var instruction crashInstruction
	if err := json.Unmarshal([]byte(encoded), &instruction); err != nil {
		t.Fatal(err)
	}
	selections, err := instruction.egressSelections()
	if err != nil {
		t.Fatal(err)
	}
	directory := rundirectory.New()
	live, err := directory.Create(mustRunID(t, instruction.RunID))
	if err != nil {
		t.Fatal(err)
	}
	owner := crashNetworkOwner(instruction)
	if instruction.Boundary == crashAfterClaim || instruction.Boundary == crashAfterFirewall {
		owner.host = crashBoundaryHost{kernelHost: owner.host, boundary: instruction.Boundary,
			reached: func() { stopCrashChild(instruction.ReadyPath) }}
	}
	if _, err := owner.open(context.Background(), live, selections); err != nil {
		t.Fatal(err)
	}
	if instruction.Boundary != crashAfterActivation {
		t.Fatalf("host-network open passed crash boundary %s", instruction.Boundary)
	}
	stopCrashChild(instruction.ReadyPath)
}

func crashNetworkOwner(instruction crashInstruction) *Owner {
	selections, _ := instruction.egressSelections()
	routing := weakFIBKernel(netlinkRoutingKernel{})
	if instruction.Boundary == crashAfterStrongLink || instruction.Boundary == crashAfterWeakFIB {
		routing = crashBoundaryRouting{weakFIBKernel: routing, boundary: instruction.Boundary,
			reached: func() { stopCrashChild(instruction.ReadyPath) }}
	}
	host := linuxHost{conntrack: netlinkConntrack{}, routing: routing,
		firewalls: crashFirewallBackends(instruction)}
	return newOwner(currentSelections{selections}, host, systemFileSync{}, instruction.LockPath)
}

func crashFirewallBackends(instruction crashInstruction) firewallBackends {
	if instruction.Backend == firewallNFTables {
		return firewallBackends{nftables: nftFirewall{},
			iptables:       unavailableCrashFirewall{kind: firewallIPTables},
			legacyIPTables: staticLegacyIPTablesReader{}}
	}
	runner := systemArgvRunner{}
	tables := procLegacyIPTablesTables{}
	activeRules, activeSave := iptablesNFTRulesCommand, iptablesNFTSaveCommand
	if instruction.Frontend == iptablesFrontendLegacy {
		activeRules, activeSave = iptablesLegacyRulesCommand, iptablesLegacySaveCommand
	}
	iptables := iptablesFirewall{activeRulesCommand: activeRules, activeSaveCommand: activeSave,
		runner: runner, legacyTables: tables}
	return firewallBackends{nftables: unavailableRootNFT{}, iptables: iptables,
		legacyIPTables: fixedLegacyIPTablesReader{runner: runner, tables: tables}}
}

func (instruction crashInstruction) egressSelections() ([]egressSelection, error) {
	result := make([]egressSelection, 0, len(instruction.Selections))
	for _, raw := range instruction.Selections {
		selection, err := selectionFromCrashInstruction(raw)
		if err != nil {
			return nil, err
		}
		result = append(result, selection)
	}
	return orderedSelections(result)
}

func selectionFromCrashInstruction(raw crashSelection) (egressSelection, error) {
	transfer, err := transfernumber.New(raw.Number)
	if err != nil {
		return egressSelection{}, err
	}
	source, err := netip.ParseAddr(raw.Source)
	if err != nil {
		return egressSelection{}, err
	}
	gateway, err := netip.ParseAddr(raw.Gateway)
	if err != nil {
		return egressSelection{}, err
	}
	return egressSelection{transfer: transfer, source: source, providerName: raw.ProviderName,
		providerIndex: raw.ProviderIndex, routeTable: raw.RouteTable, gateway: gateway, hasGateway: true}, nil
}

func stopCrashChild(ready string) {
	if err := os.WriteFile(ready, []byte("ready\n"), 0o600); err != nil {
		panic(err)
	}
	select {}
}

type crashBoundaryRouting struct {
	weakFIBKernel
	boundary crashBoundary
	reached  func()
}

func (routing crashBoundaryRouting) addReturnRoute(claim networkClaim, transfer transferAllocation) error {
	if routing.boundary == crashAfterStrongLink {
		routing.reached()
	}
	return routing.weakFIBKernel.addReturnRoute(claim, transfer)
}

func (routing crashBoundaryRouting) addReturnRule(claim networkClaim, transfer transferAllocation) error {
	if routing.boundary == crashAfterWeakFIB {
		routing.reached()
	}
	return routing.weakFIBKernel.addReturnRule(claim, transfer)
}

type crashBoundaryHost struct {
	kernelHost
	boundary crashBoundary
	reached  func()
}

func (host crashBoundaryHost) Install(ctx context.Context, claim networkClaim) (*hostInstallation, error) {
	if host.boundary == crashAfterClaim {
		host.reached()
	}
	return host.kernelHost.Install(ctx, claim)
}

func (host crashBoundaryHost) Verify(ctx context.Context, claim networkClaim,
	installation *hostInstallation,
) error {
	if host.boundary == crashAfterFirewall {
		host.reached()
	}
	return host.kernelHost.Verify(ctx, claim, installation)
}

type unavailableCrashFirewall struct{ kind firewallKind }

func (firewall unavailableCrashFirewall) Kind() firewallKind { return firewall.kind }

func (firewall unavailableCrashFirewall) ObservePublished(context.Context,
	[]networkClaim,
) ([]bool, error) {
	return nil, errFirewallUnavailable
}
func (unavailableCrashFirewall) Inspect(context.Context) (firewallInventory, error) {
	return firewallInventory{}, errFirewallUnavailable
}
func (unavailableCrashFirewall) Preflight(context.Context, networkClaim) error {
	return errFirewallUnavailable
}
func (unavailableCrashFirewall) Install(context.Context, networkClaim) (firewallReceipts, error) {
	return firewallReceipts{}, errFirewallUnavailable
}
func (unavailableCrashFirewall) Verify(context.Context, networkClaim, bool) error {
	return errFirewallUnavailable
}
func (unavailableCrashFirewall) Remove(context.Context, networkClaim, firewallReceipts) error {
	return errFirewallUnavailable
}
func (unavailableCrashFirewall) Recover(context.Context, networkClaim) error {
	return errFirewallUnavailable
}
func (unavailableCrashFirewall) VerifyAbsent(context.Context, networkClaim) error {
	return errFirewallUnavailable
}

func assertCrashFirewallPresent(t *testing.T, instruction crashInstruction, claim networkClaim) {
	t.Helper()
	backend, err := crashFirewallBackends(instruction).forClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Verify(context.Background(), claim, false); err != nil {
		t.Fatalf("observe crash firewall: %v", err)
	}
}

func assertCrashFirewallAbsent(t *testing.T, instruction crashInstruction, claim networkClaim) {
	t.Helper()
	backend, err := crashFirewallBackends(instruction).forClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.VerifyAbsent(context.Background(), claim); err != nil {
		t.Fatalf("stale recovery did not consume strong firewall marker: %v", err)
	}
}

func assertCrashRunRemoved(t *testing.T, instruction crashInstruction, claim networkClaim) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(rundirectory.AuthorityRoot, instruction.RunID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered run root remains: %v", err)
	}
	if err := verifyClaimedWeakFIBAbsent(claim); err != nil {
		t.Fatalf("recovered weak FIB remains: %v", err)
	}
	for _, transfer := range claim.transfers {
		if err := verifyOwnedVethAbsent(transfer); err != nil {
			t.Fatalf("recovered strong link remains: %v", err)
		}
	}
	assertCrashFirewallAbsent(t, instruction, claim)
}

func observeCrashActivation(root *os.File, claim networkClaim) (bool, error) {
	directory, _, err := openNetworkDirectory(root, false)
	if err != nil {
		return false, err
	}
	defer directory.Close()
	observed, err := inspectEvidenceDirectory(directory)
	if err != nil {
		return false, err
	}
	if observed.activation {
		if err := readActivationFile(directory, claim); err != nil {
			return false, err
		}
	}
	return observed.activation, nil
}

func registerCrashCleanup(t *testing.T, instruction crashInstruction, claim networkClaim) {
	t.Helper()
	t.Cleanup(func() {
		_ = crashFirewallBackends(instruction).Recover(context.Background(), claim)
		_ = removeActivatedWeakFIB(netlinkRoutingKernel{}, claim)
		_ = removeClaimedVeths(context.Background(), claim)
		owner := crashNetworkOwner(instruction)
		_ = recoverCrashNetworks(rundirectory.New(), owner)
	})
}

func recoverCrashNetworks(directory *rundirectory.Owner, owner *Owner) error {
	ctx := context.Background()
	return directory.RecoverStale(ctx, func(stale *rundirectory.StaleRun) error {
		return owner.Recover(ctx, stale)
	})
}
