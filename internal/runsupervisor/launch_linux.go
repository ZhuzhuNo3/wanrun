//go:build linux

package runsupervisor

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

const inheritedArgument = "--transferlanes-internal-run-supervisor"

func startClientLaunch(executable string, source *os.File) (*clientLaunch, error) {
	launch := &clientLaunch{source: source}
	var err error
	if launch.controlRead, launch.controlWrite, err = os.Pipe(); err != nil {
		return nil, errors.Join(fmt.Errorf("create supervisor control pipe: %w", err), launch.closeDescriptors())
	}
	if launch.eventRead, launch.eventWrite, err = os.Pipe(); err != nil {
		return nil, errors.Join(fmt.Errorf("create supervisor event pipe: %w", err), launch.closeDescriptors())
	}
	if launch.cancelRead, launch.cancelWrite, err = os.Pipe(); err != nil {
		return nil, errors.Join(fmt.Errorf("create supervisor cancellation pipe: %w", err), launch.closeDescriptors())
	}
	if launch.wake, err = controlWriteWake(); err != nil {
		return nil, errors.Join(err, launch.closeDescriptors())
	}
	if err := validateSourceDirectory(source); err != nil {
		return nil, errors.Join(err, launch.closeDescriptors())
	}
	if err := unix.SetNonblock(int(launch.controlWrite.Fd()), true); err != nil {
		return nil, errors.Join(fmt.Errorf("make supervisor control writes interruptible: %w", err),
			launch.closeDescriptors())
	}
	command := supervisorCommand(executable, launch.controlRead, launch.eventWrite, launch.cancelRead, source)
	if err := command.Start(); err != nil {
		return nil, errors.Join(fmt.Errorf("start supervisor: %w", err), launch.closeDescriptors())
	}
	launch.process = newSupervisorProcess(command)
	childCloseErr := closeFilesWithErrors(launch.controlRead, launch.eventWrite, launch.cancelRead)
	launch.controlRead, launch.eventWrite, launch.cancelRead = nil, nil, nil
	if childCloseErr != nil {
		killErr, waitErr := launch.process.terminate()
		return nil, errors.Join(childCloseErr, killErr, waitErr, launch.closeDescriptors())
	}
	return launch, nil
}

func supervisorCommand(executable string, files ...*os.File) *exec.Cmd {
	command := exec.Command(executable, inheritedArgument)
	command.Env, command.ExtraFiles, command.Stderr = os.Environ(), files, os.Stderr
	command.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: unix.CLONE_NEWPID | unix.CLONE_NEWNS, Setsid: true,
	}
	return command
}

func validateSourceDirectory(source *os.File) error {
	if source == nil {
		return errors.New("supervisor source directory is required")
	}
	var status unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &status); err != nil ||
		uint32(status.Mode)&unix.S_IFMT != unix.S_IFDIR {
		return errors.Join(errors.New("supervisor source descriptor is not a directory"), err)
	}
	return nil
}
