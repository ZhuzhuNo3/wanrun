package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
)

func TestReadOnlyFailureCreatesNoRunOrHostNetwork(t *testing.T) {
	for _, test := range []struct {
		name     string
		source   func(*testing.T) string
		egresses egressResolver
	}{
		{name: "catalog", source: sourceTree,
			egresses: &observedEgressResolver{err: errors.New("capture network")}},
		{name: "empty source", source: emptySource,
			egresses: &observedEgressResolver{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := test.source(t)
			networks := &observedHostNetworkOwner{}
			directory := newPreflightTransferDirectory(t, test.egresses, networks, &observedWeightMeasurer{},
				&observedCommandGroupStarter{}, time.Second)
			result := runTransfer(t, directory.TransferDirectory, context.Background(), source,
				validManualRequest(t, source))
			_, hasRun := result.RunID()
			if result.RunError() == nil || hasRun || networks.opened != 0 {
				t.Fatalf("read-only gate result=%#v network opens=%d", result, networks.opened)
			}
			if _, present := result.SourceSummary(); !present {
				t.Fatal("completed source scan summary was not retained")
			}
		})
	}
}

func TestFileViewPreflightFailureCreatesNoRunOrHostNetwork(t *testing.T) {
	source := sourceTree(t)
	roots, _ := isolatedRunRoots(t)
	egresses := &observedEgressResolver{}
	networks := &observedHostNetworkOwner{}
	views := newObservedFileViews()
	views.preflightErr = errors.New("FUSE prerequisite unavailable")
	directory := newTransferDirectory(roots, egresses, networks, newObservedResolvers(t),
		&observedWeightMeasurer{}, views, &observedCommandGroupStarter{}, time.Second)

	result := runTransfer(t, directory, context.Background(), source, validManualRequest(t, source))
	if !errors.Is(result.RunError(), views.preflightErr) || views.preflightCalls != 1 ||
		egresses.calls != 0 || networks.opened != 0 {
		t.Fatalf("preflight gate result=%v calls=%d catalog=%d network=%d", result.RunError(),
			views.preflightCalls, egresses.calls, networks.opened)
	}
	if _, present := result.SourceSummary(); present {
		t.Fatal("preflight failure unexpectedly captured the source")
	}
}

func TestStaleOwnerFailuresAreAggregatedAndPreserveRealRoot(t *testing.T) {
	roots, authority := isolatedRunRoots(t)
	id, _ := runid.New()
	live, err := roots.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	networkErr := errors.New("ambiguous network evidence")
	viewErr := errors.New("ambiguous file-view evidence")
	networks := &observedHostNetworkOwner{recoverErr: networkErr}
	views := newObservedFileViews()
	views.recoverErr = viewErr
	directory := newTransferDirectory(roots, &observedEgressResolver{}, networks, newObservedResolvers(t), &observedWeightMeasurer{},
		views, &observedCommandGroupStarter{}, time.Second)
	source := sourceTree(t)
	result := runTransfer(t, directory, context.Background(), source, validManualRequest(t, source))
	_, hasRun := result.RunID()
	if !errors.Is(result.RunError(), networkErr) || !errors.Is(result.RunError(), viewErr) ||
		hasRun || networks.opened != 0 || views.recoverCalls != 1 {
		t.Fatalf("stale failure crossed new-run gate: result=%#v opens=%d", result, networks.opened)
	}
	runRoot := filepath.Join(authority, id.String())
	if _, err := os.Stat(runRoot); err != nil {
		t.Fatalf("stale root was lost: %v", err)
	}
	networks.recoverErr = nil
	views.recoverErr = nil
	callbacks, err := recoverExactRun(roots, networks, views, id)
	if err != nil || callbacks != 1 {
		t.Fatalf("recover exact stale run: callbacks=%d error=%v", callbacks, err)
	}
	assertExactRunAbsent(t, authority, id)
}

func newPreflightTransferDirectory(t *testing.T, egresses egressResolver, networks hostNetworkLifecycle,
	weights weightMeasurer, commands commandGroupStarter, cleanup time.Duration,
) *isolatedTransferDirectory {
	t.Helper()
	roots, authority := isolatedRunRoots(t)
	resolvers := newObservedResolvers(t)
	views := newObservedFileViews()
	return &isolatedTransferDirectory{
		TransferDirectory: newTransferDirectory(roots, egresses, networks, resolvers, weights, views, commands, cleanup),
		roots:             roots, authority: authority, resolvers: resolvers, views: views,
	}
}
