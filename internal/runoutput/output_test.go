package runoutput

import (
	"bytes"
	"errors"
	"io"
	"net/netip"
	"slices"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/plainoutput"
	"github.com/ZhuzhuNo3/transferlanes/internal/runlogs"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type observedRunSupervisorOutput struct {
	events []runsupervisor.Event
}

func (client *observedRunSupervisorOutput) Next() (runsupervisor.Event, error) {
	if len(client.events) == 0 {
		return runsupervisor.Event{}, io.EOF
	}
	event := client.events[0]
	client.events = client.events[1:]
	return event, nil
}

type orderedRawLogs struct {
	order *[]string
	err   error
}

type exactRawLogs struct{ content []byte }

func (logs *exactRawLogs) Write(_ transfernumber.Number, _ runlogs.Stream, content []byte) error {
	logs.content = append(logs.content, content...)
	return nil
}

func (logs *orderedRawLogs) Write(_ transfernumber.Number, _ runlogs.Stream, content []byte) error {
	*logs.order = append(*logs.order, "log:"+string(content))
	return logs.err
}

type orderedDisplay struct {
	order    *[]string
	beginErr error
	err      error
}

func (display *orderedDisplay) Begin() error {
	*display.order = append(*display.order, "begin")
	return display.beginErr
}

func TestRunOutputRequestsOnlyOneTerminationAcrossMultipleActiveFailures(t *testing.T) {
	id, _ := transfernumber.New(1)
	outputEvent, _ := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStdout, []byte("after-begin"))
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{Cancelled: true, Reason: runsupervisor.CancelInternal,
		Transfers: []runsupervisor.TransferResult{{Transfer: id, ExitCode: -1, Signal: 15}}})
	client := &observedRunSupervisorOutput{events: []runsupervisor.Event{outputEvent, final}}
	var order []string
	displayFailure := errors.New("begin failed")
	logFailure := errors.New("log failed")
	notifications := 0
	result := readOutput(client, &orderedRawLogs{order: &order, err: logFailure},
		&orderedDisplay{order: &order, beginErr: displayFailure}, &notifications)
	finalResult, received := result.Final()
	if !errors.Is(result.DisplayError(), displayFailure) || !errors.Is(result.LogError(), logFailure) ||
		notifications != 1 || !received || result.DisplayEventsComplete() || !finalResult.Cancelled {
		t.Fatalf("result=%#v notifications=%d", result, notifications)
	}
}

func TestRunOutputReportsReadFailureAndRequestsTerminationBeforeFinal(t *testing.T) {
	client := &observedRunSupervisorOutput{}
	var order []string
	notifications := 0
	result := readOutput(client, nil, &orderedDisplay{order: &order}, &notifications)
	_, received := result.Final()
	if !errors.Is(result.ReadError(), io.EOF) || notifications != 1 || received || result.DisplayEventsComplete() {
		t.Fatalf("result=%#v notifications=%d", result, notifications)
	}
}

func (display *orderedDisplay) Show(event runsupervisor.Event) error {
	value := "status"
	if event.Kind() == runsupervisor.EventOutput {
		value = "display:" + string(event.Bytes())
	}
	*display.order = append(*display.order, value)
	return display.err
}

func TestRunOutputWritesRawLogBeforeTheOnlyDisplay(t *testing.T) {
	id, _ := transfernumber.New(1)
	first, _ := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStdout, []byte("first"))
	status, _ := runsupervisor.NewStatusEvent(runsupervisor.TransferStatus{Transfer: id, State: runsupervisor.TransferRunning})
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{Transfers: []runsupervisor.TransferResult{{Transfer: id}}})
	client := &observedRunSupervisorOutput{events: []runsupervisor.Event{first, status, final}}
	var order []string
	notifications := 0
	result := readOutput(client, &orderedRawLogs{order: &order}, &orderedDisplay{order: &order}, &notifications)
	finalResult, received := result.Final()
	if result.Failure() != nil || notifications != 0 || !received ||
		!result.DisplayEventsComplete() || len(finalResult.Transfers) != 1 {
		t.Fatalf("result=%#v notifications=%d", result, notifications)
	}
	if want := []string{"begin", "log:first", "display:first", "status"}; !slices.Equal(order, want) {
		t.Fatalf("order=%v, want %v", order, want)
	}
}

