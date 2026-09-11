//go:build linux

package runsupervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	inheritedControlFD = 3
	inheritedEventFD   = 4
	inheritedCancelFD  = 5
	inheritedSourceFD  = 6
)

// MaybeRun recognizes only the private inherited supervisor entry.
func MaybeRun(argv []string, work Work) (bool, int) {
	if len(argv) != 1 || argv[0] != inheritedArgument {
		return false, 0
	}
	if err := runInherited(work); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "transferlanes supervisor: %v\n", err)
		return true, 1
	}
	return true, 0
}

func runInherited(work Work) error {
	if os.Getpid() != 1 {
		return fmt.Errorf("supervisor is not PID namespace init")
	}
	if err := makeMountNamespacePrivate(); err != nil {
		return err
	}
	if err := mountPIDNamespaceProc(); err != nil {
		return err
	}
	if signal, err := parentDeathSignal(); err != nil || signal != 0 {
		return errors.Join(fmt.Errorf("supervisor parent-death signal is unsafe"), err)
	}
	files, err := openInheritedFiles()
	if err != nil {
		return err
	}
	defer files.close()
	wrapped := func(ctx context.Context, run *RunSupervisor) Final {
		return callContainedWork(ctx, run, work)
	}
	return serveWireSessionWithSource(files.control, files.cancellation, files.events,
		files.takeSource(), wrapped)
}

type inheritedFiles struct {
	control      *os.File
	events       *os.File
	cancellation *os.File
	source       *os.File
}

func openInheritedFiles() (*inheritedFiles, error) {
	files := &inheritedFiles{
		control:      os.NewFile(inheritedControlFD, "supervisor-control"),
		events:       os.NewFile(inheritedEventFD, "supervisor-events"),
		cancellation: os.NewFile(inheritedCancelFD, "supervisor-cancellation"),
		source:       os.NewFile(inheritedSourceFD, "supervisor-source-directory"),
	}
	if files.control == nil || files.events == nil || files.cancellation == nil || files.source == nil {
		return nil, errors.Join(errors.New("supervisor inherited descriptors are unavailable"), files.close())
	}
	for _, check := range []struct {
		name   string
		file   *os.File
		access int
	}{
		{name: "control", file: files.control, access: unix.O_RDONLY},
		{name: "event", file: files.events, access: unix.O_WRONLY},
		{name: "cancellation", file: files.cancellation, access: unix.O_RDONLY},
	} {
		if err := validateInheritedPipe(int(check.file.Fd()), check.access); err != nil {
			return nil, errors.Join(fmt.Errorf("validate supervisor %s pipe: %w", check.name, err), files.close())
		}
	}
	if err := validateInheritedSource(files.source); err != nil {
		return nil, errors.Join(err, files.close())
	}
	for _, file := range []*os.File{files.control, files.events, files.cancellation, files.source} {
		unix.CloseOnExec(int(file.Fd()))
	}
	return files, nil
}

func validateInheritedSource(source *os.File) error {
	var status unix.Stat_t
	if err := unix.Fstat(int(source.Fd()), &status); err != nil {
		return fmt.Errorf("inspect inherited source directory: %w", err)
	}
	if uint32(status.Mode)&unix.S_IFMT != unix.S_IFDIR {
		return errors.New("inherited source descriptor is not a directory")
	}
	return nil
}

func (files *inheritedFiles) takeSource() *os.File {
	if files == nil {
		return nil
	}
	source := files.source
	files.source = nil
	return source
}

func (files *inheritedFiles) close() error {
	if files == nil {
		return nil
	}
	err := closeFilesWithErrors(files.control, files.events, files.cancellation, files.source)
	files.control, files.events, files.cancellation, files.source = nil, nil, nil, nil
	return err
}

func makeMountNamespacePrivate() error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make supervisor mount namespace private: %w", err)
	}
	return nil
}

func mountPIDNamespaceProc() error {
	flags := uintptr(unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC)
	if err := unix.Mount("proc", "/proc", "proc", flags, ""); err != nil {
		return fmt.Errorf("mount supervisor PID namespace proc: %w", err)
	}
	return nil
}

func validateInheritedPipe(fd int, access int) error {
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFIFO {
		return fmt.Errorf("descriptor is not a pipe")
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return err
	}
	if flags&unix.O_ACCMODE != access {
		return fmt.Errorf("pipe direction is invalid")
	}
	return nil
}

func parentDeathSignal() (int, error) {
	var signal int
	_, _, errno := unix.Syscall(unix.SYS_PRCTL, unix.PR_GET_PDEATHSIG,
		uintptr(unsafe.Pointer(&signal)), 0)
	if errno != 0 {
		return 0, errno
	}
	return signal, nil
}

func closeFilesWithErrors(files ...*os.File) error {
	var err error
	for _, file := range files {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
	}
	return err
}
