package transfer

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/namespaceresolvers"
	"github.com/ZhuzhuNo3/transferlanes/internal/networkcatalog"
	"github.com/ZhuzhuNo3/transferlanes/internal/rundirectory"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

func waitForResolverClose(t *testing.T, resolvers *observedResolvers) {
	t.Helper()
	waitForTestSignal(t, resolvers.closeDone, time.Second, "namespace resolver cleanup")
	if resolvers.closed.Load() != 1 {
		t.Fatalf("namespace resolver cleanup count=%d", resolvers.closed.Load())
	}
}

type isolatedTransferDirectory struct {
	*TransferDirectory
	roots     *rundirectory.Owner
	authority string
	resolvers *observedResolvers
	views     *observedFileViews
}

func runTransfer(t *testing.T, transfer *TransferDirectory, ctx context.Context, source string,
	request Request,
) Result {
	t.Helper()
	directory, err := sourcefiles.OpenSourceRoot(source)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	return transfer.Run(ctx, directory, request)
}

func isolatedRunRoots(t *testing.T) (*rundirectory.Owner, string) {
	t.Helper()
	roots, authority, err := rundirectory.CreatePrivateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := roots.ClosePrivateAuthority(context.Background()); err != nil {
			t.Errorf("close private authority: %v", err)
		}
	})
	return roots, authority
}

func recoverExactRun(roots *rundirectory.Owner, networks hostNetworkLifecycle,
	views fileViewMounts, id runid.ID,
) (int, error) {
	callbacks := 0
	err := roots.RecoverStale(context.Background(), func(stale *rundirectory.StaleRun) error {
		if stale.ID() != id {
			return errors.New("unexpected stale run")
		}
		callbacks++
		if err := networks.recover(context.Background(), stale); err != nil {
			return err
		}
		return views.recover(context.Background(), stale)
	})
	return callbacks, err
}

func assertRunRootAbsent(t *testing.T, authority string, result Result) {
	t.Helper()
	id, ok := result.RunID()
	if !ok {
		t.Fatal("result has no run identity")
	}
	assertPathAbsent(t, filepath.Join(authority, id.String()))
	assertPathAbsent(t, filepath.Join(rundirectory.AuthorityRoot, id.String()))
}

func assertExactRunAbsent(t *testing.T, authority string, id runid.ID) {
	t.Helper()
	assertPathAbsent(t, filepath.Join(authority, id.String()))
	assertPathAbsent(t, filepath.Join(rundirectory.AuthorityRoot, id.String()))
}

func waitForTransferredRunLivenessRelease(t *testing.T, authority string, id runid.ID) {
	t.Helper()
	liveness, err := os.Open(filepath.Join(authority, id.String(), "liveness.lock"))
	if err != nil {
		t.Fatalf("open exact run liveness: %v", err)
	}
	locked := make(chan error, 1)
	go func() {
		err := unix.Flock(int(liveness.Fd()), unix.LOCK_EX)
		if err == nil {
			err = unix.Flock(int(liveness.Fd()), unix.LOCK_UN)
		}
		locked <- errors.Join(err, liveness.Close())
	}()
	select {
	case err := <-locked:
		if err != nil {
			t.Fatalf("observe exact run liveness release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("exact run liveness was not released after owner cleanup")
	}
}

func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path %q remains: %v", path, err)
	}
}

