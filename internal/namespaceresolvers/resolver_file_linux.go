//go:build linux

package namespaceresolvers

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func readHostResolver() ([]byte, error) {
	file, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil, fmt.Errorf("open host resolver configuration: %w", err)
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, maximumResolverBytes+1))
	if err != nil || len(content) > maximumResolverBytes {
		return nil, errors.Join(errors.New("host resolver configuration is too large or unreadable"), err)
	}
	return content, nil
}

func sealedResolverFile(content []byte) (*os.File, error) {
	fd, err := unix.MemfdCreate("transferlanes-resolver", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("create resolver memory file: %w", err)
	}
	file := os.NewFile(uintptr(fd), "transferlanes-resolver")
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("write resolver memory file: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("rewind resolver memory file: %w", err)
	}
	seals := unix.F_SEAL_SEAL | unix.F_SEAL_SHRINK | unix.F_SEAL_GROW | unix.F_SEAL_WRITE
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, seals); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("seal resolver memory file: %w", err)
	}
	return file, nil
}
