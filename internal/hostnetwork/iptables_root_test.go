//go:build linux && rootintegration

package hostnetwork

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestIPTablesHostNetworkClosesCleanly(t *testing.T) {
	for _, frontend := range []iptablesFrontend{iptablesFrontendNFT, iptablesFrontendLegacy} {
		t.Run(string(frontend), func(t *testing.T) {
			fixture := newIPTablesRootFixture(t, frontend)
			fixture.install()
			baseline := fixture.surface()
			fixture.exerciseNormalClose(baseline)
		})
	}
}

func TestIPTablesHostNetworkSupportsConcurrentSourceRuns(t *testing.T) {
	for _, frontend := range []iptablesFrontend{iptablesFrontendNFT, iptablesFrontendLegacy} {
		t.Run(string(frontend), func(t *testing.T) {
			fixture := newIPTablesRootFixture(t, frontend)
			fixture.install()
			baseline := fixture.surface()
			fixture.exerciseConcurrentSourceRuns(baseline)
		})
	}
}

func TestIPTablesHostNetworkRecoversActivatedRun(t *testing.T) {
	for _, frontend := range []iptablesFrontend{iptablesFrontendNFT, iptablesFrontendLegacy} {
		t.Run(string(frontend), func(t *testing.T) {
			fixture := newIPTablesRootFixture(t, frontend)
			fixture.install()
			baseline := fixture.surface()
			fixture.exerciseStaleActivatedRecovery(baseline)
		})
	}
}

func TestIPTablesHostNetworkRejectsDisabledProviderForwarding(t *testing.T) {
	for _, frontend := range []iptablesFrontend{iptablesFrontendNFT, iptablesFrontendLegacy} {
		t.Run(string(frontend), func(t *testing.T) {
			fixture := newIPTablesRootFixture(t, frontend)
			fixture.install()
			fixture.exerciseProviderForwardingPreflight()
		})
	}
}

func TestIPTablesHostNetworkFailsClosedOnAmbiguousStrongMarker(t *testing.T) {
	fixture := newIPTablesRootFixture(t, iptablesFrontendNFT)
	fixture.install()
	baseline := fixture.surface()
	run := fixture.newRun(t, "6a112233445566778899aabbccddeeff")
	session, err := fixture.owner(netlinkRoutingKernel{}).open(context.Background(), run, fixture.selections)
	if err != nil {
		t.Fatal(err)
	}
	disableCleanup := registerRootSessionCleanup(t, session)
	program, err := buildIPTablesProgram(session.claim)
	if err != nil {
		t.Fatal(err)
	}
	unknownOwner := program.ownerPrefix + "1:unknown"
	unknown := []string{"-w", "5", "-t", "filter", "-I", "FORWARD", "1",
		"-m", "comment", "--comment", unknownOwner, "-j", "DROP"}
	fixture.run(fixture.rulesCommand, unknown...)
	removeUnknown := true
	t.Cleanup(func() {
		if removeUnknown {
			fixture.runCleanup(fixture.rulesCommand, "-w", "5", "-t", "filter", "-D", "FORWARD",
				"-m", "comment", "--comment", unknownOwner, "-j", "DROP")
		}
	})
	if err := session.Close(context.Background()); err == nil {
		t.Fatal("cleanup accepted an unknown rule with the claim owner token")
	}
	evidence, exists, err := loadNetworkEvidence(run.root, run.id, systemFileSync{})
	if err != nil || !exists || !evidence.activated {
		t.Fatalf("ambiguous cleanup evidence = %#v, exists=%v, error=%v", evidence, exists, err)
	}
	fixture.run(fixture.rulesCommand, "-w", "5", "-t", "filter", "-D", "FORWARD",
		"-m", "comment", "--comment", unknownOwner, "-j", "DROP")
	removeUnknown = false
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("cleanup after removing ambiguous foreign rule: %v", err)
	}
	disableCleanup()
	fixture.assertSurface(baseline)
}

func TestHostNetworkRejectsReturnRouteWithUnexpectedRealm(t *testing.T) {
	fixture := newIPTablesRootFixture(t, iptablesFrontendNFT)
	fixture.install()
	baseline := fixture.surface()
	run := newModifiedReturnRouteRun(t, fixture)
	if err := verifyClaimedReturnRoute(run.session.claim,
		run.session.claim.transfers[0], false); err == nil {
		t.Fatal("activation verification accepted a return route with an unexpected realm")
	}
	assertModifiedReturnRoute(t, run.session.claim, run.modified)
	if err := (netlinkRoutingKernel{}).deleteReturnRoute(run.session.claim,
		run.session.claim.transfers[0]); err == nil {
		t.Fatal("cleanup accepted a return route with an unexpected realm")
	}
	assertModifiedReturnRoute(t, run.session.claim, run.modified)
	run.releaseLiveness()
	cleanupErr := run.recover()
	if cleanupErr == nil || !strings.Contains(cleanupErr.Error(),
		"return route identity is ambiguous before deletion") {
		t.Fatalf("stale recovery error = %v, want modified return-route rejection", cleanupErr)
	}
	run.assertActivatedEvidence()
	run.assertOwnerVethAbsent()
	run.assertReturnRouteAbsentAfterVethCascade()
	if err := run.recover(); err != nil {
		t.Fatalf("recover after dependent return-route cascade: %v", err)
	}
	run.assertEvidenceRemoved()
	run.finishRecoveredSession()
	fixture.assertSurface(baseline)
}

type modifiedReturnRouteRun struct {
	t         *testing.T
	directory *rundirectory.Owner
	owner     *Owner
	live      *rundirectory.LiveRun
	session   *Session
	modified  netlink.Route
}

func newModifiedReturnRouteRun(t *testing.T, fixture *iptablesRootFixture) *modifiedReturnRouteRun {
	t.Helper()
	directory, _, err := rundirectory.CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	run := &modifiedReturnRouteRun{t: t, directory: directory,
		owner: fixture.ownerFor(netlinkRoutingKernel{}, fixture.selections[:1])}
	t.Cleanup(run.cleanup)
	run.live, err = directory.Create(mustRunID(t, "68112233445566778899aabbccddeeff"))
	if err != nil {
		t.Fatal(err)
	}
	run.session, err = run.owner.open(context.Background(), run.live, fixture.selections[:1])
	if err != nil {
		t.Fatal(err)
	}
	run.modified = replaceClaimedReturnRouteRealm(t, run.session.claim, 42)
	return run
}

func (run *modifiedReturnRouteRun) releaseLiveness() {
	run.t.Helper()
	if err := run.live.Close(); err != nil {
		run.t.Fatal(err)
	}
	run.live = nil
}

func (run *modifiedReturnRouteRun) recover() error {
	return run.directory.RecoverStale(context.Background(), func(stale *rundirectory.StaleRun) error {
		return run.owner.Recover(context.Background(), stale)
	})
}

func (run *modifiedReturnRouteRun) assertActivatedEvidence() {
	run.t.Helper()
	evidence, exists, err := loadNetworkEvidence(run.session.root,
		run.session.claim.runID, systemFileSync{})
	if err != nil || !exists || !evidence.activated {
		run.t.Fatalf("modified return-route evidence = %#v, exists=%t, error=%v",
			evidence, exists, err)
	}
}

func (run *modifiedReturnRouteRun) assertOwnerVethAbsent() {
	run.t.Helper()
	_, err := netlink.LinkByName(run.session.claim.transfers[0].hostVeth)
	if !isLinkNotFound(err) {
		run.t.Fatalf("owner veth after failed recovery: %v", err)
	}
}

func (run *modifiedReturnRouteRun) assertReturnRouteAbsentAfterVethCascade() {
	run.t.Helper()
	routes, err := routesForReturnPrefix(run.session.claim.returnTable,
		run.session.claim.transfers[0])
	if err != nil || len(routes) != 0 {
		run.t.Fatalf("return route after owner-veth cascade: routes=%#v error=%v", routes, err)
	}
}

func (run *modifiedReturnRouteRun) assertEvidenceRemoved() {
	run.t.Helper()
	evidence, exists, err := loadNetworkEvidence(run.session.root,
		run.session.claim.runID, systemFileSync{})
	if err != nil || exists {
		run.t.Fatalf("recovered return-route evidence = %#v, exists=%t, error=%v",
			evidence, exists, err)
	}
}