func sourceTree(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "captured-source")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, contents := range map[string]string{"large.bin": "12345678", ".hidden": "abc", "small": "x"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func emptySource(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "empty-source")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

func validManualRequest(t *testing.T, source string) Request {
	t.Helper()
	first, _ := transfernumber.New(1)
	second, _ := transfernumber.New(2)
	firstSelection, _ := NewManualNetworkSelection(first, netip.MustParseAddr("192.0.2.10"), 2)
	secondSelection, _ := NewManualNetworkSelection(second, netip.MustParseAddr("192.0.2.11"), 1)
	size, _ := childprocesses.NewTerminalSize(80, 24)
	request, err := NewManualRequest(source, []ManualNetworkSelection{firstSelection, secondSelection},
		[]string{"copy", "--source", "{}"}, []string{"PATH=/usr/bin"}, namespaceresolvers.DefaultIntent(), size, false)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func validAutomaticRequest(t *testing.T, source string) Request {
	t.Helper()
	first, _ := transfernumber.New(1)
	second, _ := transfernumber.New(2)
	firstSelection, _ := NewAutomaticNetworkSelection(first, netip.MustParseAddr("192.0.2.10"))
	secondSelection, _ := NewAutomaticNetworkSelection(second, netip.MustParseAddr("192.0.2.11"))
	size, _ := childprocesses.NewTerminalSize(80, 24)
	window, _ := throughput.NewSettings("https://measure.example/upload", time.Second)
	request, err := NewAutomaticRequest(source, []AutomaticNetworkSelection{firstSelection, secondSelection},
		[]string{"copy", "--source", "{}"}, []string{"PATH=/usr/bin"}, namespaceresolvers.DefaultIntent(), size, window, false)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func successfulProcessResult(request Request) childprocesses.Result {
	selectedNetworks := request.selectedNetworks()
	results := make([]childprocesses.TransferResult, len(selectedNetworks))
	for index, selected := range selectedNetworks {
		results[index] = childprocesses.TransferResult{Transfer: selected.transfer}
	}
	return childprocesses.Result{Transfers: results}
}

type observedEgressResolver struct {
	calls int
	err   error
}

func (resolver *observedEgressResolver) resolve(_ context.Context, selected []selectedNetwork) (
	map[transfernumber.Number]networkcatalog.Egress, error,
) {
	resolver.calls++
	if resolver.err != nil {
		return nil, resolver.err
	}
	result := make(map[transfernumber.Number]networkcatalog.Egress, len(selected))
	for _, value := range selected {
		result[value.transfer] = networkcatalog.Egress{}
	}
	return result, nil
}

type observedHostNetworkOwner struct {
	recovered     int
	recoverErr    error
	opened        int
	openErr       error
	closed        int
	closeErr      error
	closeDelay    time.Duration
	closeContexts int
	closeDone     chan struct{}
	closeOnce     sync.Once
}

type observedResolvers struct {
	resolverPath  string
	openErr       error
	diagnosticErr error
	closed        atomic.Int32
	closeCalls    atomic.Int32
	closeDelay    time.Duration
	closeErr      error
	stopped       bool
	closeFunc     func(context.Context, int32) (bool, error)
	closeDone     chan struct{}
	closeOnce     sync.Once
}

func newObservedResolvers(t *testing.T) *observedResolvers {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("nameserver 192.0.2.53\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &observedResolvers{resolverPath: path, stopped: true, closeDone: make(chan struct{})}
}

func (resolvers *observedResolvers) open(context.Context, *hostnetwork.Session,
	[]selectedNetwork, namespaceresolvers.Intent) (resolverGroup, error) {
	if resolvers.openErr != nil {
		return nil, resolvers.openErr
	}
	return resolvers, nil
}

func (resolvers *observedResolvers) Access(transfernumber.Number) (namespaceresolvers.ResolverAccess, error) {
	return observedResolverAccess{path: resolvers.resolverPath}, nil
}

func (resolvers *observedResolvers) DiagnosticError() error { return resolvers.diagnosticErr }

func (resolvers *observedResolvers) Close(ctx context.Context) (bool, error) {
	call := resolvers.closeCalls.Add(1)
	var stopped bool
	var err error
	if resolvers.closeFunc != nil {
		stopped, err = resolvers.closeFunc(ctx, call)
	} else {
		err = waitDelay(ctx, resolvers.closeDelay)
		stopped = resolvers.stopped && err == nil
	}
	if stopped {
		resolvers.closed.Add(1)
		resolvers.closeOnce.Do(func() { close(resolvers.closeDone) })
	}
	return stopped, errors.Join(err, resolvers.closeErr)
}

type observedResolverAccess struct{ path string }

func (access observedResolverAccess) Open() (*os.File, error) { return os.Open(access.path) }

func (networks *observedHostNetworkOwner) recover(_ context.Context, _ *rundirectory.StaleRun) error {
	networks.recovered++
	return networks.recoverErr
}

func (networks *observedHostNetworkOwner) open(_ context.Context, _ *rundirectory.LiveRun,
	_ map[transfernumber.Number]networkcatalog.Egress,
) (*hostnetwork.Session, error) {
	networks.opened++
	return nil, networks.openErr
}

func (networks *observedHostNetworkOwner) close(ctx context.Context, _ *hostnetwork.Session) error {
	networks.closeContexts++
	if err := waitDelay(ctx, networks.closeDelay); err != nil {
		return err
	}
	if networks.closeErr == nil {
		networks.closed++
		if networks.closeDone != nil {
			networks.closeOnce.Do(func() { close(networks.closeDone) })
		}
	}
	return networks.closeErr
}

type observedWeightMeasurer struct {
	err   error
	owner supervisedProcesses
}

func (weights *observedWeightMeasurer) measure(_ context.Context, _ *hostnetwork.Session,
	request Request, _ resolverGroup, _ string,
) ([]sourcefiles.TransferWeight, supervisedProcesses, error) {
	if weights.err != nil {
		return nil, weights.owner, weights.err
	}
	selectedNetworks := request.selectedNetworks()
	result := make([]sourcefiles.TransferWeight, len(selectedNetworks))
	for index, selected := range selectedNetworks {
		result[index], _ = sourcefiles.NewTransferWeight(selected.transfer, uint64(index+1))
	}
	return result, nil, nil
}

type observedCommandGroupStarter struct {
	started    int
	executions []TransferExecution
	request    Request
	startErr   error
	startOwner supervisedProcesses
	group      *observedSupervisedProcesses
	result     childprocesses.Result
}

func (starter *observedCommandGroupStarter) start(_ context.Context, _ *hostnetwork.Session,
	_ resolverGroup, executions []TransferExecution, request Request,
) (supervisedProcesses, error) {
	starter.started++
	starter.executions = append([]TransferExecution(nil), executions...)
	starter.request = request
	if starter.startErr != nil {
		return starter.startOwner, starter.startErr
	}
	if starter.group == nil {
		done := make(chan struct{})
		close(done)
		starter.group = &observedSupervisedProcesses{done: done, result: starter.result}
	}
	return starter.group, nil
}

type observedSupervisedProcesses struct {
	done         chan struct{}
	result       childprocesses.Result
	waitErr      error
	confirmDelay time.Duration
}

func (processes *observedSupervisedProcesses) Wait() (childprocesses.Result, error) {
	return processes.result, processes.waitErr
}

func (processes *observedSupervisedProcesses) FinishStartFailure(ctx context.Context) childprocesses.StartFailureResult {
	finished := childprocesses.StartFailureResult{SupervisionError: processes.waitErr}
	select {
	case <-processes.done:
	case <-ctx.Done():
		finished.ContainmentError = ctx.Err()
	}
	return finished
}

func (processes *observedSupervisedProcesses) ConfirmContainment(ctx context.Context) error {
	if err := waitDelay(ctx, processes.confirmDelay); err != nil {
		return err
	}
	select {
	case <-processes.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (processes *observedSupervisedProcesses) ContainmentDone() <-chan struct{} {
	return processes.done
}

func waitDelay(ctx context.Context, delay time.Duration) error {
	if delay == 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}
