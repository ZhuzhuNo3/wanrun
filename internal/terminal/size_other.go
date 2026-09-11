//go:build !unix

package terminal

import (
	"errors"
	"io"
)

func CurrentSize(io.Reader, io.Writer) (int, int, error) {
	return 0, 0, errors.New("terminal size inspection requires Unix")
}
