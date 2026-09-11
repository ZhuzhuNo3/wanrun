//go:build linux

package childprocesses

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"github.com/charmbracelet/x/term"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func TestStoppingControlsInterruptsPendingPTYInput(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	oldState, err := term.MakeRaw(slave.Fd())
	if err != nil {
		_ = master.Close()
		t.Fatal(err)
	}
	defer term.Restore(slave.Fd(), oldState)
	id, _ := transfernumber.New(1)
	bound, err := bindProcessStream(id, preparedOutput{stream: StreamPTY, file: master})
	if err != nil {
		t.Fatal(err)
	}
	outputWake, controlWake := testProcessWakes(t)
	processIO := &processIO{acceptingControl: true, controlWake: controlWake,
		outputWake: outputWake, streams: []*processStream{bound},
		ptyByTransfer: map[transfernumber.Number]int{id: 0}}
	session := &ProcessSet{io: processIO, completion: newProcessCompletion(),
		terminator: newProcessTerminator()}
	written := make(chan error, 1)
	go func() {
		written <- session.WriteInput(id, bytes.Repeat([]byte{'x'}, 16*1024*1024))
	}()

	firstRead := make(chan error, 1)
	go func() {
		var first [1]byte
		_, err := slave.Read(first[:])
		firstRead <- err
	}()
	select {
	case err := <-firstRead:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-written:
		t.Fatalf("PTY input ended before its peer read a byte: %v", err)
	case <-time.After(time.Second):
		t.Fatal("PTY input did not become observable")
	}
	session.io.stopControls()
	select {
	case <-written:
	case <-time.After(time.Second):
		_ = slave.Close()
		<-written
		t.Fatal("closing the PTY owner did not interrupt its pending input write")
	}
	session.io.close()
}

func TestWaitInterruptsAndJoinsPendingPTYInput(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := term.MakeRaw(slave.Fd()); err != nil {
		_ = master.Close()
		_ = slave.Close()
		t.Fatal(err)
	}
	id, _ := transfernumber.New(1)
	child := &launchedChild{transfer: id, pid: 177, outputs: []preparedOutput{{
		stream: StreamPTY, file: master,
	}}}
	releaseReaper := make(chan struct{})
	waitCalls := 0
	reaper := startReaperWith([]*launchedChild{child}, func(status *unix.WaitStatus) (int, error) {
		if waitCalls == 0 {
			<-releaseReaper
			waitCalls++
			return child.pid, nil
		}
		return -1, unix.ECHILD
	})
	outputWake, controlWake := testProcessWakes(t)
	processes := newProcessSet(context.Background(), []*launchedChild{child}, reaper,
		outputWake, controlWake)
	written := make(chan error, 1)
	go func() {
		written <- processes.WriteInput(id, bytes.Repeat([]byte{'x'}, 16*1024*1024))
	}()
	var first [1]byte
	if _, err := slave.Read(first[:]); err != nil {
		t.Fatal(err)
	}
	close(releaseReaper)
	if err := slave.Close(); err != nil {
		t.Fatal(err)
	}

	waited := make(chan error, 1)
	go func() {
		_, err := processes.Wait()
		waited <- err
	}()
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Wait did not interrupt and join the pending PTY input writer")
	}
	select {
	case <-written:
	default:
		t.Fatal("Wait returned before the pending PTY input writer ended")
	}
}

func TestConfirmedContainmentDrainsEveryPTYBeforeClosingMasters(t *testing.T) {
	const transfers = 3
	first := bytes.Repeat([]byte{'a'}, 4096)
	tail := bytes.Repeat([]byte{'z'}, maximumOutputReadSize+2048)
	want := append(append([]byte(nil), first...), tail...)
	releaseContainment := make(chan struct{})
	children := make([]*launchedChild, transfers)
	writers := make([]*os.File, transfers)
	for index := range children {
		id, _ := transfernumber.New(index + 1)
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		children[index] = &launchedChild{transfer: id, pid: 100 + index,
			outputs: []preparedOutput{{stream: StreamPTY, file: reader}}}
		writers[index] = writer
		if _, err := writer.Write(first); err != nil {
			t.Fatal(err)
		}
	}
	reapIndex := 0
	reaper := startReaperWith(children, func(status *unix.WaitStatus) (int, error) {
		if reapIndex < len(children) {
			pid := children[reapIndex].pid
			reapIndex++
			return pid, nil
		}
		<-releaseContainment
		return -1, unix.ECHILD
	})
	session := newUnbufferedOutputSession(t, children, reaper)
	got := make(map[transfernumber.Number][]byte, transfers)
	for range children {
		event := <-session.Output()
		got[event.Transfer] = append(got[event.Transfer], event.Bytes...)
	}
	written := make(chan error, transfers)
	for _, writer := range writers {
		go func(writer *os.File) {
			_, err := writer.Write(tail)
			written <- errors.Join(err, writer.Close())
		}(writer)
	}
	writesComplete := make(chan error, 1)
	go func() {
		var failures []error
		for range writers {
			failures = append(failures, <-written)
		}
		close(releaseContainment)
		writesComplete <- errors.Join(failures...)
	}()
	for event := range session.Output() {
		got[event.Transfer] = append(got[event.Transfer], event.Bytes...)
	}
	if err := <-writesComplete; err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if !bytes.Equal(got[child.transfer], want) {
			t.Errorf("transfer %d PTY bytes=%d, want %d exact bytes", child.transfer.Value(),
				len(got[child.transfer]), len(want))
		}
	}
	if _, err := session.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestOutputBackpressureBecomesSupervisionFailure(t *testing.T) {
	id, _ := transfernumber.New(1)
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	child := &launchedChild{transfer: id, pid: 211, outputs: []preparedOutput{{
		stream: StreamStdout, file: reader,
	}}}
	reaper := startReaperWith([]*launchedChild{child}, scriptedWait4(
		wait4Result{pid: child.pid}, wait4Result{err: unix.ECHILD},
	))
	outputWake, controlWake := testProcessWakes(t)
	processes := newProcessSet(context.Background(), []*launchedChild{child}, reaper,
		outputWake, controlWake)
	written := make(chan error, 1)
	go func() {
		_, writeErr := writer.Write(bytes.Repeat([]byte{'x'}, 8<<20))
		written <- errors.Join(writeErr, writer.Close())
	}()
	started := time.Now()
	result, waitErr := processes.Wait()
	if waitErr == nil || !strings.Contains(waitErr.Error(), "output backpressure did not advance") ||
		!result.Cancelled {
		t.Fatalf("backpressure result=%#v error=%v", result, waitErr)
	}
	if elapsed := time.Since(started); elapsed < abortTimeout || elapsed > abortTimeout+2*time.Second {
		t.Fatalf("backpressure failure took %s", elapsed)
	}
	select {
	case <-written:
	case <-time.After(time.Second):
		t.Fatal("backpressure cleanup left output writer blocked")
	}
}