func TestRunOutputNotifiesOnceAndDrainsThroughFinalAfterLogFailure(t *testing.T) {
	id, _ := transfernumber.New(1)
	first, _ := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStderr, []byte("lost-display"))
	remaining, _ := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStderr, []byte("remaining"))
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{Cancelled: true, Reason: runsupervisor.CancelInternal,
		Transfers: []runsupervisor.TransferResult{{Transfer: id, Signal: 15, ExitCode: -1}}})
	client := &observedRunSupervisorOutput{events: []runsupervisor.Event{first, remaining, final}}
	var order []string
	failure := errors.New("disk full")
	notifications := 0
	result := readOutput(client, &orderedRawLogs{order: &order, err: failure},
		&orderedDisplay{order: &order}, &notifications)
	finalResult, _ := result.Final()
	if !errors.Is(result.Failure(), failure) || !errors.Is(result.LogError(), failure) || !finalResult.Cancelled ||
		result.DisplayEventsComplete() || notifications != 1 {
		t.Fatalf("result=%#v notifications=%d", result, notifications)
	}
	if want := []string{"begin", "log:lost-display"}; !slices.Equal(order, want) {
		t.Fatalf("order after fatal log failure=%v, want %v", order, want)
	}
}

func TestRunOutputKeepsLoggingBufferedOutputAfterDisplayFailure(t *testing.T) {
	id, _ := transfernumber.New(1)
	first, _ := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStdout, []byte("first"))
	remaining, _ := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStdout, []byte("remaining"))
	final, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{Cancelled: true, Reason: runsupervisor.CancelInternal,
		Transfers: []runsupervisor.TransferResult{{Transfer: id, Signal: 15, ExitCode: -1}}})
	client := &observedRunSupervisorOutput{events: []runsupervisor.Event{first, remaining, final}}
	var order []string
	failure := errors.New("stdout failed")
	notifications := 0
	result := readOutput(client, &orderedRawLogs{order: &order},
		&orderedDisplay{order: &order, err: failure}, &notifications)
	finalResult, _ := result.Final()
	if !errors.Is(result.Failure(), failure) || !errors.Is(result.DisplayError(), failure) ||
		!finalResult.Cancelled || result.DisplayEventsComplete() || notifications != 1 {
		t.Fatalf("result=%#v notifications=%d", result, notifications)
	}
	if want := []string{"begin", "log:first", "display:first", "log:remaining"}; !slices.Equal(order, want) {
		t.Fatalf("order after display failure=%v, want %v", order, want)
	}
}

func TestRunOutputDoesNotGiveSanitizedBytesToRawLogs(t *testing.T) {
	id, _ := transfernumber.New(1)
	raw := []byte("中\x1b(0文\xc2\x9b31m🙂\xc2\x9b0m\n")
	outputEvent, _ := runsupervisor.NewOutputEvent(id, runsupervisor.OutputStdout, raw)
	finalEvent, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{
		Transfers: []runsupervisor.TransferResult{{Transfer: id}}})
	client := &observedRunSupervisorOutput{events: []runsupervisor.Event{outputEvent, finalEvent}}
	logs := &exactRawLogs{}
	transfer, err := plainoutput.NewTransfer(id, netip.MustParseAddr("192.0.2.1"))
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	display, err := plainoutput.New(&stdout, io.Discard, []plainoutput.Transfer{transfer})
	if err != nil {
		t.Fatal(err)
	}
	notifications := 0
	result := readOutput(client, logs, display, &notifications)
	if closeErr := display.Close(); result.Failure() != nil || closeErr != nil {
		t.Fatalf("output=%v close=%v", result.Failure(), closeErr)
	}
	if !result.DisplayEventsComplete() {
		t.Fatal("successful display delivery was reported incomplete")
	}
	if !bytes.Equal(logs.content, raw) {
		t.Fatalf("raw log=%q, want byte-exact %q", logs.content, raw)
	}
	if got := stdout.String(); got != "transferlanes: starting 1 transfers\n[1 192.0.2.1] 中文🙂\n" {
		t.Fatalf("plain output=%q", got)
	}
}

func TestRunOutputFinalGetterDeepClonesAllReferenceMembers(t *testing.T) {
	id, _ := transfernumber.New(1)
	finalEvent, _ := runsupervisor.NewFinalEvent(runsupervisor.Final{
		Transfers: []runsupervisor.TransferResult{{Transfer: id}},
		Source:    &runsupervisor.SourceSummary{FollowedSymlinks: 3},
	})
	client := &observedRunSupervisorOutput{events: []runsupervisor.Event{finalEvent}}
	var order []string
	result := readOutput(client, nil, &orderedDisplay{order: &order}, new(int))
	first, ok := result.Final()
	if !ok {
		t.Fatal("Final is absent")
	}
	first.Transfers[0].ExitCode = 42
	first.Source.FollowedSymlinks = 99
	second, _ := result.Final()
	if second.Transfers[0].ExitCode != 0 || second.Source.FollowedSymlinks != 3 {
		t.Fatalf("saved Final mutated through getter: %+v", second)
	}
}

func readOutput(source eventSource, logs rawLogWriter, display Display, notifications *int) Result {
	completion := Start(source, logs, display, func() { *notifications++ })
	<-completion.Done()
	return completion.Result()
}