func (run *modifiedReturnRouteRun) finishRecoveredSession() {
	closeNamespaceFiles(run.session.namespaces)
	_ = run.session.root.Close()
	run.session = nil
}

func (run *modifiedReturnRouteRun) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if run.session != nil {
		_ = run.session.Close(ctx)
	}
	if run.live != nil {
		_ = run.live.Close()
	}
	_ = run.directory.RecoverStale(ctx, func(stale *rundirectory.StaleRun) error {
		return run.owner.Recover(ctx, stale)
	})
	if run.session != nil {
		closeNamespaceFiles(run.session.namespaces)
		_ = run.session.root.Close()
	}
	if err := run.directory.ClosePrivateAuthority(ctx); err != nil {
		run.t.Errorf("close private run authority: %v", err)
	}
}

func replaceClaimedReturnRouteRealm(t *testing.T, claim networkClaim, realm int) netlink.Route {
	t.Helper()
	transfer := claim.transfers[0]
	routes, err := routesForReturnPrefix(claim.returnTable, transfer)
	if err != nil || len(routes) != 1 {
		t.Fatalf("observe return route before realm change: routes=%#v error=%v", routes, err)
	}
	if err := netlink.RouteDel(&routes[0]); err != nil {
		t.Fatalf("remove plain return route before realm change: %v", err)
	}
	modified := routes[0]
	modified.Realm = realm
	if err := netlink.RouteAdd(&modified); err != nil {
		t.Fatalf("add return route with realm: %v", err)
	}
	assertModifiedReturnRoute(t, claim, modified)
	return modified
}

func assertModifiedReturnRoute(t *testing.T, claim networkClaim, expected netlink.Route) {
	t.Helper()
	routes, err := routesForReturnPrefix(claim.returnTable, claim.transfers[0])
	if err != nil || len(routes) != 1 || !sameReturnRouteIdentity(routes[0], expected) ||
		routes[0].Realm != expected.Realm {
		t.Fatalf("modified return route changed: routes=%#v error=%v", routes, err)
	}
}

func TestWeakFIBUncertainAddsNeverGainRecoveryAuthority(t *testing.T) {
	for _, behavior := range []uncertainAddBehavior{uncertainACK, exactEEXIST} {
		for _, operation := range []string{"return-route", "outbound-rule", "return-rule"} {
			t.Run(string(behavior)+"/"+operation, func(t *testing.T) {
				fixture := newIPTablesRootFixture(t, iptablesFrontendNFT)
				fixture.install()
				baseline := fixture.surface()
				kernel := &uncertainRootRouting{delegate: netlinkRoutingKernel{},
					behavior: behavior, failOperation: operation}
				run := fixture.newRun(t, rootFailureRunID(behavior, operation))
				session, openErr := fixture.ownerFor(kernel, fixture.selections[:1]).open(
					context.Background(), run, fixture.selections[:1])
				if openErr == nil || session != nil {
					t.Fatal("uncertain weak-FIB add returned an activated session")
				}
				if kernel.injectionHits != 1 || !kernel.objectCommitted || kernel.injectedError == nil {
					t.Fatalf("routing uncertainty injection hits=%d committed=%v error=%v",
						kernel.injectionHits, kernel.objectCommitted, kernel.injectedError)
				}
				if !errors.Is(openErr, kernel.injectedError) {
					t.Fatalf("open error %v does not retain injected cause %v", openErr, kernel.injectedError)
				}
				objectExists, err := kernel.failedOperationExists()
				wantExists := operation != "return-route"
				if err != nil || objectExists != wantExists {
					t.Fatalf("injected %s after rollback: exists=%v want=%v error=%v",
						operation, objectExists, wantExists, err)
				}
				if kernel.deletedFailedOperation {
					t.Fatalf("%s without a positive receipt was explicitly deleted", operation)
				}
				evidence, exists, err := loadNetworkEvidence(run.root, run.id, systemFileSync{})
				if err != nil {
					t.Fatal(err)
				}
				if operation == "return-route" {
					if exists {
						t.Fatal("veth-dependent return route did not disappear with its owner graph")
					}
					fixture.assertSurface(baseline)
					return
				}
				if !exists || evidence.activated {
					t.Fatalf("independent uncertain object evidence = %#v, exists=%v", evidence, exists)
				}
				owner := fixture.owner(netlinkRoutingKernel{})
				if err := owner.recoverLocked(context.Background(), run.root, run.id); err == nil {
					t.Fatal("unactivated recovery deleted an independent weak-FIB object")
				}
				afterRecovery, stillExists, err := loadNetworkEvidence(run.root, run.id, systemFileSync{})
				if err != nil || !stillExists || afterRecovery.activated {
					t.Fatalf("recovery inferred TrafficActivated: %#v, exists=%v, error=%v",
						afterRecovery, stillExists, err)
				}
				if err := kernel.deleteFailedOperation(); err != nil {
					t.Fatal(err)
				}
				if err := owner.recoverLocked(context.Background(), run.root, run.id); err != nil {
					t.Fatalf("recover after operator removed ambiguous weak object: %v", err)
				}
				fixture.assertSurface(baseline)
			})
		}
	}
}

func TestRealVethCreationKeepsCompositeMarkerAndUncertainResultsSeparate(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatalf("root integration requires uid 0, got %d", os.Geteuid())
	}
	for _, test := range []struct {
		name      string
		prepare   bool
		commit    bool
		wantExist bool
	}{
		{name: "definite-eexist", prepare: true, wantExist: true},
		{name: "commit-before-ack", commit: true, wantExist: true},
		{name: "socket-failure"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := testAllocation(t, firewallNFTables).transfers[0]
			namespace, err := createAnonymousNamespace()
			if err != nil {
				t.Fatal(err)
			}
			defer namespace.Close()
			defer deleteRootTestLink(value.hostVeth)

			links := systemVethCreation{}
			if test.prepare {
				if err := links.CreateAtomic(value.hostVeth, value.hostMAC, value.peerVeth,
					value.peerMAC, 1500, int(namespace.Fd())); err != nil {
					t.Fatal(err)
				}
			}
			var creation vethCreation = uncertainRootVethCreation{delegate: links, commit: test.commit}
			if test.prepare {
				creation = links
			}
			receipt, err := establishVethPair(creation, value, int(namespace.Fd()))
			if err == nil || receipt != (linkReceipt{}) {
				t.Fatalf("uncertain LinkAdd = receipt %#v, error %v", receipt, err)
			}
			host, observeErr := netlink.LinkByName(value.hostVeth)
			if test.wantExist {
				if observeErr != nil || !exactHostVethMarker(host, value) {
					t.Fatalf("failed LinkAdd changed exact composite marker: host=%#v error=%v", host, observeErr)
				}
				return
			}
			if !isLinkNotFound(observeErr) {
				t.Fatalf("pre-commit failure created host link: %#v, %v", host, observeErr)
			}
		})
	}
}

func TestRealVethStrongMarkerAuthorizesOnlyExactStaleLink(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatalf("root integration requires uid 0, got %d", os.Geteuid())
	}
	for _, test := range []struct {
		name      string
		mutateMAC bool
		wantError bool
	}{
		{name: "exact-marker"},
		{name: "changed-marker", mutateMAC: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := testAllocation(t, firewallNFTables).transfers[0]
			namespace, err := createAnonymousNamespace()
			if err != nil {
				t.Fatal(err)
			}
			defer namespace.Close()
			defer deleteRootTestLink(value.hostVeth)
			links := systemVethCreation{}
			if err := links.CreateAtomic(value.hostVeth, value.hostMAC, value.peerVeth,
				value.peerMAC, 1500, int(namespace.Fd())); err != nil {
				t.Fatal(err)
			}
			host, err := netlink.LinkByName(value.hostVeth)
			if err != nil {
				t.Fatal(err)
			}
			if host.Attrs().Alias != "" {
				t.Fatalf("veth unexpectedly depends on alias %q", host.Attrs().Alias)
			}
			if test.mutateMAC {
				if err := netlink.LinkSetHardwareAddr(host, net.HardwareAddr{0x02, 1, 2, 3, 4, 5}); err != nil {
					t.Fatal(err)
				}
			}
			err = removeOwnedVeth(value)
			if (err != nil) != test.wantError {
				t.Fatalf("stale strong-marker cleanup error = %v, want error=%v", err, test.wantError)
			}
			_, observeErr := netlink.LinkByName(value.hostVeth)
			if test.wantError && observeErr != nil {
				t.Fatalf("ambiguous same-name link was removed: %v", observeErr)
			}
			if !test.wantError && !isLinkNotFound(observeErr) {
				t.Fatalf("exact owner-marked link remains: %v", observeErr)
			}
		})
	}
}

