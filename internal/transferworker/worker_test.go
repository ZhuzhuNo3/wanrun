package transferworker

import (
	"net/netip"
	"slices"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfer"
	"github.com/ZhuzhuNo3/transferlanes/internal/transferstart"
)

func TestChildEnvironmentReplacesEveryTERMWithOneCanonicalValue(t *testing.T) {
	got := childEnvironment([]string{"PATH=/usr/bin", "TERM=old", "HOME=/tmp", "TERM=older"})
	want := []string{"PATH=/usr/bin", "HOME=/tmp", "TERM=xterm-256color"}
	if !slices.Equal(got, want) {
		t.Fatalf("child environment=%q, want %q", got, want)
	}
}

func TestTransferRequestRestoresRecordOrderAsContinuousRuntimeIdentity(t *testing.T) {
	first, err := transferstart.NewManualNetwork(netip.MustParseAddr("192.0.2.10"), 3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := transferstart.NewManualNetwork(netip.MustParseAddr("192.0.2.11"), 1)
	if err != nil {
		t.Fatal(err)
	}
	request, err := transferstart.NewManualRequest("/source", []transferstart.ManualNetwork{first, second}, nil,
		[]string{"copy", "{}"}, transferstart.Pipes(), false)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := transferRequest(request, []string{"PATH=/usr/bin", "TERM=xterm-256color"})
	if err != nil {
		t.Fatalf("ordered start records did not form one valid runtime request: %v", err)
	}
	if intent.CommandIO() != transfer.PlainCommandIO {
		t.Fatalf("runtime command I/O=%v, want pipes", intent.CommandIO())
	}
}
