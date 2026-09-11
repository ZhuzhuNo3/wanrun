package transfer

import (
	"context"

	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/networkcatalog"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

// hostNetworkLifecycle is the complete host-mutation boundary consumed by one transfer run.
type hostNetworkLifecycle interface {
	recover(context.Context, *rundirectory.StaleRun) error
	open(context.Context, *rundirectory.LiveRun,
		map[transfernumber.Number]networkcatalog.Egress) (*hostnetwork.Session, error)
	close(context.Context, *hostnetwork.Session) error
}

type kernelHostNetworkLifecycle struct{ owner *hostnetwork.Owner }

func (networks kernelHostNetworkLifecycle) recover(ctx context.Context,
	run *rundirectory.StaleRun,
) error {
	return networks.owner.Recover(ctx, run)
}

func (networks kernelHostNetworkLifecycle) open(ctx context.Context, run *rundirectory.LiveRun,
	egresses map[transfernumber.Number]networkcatalog.Egress,
) (*hostnetwork.Session, error) {
	return networks.owner.Open(ctx, run, egresses)
}

func (kernelHostNetworkLifecycle) close(ctx context.Context, session *hostnetwork.Session) error {
	return session.Close(ctx)
}