type uncertainRootVethCreation struct {
	delegate systemVethCreation
	commit   bool
}

func (creation uncertainRootVethCreation) CreateAtomic(hostName string, hostMAC linkMAC,
	peerName string, peerMAC linkMAC, mtu, namespaceFD int) error {
	if creation.commit {
		if err := creation.delegate.CreateAtomic(hostName, hostMAC, peerName, peerMAC,
			mtu, namespaceFD); err != nil {
			return err
		}
	}
	return errors.New("injected LinkAdd result uncertainty")
}

func (creation uncertainRootVethCreation) HostLink(name string) (vethIdentity, error) {
	return creation.delegate.HostLink(name)
}

func (creation uncertainRootVethCreation) NamespaceLink(namespaceFD int,
	name string) (vethIdentity, error) {
	return creation.delegate.NamespaceLink(namespaceFD, name)
}

func deleteRootTestLink(name string) {
	if link, err := netlink.LinkByName(name); err == nil {
		_ = netlink.LinkDel(link)
	}
}

type iptablesRootFixture struct {
	t                *testing.T
	frontend         iptablesFrontend
	rulesCommand     string
	saveCommand      string
	prefix           string
	router           string
	providers        []string
	peers            []string
	sources          []netip.Addr
	gateways         []netip.Addr
	tables           []int
	remote           netip.Addr
	server           *http.Server
	selections       []egressSelection
	forwardPolicy    string
	sysctls          map[string][]byte
	lockPath         string
	namespaceNames   string
	originalSurface  string
	defaultRoute     *defaultRouteReceipt
	reversePathRoute *reversePathRouteReceipt
	compatibility    map[string]compatibilityTableBaseline
}

type defaultRouteReceipt struct {
	route netlink.Route
}

type reversePathRouteReceipt struct {
	route netlink.Route
}

type compatibilityTableBaseline struct {
	existed    bool
	introduced bool
	save       string
}

func newIPTablesRootFixture(t *testing.T, frontend iptablesFrontend) *iptablesRootFixture {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatalf("root integration requires uid 0, got %d", os.Geteuid())
	}
	rules, save := iptablesNFTRulesCommand, iptablesNFTSaveCommand
	if frontend == iptablesFrontendLegacy {
		rules, save = iptablesLegacyRulesCommand, iptablesLegacySaveCommand
	}
	for _, command := range []string{"ip", "nft", iptablesNFTRulesCommand, iptablesNFTSaveCommand,
		iptablesLegacyRulesCommand, iptablesLegacySaveCommand} {
		if _, err := exec.LookPath(command); err != nil {
			t.Fatalf("root integration requires %s: %v", command, err)
		}
	}
	sequence := uint64(time.Now().UnixNano()) & 0xfffff
	prefix := fmt.Sprintf("wp%05x", sequence)
	tableStart := 42000 + int(sequence&0x3ff)*2
	lockDirectory := t.TempDir()
	if err := os.Chmod(lockDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	return &iptablesRootFixture{t: t, frontend: frontend, rulesCommand: rules, saveCommand: save,
		prefix: prefix, router: prefix + "r", providers: []string{prefix + "a", prefix + "b"},
		peers: []string{prefix + "x", prefix + "y"},
		sources: []netip.Addr{netip.MustParseAddr("10.231.1.2"), netip.MustParseAddr("10.231.1.3"),
			netip.MustParseAddr("10.232.1.2")},
		gateways: []netip.Addr{netip.MustParseAddr("10.231.1.1"), netip.MustParseAddr("10.232.1.1")},
		tables:   []int{tableStart, tableStart + 1}, remote: netip.MustParseAddr("203.0.113.210"),
		lockPath: filepath.Join(lockDirectory, "host-network.lock")}
}

func (fixture *iptablesRootFixture) install() {
	fixture.t.Helper()
	fixture.originalSurface = fixture.surface()
	fixture.t.Cleanup(func() { fixture.assertSurface(fixture.originalSurface) })
	fixture.t.Cleanup(fixture.remove)
	fixture.snapshotCompatibilityTables()
	fixture.snapshotSysctls()
	fixture.forwardPolicy = fixture.readForwardPolicy()
	fixture.namespaceNames = fixture.run("ip", "netns", "list")
	fixture.run(fixture.rulesCommand, "-w", "5", "-t", "filter", "-P", "FORWARD", "DROP")
	fixture.run("ip", "netns", "add", fixture.router)
	fixture.run("ip", "-n", fixture.router, "link", "set", "lo", "up")
	fixture.run("ip", "-n", fixture.router, "addr", "add", fixture.remote.String()+"/32", "dev", "lo")
	for index := range fixture.providers {
		fixture.installProvider(index)
	}
	fixture.installDefaultReachabilityRoute()
	fixture.installRemoteReversePathRoute()
	fixture.configureForwarding()
	fixture.installForeignRules()
	fixture.recordIntroducedCompatibilityTables()
	fixture.startServer()
	fixture.selections = fixture.currentSelections()
}

func (fixture *iptablesRootFixture) recordIntroducedCompatibilityTables() {
	if fixture.frontend != iptablesFrontendNFT {
		return
	}
	for _, table := range []string{"filter", "nat"} {
		baseline := fixture.compatibility[table]
		if baseline.existed {
			continue
		}
		if err := exec.Command("nft", "list", "table", "ip", table).Run(); err != nil {
			fixture.t.Fatalf("iptables-nft did not create expected %s table: %v", table, err)
		}
		baseline.introduced = true
		fixture.compatibility[table] = baseline
	}
}

func (fixture *iptablesRootFixture) snapshotCompatibilityTables() {
	fixture.compatibility = make(map[string]compatibilityTableBaseline, 2)
	for _, table := range []string{"filter", "nat"} {
		command := exec.Command("nft", "list", "table", "ip", table)
		err := command.Run()
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				fixture.t.Fatalf("inspect iptables-nft compatibility table %s: %v", table, err)
			}
		}
		fixture.compatibility[table] = compatibilityTableBaseline{
			existed: err == nil,
			save:    stableIPTablesSave(fixture.run(iptablesNFTSaveCommand, "-t", table)),
		}
	}
}

func (fixture *iptablesRootFixture) installProvider(index int) {
	provider, peer := fixture.providers[index], fixture.peers[index]
	fixture.run("ip", "link", "add", provider, "type", "veth", "peer", "name", peer)
	fixture.run("ip", "link", "set", peer, "netns", fixture.router)
	fixture.writeSysctl("/proc/sys/net/ipv6/conf/"+provider+"/disable_ipv6", "1\n")
	fixture.run("ip", "link", "set", provider, "up")
	fixture.run("ip", "-n", fixture.router, "link", "set", peer, "up")
	fixture.run("ip", "-n", fixture.router, "addr", "add", fixture.gateways[index].String()+"/24", "dev", peer)
	if index == 0 {
		fixture.run("ip", "addr", "add", fixture.sources[0].String()+"/24", "dev", provider)
		fixture.run("ip", "addr", "add", fixture.sources[1].String()+"/24", "dev", provider)
	} else {
		fixture.run("ip", "addr", "add", fixture.sources[2].String()+"/24", "dev", provider)
	}
	fixture.run("ip", "route", "add", "table", strconv.Itoa(fixture.tables[index]), "default",
		"via", fixture.gateways[index].String(), "dev", provider)
}

