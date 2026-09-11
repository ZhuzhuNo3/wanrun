//go:build linux

package childprocesses

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type processSetLaunch struct {
	barrierReader *os.File
	barrierWriter *os.File
	wakes         *processWakeOwner
	children      []*launchedChild
	reaper        *reapedProcesses
}

func newProcessSetLaunch(capacity int) (*processSetLaunch, error) {
	wakes, err := newProcessWakeOwner()
	if err != nil {
		return nil, err
	}
	barrierReader, barrierWriter, err := os.Pipe()
	if err != nil {
		wakes.close()
		return nil, fmt.Errorf("create child start barrier: %w", err)
	}
	return &processSetLaunch{
		barrierReader: barrierReader,
		barrierWriter: barrierWriter,
		wakes:         wakes,
		children:      make([]*launchedChild, 0, capacity),
	}, nil
}

func (launch *processSetLaunch) launchChild(executable string, network *hostnetwork.Session,
	command Command, ioKind childIOKind, size TerminalSize,
) error {
	child, err := prepareChildLaunch(network, command, ioKind, size)
	if err != nil {
		return err
	}
	defer child.close()

	cmd := exec.Command(executable, helperArgument)
	cmd.Env = os.Environ()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = child.io.stdin, child.io.stdout, child.io.stderr
	cmd.ExtraFiles = []*os.File{child.namespace, launch.barrierReader, child.readyWriter,
		child.payload, child.resolver}
	cmd.ExtraFiles = append(cmd.ExtraFiles, child.inherited...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start transfer %d helper: %w", command.transfer.Value(), err)
	}

	launched := child.takeLaunchedChild(cmd.Process.Pid)
	launch.children = append(launch.children, launched)
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release transfer %d helper process handle: %w",
			command.transfer.Value(), err)
	}
	return nil
}

func (launch *processSetLaunch) startReaper() *reapedProcesses {
	if launch.reaper == nil {
		launch.reaper = startReaper(launch.children)
	}
	return launch.reaper
}

func (launch *processSetLaunch) closeBarrierReader() {
	closeFile(launch.barrierReader)
	launch.barrierReader = nil
}

func (launch *processSetLaunch) closeBarrier() {
	launch.closeBarrierReader()
	closeFile(launch.barrierWriter)
	launch.barrierWriter = nil
}

func (launch *processSetLaunch) releaseBarrier() error {
	writer := launch.barrierWriter
	launch.barrierWriter = nil
	if writer == nil {
		return fmt.Errorf("release complete child barrier: writer is unavailable")
	}
	markers := bytes.Repeat([]byte{helperReleaseMarker}, len(launch.children))
	for {
		written, err := unix.Write(int(writer.Fd()), markers)
		if err == unix.EINTR {
			continue
		}
		closeErr := writer.Close()
		if err != nil || written != len(markers) || closeErr != nil {
			return errors.Join(fmt.Errorf("release complete child barrier"), err, closeErr)
		}
		return nil
	}
}

func (launch *processSetLaunch) abortBeforeBarrier(cause error) (*ProcessSet, error) {
	launch.closeBarrier()
	reaper := launch.startReaper()
	launch.closeChildDescriptorsForAbort()
	cleanupCtx, cancel := context.WithTimeout(context.Background(), abortTimeout)
	waitErr := waitForReap(cleanupCtx, reaper)
	cancel()
	if waitErr == nil {
		return nil, cause
	}
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		return launch.releaseToProcessSet(context.Background()), cause
	}
	_ = unix.Kill(-1, unix.SIGKILL)
	killCtx, stop := context.WithTimeout(context.Background(), abortTimeout)
	defer stop()
	if waitErr := waitForReap(killCtx, reaper); waitErr == nil {
		return nil, cause
	}
	return launch.releaseToProcessSet(context.Background()), cause
}

func (launch *processSetLaunch) failAfterBarrier(ctx context.Context, cause error) (*ProcessSet, error) {
	launch.closeHandoffs()
	processes := launch.releaseToProcessSet(ctx)
	processes.beginCancellation()
	return processes, cause
}

func (launch *processSetLaunch) releaseToProcessSet(ctx context.Context) *ProcessSet {
	children, reaper, outputWake, controlWake := launch.takeRuntime()
	return newProcessSet(ctx, children, reaper, outputWake, controlWake)
}

func (launch *processSetLaunch) takeRuntime() ([]*launchedChild, *reapedProcesses, int, int) {
	launch.closeBarrier()
	children := launch.children
	reaper := launch.reaper
	launch.children = nil
	launch.reaper = nil
	outputWake, controlWake := -1, -1
	if launch.wakes != nil {
		outputWake, controlWake = launch.wakes.take()
		launch.wakes = nil
	}
	return children, reaper, outputWake, controlWake
}

func (launch *processSetLaunch) closeHandoffs() {
	for _, child := range launch.children {
		closeFile(child.handoff)
		child.handoff = nil
	}
}

