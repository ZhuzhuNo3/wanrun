//go:build linux || darwin

package hostnetwork

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"reflect"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/networkcatalog"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const hostMutationLock = "/run/transferlanes/host-network.lock"

const rollbackTimeout = 30 * time.Second

type runAccess interface {
	ID() runid.ID
	OpenRoot() (*os.File, error)
	OpenPublishedRunRoots(context.Context) (rundirectory.PublishedRunRoots, error)
}

type currentEgresses interface {
	Current(context.Context, []egressSelection) ([]egressSelection, error)
}

type kernelHost interface {
	Inspect(context.Context, []egressSelection) (hostInventory, error)
	ObservePublishedFirewalls(context.Context, []networkClaim) (publishedFirewallObservations, error)
	Install(context.Context, networkClaim) (*hostInstallation, error)
	Verify(context.Context, networkClaim, *hostInstallation) error
	ConfirmConntrackEmpty(context.Context, networkClaim) error
	Rollback(context.Context, networkClaim, *hostInstallation, bool) error
	Close(context.Context, networkClaim, *hostInstallation) error
	Cleanup(context.Context, networkClaim, bool) error
}

type hostInstallation struct {
	namespaces map[transfernumber.Number]*os.File
	links      linkReceipts
	weakFIB    weakFIBReceipts
	firewall   firewallReceipts
}

func (installation *hostInstallation) requireComplete(claim networkClaim) error {
	if installation == nil {
		return errors.New("live host installation is missing")
	}
	return errors.Join(installation.links.requireComplete(claim),
		installation.weakFIB.requireComplete(claim), installation.firewall.requireComplete(claim))
}

type fileSync interface{ Sync(*os.File) error }

type systemFileSync struct{}

func (systemFileSync) Sync(file *os.File) error { return file.Sync() }

// Owner exclusively owns the complete host-network transaction for a run.
type Owner struct {
	egresses    currentEgresses
	host        kernelHost
	syncer      fileSync
	lockPath    string
	ownerTokens networkOwnerTokens
}

// New constructs the Linux host-network owner with preflight backend selection.
func New() *Owner {
	return newOwner(catalogEgresses{catalog: networkcatalog.New()}, defaultKernelHost(),
		systemFileSync{}, hostMutationLock)
}

func newOwner(egresses currentEgresses, host kernelHost, syncer fileSync, lockPath string) *Owner {
	return &Owner{egresses: egresses, host: host, syncer: syncer, lockPath: lockPath,
		ownerTokens: cryptographicNetworkOwnerTokens{}}
}

// Open atomically establishes every selected source egress for a live run.
func (owner *Owner) Open(ctx context.Context, run *rundirectory.LiveRun,
	selected map[transfernumber.Number]networkcatalog.Egress) (*Session, error) {
	if run == nil {
		return nil, errors.New("live run is required")
	}
	selections, err := selectionsFromCatalog(selected)
	if err != nil {
		return nil, err
	}
	return owner.open(ctx, run, selections)
}

func (owner *Owner) open(ctx context.Context, run runAccess, selected []egressSelection) (*Session, error) {
	root, err := run.OpenRoot()
	if err != nil {
		return nil, fmt.Errorf("open live run root: %w", err)
	}
	lock, err := acquireHostLock(ctx, owner.lockPath)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	defer lock.Close()
	session, err := owner.openLocked(ctx, run, root, selected)
	if err != nil {
		_ = root.Close()
	}
	return session, err
}

func (owner *Owner) openLocked(ctx context.Context, run runAccess, root *os.File,
	selected []egressSelection) (*Session, error) {
	current, err := owner.egresses.Current(ctx, selected)
	if err != nil {
		return nil, fmt.Errorf("refresh selected egresses: %w", err)
	}
	if !reflect.DeepEqual(current, selected) {
		return nil, errors.New("selected egress facts changed before host mutation")
	}
	inventory, err := owner.host.Inspect(ctx, selected)
	if err != nil {
		return nil, fmt.Errorf("inspect host network: %w", err)
	}
	inventory, err = owner.reservePublishedClaims(ctx, run, inventory)
	if err != nil {
		return nil, err
	}
	ownerToken, err := owner.ownerTokens.New()
	if err != nil {
		return nil, err
	}
	allocation, err := allocateClaimNetwork(run.ID(), ownerToken, selected, inventory)
	if err != nil {
		return nil, err
	}
	claim := claimFromAllocation(allocation)
	if err := publishClaim(root, claim, owner.syncer); err != nil {
		return nil, err
	}
	return owner.installPublished(ctx, root, claim)
}