func (fixture *iptablesRootFixture) installRemoteReversePathRoute() {
	fixture.t.Helper()
	provider, err := netlink.LinkByName(fixture.providers[0])
	if err != nil {
		fixture.t.Fatalf("open reverse-path provider: %v", err)
	}
	destination := netip.PrefixFrom(fixture.remote, 32)
	filter := netlink.Route{Table: unix.RT_TABLE_MAIN, Dst: ipNet(destination)}
	existing, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &filter,
		netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
	if err != nil {
		fixture.t.Fatalf("inspect remote reverse-path route: %v", err)
	}
	if len(existing) != 0 {
		fixture.t.Fatalf("remote reverse-path prefix already has %d route(s): %#v", len(existing), existing)
	}
	route := netlink.Route{LinkIndex: provider.Attrs().Index, Dst: ipNet(destination),
		Gw: net.IP(fixture.gateways[0].AsSlice()), Table: unix.RT_TABLE_MAIN,
		Protocol: unix.RTPROT_STATIC, Scope: netlink.SCOPE_UNIVERSE, Type: unix.RTN_UNICAST}
	if err := netlink.RouteAdd(&route); err != nil {
		fixture.t.Fatalf("add remote reverse-path route: %v", err)
	}
	fixture.reversePathRoute = &reversePathRouteReceipt{route: route}
	fixture.assertRemoteReversePathRoute()
}

func (fixture *iptablesRootFixture) installDefaultReachabilityRoute() {
	fixture.t.Helper()
	routes, err := fixture.mainDefaultRoutes()
	if err != nil {
		fixture.t.Fatal(err)
	}
	if len(routes) != 0 {
		return
	}
	provider, err := netlink.LinkByName(fixture.providers[0])
	if err != nil {
		fixture.t.Fatalf("open default-route provider: %v", err)
	}
	route := netlink.Route{LinkIndex: provider.Attrs().Index,
		Gw: net.IP(fixture.gateways[0].AsSlice()), Table: unix.RT_TABLE_MAIN,
		Protocol: unix.RTPROT_STATIC, Scope: netlink.SCOPE_UNIVERSE, Type: unix.RTN_UNICAST}
	if err := netlink.RouteAdd(&route); err != nil {
		fixture.t.Fatalf("add default reachability route: %v", err)
	}
	fixture.defaultRoute = &defaultRouteReceipt{route: route}
	routes, err = fixture.mainDefaultRoutes()
	if err != nil || len(routes) != 1 || !exactFixtureDefaultRoute(routes[0], route) {
		fixture.t.Fatalf("default reachability route identity mismatch: %#v, %v", routes, err)
	}
}

func (fixture *iptablesRootFixture) mainDefaultRoutes() ([]netlink.Route, error) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		return nil, fmt.Errorf("list main-table default routes: %w", err)
	}
	defaults := make([]netlink.Route, 0, 1)
	for _, route := range routes {
		if isDefaultRoute(route.Dst) {
			defaults = append(defaults, route)
		}
	}
	return defaults, nil
}

func exactFixtureDefaultRoute(actual, expected netlink.Route) bool {
	return actual.Table == expected.Table && actual.LinkIndex == expected.LinkIndex &&
		actual.Type == unix.RTN_UNICAST && actual.Scope == netlink.SCOPE_UNIVERSE &&
		actual.Protocol == expected.Protocol && isDefaultRoute(actual.Dst) && isDefaultRoute(expected.Dst) &&
		net.IP(actual.Gw).Equal(expected.Gw) && actual.Src == nil && actual.Priority == 0
}

func (fixture *iptablesRootFixture) assertRemoteReversePathRoute() {
	fixture.t.Helper()
	if fixture.reversePathRoute == nil {
		fixture.t.Fatal("remote reverse-path route has no successful add receipt")
	}
	routes, err := fixture.remoteReversePathRoutes()
	if err != nil {
		fixture.t.Fatal(err)
	}
	if len(routes) != 1 || !exactFixtureReversePathRoute(routes[0], fixture.reversePathRoute.route) {
		fixture.t.Fatalf("remote reverse-path route identity mismatch: %#v", routes)
	}
}

func (fixture *iptablesRootFixture) remoteReversePathRoutes() ([]netlink.Route, error) {
	destination := netip.PrefixFrom(fixture.remote, 32)
	filter := netlink.Route{Table: unix.RT_TABLE_MAIN, Dst: ipNet(destination)}
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4, &filter,
		netlink.RT_FILTER_TABLE|netlink.RT_FILTER_DST)
	if err != nil {
		return nil, fmt.Errorf("list remote reverse-path route: %w", err)
	}
	return routes, nil
}

func exactFixtureReversePathRoute(actual, expected netlink.Route) bool {
	return actual.Table == expected.Table && actual.LinkIndex == expected.LinkIndex &&
		actual.Type == unix.RTN_UNICAST && actual.Scope == netlink.SCOPE_UNIVERSE &&
		actual.Protocol == expected.Protocol && equalIPNet(actual.Dst, expected.Dst) &&
		net.IP(actual.Gw).Equal(expected.Gw) && actual.Src == nil && actual.Priority == 0
}

func (fixture *iptablesRootFixture) configureForwarding() {
	fixture.writeSysctl("/proc/sys/net/ipv4/ip_forward", "1\n")
	fixture.writeSysctl("/proc/sys/net/ipv4/conf/all/rp_filter", "2\n")
	fixture.writeSysctl("/proc/sys/net/ipv4/conf/default/rp_filter", "2\n")
	fixture.writeSysctl("/proc/sys/net/ipv4/conf/all/src_valid_mark", "0\n")
	fixture.writeSysctl("/proc/sys/net/ipv4/conf/default/src_valid_mark", "0\n")
	for _, provider := range fixture.providers {
		fixture.writeSysctl("/proc/sys/net/ipv4/conf/"+provider+"/forwarding", "1\n")
		fixture.writeSysctl("/proc/sys/net/ipv4/conf/"+provider+"/rp_filter", "2\n")
	}
}

func (fixture *iptablesRootFixture) installForeignRules() {
	fixture.run(fixture.rulesCommand, "-w", "5", "-t", "filter", "-A", "FORWARD",
		"-s", "203.0.113.254/32", "-j", "DROP")
	fixture.run(fixture.rulesCommand, "-w", "5", "-t", "nat", "-A", "POSTROUTING",
		"-s", "203.0.113.254/32", "-j", "MASQUERADE")
}

func (fixture *iptablesRootFixture) currentSelections() []egressSelection {
	result := make([]egressSelection, len(fixture.sources))
	for index, source := range fixture.sources {
		providerIndex := min(index, 1)
		if index == 1 {
			providerIndex = 0
		}
		link, err := netlink.LinkByName(fixture.providers[providerIndex])
		if err != nil {
			fixture.t.Fatal(err)
		}
		number, _ := transfernumber.New(index + 1)
		result[index] = egressSelection{transfer: number, source: source,
			providerName: fixture.providers[providerIndex], providerIndex: link.Attrs().Index,
			routeTable: fixture.tables[providerIndex], gateway: fixture.gateways[providerIndex], hasGateway: true}
	}
	return result
}

func (fixture *iptablesRootFixture) owner(routing weakFIBKernel) *Owner {
	return fixture.ownerFor(routing, fixture.selections)
}

func (fixture *iptablesRootFixture) ownerFor(routing weakFIBKernel,
	selections []egressSelection) *Owner {
	runner := systemArgvRunner{}
	tables := procLegacyIPTablesTables{}
	iptables := iptablesFirewall{activeRulesCommand: fixture.rulesCommand,
		activeSaveCommand: fixture.saveCommand, runner: runner, legacyTables: tables}
	host := linuxHost{conntrack: netlinkConntrack{}, routing: routing, firewalls: firewallBackends{
		nftables: unavailableRootNFT{}, iptables: iptables,
		legacyIPTables: fixedLegacyIPTablesReader{runner: runner, tables: tables}}}
	return newOwner(currentSelections{selections}, host, systemFileSync{}, fixture.lockPath)
}

func (fixture *iptablesRootFixture) newRun(t *testing.T, value string) testRun {
	return testRun{id: mustRunID(t, value), root: openTestRunRoot(t)}
}

