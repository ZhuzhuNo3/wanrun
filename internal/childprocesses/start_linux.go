//go:build linux

package childprocesses

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"golang.org/x/sys/unix"
)

const (
	abortTimeout          = 5 * time.Second
	signalGrace           = 500 * time.Millisecond
	maximumOutputReadSize = 32 * 1024
	maximumPTYWriteSize   = 256
)

type childIOKind uint8

const (
	interactiveChildIO childIOKind = iota + 1
	plainChildIO
)

// ChildProcesses creates complete process sets inside one supervisor PID namespace.
type ChildProcesses struct{}

func New() *ChildProcesses { return &ChildProcesses{} }

// StartInteractive consumes short namespace leases and releases one barrier only after every user
// command is ready. An error may accompany a non-nil ProcessSet whenever spawned processes cannot
// yet be proven absent. The caller must continue consuming its outputs and statuses, then
// FinishStartFailure and retain ownership until containment is confirmed.
func (owner *ChildProcesses) StartInteractive(ctx context.Context, network *hostnetwork.Session, commands []Command,
	size TerminalSize) (*ProcessSet, error) {
	return owner.start(ctx, network, commands, interactiveChildIO, size)
}

// StartPlain starts the complete command set with independent stdout/stderr pipes and EOF stdin.
func (owner *ChildProcesses) StartPlain(ctx context.Context, network *hostnetwork.Session,
	commands []Command) (*ProcessSet, error) {
	return owner.start(ctx, network, commands, plainChildIO, TerminalSize{})
}

func (*ChildProcesses) start(ctx context.Context, network *hostnetwork.Session, commands []Command,
	ioKind childIOKind, size TerminalSize) (*ProcessSet, error) {
	if ctx == nil || network == nil || os.Getpid() != 1 ||
		ioKind != interactiveChildIO && ioKind != plainChildIO ||
		ioKind == interactiveChildIO && (size.cols == 0 || size.rows == 0) {
		return nil, fmt.Errorf("child process owner requires supervisor PID namespace init and complete inputs")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateCommands(commands); err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil || !cleanAbsolutePath(executable) {
		return nil, errors.Join(fmt.Errorf("resolve supervisor executable"), err)
	}
	launch, err := newProcessSetLaunch(len(commands))
	if err != nil {
		return nil, err
	}
	defer launch.close()
	for _, command := range commands {
		if err := launch.launchChild(executable, network, command, ioKind, size); err != nil {
			return launch.abortBeforeBarrier(err)
		}
	}
	launch.closeBarrierReader()
	launch.startReaper()
	if err := awaitReadySet(ctx, launch.children); err != nil {
		return launch.abortBeforeBarrier(err)
	}
	if err := launch.releaseBarrier(); err != nil {
		return launch.failAfterBarrier(ctx, err)
	}
	if err := awaitExecSet(ctx, launch.children); err != nil {
		return launch.failAfterBarrier(ctx, err)
	}
	return launch.releaseToProcessSet(ctx), nil
}

func awaitReadySet(ctx context.Context, children []*launchedChild) error {
	for _, child := range children {
		if err := awaitReady(ctx, child.handoff); err != nil {
			return errors.Join(fmt.Errorf("prepare transfer %d before barrier: %w", child.transfer.Value(), err),
				readPreparationDiagnostic(child))
		}
	}
	return nil
}

func readPreparationDiagnostic(child *launchedChild) error {
	var diagnostic *os.File
	for _, output := range child.outputs {
		if output.stream == StreamPTY || output.stream == StreamStderr {
			diagnostic = output.file
			break
		}
	}
	if diagnostic == nil {
		return nil
	}
	fd := int(diagnostic.Fd())
	if err := unix.SetNonblock(fd, true); err != nil {
		return fmt.Errorf("read child preparation diagnostic: %w", err)
	}
	buffer := make([]byte, 4096)
	count, err := unix.Read(fd, buffer)
	if err == unix.EAGAIN || err == unix.EIO {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read child preparation diagnostic: %w", err)
	}
	if count == 0 {
		return nil
	}
	return fmt.Errorf("child preparation output: %q", buffer[:count])
}

func awaitReady(ctx context.Context, ready *os.File) error {
	handoff, err := awaitHelperHandoff(ctx, ready)
	if err != nil {
		return err
	}
	if handoff.kind != helperPrepared {
		return fmt.Errorf("child prepared handshake is invalid")
	}
	return nil
}

func awaitExecSet(ctx context.Context, children []*launchedChild) error {
	for _, child := range children {
		handoff, err := awaitHelperHandoff(ctx, child.handoff)
		closeFile(child.handoff)
		child.handoff = nil
		if errors.Is(err, io.EOF) {
			continue
		}
		if err != nil {
			return fmt.Errorf("confirm transfer %d exec handoff: %w", child.transfer.Value(), err)
		}
		if handoff.kind != helperExecFailed {
			return fmt.Errorf("confirm transfer %d exec handoff: unexpected helper transition", child.transfer.Value())
		}
		return fmt.Errorf("confirm transfer %d exec handoff: %s", child.transfer.Value(), handoff.diagnostic)
	}
	return nil
}

func awaitHelperHandoff(ctx context.Context, ready *os.File) (helperHandoff, error) {
	type outcome struct {
		handoff helperHandoff
		err     error
	}
	result := make(chan outcome, 1)
	go func() {
		handoff, err := readHelperHandoff(ready)
		result <- outcome{handoff: handoff, err: err}
	}()
	select {
	case got := <-result:
		return got.handoff, got.err
	case <-ctx.Done():
		return helperHandoff{}, ctx.Err()
	}
}

func closeFile(file *os.File) {
	if file != nil {
		_ = file.Close()
	}
}

func closeFiles(files ...*os.File) {
	for _, file := range files {
		closeFile(file)
	}
}

func waitForReap(ctx context.Context, reaped *reapedProcesses) error {
	return reaped.wait(ctx)
}