func (owner *Owner) installPublished(ctx context.Context, root *os.File,
	claim networkClaim) (*Session, error) {
	installation, err := owner.host.Install(ctx, claim)
	if err != nil {
		return nil, owner.rollbackOpen(root, claim, installation, fmt.Errorf("install host network: %w", err))
	}
	if installation == nil {
		return nil, owner.rollbackOpen(root, claim, nil,
			errors.New("install host network returned no live installation"))
	}
	if err := installation.requireComplete(claim); err != nil {
		return nil, owner.rollbackOpen(root, claim, installation, err)
	}
	if err := validateNamespaceFiles(claim, installation.namespaces); err != nil {
		return nil, owner.rollbackOpen(root, claim, installation, err)
	}
	if err := owner.host.Verify(ctx, claim, installation); err != nil {
		return nil, owner.rollbackOpen(root, claim, installation, fmt.Errorf("verify host network: %w", err))
	}
	if err := owner.host.ConfirmConntrackEmpty(ctx, claim); err != nil {
		return nil, owner.rollbackOpen(root, claim, installation, err)
	}
	if err := publishTrafficActivation(root, claim, owner.syncer); err != nil {
		return nil, owner.rollbackOpen(root, claim, installation, err)
	}
	return newSession(owner, root, claim, installation), nil
}

func (owner *Owner) rollbackOpen(root *os.File, claim networkClaim,
	installation *hostInstallation, cause error) error {
	defer closeInstallationNamespaces(installation)
	cleanupContext, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()
	evidence, exists, err := loadNetworkEvidence(root, claim.runID, owner.syncer)
	if err != nil || !exists {
		if err == nil {
			err = errors.New("published network claim disappeared during rollback")
		}
		return errors.Join(cause, fmt.Errorf("load host network rollback evidence: %w", err))
	}
	if !claimsEqual(evidence.claim, claim) {
		return errors.Join(cause, errors.New("network claim changed during rollback"))
	}
	if err := owner.host.Rollback(cleanupContext, claim, installation, evidence.activated); err != nil {
		return errors.Join(cause, fmt.Errorf("roll back host network: %w", err))
	}
	if err := removeNetworkEvidence(root, evidence, owner.syncer); err != nil {
		return errors.Join(cause, fmt.Errorf("remove rolled-back network claim: %w", err))
	}
	return cause
}

// Recover removes precisely the HostNetwork objects claimed by a stale run.
func (owner *Owner) Recover(ctx context.Context, run *rundirectory.StaleRun) error {
	if run == nil {
		return errors.New("stale run is required")
	}
	root, err := run.OpenRoot()
	if err != nil {
		return fmt.Errorf("open stale run root: %w", err)
	}
	defer root.Close()
	lock, err := acquireHostLock(ctx, owner.lockPath)
	if err != nil {
		return err
	}
	defer lock.Close()
	return owner.recoverLocked(ctx, root, run.ID())
}

func (owner *Owner) recoverLocked(ctx context.Context, root *os.File, id runid.ID) error {
	evidence, exists, err := loadNetworkEvidence(root, id, owner.syncer)
	if err != nil || !exists {
		return err
	}
	if err := owner.host.Cleanup(ctx, evidence.claim, evidence.activated); err != nil {
		return fmt.Errorf("recover host network: %w", err)
	}
	return removeNetworkEvidence(root, evidence, owner.syncer)
}

func closeInstallationNamespaces(installation *hostInstallation) {
	if installation != nil {
		closeNamespaceFiles(installation.namespaces)
	}
}

func selectionsFromCatalog(selected map[transfernumber.Number]networkcatalog.Egress) ([]egressSelection, error) {
	result := make([]egressSelection, 0, len(selected))
	for id, egress := range selected {
		if !egress.Runnable() {
			return nil, fmt.Errorf("transfer %d egress is not runnable: %s", id.Value(), egress.Reason())
		}
		gateway, hasGateway := egress.Gateway()
		result = append(result, egressSelection{transfer: id, source: egress.LocalIP(),
			providerName: egress.Interface().Name(), providerIndex: egress.Interface().Index(),
			routeTable: egress.Table(), gateway: gateway, hasGateway: hasGateway})
	}
	return orderedSelections(result)
}

type catalogEgresses struct{ catalog networkcatalog.Catalog }

func (reader catalogEgresses) Current(ctx context.Context,
	early []egressSelection) ([]egressSelection, error) {
	sources := make([]netip.Addr, len(early))
	for index := range early {
		sources[index] = early[index].source
	}
	snapshot, err := reader.catalog.Capture(ctx)
	if err != nil {
		return nil, err
	}
	resolved := snapshot.Resolve(sources)
	if len(resolved) != len(early) {
		return nil, errors.New("network catalog returned incomplete egress facts")
	}
	result := make([]egressSelection, 0, len(early))
	for index, egress := range resolved {
		converted, err := selectionFromCurrent(early[index].transfer, egress)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}
	return result, nil
}

func selectionFromCurrent(id transfernumber.Number, egress networkcatalog.Egress) (egressSelection, error) {
	if !egress.Runnable() {
		return egressSelection{}, fmt.Errorf("transfer %d current egress is not runnable: %s", id.Value(), egress.Reason())
	}
	gateway, hasGateway := egress.Gateway()
	return egressSelection{transfer: id, source: egress.LocalIP(), providerName: egress.Interface().Name(),
		providerIndex: egress.Interface().Index(), routeTable: egress.Table(), gateway: gateway,
		hasGateway: hasGateway}, nil
}