func (fixture *iptablesRootFixture) exerciseNormalClose(baseline string) {
	run := fixture.newRun(fixture.t, "61112233445566778899aabbccddeeff")
	session, err := fixture.owner(netlinkRoutingKernel{}).open(context.Background(), run, fixture.selections)
	if err != nil {
		fixture.t.Fatalf("open iptables host network: %v", err)
	}
	disableCleanup := registerRootSessionCleanup(fixture.t, session)
	fixture.assertActivated(run)
	fixture.assertNoNamedTransferLanesNamespace()
	fixture.assertDirectRules(session.claim)
	fixture.assertNoMainTemporaryRoutes(session.claim)
	fixture.assertClaimedRouteLookups(session.claim)
	fixture.assertTraffic(session)
	if err := session.Close(context.Background()); err != nil {
		fixture.t.Fatalf("close iptables host network: %v", err)
	}
	disableCleanup()
	assertNoNetworkEvidence(fixture.t, run.root)
	fixture.assertSurface(baseline)
}

func registerRootSessionCleanup(t *testing.T, session *Session) func() {
	t.Helper()
	enabled := true
	t.Cleanup(func() {
		if !enabled {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := session.Close(ctx); err != nil {
			t.Errorf("close successful root-test network session: %v", err)
		}
	})
	return func() { enabled = false }
}

func (fixture *iptablesRootFixture) assertClaimedRouteLookups(claim networkClaim) {
	fixture.t.Helper()
	for _, transfer := range claim.transfers {
		if err := verifyOutboundRoute(transfer); err != nil {
			fixture.t.Fatal(err)
		}
		if err := verifyReturnPath(claim.returnTable, transfer); err != nil {
			fixture.t.Fatal(err)
		}
	}
}

func (fixture *iptablesRootFixture) exerciseConcurrentSourceRuns(baseline string) {
	directory, _, err := rundirectory.CreatePrivateAuthority()
	if err != nil {
		fixture.t.Fatal(err)
	}
	owner := fixture.ownerFor(netlinkRoutingKernel{}, fixture.selections[:2])
	firstRun, err := directory.Create(mustRunID(fixture.t, "62112233445566778899aabbccddeeff"))
	if err != nil {
		_ = directory.ClosePrivateAuthority(context.Background())
		fixture.t.Fatal(err)
	}
	secondRun, err := directory.Create(mustRunID(fixture.t, "63112233445566778899aabbccddeeff"))
	if err != nil {
		_ = closePublishedIPTablesRuns(context.Background(), directory, owner, firstRun)
		fixture.t.Fatal(err)
	}
	var first, second *Session
	cleanupEnabled := true
	fixture.t.Cleanup(func() {
		if !cleanupEnabled {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var cleanupErrors []error
		if second != nil {
			cleanupErrors = append(cleanupErrors, second.Close(ctx))
		}
		if first != nil {
			cleanupErrors = append(cleanupErrors, first.Close(ctx))
		}
		cleanupErrors = append(cleanupErrors,
			closePublishedIPTablesRuns(ctx, directory, owner, secondRun, firstRun))
		if err := errors.Join(cleanupErrors...); err != nil {
			fixture.t.Errorf("clean published concurrent iptables runs: %v", err)
		}
	})
	first, err = owner.open(context.Background(), firstRun, fixture.selections[:2])
	if err != nil {
		fixture.t.Fatal(err)
	}
	second, err = owner.open(context.Background(), secondRun, fixture.selections[:2])
	if err != nil {
		fixture.t.Fatal(err)
	}
	fixture.assertTraffic(first)
	fixture.assertTraffic(second)
	if first.claim.transfers[0].subnet == second.claim.transfers[0].subnet {
		fixture.t.Fatal("concurrent same-source runs reused a temporary subnet")
	}
	if err := first.Close(context.Background()); err != nil {
		fixture.t.Fatal(err)
	}
	first = nil
	fixture.assertTraffic(second)
	if err := second.Close(context.Background()); err != nil {
		fixture.t.Fatal(err)
	}
	second = nil
	if err := closePublishedIPTablesRuns(context.Background(), directory, owner,
		secondRun, firstRun); err != nil {
		fixture.t.Fatalf("close published concurrent iptables runs: %v", err)
	}
	cleanupEnabled = false
	fixture.assertSurface(baseline)
}

func closePublishedIPTablesRuns(ctx context.Context, directory *rundirectory.Owner,
	networkOwner *Owner, runs ...*rundirectory.LiveRun,
) error {
	var cleanupErrors []error
	for _, run := range runs {
		cleanupErrors = append(cleanupErrors, run.Close())
	}
	cleanupErrors = append(cleanupErrors, directory.RecoverStale(ctx,
		func(stale *rundirectory.StaleRun) error {
			return networkOwner.Recover(ctx, stale)
		}), directory.ClosePrivateAuthority(ctx))
	return errors.Join(cleanupErrors...)
}

func (fixture *iptablesRootFixture) exerciseStaleActivatedRecovery(baseline string) {
	run := fixture.newRun(fixture.t, "64112233445566778899aabbccddeeff")
	owner := fixture.owner(netlinkRoutingKernel{})
	session, err := owner.open(context.Background(), run, fixture.selections)
	if err != nil {
		fixture.t.Fatal(err)
	}
	disableCleanup := registerRootSessionCleanup(fixture.t, session)
	fixture.assertTraffic(session)
	if err := owner.recoverLocked(context.Background(), run.root, run.id); err != nil {
		fixture.t.Fatalf("recover activated iptables host network: %v", err)
	}
	disableCleanup()
	closeNamespaceFiles(session.namespaces)
	_ = session.root.Close()
	assertNoNetworkEvidence(fixture.t, run.root)
	fixture.assertSurface(baseline)
}

func (fixture *iptablesRootFixture) exerciseProviderForwardingPreflight() {
	path := "/proc/sys/net/ipv4/conf/" + fixture.providers[0] + "/forwarding"
	fixture.writeSysctl(path, "0\n")
	defer fixture.writeSysctl(path, "1\n")
	baseline := fixture.surface()
	run := fixture.newRun(fixture.t, "65112233445566778899aabbccddeeff")
	session, err := fixture.owner(netlinkRoutingKernel{}).open(context.Background(), run, fixture.selections)
	if err == nil || session != nil {
		fixture.t.Fatal("provider forwarding=0 passed the zero-mutation preflight")
	}
	assertNoNetworkEvidence(fixture.t, run.root)
	fixture.assertSurface(baseline)
}

func (fixture *iptablesRootFixture) assertActivated(run testRun) {
	evidence, exists, err := loadNetworkEvidence(run.root, run.id, systemFileSync{})
	if err != nil || !exists || !evidence.activated {
		fixture.t.Fatalf("live network evidence = %#v, exists=%v, error=%v", evidence, exists, err)
	}
}

func (fixture *iptablesRootFixture) assertNoNamedTransferLanesNamespace() {
	if actual := fixture.run("ip", "netns", "list"); !sameNamespaceListing(actual, fixture.namespaceNames, fixture.router) {
		fixture.t.Fatalf("Transfer Lanes created a named namespace:\nbefore:\n%s\nafter:\n%s",
			fixture.namespaceNames, actual)
	}
}

func sameNamespaceListing(actual, before, added string) bool {
	want := namespaceListingNames(before)
	want = append(want, added)
	got := namespaceListingNames(actual)
	slices.Sort(want)
	slices.Sort(got)
	return slices.Equal(got, want)
}

func namespaceListingNames(output string) []string {
	var names []string
	for _, raw := range strings.Split(output, "\n") {
		if fields := strings.Fields(raw); len(fields) > 0 {
			names = append(names, fields[0])
		}
	}
	return names
}

func (fixture *iptablesRootFixture) assertDirectRules(claim networkClaim) {
	program, err := buildIPTablesProgram(claim)
	if err != nil {
		fixture.t.Fatal(err)
	}
	owned := fixture.ownedRules(program.ownerPrefix)
	if len(owned) != len(program.rules) {
		fixture.t.Fatalf("owned direct rule count = %d, want %d:\n%q", len(owned), len(program.rules), owned)
	}
	for _, rule := range owned {
		if len(rule) < 2 || rule[0] != "-A" || rule[1] != "FORWARD" && rule[1] != "POSTROUTING" ||
			!strings.HasPrefix(iptablesOption(rule[2:], "--comment"), program.ownerPrefix) {
			fixture.t.Fatalf("owned rule is not a direct full-token rule: %q", rule)
		}
	}
}

func (fixture *iptablesRootFixture) ownedRules(ownerPrefix string) [][]string {
	var result [][]string
	for _, table := range []string{"filter", "nat"} {
		lines, err := parseIPTablesLines([]byte(fixture.run(fixture.saveCommand, "-t", table)))
		if err != nil {
			fixture.t.Fatal(err)
		}
		for _, line := range lines {
			if len(line) >= 3 && strings.HasPrefix(iptablesOption(line[2:], "--comment"), ownerPrefix) {
				result = append(result, line)
			}
		}
	}
	return result
}

func (fixture *iptablesRootFixture) assertNoMainTemporaryRoutes(claim networkClaim) {
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V4,
		&netlink.Route{Table: unix.RT_TABLE_MAIN}, netlink.RT_FILTER_TABLE)
	if err != nil {
		fixture.t.Fatal(err)
	}
	for _, route := range routes {
		prefix, valid := prefixFromIPNet(route.Dst)
		if !valid {
			continue
		}
		for _, transfer := range claim.transfers {
			if prefix == transfer.subnet {
				fixture.t.Fatalf("main table contains a temporary-prefix route: %s", prefix)
			}
		}
	}
}

func (fixture *iptablesRootFixture) assertTraffic(session *Session) {
	for index := range session.claim.transfers {
		number, _ := transfernumber.New(index + 1)
		lease, err := session.OpenNamespace(number)
		if err != nil {
			fixture.t.Fatal(err)
		}
		connection, connectErr := openNamespaceTCP(lease.Descriptor(), fixture.remote, 18082)
		closeErr := lease.Close()
		if connectErr != nil || closeErr != nil {
			fixture.t.Fatalf("transfer %d connect/lease close: %v / %v\n%s", index+1,
				connectErr, closeErr, fixture.trafficFailureFacts(session.claim.transfers[index]))
		}
		observed, err := httpSource(connection, fixture.remote)
		_ = connection.Close()
		if err != nil || observed != session.claim.transfers[index].source.String() {
			fixture.t.Fatalf("transfer %d observed source = %q, %v; want %s",
				index+1, observed, err, session.claim.transfers[index].source)
		}
	}
}

func (fixture *iptablesRootFixture) trafficFailureFacts(transfer transferAllocation) string {
	commands := [][]string{
		{"ip", "-4", "route", "get", transfer.namespaceIP.String()},
		{"ip", "-4", "route", "get", fixture.remote.String()},
		{"ip", "-n", fixture.router, "-4", "route", "get", transfer.source.String()},
		{"sysctl", "net.ipv4.conf." + transfer.hostVeth + ".rp_filter"},
		{fixture.rulesCommand, "-w", "5", "-t", "filter", "-nvL", "FORWARD"},
		{fixture.rulesCommand, "-w", "5", "-t", "nat", "-nvL", "POSTROUTING"},
	}
	var facts strings.Builder
	for _, command := range commands {
		output, err := exec.Command(command[0], command[1:]...).CombinedOutput()
		fmt.Fprintf(&facts, "$ %s %s\n%s(error: %v)\n",
			command[0], strings.Join(command[1:], " "), output, err)
	}
	return facts.String()
}

type unavailableRootNFT struct{}

func (unavailableRootNFT) Kind() firewallKind { return firewallNFTables }

func (unavailableRootNFT) ObservePublished(context.Context, []networkClaim) ([]bool, error) {
	return nil, errFirewallUnavailable
}
func (unavailableRootNFT) Inspect(context.Context) (firewallInventory, error) {
	return firewallInventory{}, errFirewallUnavailable
}
func (unavailableRootNFT) Preflight(context.Context, networkClaim) error {
	return errFirewallUnavailable
}
func (unavailableRootNFT) Install(context.Context, networkClaim) (firewallReceipts, error) {
	return firewallReceipts{}, errors.New("unexpected nftables install")
}
func (unavailableRootNFT) Verify(context.Context, networkClaim, bool) error {
	return errors.New("unexpected nftables verify")
}
func (unavailableRootNFT) Remove(context.Context, networkClaim, firewallReceipts) error {
	return errors.New("unexpected nftables remove")
}
func (unavailableRootNFT) Recover(context.Context, networkClaim) error {
	return errors.New("unexpected nftables recovery")
}
func (unavailableRootNFT) VerifyAbsent(context.Context, networkClaim) error {
	return errors.New("unexpected nftables absence check")
}

type uncertainAddBehavior string

const (
	uncertainACK uncertainAddBehavior = "ack-loss"
	exactEEXIST  uncertainAddBehavior = "exact-eexist"
)

type uncertainRootRouting struct {
	delegate               netlinkRoutingKernel
	behavior               uncertainAddBehavior
	failOperation          string
	failed                 bool
	injectionHits          int
	objectCommitted        bool
	injectedError          error
	deletedFailedOperation bool
	claim                  networkClaim
	transfer               transferAllocation
}

func (kernel *uncertainRootRouting) addReturnRoute(claim networkClaim, transfer transferAllocation) error {
	return kernel.add("return-route", claim, transfer, kernel.delegate.addReturnRoute)
}

func (kernel *uncertainRootRouting) addOutboundRule(claim networkClaim, transfer transferAllocation) error {
	return kernel.add("outbound-rule", claim, transfer, kernel.delegate.addOutboundRule)
}

func (kernel *uncertainRootRouting) addReturnRule(claim networkClaim, transfer transferAllocation) error {
	return kernel.add("return-rule", claim, transfer, kernel.delegate.addReturnRule)
}

func (kernel *uncertainRootRouting) add(operation string, claim networkClaim, transfer transferAllocation,
	create func(networkClaim, transferAllocation) error,
) error {
	if kernel.failed || kernel.failOperation != operation {
		return create(claim, transfer)
	}
	kernel.injectionHits++
	if err := create(claim, transfer); err != nil {
		return err
	}
	kernel.failed, kernel.claim, kernel.transfer = true, claim, transfer
	if err := kernel.observeFailedOperation(); err != nil {
		return fmt.Errorf("observe injected routing commit: %w", err)
	}
	kernel.objectCommitted = true
	if kernel.behavior == exactEEXIST {
		kernel.injectedError = create(claim, transfer)
		return kernel.injectedError
	}
	kernel.injectedError = errors.New("injected netlink ACK uncertainty after kernel commit")
	return kernel.injectedError
}

func (kernel *uncertainRootRouting) observeFailedOperation() error {
	switch kernel.failOperation {
	case "return-route":
		return verifyClaimedReturnRoute(kernel.claim, kernel.transfer, false)
	case "outbound-rule":
		return verifyClaimedRule(claimedOutboundRule(kernel.claim, kernel.transfer), false)
	case "return-rule":
		return verifyClaimedRule(claimedReturnRule(kernel.claim, kernel.transfer), false)
	default:
		return fmt.Errorf("unknown failed routing operation %q", kernel.failOperation)
	}
}

func (kernel *uncertainRootRouting) failedOperationExists() (bool, error) {
	switch kernel.failOperation {
	case "return-route":
		routes, err := routesForReturnPrefix(kernel.claim.returnTable, kernel.transfer)
		return len(routes) != 0, err
	case "outbound-rule":
		return claimedRuleExists(claimedOutboundRule(kernel.claim, kernel.transfer))
	case "return-rule":
		return claimedRuleExists(claimedReturnRule(kernel.claim, kernel.transfer))
	default:
		return false, fmt.Errorf("unknown failed routing operation %q", kernel.failOperation)
	}
}

func claimedRuleExists(expected netlink.Rule) (bool, error) {
	rules, err := netlink.RuleList(netlink.FAMILY_V4)
	if err != nil {
		return false, err
	}
	for _, rule := range rules {
		if rule.Priority == expected.Priority && exactPolicyRuleWithProtocol(rule, expected) {
			return true, nil
		}
	}
	return false, nil
}

func (kernel *uncertainRootRouting) deleteReturnRoute(claim networkClaim, transfer transferAllocation) error {
	if kernel.failed && kernel.failOperation == "return-route" && transfer.transfer == kernel.transfer.transfer {
		kernel.deletedFailedOperation = true
	}
	return kernel.delegate.deleteReturnRoute(claim, transfer)
}

func (kernel *uncertainRootRouting) deleteOutboundRule(claim networkClaim, transfer transferAllocation) error {
	if kernel.failed && kernel.failOperation == "outbound-rule" && transfer.transfer == kernel.transfer.transfer {
		kernel.deletedFailedOperation = true
	}
	return kernel.delegate.deleteOutboundRule(claim, transfer)
}

func (kernel *uncertainRootRouting) deleteReturnRule(claim networkClaim, transfer transferAllocation) error {
	if kernel.failed && kernel.failOperation == "return-rule" && transfer.transfer == kernel.transfer.transfer {
		kernel.deletedFailedOperation = true
	}
	return kernel.delegate.deleteReturnRule(claim, transfer)
}

func (kernel *uncertainRootRouting) deleteFailedOperation() error {
	switch kernel.failOperation {
	case "return-route":
		return kernel.delegate.deleteReturnRoute(kernel.claim, kernel.transfer)
	case "outbound-rule":
		return kernel.delegate.deleteOutboundRule(kernel.claim, kernel.transfer)
	case "return-rule":
		return kernel.delegate.deleteReturnRule(kernel.claim, kernel.transfer)
	default:
		return fmt.Errorf("unknown failed routing operation %q", kernel.failOperation)
	}
}

func rootFailureRunID(behavior uncertainAddBehavior, operation string) string {
	identities := map[string]string{
		string(uncertainACK) + "/return-route":  "71112233445566778899aabbccddeeff",
		string(uncertainACK) + "/outbound-rule": "72112233445566778899aabbccddeeff",
		string(uncertainACK) + "/return-rule":   "73112233445566778899aabbccddeeff",
		string(exactEEXIST) + "/return-route":   "74112233445566778899aabbccddeeff",
		string(exactEEXIST) + "/outbound-rule":  "75112233445566778899aabbccddeeff",
		string(exactEEXIST) + "/return-rule":    "76112233445566778899aabbccddeeff",
	}
	return identities[string(behavior)+"/"+operation]
}

func openNamespaceTCP(descriptor uintptr, destination netip.Addr, port int) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan struct {
		fd  int
		err error
	}, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		original, err := netns.Get()
		if err != nil {
			result <- struct {
				fd  int
				err error
			}{-1, err}
			return
		}
		defer original.Close()
		if err := netns.Set(netns.NsHandle(descriptor)); err != nil {
			result <- struct {
				fd  int
				err error
			}{-1, err}
			return
		}
		fd, socketErr := unix.Socket(unix.AF_INET, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if socketErr == nil {
			address := destination.As4()
			socketErr = connectRootTestSocket(ctx, fd,
				&unix.SockaddrInet4{Port: port, Addr: address})
		}
		restoreErr := netns.Set(original)
		if socketErr != nil || restoreErr != nil {
			if fd >= 0 {
				_ = unix.Close(fd)
			}
			result <- struct {
				fd  int
				err error
			}{-1, errors.Join(socketErr, restoreErr)}
			return
		}
		result <- struct {
			fd  int
			err error
		}{fd: fd}
	}()
	created := <-result
	if created.err != nil {
		return nil, created.err
	}
	file := os.NewFile(uintptr(created.fd), "iptables-root-namespace-tcp")
	connection, err := net.FileConn(file)
	return connection, errors.Join(err, file.Close())
}