func newUnbufferedOutputSession(t *testing.T, children []*launchedChild, reaper *reapedProcesses) *ProcessSet {
	t.Helper()
	outputWake, controlWake := testProcessWakes(t)
	processIO, failures := newProcessIO(children, 0, outputWake, controlWake)
	session := &ProcessSet{io: processIO,
		directPIDs: make(map[int]transfernumber.Number, len(children)), reaper: reaper,
		terminator: newProcessTerminator(), completion: newProcessCompletion(),
		statuses: make(chan Status, 2*len(children))}
	for _, failure := range failures {
		session.completion.recordSupervisionFailure(failure)
	}
	for _, child := range children {
		session.directPIDs[child.pid] = child.transfer
		session.statuses <- Status{Transfer: child.transfer, Kind: StatusRunning}
	}
	go session.supervise()
	go func() {
		for range session.Statuses() {
		}
	}()
	return session
}

func testProcessWakes(t *testing.T) (int, int) {
	t.Helper()
	output, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	control, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		_ = unix.Close(output)
		t.Fatal(err)
	}
	return output, control
}

func TestConfirmContainmentDoesNotRepeatObservedSupervisionError(t *testing.T) {
	contained := make(chan struct{})
	close(contained)
	supervisionErr := errors.New("supervision failed after containment")
	session := completedProcessSet(contained, supervisionErr)

	if _, err := session.Wait(); !errors.Is(err, supervisionErr) {
		t.Fatalf("Wait error=%v", err)
	}
	if err := session.ConfirmContainment(context.Background()); err != nil {
		t.Fatalf("ConfirmContainment repeated observed supervision: %v", err)
	}
}

func TestConfirmContainmentFailsWhenSupervisionStopsWithoutEvidence(t *testing.T) {
	contained := make(chan struct{})
	supervisionErr := errors.New("supervision stopped")
	session := completedProcessSet(contained, supervisionErr)

	if _, err := session.Wait(); !errors.Is(err, supervisionErr) {
		t.Fatalf("Wait error=%v", err)
	}
	if err := session.ConfirmContainment(context.Background()); err == nil ||
		errors.Is(err, supervisionErr) {
		t.Fatalf("ConfirmContainment error=%v", err)
	}
}

func TestConfirmContainmentPrefersPublishedEvidenceToExpiredContext(t *testing.T) {
	contained := make(chan struct{})
	close(contained)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session := &ProcessSet{reaper: &reapedProcesses{containment: contained},
		completion: newProcessCompletion()}
	for range 100 {
		if err := session.ConfirmContainment(ctx); err != nil {
			t.Fatalf("confirmed containment lost to context: %v", err)
		}
	}
}

func TestFinishStartFailureSeparatesNewSupervisionFromConfirmedContainment(t *testing.T) {
	contained := make(chan struct{})
	close(contained)
	supervisionErr := errors.New("supervision failed while stopping")
	session := completedProcessSet(contained, supervisionErr)

	result := session.FinishStartFailure(context.Background())
	if !errors.Is(result.SupervisionError, supervisionErr) || result.ContainmentError != nil {
		t.Fatalf("failed-start result=%#v", result)
	}
}

func TestFinishStartFailureSeparatesNewSupervisionFromUnconfirmedContainment(t *testing.T) {
	contained := make(chan struct{})
	supervisionErr := errors.New("supervision failed while stopping")
	session := completedProcessSet(contained, supervisionErr)

	result := session.FinishStartFailure(context.Background())
	if !errors.Is(result.SupervisionError, supervisionErr) || result.ContainmentError == nil ||
		errors.Is(result.ContainmentError, supervisionErr) {
		t.Fatalf("failed-start result=%#v", result)
	}
}

func completedProcessSet(contained chan struct{}, waitErr error) *ProcessSet {
	completion := newProcessCompletion()
	completion.publish(nil, 0, waitErr)
	return &ProcessSet{reaper: &reapedProcesses{containment: contained},
		completion: completion, terminator: newProcessTerminator()}
}
