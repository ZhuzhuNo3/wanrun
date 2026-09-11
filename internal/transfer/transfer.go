package transfer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/fileviews"
	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/networkcatalog"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const cleanupTimeout = 30 * time.Second

// TransferDirectory is the sole application flow for one owned directory transfer.
type TransferDirectory struct {
	roots        *rundirectory.Owner
	egresses     egressResolver
	networks     hostNetworkLifecycle
	resolvers    namespaceResolvers
	weights      weightMeasurer
	views        fileViewMounts
	commands     commandGroupStarter
	cleanupLimit time.Duration
}

// New constructs the production application flow from its concrete collaborators.
func New() *TransferDirectory {
	return NewWithCommandEvents(discardCommandEvents{})
}

// NewWithCommandEvents constructs the production flow and forwards the owned command group's
// PTY/status/control surface to one supervisor-side consumer.
func NewWithCommandEvents(events CommandEvents) *TransferDirectory {
	processOwner := childprocesses.New()
	return newTransferDirectory(
		rundirectory.New(),
		catalogEgressResolver{catalog: networkcatalog.New()},
		kernelHostNetworkLifecycle{owner: hostnetwork.New()},
		networkNamespaceResolvers{},
		throughputWeightMeasurer{tester: throughput.New(), processes: processOwner},
		fuseFileViewMounts{owner: fileviews.New()},
		childCommandStarter{owner: processOwner, events: events},
		cleanupTimeout,
	)
}

func newTransferDirectory(roots *rundirectory.Owner, egresses egressResolver,
	networks hostNetworkLifecycle, resolvers namespaceResolvers, weights weightMeasurer, views fileViewMounts,
	commands commandGroupStarter, cleanupLimit time.Duration,
) *TransferDirectory {
	return &TransferDirectory{roots: roots, egresses: egresses, networks: networks, resolvers: resolvers,
		weights: weights, views: views, commands: commands, cleanupLimit: cleanupLimit}
}

// Run recovers old runs, executes one transfer, and releases every acquired owner in fixed order.
func (transfer *TransferDirectory) Run(ctx context.Context, source *sourcefiles.SourceRoot,
	request Request,
) Result {
	if err := transfer.validate(ctx, source, request); err != nil {
		return Result{runErr: err}
	}
	if err := transfer.recoverOldRuns(ctx); err != nil {
		return Result{runErr: fmt.Errorf("recover stale runs: %w", err)}
	}
	if err := transfer.views.preflight(); err != nil {
		return Result{runErr: fmt.Errorf("preflight file views: %w", err)}
	}
	egresses, snapshot, snapshotCaptured, err := transfer.readOnlyFacts(ctx, source, request)
	if err != nil {
		return resultWithSourceSummary(Result{runErr: err}, snapshot, snapshotCaptured)
	}
	live, result, err := transfer.createLiveRun()
	result = resultWithSourceSummary(result, snapshot, true)
	if err != nil {
		return result
	}
	network, err := transfer.networks.open(ctx, live, egresses)
	if err != nil {
		result.runErr = fmt.Errorf("open host network: %w", err)
		result.cleanupErr = transfer.cleanupRoot(live)
		return result
	}
	return transfer.executeWithNetwork(ctx, source, request, egresses, snapshot, live, network, result)
}

func (transfer *TransferDirectory) executeWithNetwork(ctx context.Context,
	source *sourcefiles.SourceRoot, request Request,
	egresses map[transfernumber.Number]networkcatalog.Egress, snapshot sourcefiles.SourceSnapshot,
	live *rundirectory.LiveRun, network *hostnetwork.Session, result Result,
) Result {
	resolvers, err := transfer.resolvers.open(ctx, network, request.selectedNetworks(), request.resolver)
	if err != nil {
		result.runErr = fmt.Errorf("open namespace resolvers: %w", err)
		result.cleanupErr = transfer.cleanupNetwork(live, network)
		return result
	}
	result = transfer.executeWithResolvers(ctx, source, request, egresses, snapshot, live, network, resolvers, result)
	if diagnosticErr := resolvers.DiagnosticError(); diagnosticErr != nil {
		result.runErr = errors.Join(result.runErr, fmt.Errorf("namespace resolver: %w", diagnosticErr))
	}
	return result
}

