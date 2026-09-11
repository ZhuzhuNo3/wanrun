package transfer

import (
	"context"
	"errors"
	"fmt"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
)

func (transfer *TransferDirectory) stopStartupProcesses(live *rundirectory.LiveRun,
	network *hostnetwork.Session, resolvers resolverGroup, views activeFileViews, leases []fileViewLease,
	processes supervisedProcesses,
) (childprocesses.StartFailureResult, error) {
	ctx, cancel := transfer.newCleanupContext()
	finished := processes.FinishStartFailure(ctx)
	cancel()
	if !containmentConfirmed(processes) {
		return finished, transfer.retainUntilProcessContainment(live, network, resolvers, views,
			leases, processes, finished.ContainmentError)
	}
	return finished, transfer.cleanupContainedOwners(live, network, resolvers, views, leases)
}

func (transfer *TransferDirectory) cleanupObservedProcesses(live *rundirectory.LiveRun,
	network *hostnetwork.Session, resolvers resolverGroup, views activeFileViews, leases []fileViewLease,
	processes supervisedProcesses,
) error {
	ctx, cancel := transfer.newCleanupContext()
	confirmErr := processes.ConfirmContainment(ctx)
	cancel()
	if !containmentConfirmed(processes) {
		return transfer.retainUntilProcessContainment(live, network, resolvers, views, leases,
			processes, confirmErr)
	}
	return transfer.cleanupContainedOwners(live, network, resolvers, views, leases)
}

func (transfer *TransferDirectory) retainUntilProcessContainment(live *rundirectory.LiveRun,
	network *hostnetwork.Session, resolvers resolverGroup, views activeFileViews, leases []fileViewLease,
	processes supervisedProcesses, evidence error,
) error {
	if evidence == nil {
		evidence = errors.New("child process containment is unconfirmed")
	}
	cleanupDone := make(chan struct{})
	retainErr := live.RetainLivenessUntil(cleanupDone)
	if retainErr == nil {
		go transfer.closeAfterDelayedContainment(cleanupDone, processes.ContainmentDone(), network,
			resolvers, views, leases)
	}
	return errors.Join(fmt.Errorf("confirm child process cleanup: %w", evidence), retainErr)
}

func (transfer *TransferDirectory) closeAfterDelayedContainment(cleanupDone chan<- struct{},
	contained <-chan struct{}, network *hostnetwork.Session, resolvers resolverGroup,
	views activeFileViews, leases []fileViewLease,
) {
	<-contained
	resolverStopped, _ := transfer.closeResolverChain(network, resolvers)
	viewsStopped, _ := transfer.closeFileViewChain(views, leases)
	if resolverStopped && viewsStopped {
		close(cleanupDone)
	}
}

func (transfer *TransferDirectory) cleanupContainedOwners(live *rundirectory.LiveRun,
	network *hostnetwork.Session, resolvers resolverGroup, views activeFileViews,
	leases []fileViewLease,
) error {
	resolverStopped, resolverErr := transfer.closeResolverChain(network, resolvers)
	viewsStopped, viewErr := transfer.closeFileViewChain(views, leases)
	ownerErr := errors.Join(resolverErr, viewErr)
	if !resolverStopped || !viewsStopped {
		return errors.Join(ownerErr, transfer.retainClosingOwners(live, network, resolvers, views,
			resolverStopped, viewsStopped))
	}
	return errors.Join(ownerErr, transfer.completeRun(live))
}

func (transfer *TransferDirectory) retainClosingOwners(live *rundirectory.LiveRun,
	network *hostnetwork.Session, resolvers resolverGroup, views activeFileViews,
	resolverStopped, viewsStopped bool,
) error {
	cleanupDone := make(chan struct{})
	if err := live.RetainLivenessUntil(cleanupDone); err != nil {
		return err
	}
	go func() {
		if !resolverStopped {
			resolverStopped, _ = transfer.closeResolverChain(network, resolvers)
		}
		if !viewsStopped {
			viewsStopped, _ = transfer.closeFileViewChain(views, nil)
		}
		if resolverStopped && viewsStopped {
			close(cleanupDone)
		}
	}()
	return nil
}

func containmentConfirmed(processes supervisedProcesses) bool {
	select {
	case <-processes.ContainmentDone():
		return true
	default:
		return false
	}
}

func (transfer *TransferDirectory) cleanupViews(live *rundirectory.LiveRun,
	network *hostnetwork.Session, resolvers resolverGroup, views activeFileViews,
	leases []fileViewLease,
) error {
	return transfer.cleanupContainedOwners(live, network, resolvers, views, leases)
}

func (transfer *TransferDirectory) cleanupResolvers(live *rundirectory.LiveRun,
	network *hostnetwork.Session, resolvers resolverGroup,
) error {
	return transfer.cleanupContainedOwners(live, network, resolvers, nil, nil)
}

func (transfer *TransferDirectory) closeResolverChain(network *hostnetwork.Session,
	resolvers resolverGroup,
) (bool, error) {
	ctx, cancel := transfer.newCleanupContext()
	stopped, resolverErr := resolvers.Close(ctx)
	cancel()
	if !stopped {
		return false, resolverContainmentError(resolverErr)
	}
	return true, errors.Join(resolverErr, transfer.closeNetwork(network))
}

func resolverContainmentError(err error) error {
	if err == nil {
		err = errors.New("namespace resolver workers did not stop")
	}
	return fmt.Errorf("confirm namespace resolver cleanup: %w", err)
}

func (transfer *TransferDirectory) closeFileViewChain(views activeFileViews,
	leases []fileViewLease,
) (bool, error) {
	leaseErr := closeViewLeases(leases)
	if views == nil {
		return true, leaseErr
	}
	ctx, cancel := transfer.newCleanupContext()
	stopped, viewErr := views.close(ctx)
	cancel()
	if viewErr != nil {
		viewErr = fmt.Errorf("close file views: %w", viewErr)
	}
	return stopped, errors.Join(leaseErr, viewErr)
}

func (transfer *TransferDirectory) cleanupNetwork(live *rundirectory.LiveRun,
	network *hostnetwork.Session,
) error {
	return errors.Join(transfer.closeNetwork(network), transfer.completeRun(live))
}

func (transfer *TransferDirectory) closeNetwork(network *hostnetwork.Session) error {
	ctx, cancel := transfer.newCleanupContext()
	defer cancel()
	if err := transfer.networks.close(ctx, network); err != nil {
		return fmt.Errorf("close host network: %w", err)
	}
	return nil
}

func (transfer *TransferDirectory) cleanupRoot(live *rundirectory.LiveRun) error {
	return transfer.completeRun(live)
}

func (transfer *TransferDirectory) completeRun(live *rundirectory.LiveRun) error {
	ctx, cancel := transfer.newCleanupContext()
	defer cancel()
	completeErr := live.Complete(ctx)
	if completeErr == nil {
		return nil
	}
	return errors.Join(fmt.Errorf("complete run root: %w", completeErr), live.Close())
}

func (transfer *TransferDirectory) newCleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), transfer.cleanupLimit)
}

func closeViewLeases(leases []fileViewLease) error {
	failures := make([]error, 0, len(leases))
	for _, lease := range leases {
		if lease != nil {
			failures = append(failures, lease.close())
		}
	}
	return errors.Join(failures...)
}
