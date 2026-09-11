package hostnetwork

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestOpenPublishesDurableClaimBeforeKernelMutation(t *testing.T) {
	for failAt := 1; failAt <= 3; failAt++ {
		t.Run(syncPointName(failAt), func(t *testing.T) {
			root := openTestRunRoot(t)
			host := &recordingHost{}
			owner := testOwner(t, host, &failingSync{failAt: failAt})
			session, err := owner.open(context.Background(), testRun{
				mustRunID(t, "30112233445566778899aabbccddeeff"), root}, testSelections())
			if err == nil || session != nil {
				t.Fatalf("durability failure = %#v, %v; want nil/error", session, err)
			}
			if host.installCalls != 0 || host.rollbackCalls != 0 {
				t.Fatalf("kernel calls = install %d rollback %d, want zero",
					host.installCalls, host.rollbackCalls)
			}
		})
	}
}

func TestOpenActivatesOnlyAfterReceiptsVerificationAndConntrackRecheck(t *testing.T) {
	root := openTestRunRoot(t)
	host := &recordingHost{}
	owner := testOwner(t, host, systemFileSync{})
	run := testRun{mustRunID(t, "31112233445566778899aabbccddeeff"), root}
	host.onConfirmConntrack = func(claim networkClaim) error {
		evidence, exists, err := loadNetworkEvidence(root, claim.runID, systemFileSync{})
		if err != nil || !exists || evidence.activated {
			t.Fatalf("pre-activation evidence = %#v, exists=%t, error=%v", evidence, exists, err)
		}
		return nil
	}
	session, err := owner.open(context.Background(), run, testSelections())
	if err != nil {
		t.Fatal(err)
	}
	evidence, exists, err := loadNetworkEvidence(root, run.id, systemFileSync{})
	if err != nil || !exists || !evidence.activated {
		t.Fatalf("opened evidence = %#v, exists=%t, error=%v", evidence, exists, err)
	}
	if host.eventsString() != "install,verify,conntrack" {
		t.Fatalf("host event order = %s", host.eventsString())
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if host.cleanupActivated != 1 {
		t.Fatalf("activated cleanup calls = %d, want one", host.cleanupActivated)
	}
	if host.liveCloseCalls != 1 || host.staleCleanupCalls != 0 {
		t.Fatalf("cleanup authority = live %d stale %d, want 1/0",
			host.liveCloseCalls, host.staleCleanupCalls)
	}
}

func TestOpenRejectsIncompletePositiveReceiptsBeforeVerificationOrActivation(t *testing.T) {
	for name, removeReceipt := range map[string]func(networkClaim, *hostInstallation){
		"link": func(claim networkClaim, installation *hostInstallation) {
			delete(installation.links, claim.transfers[0].transfer)
		},
		"return rule": func(claim networkClaim, installation *hostInstallation) {
			delete(installation.weakFIB.returnRules, claim.transfers[0].transfer)
		},
		"firewall": func(claim networkClaim, installation *hostInstallation) {
			owners, _ := firewallReceiptOwners(claim)
			delete(installation.firewall.owners, owners[0])
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := openTestRunRoot(t)
			host := &recordingHost{makeInstallation: func(claim networkClaim) *hostInstallation {
				installation := completeTestInstallation(claim)
				removeReceipt(claim, installation)
				return installation
			}}
			owner := testOwner(t, host, systemFileSync{})
			run := testRun{mustRunID(t, "32112233445566778899aabbccddeeff"), root}
			if session, err := owner.open(context.Background(), run,
				testSelections()); err == nil || session != nil {
				t.Fatalf("incomplete receipts = %#v, %v; want nil/error", session, err)
			}
			if host.verifyCalls != 0 || host.conntrackCalls != 0 || host.rollbackCalls != 1 {
				t.Fatalf("calls = verify %d conntrack %d rollback %d", host.verifyCalls,
					host.conntrackCalls, host.rollbackCalls)
			}
			assertNoNetworkEvidence(t, root)
		})
	}
}

