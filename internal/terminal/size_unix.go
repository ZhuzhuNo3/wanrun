//go:build unix

package terminal

import (
	"errors"
	"fmt"
	"io"

	"golang.org/x/sys/unix"
)

// CurrentSize verifies both outer descriptors and returns the latest output dimensions.
func CurrentSize(input io.Reader, output io.Writer) (int, int, error) {
	in, ok := input.(interface{ Fd() uintptr })
	if !ok {
		return 0, 0, errors.New("stdin is not a terminal")
	}
	out, ok := output.(interface{ Fd() uintptr })
	if !ok {
		return 0, 0, errors.New("stdout is not a terminal")
	}
	if _, err := unix.IoctlGetWinsize(int(in.Fd()), unix.TIOCGWINSZ); err != nil {
		return 0, 0, fmt.Errorf("inspect stdin terminal: %w", err)
	}
	size, err := unix.IoctlGetWinsize(int(out.Fd()), unix.TIOCGWINSZ)
	if err != nil || size.Col == 0 || size.Row == 0 {
		return 0, 0, errors.Join(errors.New("inspect stdout terminal"), err)
	}
	return int(size.Col), int(size.Row), nil
}
