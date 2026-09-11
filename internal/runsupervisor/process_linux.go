//go:build linux

package runsupervisor

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

func (process *supervisorProcess) exitedBeforeKill() (bool, error) {
	if process == nil || process.command == nil || process.command.Process == nil {
		return false, errors.New("supervisor process is unavailable before termination")
	}
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, process.pid, &info,
			unix.WEXITED|unix.WNOHANG|unix.WNOWAIT, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, fmt.Errorf("observe supervisor exit before termination: %w", err)
		}
		return info.Signo != 0, nil
	}
}