func TestOpenFailuresBeforeActivationUseOnlyLiveRollbackAuthority(t *testing.T) {
	installFailure := errors.New("uncertain add")
	verifyFailure := errors.New("object graph mismatch")
	conntrackFailure := errors.New("source became active")
	for name, testCase := range map[string]struct {
		cause     error
		configure func(*recordingHost)
	}{
		"install": {installFailure, func(host *recordingHost) { host.installErr = installFailure }},
		"verify":  {verifyFailure, func(host *recordingHost) { host.verifyErr = verifyFailure }},
		"conntrack": {conntrackFailure, func(host *recordingHost) {
			host.conntrackErr = conntrackFailure
		}},
	} {
		t.Run(name, func(t *testing.T) {
			root := openTestRunRoot(t)
			host := &recordingHost{}
			testCase.configure(host)
			owner := testOwner(t, host, systemFileSync{})
			run := testRun{mustRunID(t, "40112233445566778899aabbccddeeff"), root}
			session, openErr := owner.open(context.Background(), run, testSelections())
			if openErr == nil || session != nil {
				t.Fatalf("faulted open = %#v, %v; want nil/error", session, openErr)
			}
			if host.rollbackCalls != 1 || host.rollbackActivated != 0 {
				t.Fatalf("rollback calls/activated = %d/%d, want 1/0",
					host.rollbackCalls, host.rollbackActivated)
			}
			assertFailedOpenRemovedEvidence(t, root, run.id, testCase.cause, openErr)
		})
	}
}

func assertFailedOpenRemovedEvidence(t *testing.T, root *os.File, id runid.ID,
	cause, openErr error,
) {
	t.Helper()
	if !errors.Is(openErr, cause) {
		t.Fatalf("open error = %v, want cause %v", openErr, cause)
	}
	if strings.Contains(openErr.Error(), "remove rolled-back network claim") {
		t.Fatalf("open error contains evidence-removal failure: %v", openErr)
	}
	if evidence, exists, err := loadNetworkEvidence(root, id, systemFileSync{}); err != nil || exists {
		t.Fatalf("network evidence = %#v, exists=%t, error=%v; want absent", evidence, exists, err)
	}
	if _, err := os.Stat(filepath.Join(root.Name(), networkDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("network evidence directory remains after successful removal: %v", err)
	}
}

func TestActivationRenameSurvivesDirectorySyncErrorAsCleanupAuthority(t *testing.T) {
	root := openTestRunRoot(t)
	host := &recordingHost{}
	owner := testOwner(t, host, &activationDirectoryFailingSync{})
	run := testRun{mustRunID(t, "50112233445566778899aabbccddeeff"), root}
	if session, err := owner.open(context.Background(), run, testSelections()); err == nil || session != nil {
		t.Fatalf("activation sync failure = %#v, %v; want nil/error", session, err)
	}
	if host.rollbackCalls != 1 || host.rollbackActivated != 1 {
		t.Fatalf("rollback calls/activated = %d/%d, want 1/1",
			host.rollbackCalls, host.rollbackActivated)
	}
	assertNoNetworkEvidence(t, root)
}

func TestFailedRollbackPreservesTheExactClaimAndActivationState(t *testing.T) {
	root := openTestRunRoot(t)
	host := &recordingHost{verifyErr: errors.New("verify failed"),
		rollbackErr: errors.New("ambiguous independent rule")}
	owner := testOwner(t, host, systemFileSync{})
	run := testRun{mustRunID(t, "60112233445566778899aabbccddeeff"), root}
	if _, err := owner.open(context.Background(), run, testSelections()); err == nil {
		t.Fatal("faulted open succeeded")
	}
	evidence, exists, err := loadNetworkEvidence(root, run.id, systemFileSync{})
	if err != nil || !exists || evidence.activated {
		t.Fatalf("retained evidence = %#v, exists=%t, error=%v", evidence, exists, err)
	}
}

func TestRecoveryReadsActivationButNeverCreatesIt(t *testing.T) {
	for _, activated := range []bool{false, true} {
		t.Run(map[bool]string{false: "claim-only", true: "activated"}[activated], func(t *testing.T) {
			root := openTestRunRoot(t)
			claim := claimFromAllocation(testAllocation(t, firewallNFTables))
			if err := publishClaim(root, claim, systemFileSync{}); err != nil {
				t.Fatal(err)
			}
			if activated {
				if err := publishTrafficActivation(root, claim, systemFileSync{}); err != nil {
					t.Fatal(err)
				}
			}
			host := &recordingHost{}
			owner := testOwner(t, host, systemFileSync{})
			if err := owner.recoverLocked(context.Background(), root, claim.runID); err != nil {
				t.Fatal(err)
			}
			if host.cleanupCalls != 1 || host.cleanupActivated != boolCount(activated) ||
				host.liveCloseCalls != 0 || host.staleCleanupCalls != 1 ||
				host.conntrackCalls != 0 || host.verifyCalls != 0 {
				t.Fatalf("recovery calls = cleanup %d activated %d live %d stale %d conntrack %d verify %d",
					host.cleanupCalls, host.cleanupActivated, host.liveCloseCalls,
					host.staleCleanupCalls, host.conntrackCalls, host.verifyCalls)
			}
			assertNoNetworkEvidence(t, root)
		})
	}
}

func TestOpenRevalidatesBeforePublishingClaim(t *testing.T) {
	for name, configure := range map[string]func(*Owner, *recordingHost){
		"egress changed": func(owner *Owner, _ *recordingHost) {
			changed := testSelections()
			changed[0].routeTable = 100
			owner.egresses = currentSelections{changed}
		},
		"host preflight failed": func(_ *Owner, host *recordingHost) {
			host.inspectErr = errors.New("routing observation unavailable")
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := openTestRunRoot(t)
			host := &recordingHost{}
			owner := testOwner(t, host, systemFileSync{})
			configure(owner, host)
			session, err := owner.open(context.Background(), testRun{
				mustRunID(t, "70112233445566778899aabbccddeeff"), root}, testSelections())
			if err == nil || session != nil {
				t.Fatalf("preflight result = %#v, %v; want nil/error", session, err)
			}
			if host.installCalls != 0 {
				t.Fatalf("preflight failure reached %d install calls", host.installCalls)
			}
			assertNoNetworkEvidence(t, root)
		})
	}
}

