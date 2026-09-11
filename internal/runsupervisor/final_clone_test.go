package runsupervisor

import (
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestFinalCloneIsolatesEveryReferenceMember(t *testing.T) {
	number, _ := transfernumber.New(1)
	original := Final{
		Source:    &SourceSummary{FollowedSymlinks: 4},
		Transfers: []TransferResult{{Transfer: number, ExitCode: 7}},
	}
	first := original.Clone()
	second := original.Clone()
	first.Source.FollowedSymlinks = 99
	first.Transfers[0].ExitCode = 42
	if original.Source.FollowedSymlinks != 4 || original.Transfers[0].ExitCode != 7 ||
		second.Source.FollowedSymlinks != 4 || second.Transfers[0].ExitCode != 7 {
		t.Fatalf("Final.Clone shared state: original=%#v first=%#v second=%#v", original, first, second)
	}

	event, err := NewFinalEvent(original)
	if err != nil {
		t.Fatal(err)
	}
	eventFinal := event.Final()
	eventFinal.Source.FollowedSymlinks = 100
	eventFinal.Transfers[0].ExitCode = 100
	if got := event.Final(); got.Source.FollowedSymlinks != 4 || got.Transfers[0].ExitCode != 7 {
		t.Fatalf("Event.Final shared state: %#v", got)
	}
}