func (transfer *TransferDirectory) executeWithResolvers(ctx context.Context,
	source *sourcefiles.SourceRoot, request Request,
	egresses map[transfernumber.Number]networkcatalog.Egress, snapshot sourcefiles.SourceSnapshot,
	live *rundirectory.LiveRun, network *hostnetwork.Session, resolvers resolverGroup, result Result,
) Result {
	weighted, measuring, err := transfer.finalWeights(ctx, network, request, resolvers, source.StableRoot())
	if err != nil {
		result.runErr = err
		if measuring != nil {
			result.cleanupErr = transfer.cleanupObservedProcesses(live, network, resolvers, nil, nil, measuring)
		} else {
			result.cleanupErr = transfer.cleanupResolvers(live, network, resolvers)
		}
		return result
	}
	allocation, err := sourcefiles.AllocateTransferItems(snapshot, weighted)
	if err != nil {
		result.runErr = fmt.Errorf("allocate source files: %w", err)
		result.cleanupErr = transfer.cleanupResolvers(live, network, resolvers)
		return result
	}
	views, err := transfer.views.open(ctx, live, allocation, source)
	if err != nil {
		result.runErr = fmt.Errorf("open file views: %w", err)
		if views == nil {
			result.cleanupErr = transfer.cleanupResolvers(live, network, resolvers)
		} else {
			result.cleanupErr = transfer.cleanupViews(live, network, resolvers, views, nil)
		}
		return result
	}
	executions, leases, err := prepareExecutions(request, egresses, views)
	if err != nil {
		result.runErr = err
		result.cleanupErr = transfer.cleanupViews(live, network, resolvers, views, leases)
		return result
	}
	processes, err := transfer.commands.start(ctx, network, resolvers, executions, request)
	if err != nil {
		result.runErr = fmt.Errorf("start child process group: %w", err)
		if processes != nil {
			finished, cleanupErr := transfer.stopStartupProcesses(live, network, resolvers, views, leases, processes)
			result.runErr = errors.Join(result.runErr, finished.SupervisionError)
			result.cleanupErr = cleanupErr
		} else {
			result.cleanupErr = transfer.cleanupViews(live, network, resolvers, views, leases)
		}
		return result
	}
	processResult, waitErr := processes.Wait()
	result.transfers, result.cancelled, result.runErr = transferResult(request, processResult, waitErr, ctx)
	result.cleanupErr = transfer.cleanupObservedProcesses(live, network, resolvers, views, leases, processes)
	return result
}

func (transfer *TransferDirectory) createLiveRun() (*rundirectory.LiveRun, Result, error) {
	id, err := runid.New()
	if err != nil {
		return nil, Result{runErr: fmt.Errorf("create run identity: %w", err)}, err
	}
	live, err := transfer.roots.Create(id)
	if err != nil {
		return nil, Result{runErr: fmt.Errorf("create live run: %w", err)}, err
	}
	if actual := live.ID(); actual != id {
		err = fmt.Errorf("RunDirectory returned mismatched run identity %s for %s", actual, id)
		return nil, Result{id: actual, hasRun: true, runErr: err,
			cleanupErr: transfer.cleanupRoot(live)}, err
	}
	return live, Result{id: id, hasRun: true}, nil
}

func (transfer *TransferDirectory) validate(ctx context.Context, source *sourcefiles.SourceRoot,
	request Request,
) error {
	if transfer == nil || ctx == nil || transfer.roots == nil || transfer.egresses == nil ||
		transfer.networks == nil || transfer.weights == nil || transfer.views == nil ||
		transfer.resolvers == nil || transfer.commands == nil || transfer.cleanupLimit <= 0 {
		return errors.New("transfer directory requires complete capabilities and context")
	}
	if source == nil || source.BaseName() == "" || source.StableRoot() == "" {
		return errors.New("transfer directory requires an open source directory")
	}
	if source.BaseName() != request.sourceName {
		return errors.New("transfer source capability does not match the requested source name")
	}
	return validateSelectedNetworks(request.selectedNetworks())
}

func (transfer *TransferDirectory) recoverOldRuns(ctx context.Context) error {
	return transfer.roots.RecoverStale(ctx, func(stale *rundirectory.StaleRun) error {
		networkCtx, cancelNetwork := transfer.newCleanupContext()
		networkErr := transfer.networks.recover(networkCtx, stale)
		cancelNetwork()
		viewCtx, cancelViews := transfer.newCleanupContext()
		viewErr := transfer.views.recover(viewCtx, stale)
		cancelViews()
		if networkErr != nil {
			networkErr = fmt.Errorf("recover run %s host network: %w", stale.ID(), networkErr)
		}
		if viewErr != nil {
			viewErr = fmt.Errorf("recover run %s file views: %w", stale.ID(), viewErr)
		}
		return errors.Join(networkErr, viewErr)
	})
}

type egressOutcome struct {
	egresses map[transfernumber.Number]networkcatalog.Egress
	err      error
}

type sourceOutcome struct {
	snapshot sourcefiles.SourceSnapshot
	err      error
}

func (transfer *TransferDirectory) readOnlyFacts(ctx context.Context,
	source *sourcefiles.SourceRoot, request Request,
) (map[transfernumber.Number]networkcatalog.Egress, sourcefiles.SourceSnapshot, bool, error) {
	selectedNetworks := request.selectedNetworks()
	egressDone := make(chan egressOutcome, 1)
	sourceDone := make(chan sourceOutcome, 1)
	go func() {
		egresses, err := transfer.egresses.resolve(ctx, selectedNetworks)
		egressDone <- egressOutcome{egresses: egresses, err: err}
	}()
	go func() {
		snapshot, err := source.Scan(ctx, request.followSymlinks)
		sourceDone <- sourceOutcome{snapshot: snapshot, err: err}
	}()
	egressResult, sourceResult := <-egressDone, <-sourceDone
	snapshotCaptured := sourceResult.snapshot.BaseName() != ""
	if err := errors.Join(egressResult.err, sourceResult.err); err != nil {
		return nil, sourceResult.snapshot, snapshotCaptured, fmt.Errorf("prepare transfer facts: %w", err)
	}
	if err := validateEgressSet(selectedNetworks, egressResult.egresses); err != nil {
		return nil, sourceResult.snapshot, true, err
	}
	return egressResult.egresses, sourceResult.snapshot, true, nil
}
