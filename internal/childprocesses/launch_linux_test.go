//go:build linux

package childprocesses

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"golang.org/x/sys/unix"
)

func TestLaunchOwnershipTransfersDescriptors(t *testing.T) {
	id, _ := transfernumber.New(1)
	outputReader, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer outputWriter.Close()
	handoffReader, handoffWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer handoffWriter.Close()

	child := &childLaunch{transfer: id, io: &childIOFiles{outputs: []preparedOutput{{
		stream: StreamStdout, file: outputReader,
	}}}, readyReader: handoffReader}
	launched := child.takeLaunchedChild(101)
	if child.readyReader != nil || child.io == nil || child.io.outputs != nil {
		t.Fatalf("child launch retained transferred descriptors: %#v", child)
	}
	child.close()
	child.close()
	assertDescriptorOpen(t, outputReader)
	assertDescriptorOpen(t, handoffReader)

	wakes, err := newProcessWakeOwner()
	if err != nil {
		t.Fatal(err)
	}
	outputWake, controlWake := wakes.output, wakes.control
	reaper := &reapedProcesses{}
	launch := &processSetLaunch{wakes: wakes, children: []*launchedChild{launched}, reaper: reaper}
	children, transferredReaper, transferredOutputWake, transferredControlWake := launch.takeRuntime()
	if launch.children != nil || launch.reaper != nil || launch.wakes != nil {
		t.Fatalf("process-set launch retained transferred ownership: %#v", launch)
	}
	if len(children) != 1 || children[0] != launched || transferredReaper != reaper ||
		transferredOutputWake != outputWake || transferredControlWake != controlWake {
		t.Fatal("process-set launch returned different runtime resources")
	}
	launch.close()
	launch.close()
	assertDescriptorOpen(t, outputReader)
	assertDescriptorOpen(t, handoffReader)
	assertRawDescriptorOpen(t, outputWake)
	assertRawDescriptorOpen(t, controlWake)

	children[0].close()
	children[0].close()
	_ = unix.Close(transferredOutputWake)
	_ = unix.Close(transferredControlWake)
	assertDescriptorClosed(t, outputReader)
	assertDescriptorClosed(t, handoffReader)
	assertRawDescriptorClosed(t, outputWake)
	assertRawDescriptorClosed(t, controlWake)
}

func TestClosedBarrierTransfersOwnerAndCancels(t *testing.T) {
	id, _ := transfernumber.New(1)
	outputReader, outputWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = outputWriter.Close()
	handoffReader, handoffWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = handoffWriter.Close()
	barrierReader, barrierWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = barrierReader.Close()
	_ = barrierWriter.Close()
	wakes, err := newProcessWakeOwner()
	if err != nil {
		t.Fatal(err)
	}
	child := &launchedChild{transfer: id, pid: 202,
		outputs: []preparedOutput{{stream: StreamStdout, file: outputReader}}, handoff: handoffReader}
	reaper := startReaperWith([]*launchedChild{child}, scriptedWait4(
		wait4Result{pid: child.pid}, wait4Result{err: unix.ECHILD},
	))
	launch := &processSetLaunch{barrierWriter: barrierWriter, wakes: wakes,
		children: []*launchedChild{child}, reaper: reaper}
	defer launch.close()

	releaseErr := launch.releaseBarrier()
	if releaseErr == nil {
		t.Fatal("closed barrier writer unexpectedly released")
	}
	startErr := errors.New("barrier release failed")
	processes, err := launch.failAfterBarrier(context.Background(), errors.Join(startErr, releaseErr))
	if processes == nil || !errors.Is(err, startErr) {
		t.Fatalf("barrier failure owner=%v error=%v", processes, err)
	}
	drainSessionEvidence(processes)
	result, waitErr := processes.Wait()
	if waitErr != nil || !result.Cancelled || len(result.Transfers) != 1 {
		t.Fatalf("barrier failure result=%#v error=%v", result, waitErr)
	}
	select {
	case <-processes.ContainmentDone():
	default:
		t.Fatal("barrier failure did not finish containment")
	}
}

func assertDescriptorOpen(t *testing.T, file *os.File) {
	t.Helper()
	assertRawDescriptorOpen(t, int(file.Fd()))
}

func assertDescriptorClosed(t *testing.T, file *os.File) {
	t.Helper()
	assertRawDescriptorClosed(t, int(file.Fd()))
}

func assertRawDescriptorOpen(t *testing.T, descriptor int) {
	t.Helper()
	if _, err := unix.FcntlInt(uintptr(descriptor), unix.F_GETFD, 0); err != nil {
		t.Fatalf("descriptor %d is closed: %v", descriptor, err)
	}
}

func assertRawDescriptorClosed(t *testing.T, descriptor int) {
	t.Helper()
	if _, err := unix.FcntlInt(uintptr(descriptor), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
		t.Fatalf("descriptor %d remains open: %v", descriptor, err)
	}
}