func (launch *processSetLaunch) closeChildDescriptorsForAbort() {
	for _, child := range launch.children {
		closeFile(child.handoff)
		for _, output := range child.outputs {
			closeFile(output.file)
		}
	}
}

func (launch *processSetLaunch) close() {
	if launch == nil {
		return
	}
	launch.closeBarrier()
	if launch.wakes != nil {
		launch.wakes.close()
		launch.wakes = nil
	}
	for _, child := range launch.children {
		child.close()
	}
	launch.children = nil
	launch.reaper = nil
}

type processWakeOwner struct{ output, control int }

func newProcessWakeOwner() (*processWakeOwner, error) {
	output, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		return nil, fmt.Errorf("create child output wakeup: %w", err)
	}
	control, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		_ = unix.Close(output)
		return nil, fmt.Errorf("create child control wakeup: %w", err)
	}
	return &processWakeOwner{output: output, control: control}, nil
}

func (wake *processWakeOwner) take() (int, int) {
	output, control := wake.output, wake.control
	wake.output, wake.control = -1, -1
	return output, control
}

func (wake *processWakeOwner) close() {
	if wake == nil {
		return
	}
	for _, descriptor := range []*int{&wake.output, &wake.control} {
		if *descriptor >= 0 {
			_ = unix.Close(*descriptor)
			*descriptor = -1
		}
	}
}

type childLaunch struct {
	transfer    transfernumber.Number
	namespace   *os.File
	resolver    *os.File
	inherited   []*os.File
	payload     *os.File
	io          *childIOFiles
	readyReader *os.File
	readyWriter *os.File
}

func prepareChildLaunch(network *hostnetwork.Session, command Command, ioKind childIOKind,
	size TerminalSize,
) (_ *childLaunch, resultErr error) {
	child := &childLaunch{transfer: command.transfer}
	defer func() {
		if resultErr != nil {
			child.close()
		}
	}()

	lease, err := network.OpenNamespace(command.transfer)
	if err != nil {
		return nil, fmt.Errorf("borrow transfer %d namespace: %w", command.transfer.Value(), err)
	}
	var inode uint64
	child.namespace, inode, err = duplicateNamespace(lease)
	closeErr := lease.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	child.resolver, err = command.resolver.Open()
	if err != nil {
		return nil, err
	}
	var inheritedIdentity inheritedIdentity
	child.inherited, inheritedIdentity, err = duplicateInheritedCapability(command)
	if err != nil {
		return nil, err
	}
	child.payload, err = helperPayloadFile(helperRequestFromCommand(command, inode, inheritedIdentity,
		ioKind == interactiveChildIO))
	if err != nil {
		return nil, err
	}
	child.io, err = openChildIO(command.transfer, ioKind, size)
	if err != nil {
		return nil, err
	}
	child.readyReader, child.readyWriter, err = os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create transfer %d ready pipe: %w", command.transfer.Value(), err)
	}
	return child, nil
}

func (child *childLaunch) takeLaunchedChild(pid int) *launchedChild {
	outputs := child.io.outputs
	child.io.outputs = nil
	handoff := child.readyReader
	child.readyReader = nil
	return &launchedChild{transfer: child.transfer, pid: pid, outputs: outputs, handoff: handoff}
}

func (child *childLaunch) close() {
	if child == nil {
		return
	}
	closeFiles(child.namespace, child.resolver, child.payload, child.readyReader, child.readyWriter)
	child.namespace, child.resolver, child.payload = nil, nil, nil
	child.readyReader, child.readyWriter = nil, nil
	closeFiles(child.inherited...)
	child.inherited = nil
	if child.io != nil {
		child.io.close()
		child.io = nil
	}
}

type launchedChild struct {
	transfer transfernumber.Number
	pid      int
	outputs  []preparedOutput
	handoff  *os.File
}

func (child *launchedChild) close() {
	if child == nil {
		return
	}
	for _, output := range child.outputs {
		closeFile(output.file)
	}
	child.outputs = nil
	closeFile(child.handoff)
	child.handoff = nil
}

type preparedOutput struct {
	stream OutputStream
	file   *os.File
}

type childIOFiles struct {
	stdin, stdout, stderr *os.File
	child                 []*os.File
	outputs               []preparedOutput
}

func openChildIO(id transfernumber.Number, ioKind childIOKind, size TerminalSize) (*childIOFiles, error) {
	if ioKind == interactiveChildIO {
		return openInteractiveChildIO(id, size)
	}
	return openPlainChildIO(id)
}

func openInteractiveChildIO(id transfernumber.Number, size TerminalSize) (*childIOFiles, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("open transfer %d PTY: %w", id.Value(), err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Cols: size.cols, Rows: size.rows}); err != nil {
		closeFiles(master, slave)
		return nil, fmt.Errorf("size transfer %d PTY: %w", id.Value(), err)
	}
	return &childIOFiles{stdin: slave, stdout: slave, stderr: slave,
		child:   []*os.File{slave},
		outputs: []preparedOutput{{stream: StreamPTY, file: master}}}, nil
}

