//go:build linux || darwin

package namespaceresolvers

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func duplicateResolverFile(source *os.File) (*os.File, error) {
	fd, err := unix.FcntlInt(source.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("duplicate resolver capability: %w", err)
	}
	return os.NewFile(uintptr(fd), "child-resolver"), nil
}