func TestOpenRollbackUsesIndependentContextAfterCancellation(t *testing.T) {
	root := openTestRunRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	host := &recordingHost{install: func(operation context.Context) error {
		cancel()
		return operation.Err()
	}}
	owner := testOwner(t, host, systemFileSync{})
	if _, err := owner.open(ctx, testRun{
		mustRunID(t, "71112233445566778899aabbccddeeff"), root}, testSelections()); err == nil {
		t.Fatal("canceled open succeeded")
	}
	if host.rollbackCalls != 1 || host.rollbackContextErr != nil {
		t.Fatalf("rollback calls/context = %d/%v", host.rollbackCalls, host.rollbackContextErr)
	}
}

func TestRecoveryUnknownEvidenceFailsClosed(t *testing.T) {
	root := openTestRunRoot(t)
	id := mustRunID(t, "80112233445566778899aabbccddeeff")
	directory := filepath.Join(root.Name(), networkDirectoryName)
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(directory, "foreign")
	if err := os.WriteFile(foreign, []byte("do not remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	host := &recordingHost{}
	owner := testOwner(t, host, systemFileSync{})
	if err := owner.recoverLocked(context.Background(), root, id); err == nil {
		t.Fatal("unknown evidence was accepted")
	}
	if host.cleanupCalls != 0 {
		t.Fatalf("unknown evidence caused %d cleanup calls", host.cleanupCalls)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("unknown evidence was removed: %v", err)
	}
}

func TestRecoveryRejectsOutOfRangeProtocolBeforeHostCleanup(t *testing.T) {
	root := openTestRunRoot(t)
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	writeClaimEncoding(t, root, claimEncodingWithProtocol(t, claim, firstClaimProtocol-1))
	host := &recordingHost{}
	owner := testOwner(t, host, systemFileSync{})
	if err := owner.recoverLocked(context.Background(), root, claim.runID); err == nil {
		t.Fatal("recovery accepted an out-of-range claim protocol")
	}
	if host.claimedFirewallCalls != 0 || host.cleanupCalls != 0 {
		t.Fatalf("invalid recovery protocol reached firewall observations/cleanup = %d/%d",
			host.claimedFirewallCalls, host.cleanupCalls)
	}
	if _, err := os.Stat(filepath.Join(root.Name(), networkDirectoryName, claimFileName)); err != nil {
		t.Fatalf("invalid recovery claim changed: %v", err)
	}
}

type testRun struct {
	id   runid.ID
	root *os.File
}

func (run testRun) ID() runid.ID { return run.id }

func (run testRun) OpenRoot() (*os.File, error) { return os.Open(run.root.Name()) }

func (run testRun) OpenPublishedRunRoots(context.Context) (rundirectory.PublishedRunRoots, error) {
	return testPublishedRunRoots{roots: []rundirectory.PublishedRunRoot{testPublishedRunRoot{run}}}, nil
}

type testPublishedRunRoot struct{ run testRun }

func (root testPublishedRunRoot) ID() runid.ID { return root.run.ID() }

func (root testPublishedRunRoot) OpenRoot() (*os.File, error) { return root.run.OpenRoot() }

type testPublishedRunRoots struct {
	roots []rundirectory.PublishedRunRoot
}

func (roots testPublishedRunRoots) Roots() []rundirectory.PublishedRunRoot {
	return append([]rundirectory.PublishedRunRoot(nil), roots.roots...)
}

func (testPublishedRunRoots) Close() error { return nil }

type recordingHost struct {
	installCalls         int
	verifyCalls          int
	conntrackCalls       int
	rollbackCalls        int
	cleanupCalls         int
	cleanupActivated     int
	liveCloseCalls       int
	staleCleanupCalls    int
	rollbackActivated    int
	installErr           error
	inspectErr           error
	verifyErr            error
	conntrackErr         error
	claimedFirewallErr   error
	claimedPresent       bool
	claimedFirewallCalls int
	rollbackErr          error
	cleanupErr           error
	install              func(context.Context) error
	onConfirmConntrack   func(networkClaim) error
	makeInstallation     func(networkClaim) *hostInstallation
	rollbackContextErr   error
	inventory            hostInventory
	events               []string
	installClaims        []networkClaim
	cleanupClaims        []networkClaim
}

func (host *recordingHost) Inspect(context.Context, []egressSelection) (hostInventory, error) {
	if host.inventory.firewall.kind != "" {
		return host.inventory, host.inspectErr
	}
	return emptyInventory(), host.inspectErr
}

func (host *recordingHost) ObservePublishedFirewalls(_ context.Context,
	claims []networkClaim,
) (publishedFirewallObservations, error) {
	host.claimedFirewallCalls++
	if host.claimedFirewallErr != nil {
		return nil, host.claimedFirewallErr
	}
	result := make(publishedFirewallObservations, len(claims))
	for _, claim := range claims {
		result[claim.runID] = host.claimedPresent
	}
	return result, nil
}

func (host *recordingHost) Install(ctx context.Context, claim networkClaim) (*hostInstallation, error) {
	host.installCalls++
	host.events = append(host.events, "install")
	host.installClaims = append(host.installClaims, claim)
	installation := completeTestInstallation(claim)
	if host.makeInstallation != nil {
		closeNamespaceFiles(installation.namespaces)
		installation = host.makeInstallation(claim)
	}
	if host.install != nil {
		return installation, host.install(ctx)
	}
	return installation, host.installErr
}

func (host *recordingHost) Verify(context.Context, networkClaim, *hostInstallation) error {
	host.verifyCalls++
	host.events = append(host.events, "verify")
	return host.verifyErr
}

func (host *recordingHost) ConfirmConntrackEmpty(_ context.Context, claim networkClaim) error {
	host.conntrackCalls++
	host.events = append(host.events, "conntrack")
	if host.onConfirmConntrack != nil {
		return host.onConfirmConntrack(claim)
	}
	return host.conntrackErr
}

func (host *recordingHost) Rollback(ctx context.Context, claim networkClaim,
	_ *hostInstallation, activated bool) error {
	host.rollbackCalls++
	host.rollbackContextErr = ctx.Err()
	host.rollbackActivated += boolCount(activated)
	host.cleanupClaims = append(host.cleanupClaims, claim)
	return host.rollbackErr
}

func (host *recordingHost) Close(_ context.Context, claim networkClaim,
	_ *hostInstallation) error {
	host.cleanupCalls++
	host.cleanupActivated++
	host.liveCloseCalls++
	host.cleanupClaims = append(host.cleanupClaims, claim)
	return host.cleanupErr
}

func (host *recordingHost) Cleanup(_ context.Context, claim networkClaim, activated bool) error {
	host.cleanupCalls++
	host.cleanupActivated += boolCount(activated)
	host.staleCleanupCalls++
	host.cleanupClaims = append(host.cleanupClaims, claim)
	return host.cleanupErr
}

func (host *recordingHost) eventsString() string {
	result := ""
	for index, event := range host.events {
		if index > 0 {
			result += ","
		}
		result += event
	}
	return result
}

func completeTestInstallation(claim networkClaim) *hostInstallation {
	namespaces := make(map[transfernumber.Number]*os.File, len(claim.transfers))
	links := newLinkReceipts()
	weakFIB := newWeakFIBReceipts()
	for _, value := range claim.transfers {
		file, _ := os.Open(os.DevNull)
		namespaces[value.transfer] = file
		links.add(linkReceipt{transfer: value.transfer})
		weakFIB.returnRoutes[value.transfer] = returnRouteReceipt{transfer: value.transfer}
		weakFIB.outboundRules[value.transfer] = outboundRuleReceipt{transfer: value.transfer}
		weakFIB.returnRules[value.transfer] = returnRuleReceipt{transfer: value.transfer}
	}
	firewall := newFirewallReceipts(firewallKind(claim.backend))
	owners, _ := firewallReceiptOwners(claim)
	for _, owner := range owners {
		firewall.add(owner)
	}
	return &hostInstallation{namespaces: namespaces, links: links, weakFIB: weakFIB, firewall: firewall}
}

type currentSelections struct{ values []egressSelection }

func (reader currentSelections) Current(context.Context, []egressSelection) ([]egressSelection, error) {
	return append([]egressSelection(nil), reader.values...), nil
}

type failingSync struct {
	mu     sync.Mutex
	calls  int
	failAt int
}

type activationDirectoryFailingSync struct {
	activationWritten bool
	failed            bool
}

func (syncer *activationDirectoryFailingSync) Sync(file *os.File) error {
	if file.Name() == activationTemporaryName {
		syncer.activationWritten = true
	}
	if file.Name() == networkDirectoryName && syncer.activationWritten && !syncer.failed {
		syncer.failed = true
		return errors.New("injected activation directory sync failure")
	}
	return file.Sync()
}

func (syncer *failingSync) Sync(file *os.File) error {
	syncer.mu.Lock()
	defer syncer.mu.Unlock()
	syncer.calls++
	if syncer.calls == syncer.failAt {
		return errors.New("injected durability failure")
	}
	return file.Sync()
}

func testOwner(t *testing.T, host kernelHost, syncer fileSync) *Owner {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return newOwner(currentSelections{testSelections()}, host, syncer,
		filepath.Join(directory, "host-network.lock"))
}

func openTestRunRoot(t *testing.T) *os.File {
	t.Helper()
	rootPath := filepath.Join(t.TempDir(), "run")
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.Open(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func assertNoNetworkEvidence(t *testing.T, root *os.File) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(root.Name(), networkDirectoryName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("network evidence remains: %v", err)
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func syncPointName(point int) string { return "sync-point-" + string(rune('0'+point)) }

func testSelections() []egressSelection {
	id, _ := transfernumber.New(1)
	return []egressSelection{{transfer: id, source: netip.MustParseAddr("192.0.2.10"),
		providerName: "wan0", providerIndex: 2, routeTable: 254}}
}
