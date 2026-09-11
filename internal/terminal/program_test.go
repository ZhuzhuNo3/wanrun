package terminal

import (
	"errors"
	"io"
	"net/netip"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	term "github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type programControl struct {
	mu      sync.Mutex
	resizes [][2]uint16
}

func (*programControl) WriteInput(transfernumber.Number, []byte) error { return nil }

func (control *programControl) Resize(_ transfernumber.Number, columns, rows uint16) error {
	control.mu.Lock()
	defer control.mu.Unlock()
	control.resizes = append(control.resizes, [2]uint16{columns, rows})
	return nil
}

type shortTerminalWriter struct{}

func (shortTerminalWriter) Write(content []byte) (int, error) {
	return max(0, len(content)-1), nil
}

func TestProgramRejectsIncompleteInputsAndClosesPreparedDisplay(t *testing.T) {
	display := newInteractiveFixture(t, 1, MouseScroll, 80, 24)
	if _, err := NewProgram(display, nil, nil); err == nil || !display.closed {
		t.Fatalf("error=%v display closed=%t", err, display.closed)
	}
	if err := display.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestTerminalWriteRejectsPartialSnapshot(t *testing.T) {
	err := writeTerminalBytes(shortTerminalWriter{}, []byte("snapshot"), "write final command snapshots")
	if !errors.Is(err, io.ErrShortWrite) ||
		!strings.Contains(err.Error(), "write final command snapshots") {
		t.Fatalf("partial snapshot write error=%v", err)
	}
}

func TestProgramRestoresRawTerminalAfterCompleteProjection(t *testing.T) {
	master, slave, program := openProgramPTY(t, 80, 24)
	defer master.Close()
	defer slave.Close()
	go func() { _, _ = io.Copy(io.Discard, master) }()
	before, err := term.GetState(slave.Fd())
	if err != nil {
		t.Fatal(err)
	}
	outputDone := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- program.Run(&programControl{}, outputDone, make(chan struct{}), func(TerminationRequest) {})
	}()
	if err := program.Display().Begin(); err != nil {
		t.Fatal(err)
	}
	id, _ := transfernumber.New(1)
	if !program.Finish(&runsupervisor.Final{
		Transfers: []runsupervisor.TransferResult{{Transfer: id}}}, true) {
		t.Fatal("finish projection was rejected")
	}
	close(outputDone)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("program did not finish")
	}
	after, err := term.GetState(slave.Fd())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("outer terminal state was not restored")
	}
}

func TestProgramHandsDisplayRequestFailureToOutputOwner(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	readOnly, writable, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	defer writable.Close()
	id, _ := transfernumber.New(1)
	transfer, _ := NewTransfer(id, netip.MustParseAddr("192.0.2.1"))
	display, err := NewInteractiveTerminal([]Transfer{transfer}, 80, 24, MouseScroll)
	if err != nil {
		t.Fatal(err)
	}
	program, err := NewProgram(display, slave, readOnly)
	if err != nil {
		t.Fatal(err)
	}
	outputDone := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- program.Run(&programControl{}, outputDone, make(chan struct{}),
			func(TerminationRequest) {})
	}()
	displayErr := program.Display().Begin()
	select {
	case runErr := <-done:
		if displayErr == nil || !strings.Contains(displayErr.Error(), "enter alternate screen") {
			t.Fatalf("display request error=%v", displayErr)
		}
		if runErr == nil || strings.Contains(runErr.Error(), "enter alternate screen") ||
			!strings.Contains(runErr.Error(), "leave alternate screen") {
			t.Fatalf("driver result=%v, want only independent restoration failure", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("program did not finish after display request failure")
	}
}

func TestProgramResizeWakeupResamplesOuterTerminal(t *testing.T) {
	master, slave, program := openProgramPTY(t, 80, 24)
	defer master.Close()
	defer slave.Close()
	go func() { _, _ = io.Copy(io.Discard, master) }()
	control := &programControl{}
	resize := make(chan struct{}, 1)
	outputDone := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- program.Run(control, outputDone, resize, func(TerminationRequest) {}) }()
	if err := program.Display().Begin(); err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(slave, &pty.Winsize{Cols: 100, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	resize <- struct{}{}
	deadline := time.Now().Add(time.Second)
	for {
		control.mu.Lock()
		seen := len(control.resizes) != 0
		var got [2]uint16
		if seen {
			got = control.resizes[len(control.resizes)-1]
		}
		control.mu.Unlock()
		if seen {
			if got != [2]uint16{100, 25} {
				t.Fatalf("resize=%v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("resize command not observed")
		}
		time.Sleep(time.Millisecond)
	}
	program.Finish(nil, false)
	close(outputDone)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestOwnedInputStopDoesNotCloseCallerTTY(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	_, stop, err := startOwnedInput(master)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	var info unix.Stat_t
	if err := unix.Fstat(int(master.Fd()), &info); err != nil {
		t.Fatalf("caller-owned terminal was closed: %v", err)
	}
}

func TestOwnedInputStopInterruptsBackpressuredDelivery(t *testing.T) {
	events := make(chan inputEvent)
	stop := make(chan struct{})
	done := make(chan bool, 1)
	go func() { done <- deliverInputEvent(events, inputEvent{content: []byte("blocked")}, stop) }()
	close(stop)
	select {
	case delivered := <-done:
		if delivered {
			t.Fatal("backpressured input was reported delivered during stop")
		}
	case <-time.After(time.Second):
		t.Fatal("backpressured input delivery blocked terminal shutdown")
	}
}

func openProgramPTY(t *testing.T, columns, rows uint16) (*os.File, *os.File, *Program) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(slave, &pty.Winsize{Cols: columns, Rows: rows}); err != nil {
		master.Close()
		slave.Close()
		t.Fatal(err)
	}
	id, _ := transfernumber.New(1)
	transfer, _ := NewTransfer(id, netip.MustParseAddr("192.0.2.1"))
	display, err := NewInteractiveTerminal([]Transfer{transfer}, int(columns), int(rows), MouseScroll)
	if err != nil {
		master.Close()
		slave.Close()
		t.Fatal(err)
	}
	program, err := NewProgram(display, slave, slave)
	if err != nil {
		master.Close()
		slave.Close()
		t.Fatal(err)
	}
	return master, slave, program
}
