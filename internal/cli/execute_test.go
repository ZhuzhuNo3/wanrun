package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/listnetworks"
	"github.com/ZhuzhuNo3/transferlanes/internal/runcommand"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
)

type listingFixture struct {
	calls  int
	result listnetworks.Result
	err    error
}

func (fixture *listingFixture) Run(context.Context,
	listnetworks.Request,
) (listnetworks.Result, error) {
	fixture.calls++
	return fixture.result, fixture.err
}

type runFixture struct {
	calls int
	argv  []string
	err   error
}

func (fixture *runFixture) Run(_ context.Context, request runcommand.Request, _ io.Reader,
	_, _ io.Writer,
) (runcommand.Result, error) {
	fixture.calls++
	fixture.argv = request.ChildArgv()
	return runcommand.Result{}, fixture.err
}

func TestExecuteRoutesPublicHelpListAndVersionWithoutTouchingOtherUseCases(t *testing.T) {
	for _, test := range []struct {
		name     string
		argv     []string
		wantText string
		wantList int
	}{
		{name: "root help", argv: []string{"--help"}, wantText: RootHelpPage.Text()},
		{name: "list help", argv: []string{"list", "--help"}, wantText: ListHelpPage.Text()},
		{name: "run help", argv: []string{"run", "--help"}, wantText: RunHelpPage.Text()},
		{name: "list", argv: []string{"list"}, wantText: "LOCAL_IP  IFACE  ROUTE_TABLE  GATEWAY  LOCAL_STATUS\n", wantList: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			listing := &listingFixture{}
			runner := &runFixture{err: errors.New("run must not be called")}
			versionCalls := 0
			var stdout, stderr bytes.Buffer
			code := execute(test.argv, strings.NewReader(""), &stdout, &stderr,
				listing, runner, func() (string, error) { versionCalls++; return "version", nil })
			if code != 0 || stdout.String() != test.wantText || stderr.Len() != 0 ||
				listing.calls != test.wantList || runner.calls != 0 || versionCalls != 0 {
				t.Fatalf("exit=%d stdout=%q stderr=%q list/run/version=%d/%d/%d", code,
					stdout.String(), stderr.String(), listing.calls, runner.calls, versionCalls)
			}
		})
	}

	var stdout, stderr bytes.Buffer
	listing, runner := &listingFixture{}, &runFixture{}
	versionCalls := 0
	code := execute([]string{"--version"}, strings.NewReader(""), &stdout, &stderr,
		listing, runner, func() (string, error) { versionCalls++; return "transferlanes test\n", nil })
	if code != 0 || stdout.String() != "transferlanes test\n" || stderr.Len() != 0 ||
		listing.calls != 0 || runner.calls != 0 || versionCalls != 1 {
		t.Fatalf("version exit/output/calls=%d/%q/%q/%d/%d/%d", code, stdout.String(),
			stderr.String(), listing.calls, runner.calls, versionCalls)
	}
}

func TestExecutePreservesUsageGuidance(t *testing.T) {
	tests := []struct {
		argv []string
		want string
	}{
		{nil, "transferlanes: a subcommand is required (list or run)\n\n" + RootHelpPage.Text()},
		{[]string{"unknown"}, "transferlanes: unknown subcommand unknown (expected list or run)\n\n" + RootHelpPage.Text()},
		{[]string{"list", "--unknown"}, "transferlanes: unknown list option --unknown\nRun 'transferlanes list --help' for usage.\n"},
		{[]string{"run", "copy", "{}"}, "transferlanes: --source is required\nRun 'transferlanes run --help' for usage.\n"},
	}
	for _, test := range tests {
		var stdout, stderr bytes.Buffer
		if code := execute(test.argv, strings.NewReader(""), &stdout, &stderr,
			nil, nil, nil); code != 2 || stdout.Len() != 0 || stderr.String() != test.want {
			t.Fatalf("argv=%q exit/stdout/stderr=%d/%q/%q", test.argv, code,
				stdout.String(), stderr.String())
		}
	}
	for _, test := range baselineRunUsageContracts() {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			want := "transferlanes: " + test.message + "\nRun 'transferlanes run --help' for usage.\n"
			if code := execute(test.argv, strings.NewReader(""), &stdout, &stderr,
				nil, nil, nil); code != 2 || stdout.Len() != 0 || stderr.String() != want {
				t.Fatalf("argv=%q exit/stdout/stderr=%d/%q/%q, want %q", test.argv, code,
					stdout.String(), stderr.String(), want)
			}
		})
	}
}

