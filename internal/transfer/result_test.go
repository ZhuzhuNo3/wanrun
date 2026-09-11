package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

func TestTransferResultJoinRejectsDuplicateMissingAndUnknownTransfers(t *testing.T) {
	first, _ := transfernumber.New(1)
	second, _ := transfernumber.New(2)
	third, _ := transfernumber.New(3)
	selected := []selectedNetwork{{transfer: first}, {transfer: second}}
	for name, results := range map[string][]childprocesses.TransferResult{
		"duplicate": {{Transfer: first}, {Transfer: first}},
		"missing":   {{Transfer: first}},
		"unknown":   {{Transfer: first}, {Transfer: third}},
	} {
		t.Run(name, func(t *testing.T) {
			ordered, err := orderedTransferResults(selected, results)
			if err == nil {
				t.Fatal("invalid child process result set was accepted")
			}
			if len(ordered) != 0 {
				t.Fatalf("rejected child process results were exposed as trusted evidence: %#v", ordered)
			}
		})
	}
}

func TestTransferResultPreservesIgnoredSourceSummaryAfterSnapshot(t *testing.T) {
	source := sourceTree(t)
	if err := os.Symlink("large.bin", filepath.Join(source, "ignored-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "ignored-empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(source, "ignored-fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := validManualRequest(t, source)
	selectedNetworks := request.selectedNetworks()
	for _, test := range []struct {
		name   string
		result childprocesses.Result
	}{
		{name: "success", result: successfulProcessResult(request)},
		{name: "transfer failure", result: childprocesses.Result{Transfers: []childprocesses.TransferResult{
			{Transfer: selectedNetworks[0].transfer}, {Transfer: selectedNetworks[1].transfer, ExitCode: 7}}}},
		{name: "cancellation", result: childprocesses.Result{Cancelled: true,
			Transfers: successfulProcessResult(request).Transfers}},
	} {
		t.Run(test.name, func(t *testing.T) {
			commands := &observedCommandGroupStarter{result: test.result}
			directory := newResultTransferDirectory(t, &observedEgressResolver{}, &observedHostNetworkOwner{}, &observedWeightMeasurer{},
				commands, time.Second)
			result := runTransfer(t, directory.TransferDirectory, context.Background(), source, request)
			summary, present := result.SourceSummary()
			if !present || summary.IgnoredSymlinks() != 1 || summary.EmptyDirectories() != 1 ||
				summary.IgnoredSpecialFiles() != 1 {
				t.Fatalf("source summary present=%t value=%#v", present, summary)
			}
		})
	}
}

func TestTransferResultOmitsSourceSummaryBeforeSnapshot(t *testing.T) {
	roots, authority := isolatedRunRoots(t)
	networks := &observedHostNetworkOwner{recoverErr: errors.New("stale evidence is ambiguous")}
	views := newObservedFileViews()
	directory := newTransferDirectory(roots, &observedEgressResolver{}, networks, newObservedResolvers(t), &observedWeightMeasurer{},
		views, &observedCommandGroupStarter{}, time.Second)
	id, _ := runid.New()
	live, err := roots.Create(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	source := sourceTree(t)
	result := runTransfer(t, directory, context.Background(), source, validManualRequest(t, source))
	if _, present := result.SourceSummary(); present {
		t.Fatalf("source summary was fabricated before scan: %#v", result)
	}
	networks.recoverErr = nil
	callbacks, err := recoverExactRun(roots, networks, views, id)
	if err != nil || callbacks != 1 {
		t.Fatalf("recover exact stale run: callbacks=%d error=%v", callbacks, err)
	}
	assertExactRunAbsent(t, authority, id)
}

func TestResultAccessorsAndDescriptorPathAreImmutableValues(t *testing.T) {
	first, _ := transfernumber.New(1)
	value := Result{transfers: []childprocesses.TransferResult{{Transfer: first, ExitCode: 3}}}
	got := value.Transfers()
	got[0].ExitCode = 0
	if reflect.DeepEqual(got, value.Transfers()) {
		t.Fatal("result transfer evidence was mutable through accessor")
	}
	run, _ := runid.Parse("00112233445566778899aabbccddeeff")
	viewPath, err := childprocesses.DescriptorBoundViewPath(run, first, "captured-source")
	if err != nil {
		t.Fatal(err)
	}
	argv, err := replaceViewArgument([]string{"copy", "{}"}, viewPath)
	if err != nil || !reflect.DeepEqual(argv, []string{"copy", viewPath}) {
		t.Fatalf("descriptor-bound argv=%q, %v", argv, err)
	}
}

func newResultTransferDirectory(t *testing.T, egresses egressResolver, networks hostNetworkLifecycle,
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