func connectRootTestSocket(ctx context.Context, descriptor int, address unix.Sockaddr) error {
	if err := unix.SetNonblock(descriptor, true); err != nil {
		return err
	}
	if err := unix.Connect(descriptor, address); err != nil && !errors.Is(err, unix.EINPROGRESS) {
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := unix.Poll([]unix.PollFd{{Fd: int32(descriptor), Events: unix.POLLOUT}}, 50)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if ready == 0 {
			continue
		}
		connectionError, err := unix.GetsockoptInt(descriptor, unix.SOL_SOCKET, unix.SO_ERROR)
		if err != nil {
			return err
		}
		if connectionError != 0 {
			return unix.Errno(connectionError)
		}
		return unix.SetNonblock(descriptor, false)
	}
}

func httpSource(connection net.Conn, destination netip.Addr) (string, error) {
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(connection, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", destination); err != nil {
		return "", err
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	return strings.TrimSpace(string(body)), err
}

func (fixture *iptablesRootFixture) startServer() {
	listenerResult := make(chan struct {
		listener net.Listener
		err      error
	}, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		original, err := netns.Get()
		if err == nil {
			defer original.Close()
		}
		router, openErr := netns.GetFromName(fixture.router)
		if openErr == nil {
			defer router.Close()
			openErr = netns.Set(router)
		}
		var listener net.Listener
		if openErr == nil {
			listener, openErr = net.Listen("tcp4", net.JoinHostPort(fixture.remote.String(), "18082"))
		}
		var restoreErr error
		if err == nil {
			restoreErr = netns.Set(original)
		}
		listenerResult <- struct {
			listener net.Listener
			err      error
		}{listener: listener, err: errors.Join(err, openErr, restoreErr)}
	}()
	result := <-listenerResult
	if result.err != nil {
		fixture.t.Fatalf("start namespace receiver: %v", result.err)
	}
	fixture.server = &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		host, _, _ := net.SplitHostPort(request.RemoteAddr)
		_, _ = io.WriteString(writer, host)
	})}
	go func() { _ = fixture.server.Serve(result.listener) }()
}