func openPlainChildIO(id transfernumber.Number) (*childIOFiles, error) {
	stdin, err := os.Open("/dev/null")
	if err != nil {
		return nil, fmt.Errorf("open transfer %d EOF input: %w", id.Value(), err)
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		closeFile(stdin)
		return nil, fmt.Errorf("open transfer %d stdout pipe: %w", id.Value(), err)
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		closeFiles(stdin, stdoutReader, stdoutWriter)
		return nil, fmt.Errorf("open transfer %d stderr pipe: %w", id.Value(), err)
	}
	return &childIOFiles{stdin: stdin, stdout: stdoutWriter, stderr: stderrWriter,
		child: []*os.File{stdin, stdoutWriter, stderrWriter},
		outputs: []preparedOutput{{stream: StreamStdout, file: stdoutReader},
			{stream: StreamStderr, file: stderrReader}}}, nil
}

func (files *childIOFiles) close() {
	if files == nil {
		return
	}
	closeFiles(files.child...)
	files.child = nil
	for _, output := range files.outputs {
		closeFile(output.file)
	}
	files.outputs = nil
}

func duplicateInheritedCapability(command Command) ([]*os.File, inheritedIdentity, error) {
	if command.view != nil {
		files, identity, err := duplicateView(command.view)
		return files, inheritedIdentity{view: identity}, err
	}
	file, identity, err := duplicateExecChannel(command.execChannel)
	if file == nil {
		return nil, inheritedIdentity{execChannel: identity}, err
	}
	return []*os.File{file}, inheritedIdentity{execChannel: identity}, err
}

func duplicateView(source ViewAccess) ([]*os.File, viewDescriptorIdentity, error) {
	rootFile, err := source.OpenChildDescriptor()
	if err != nil {
		return nil, viewDescriptorIdentity{}, fmt.Errorf("duplicate child file-view descriptors: %w", err)
	}
	var root unix.Stat_t
	if err := unix.Fstat(int(rootFile.Fd()), &root); err != nil || root.Mode&unix.S_IFMT != unix.S_IFDIR ||
		root.Dev == 0 || root.Ino == 0 {
		closeFile(rootFile)
		return nil, viewDescriptorIdentity{}, errors.Join(fmt.Errorf("identify child file-view root"), err)
	}
	return []*os.File{rootFile}, viewDescriptorIdentity{
		root:     descriptorIdentity{device: uint64(root.Dev), inode: root.Ino},
		runID:    source.RunID(),
		baseName: source.BaseName(),
	}, nil
}

func duplicateExecChannel(source *os.File) (*os.File, descriptorIdentity, error) {
	if source == nil {
		return nil, descriptorIdentity{}, nil
	}
	fd, err := unix.FcntlInt(source.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, descriptorIdentity{}, fmt.Errorf("duplicate child exec channel: %w", err)
	}
	file := os.NewFile(uintptr(fd), "child-exec-channel")
	var info unix.Stat_t
	if file == nil {
		_ = unix.Close(fd)
		return nil, descriptorIdentity{}, fmt.Errorf("wrap child exec channel")
	}
	if err := unix.Fstat(fd, &info); err != nil || info.Mode&unix.S_IFMT != unix.S_IFSOCK ||
		info.Dev == 0 || info.Ino == 0 {
		_ = file.Close()
		return nil, descriptorIdentity{}, errors.Join(
			fmt.Errorf("child exec channel is not an identified socket"), err)
	}
	return file, descriptorIdentity{device: uint64(info.Dev), inode: info.Ino}, nil
}

func duplicateNamespace(lease *hostnetwork.NamespaceLease) (*os.File, uint64, error) {
	fd, err := unix.FcntlInt(lease.Descriptor(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("duplicate network namespace lease: %w", err)
	}
	file := os.NewFile(uintptr(fd), "child-network-namespace")
	var info unix.Stat_t
	if file == nil {
		_ = unix.Close(fd)
		return nil, 0, fmt.Errorf("wrap network namespace descriptor")
	}
	if err := unix.Fstat(fd, &info); err != nil || info.Ino == 0 {
		_ = file.Close()
		return nil, 0, errors.Join(fmt.Errorf("identify network namespace descriptor"), err)
	}
	return file, info.Ino, nil
}

func helperPayloadFile(request helperRequest) (*os.File, error) {
	encoded, err := encodeHelperRequest(request)
	if err != nil {
		return nil, err
	}
	fd, err := unix.MemfdCreate("transferlanes-child-request", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("create child request memory file: %w", err)
	}
	file := os.NewFile(uintptr(fd), "child-request")
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write child request memory file: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("rewind child request memory file: %w", err)
	}
	seals := unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, seals); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("seal child request memory file: %w", err)
	}
	return file, nil
}