func TestExecuteRunKeepsOpaqueChildArgumentsAndCapacityPrecedesUseCase(t *testing.T) {
	runner := &runFixture{}
	argv := []string{"run", "--source", "/source", "--network", "192.0.2.1",
		"copy", "{}", "--network"}
	if code := execute(argv, strings.NewReader(""), io.Discard, io.Discard,
		nil, runner, nil); code != 1 || runner.calls != 1 ||
		!slices.Equal(runner.argv, []string{"copy", "{}", "--network"}) {
		t.Fatalf("exit/calls/argv=%d/%d/%q", code, runner.calls, runner.argv)
	}

	large := strings.Repeat("x", runsupervisor.StartRequestCapacity()/2)
	oversized := []string{"run", "--source", "/source", "--network", "192.0.2.1",
		"--", "copy", "{}", large, large, large}
	var stderr bytes.Buffer
	if code := execute(oversized, strings.NewReader(""), io.Discard, &stderr,
		nil, runner, nil); code != 2 || runner.calls != 1 ||
		!strings.Contains(stderr.String(), "supervisor start request") {
		t.Fatalf("oversize exit/calls/stderr=%d/%d/%q", code, runner.calls, stderr.String())
	}
}

func TestExecuteListUsesDynamicColumnsAndPreservesUseCaseErrorClass(t *testing.T) {
	listing := &listingFixture{}
	var stdout, stderr bytes.Buffer
	if code := execute([]string{"list", "--probe", "--measure"}, strings.NewReader(""),
		&stdout, &stderr, listing, nil, nil); code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	for _, heading := range []string{"PUBLIC_IP", "PROBE_STATUS", "MBPS", "WEIGHT", "MEASURE_STATUS"} {
		if !strings.Contains(stdout.String(), heading) {
			t.Fatalf("dynamic list headings omit %q: %q", heading, stdout.String())
		}
	}
	_, usageErr := listnetworks.NewRequest([]string{"invalid"}, false, nil, nil)
	listing.err = usageErr
	stderr.Reset()
	if code := execute([]string{"list"}, strings.NewReader(""), io.Discard, &stderr,
		listing, nil, nil); code != 2 || !strings.Contains(stderr.String(), "Run 'transferlanes list --help'") {
		t.Fatalf("usage exit/stderr=%d/%q", code, stderr.String())
	}
	listing.err = errors.New("catalog failed")
	stderr.Reset()
	if code := execute([]string{"list"}, strings.NewReader(""), io.Discard, &stderr,
		listing, nil, nil); code != 1 || !strings.Contains(stderr.String(), "list networks: catalog failed") {
		t.Fatalf("runtime exit/stderr=%d/%q", code, stderr.String())
	}
}

func TestExecuteUnavailableOrFailedUseCasesAreRuntimeErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		argv []string
		list networkListing
		run  runUseCase
		want string
	}{
		{name: "list absent", argv: []string{"list"}, want: "network list capability is unavailable"},
		{name: "run absent", argv: []string{"run", "--source", "/source", "--network", "192.0.2.1", "copy", "{}"},
			want: "supervised transfer capability is unavailable"},
		{name: "run failed", argv: []string{"run", "--source", "/source", "--network", "192.0.2.1", "copy", "{}"},
			run: &runFixture{err: errors.New("source failed")}, want: "source failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if code := execute(test.argv, strings.NewReader(""), io.Discard, &stderr,
				test.list, test.run, nil); code != 1 || !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("exit=%d stderr=%q", code, stderr.String())
			}
		})
	}
}