func (fixture *iptablesRootFixture) readForwardPolicy() string {
	for _, line := range strings.Split(fixture.run(fixture.rulesCommand,
		"-w", "5", "-t", "filter", "-S", "FORWARD"), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "-P" && fields[1] == "FORWARD" {
			return fields[2]
		}
	}
	fixture.t.Fatal("iptables FORWARD chain has no policy")
	return ""
}

func (fixture *iptablesRootFixture) surface() string {
	commands := [][]string{
		{iptablesNFTSaveCommand, "-t", "filter"}, {iptablesNFTSaveCommand, "-t", "nat"},
		{iptablesLegacySaveCommand, "-t", "filter"}, {iptablesLegacySaveCommand, "-t", "nat"},
		{"nft", "-j", "list", "ruleset"},
		{"ip", "-j", "-details", "rule", "show"},
		{"ip", "-4", "-j", "-details", "route", "show", "table", "all"},
		{"ip", "-j", "-details", "link", "show"}, {"ip", "netns", "list"},
	}
	var result strings.Builder
	for _, command := range commands {
		output := fixture.run(command[0], command[1:]...)
		switch command[0] {
		case iptablesNFTSaveCommand, iptablesLegacySaveCommand:
			output = stableIPTablesSave(output)
		case "nft":
			output = stableNFTListing(output)
		case "ip":
			if len(command) == 3 && command[1] == "netns" && command[2] == "list" {
				output = strings.Join(namespaceListingNames(output), "\n") + "\n"
			}
		}
		result.WriteString(output)
		result.WriteByte('\n')
	}
	return result.String()
}

func stableIPTablesSave(output string) string {
	var stable strings.Builder
	for _, raw := range strings.Split(output, "\n") {
		if strings.HasPrefix(raw, "#") || raw == "" {
			continue
		}
		if strings.HasPrefix(raw, ":") {
			fields := strings.Fields(raw)
			if len(fields) == 3 {
				raw = fields[0] + " " + fields[1] + " [0:0]"
			}
		}
		stable.WriteString(raw)
		stable.WriteByte('\n')
	}
	return stable.String()
}

func stableNFTListing(output string) string {
	var document any
	if json.Unmarshal([]byte(output), &document) != nil {
		return output
	}
	clearNFTCounters(document)
	stable, err := json.Marshal(document)
	if err != nil {
		return output
	}
	return string(stable)
}

func clearNFTCounters(value any) {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			clearNFTCounters(child)
		}
	case map[string]any:
		if counter, found := typed["counter"].(map[string]any); found {
			counter["packets"] = float64(0)
			counter["bytes"] = float64(0)
		}
		for _, child := range typed {
			clearNFTCounters(child)
		}
	}
}

func (fixture *iptablesRootFixture) assertSurface(before string) {
	fixture.t.Helper()
	if after := fixture.surface(); after != before {
		fixture.t.Fatalf("host surface changed outside Transfer Lanes ownership\nbefore:\n%s\nafter:\n%s", before, after)
	}
	flows, err := netlink.ConntrackTableList(netlink.ConntrackTable, netlink.FAMILY_V4)
	if err != nil {
		fixture.t.Fatal(err)
	}
	for _, flow := range flows {
		if source, valid := netip.AddrFromSlice(flow.Forward.SrcIP); valid && temporaryRange.Contains(source.Unmap()) {
			fixture.t.Fatalf("temporary-source conntrack remains after cleanup: %s", flow)
		}
	}
}

