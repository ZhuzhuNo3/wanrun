//go:build linux

package runsupervisor

import (
	"bytes"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestControlSenderValidatesBeforeEveryStoppedState(t *testing.T) {
	transfer, _ := transfernumber.New(1)
	states := []struct {
		name   string
		sender *controlSender
	}{
		{name: "stopped", sender: &controlSender{state: controlStopped}},
		{name: "closed", sender: &controlSender{state: controlClosed}},
	}
	for _, state := range states {
		t.Run(state.name, func(t *testing.T) {
			if err := state.sender.resize(transfernumber.Number{}, 80, 24); err == nil {
				t.Fatal("invalid resize was hidden by stopped control state")
			}
			if err := state.sender.writeInput(transfernumber.Number{}, []byte("x")); err == nil {
				t.Fatal("invalid input was hidden by stopped control state")
			}
			if err := state.sender.resize(transfer, 80, 24); err != nil {
				t.Fatalf("valid stopped resize = %v", err)
			}
			if err := state.sender.writeInput(transfer, []byte("x")); err != nil {
				t.Fatalf("valid stopped input = %v", err)
			}
		})
	}
}

func TestControlSenderConcurrentCancelWritesOneCancellation(t *testing.T) {
	cancelRead, cancelWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFiles(cancelRead, cancelWrite) })
	sender := newControlSender(nil, cancelWrite, nil)
	const callers = 8
	results := make(chan error, callers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			results <- sender.cancel(CancelUser)
		}()
	}
	close(start)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	transfer, _ := transfernumber.New(1)
	if err := sender.resize(transfer, 80, 24); err != nil {
		t.Fatalf("valid control while cancelling = %v", err)
	}
	if err := sender.closeLifeline(); err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(cancelRead)
	if err != nil {
		t.Fatal(err)
	}
	want, err := encodeFrame(frameCancel, []byte{byte(CancelUser)})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(contents, want) {
		t.Fatalf("concurrent cancellation bytes = %x, want %x", contents, want)
	}
}

func TestControlSenderReportsTransportFailureOnlyWhileOpen(t *testing.T) {
	controlRead, controlWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	wake, err := controlWriteWake()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeFiles(controlRead, controlWrite, wake) })
	if err := controlRead.Close(); err != nil {
		t.Fatal(err)
	}
	transfer, _ := transfernumber.New(1)
	sender := newControlSender(controlWrite, nil, wake)
	if err := sender.resize(transfer, 80, 24); err == nil {
		t.Fatal("open control sender hid a transport failure")
	}
	sender.stop()
	if err := sender.resize(transfer, 80, 24); err != nil {
		t.Fatalf("stopped control sender leaked a transport failure: %v", err)
	}
}
