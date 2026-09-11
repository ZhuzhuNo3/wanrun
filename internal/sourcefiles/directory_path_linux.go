//go:build linux

package sourcefiles

import (
	"fmt"
	"path/filepath"
)

func stableDescriptorRoot(pid, fd int, _ string) string {
	return filepath.Join("/proc", fmt.Sprintf("%d", pid), "fd", fmt.Sprintf("%d", fd))
}