func (fixture *iptablesRootFixture) snapshotSysctls() {
	fixture.sysctls = make(map[string][]byte)
	for _, path := range []string{"/proc/sys/net/ipv4/ip_forward", "/proc/sys/net/ipv4/conf/all/rp_filter",
		"/proc/sys/net/ipv4/conf/default/rp_filter", "/proc/sys/net/ipv4/conf/all/src_valid_mark",
		"/proc/sys/net/ipv4/conf/default/src_valid_mark"} {
		if contents, err := os.ReadFile(path); err == nil {
			fixture.sysctls[path] = append([]byte(nil), contents...)
		}
	}
}

func (fixture *iptablesRootFixture) writeSysctl(path, value string) {
	if err := os.WriteFile(path, []byte(value), 0); err != nil {
		fixture.t.Fatalf("write %s: %v", path, err)
	}
}

func (fixture *iptablesRootFixture) remove() {
	if fixture.server != nil {
		_ = fixture.server.Close()
	}
	fixture.runCleanup(fixture.rulesCommand, "-w", "5", "-t", "filter", "-D", "FORWARD",
		"-s", "203.0.113.254/32", "-j", "DROP")
	fixture.runCleanup(fixture.rulesCommand, "-w", "5", "-t", "nat", "-D", "POSTROUTING",
		"-s", "203.0.113.254/32", "-j", "MASQUERADE")
	if fixture.forwardPolicy != "" {
		fixture.runCleanup(fixture.rulesCommand, "-w", "5", "-t", "filter", "-P", "FORWARD", fixture.forwardPolicy)
	}
	fixture.removeRemoteReversePathRoute()
	fixture.removeDefaultReachabilityRoute()
	fixture.removeIntroducedCompatibilityTables()
	for _, table := range fixture.tables {
		fixture.runCleanup("ip", "route", "flush", "table", strconv.Itoa(table))
	}
	for _, provider := range fixture.providers {
		fixture.runCleanup("ip", "link", "del", provider)
	}
	fixture.runCleanup("ip", "netns", "del", fixture.router)
	for path, value := range fixture.sysctls {
		_ = os.WriteFile(path, value, 0)
	}
}

func (fixture *iptablesRootFixture) removeDefaultReachabilityRoute() {
	if fixture.defaultRoute == nil {
		return
	}
	routes, err := fixture.mainDefaultRoutes()
	if err != nil {
		fixture.t.Errorf("cleanup default reachability route: %v", err)
		return
	}
	if len(routes) != 1 || !exactFixtureDefaultRoute(routes[0], fixture.defaultRoute.route) {
		fixture.t.Errorf("preserve changed default reachability route: %#v", routes)
		return
	}
	if err := netlink.RouteDel(&routes[0]); err != nil {
		fixture.t.Errorf("delete default reachability route: %v", err)
		return
	}
	remaining, err := fixture.mainDefaultRoutes()
	if err != nil || len(remaining) != 0 {
		fixture.t.Errorf("default reachability route remains: %#v, %v", remaining, err)
		return
	}
	fixture.defaultRoute = nil
}

func (fixture *iptablesRootFixture) removeIntroducedCompatibilityTables() {
	for _, table := range []string{"filter", "nat"} {
		baseline := fixture.compatibility[table]
		if !baseline.introduced {
			continue
		}
		saved := stableIPTablesSave(fixture.cleanupCommand(iptablesNFTSaveCommand, "-t", table))
		listing, err := exec.Command("nft", "-j", "list", "table", "ip", table).CombinedOutput()
		if err != nil || saved != baseline.save || !emptyFixtureCompatibilityTable(listing, table) {
			fixture.t.Errorf("preserve changed iptables-nft %s table: save-match=%v error=%v\n%s",
				table, saved == baseline.save, err, listing)
			continue
		}
		if output, err := exec.Command("nft", "delete", "table", "ip", table).CombinedOutput(); err != nil {
			fixture.t.Errorf("delete fixture-created iptables-nft %s table: %v\n%s", table, err, output)
			continue
		}
		if err := exec.Command("nft", "list", "table", "ip", table).Run(); err == nil {
			fixture.t.Errorf("fixture-created iptables-nft %s table remains", table)
		}
	}
}

func emptyFixtureCompatibilityTable(document []byte, table string) bool {
	var listing struct {
		Objects []json.RawMessage `json:"nftables"`
	}
	if json.Unmarshal(document, &listing) != nil {
		return false
	}
	tableCount, chainCount := 0, 0
	for _, raw := range listing.Objects {
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			return false
		}
		if _, metadata := object["metainfo"]; metadata && len(object) == 1 {
			continue
		}
		if value, found := object["table"]; found && len(object) == 1 {
			if !onlyJSONFields(value, "family", "name", "handle") {
				return false
			}
			var identity struct {
				Family string `json:"family"`
				Name   string `json:"name"`
			}
			if json.Unmarshal(value, &identity) != nil || identity.Family != "ip" || identity.Name != table {
				return false
			}
			tableCount++
			continue
		}
		if value, found := object["chain"]; found && len(object) == 1 {
			if !fixtureCompatibilityChain(value, table) {
				return false
			}
			chainCount++
			continue
		}
		return false
	}
	return tableCount == 1 && chainCount == 1
}

func fixtureCompatibilityChain(document []byte, table string) bool {
	if !onlyJSONFields(document, "family", "table", "name", "handle", "type", "hook", "prio", "policy") {
		return false
	}
	var chain struct {
		Family string `json:"family"`
		Table  string `json:"table"`
		Name   string `json:"name"`
		Type   string `json:"type"`
		Hook   string `json:"hook"`
		Policy string `json:"policy"`
		Prio   int    `json:"prio"`
	}
	if json.Unmarshal(document, &chain) != nil || chain.Family != "ip" || chain.Table != table ||
		chain.Policy != "accept" {
		return false
	}
	if table == "filter" {
		return chain.Name == "FORWARD" && chain.Type == "filter" && chain.Hook == "forward" && chain.Prio == 0
	}
	return table == "nat" && chain.Name == "POSTROUTING" && chain.Type == "nat" &&
		chain.Hook == "postrouting" && chain.Prio == 100
}

func onlyJSONFields(document []byte, allowed ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(document, &fields) != nil {
		return false
	}
	for name := range fields {
		if !slices.Contains(allowed, name) {
			return false
		}
	}
	return true
}

func (fixture *iptablesRootFixture) cleanupCommand(name string, argv ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, argv...).CombinedOutput()
	if err != nil {
		fixture.t.Errorf("%s %q during cleanup: %v\n%s", name, argv, err, output)
	}
	return string(output)
}

func (fixture *iptablesRootFixture) removeRemoteReversePathRoute() {
	if fixture.reversePathRoute == nil {
		return
	}
	routes, err := fixture.remoteReversePathRoutes()
	if err != nil {
		fixture.t.Errorf("cleanup remote reverse-path route: %v", err)
		return
	}
	if len(routes) != 1 || !exactFixtureReversePathRoute(routes[0], fixture.reversePathRoute.route) {
		fixture.t.Errorf("preserve changed remote reverse-path route: %#v", routes)
		return
	}
	if err := netlink.RouteDel(&routes[0]); err != nil {
		fixture.t.Errorf("delete remote reverse-path route: %v", err)
		return
	}
	remaining, err := fixture.remoteReversePathRoutes()
	if err != nil || len(remaining) != 0 {
		fixture.t.Errorf("remote reverse-path route remains: %#v, %v", remaining, err)
		return
	}
	fixture.reversePathRoute = nil
}

func (fixture *iptablesRootFixture) run(name string, argv ...string) string {
	fixture.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, argv...).CombinedOutput()
	if err != nil {
		fixture.t.Fatalf("%s %q: %v\n%s", name, argv, err, output)
	}
	return string(output)
}

func (fixture *iptablesRootFixture) runCleanup(name string, argv ...string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, name, argv...).Run()
}
