//go:build !unix

package runsupervisor

import (
	"io"
	"time"
)

func prepareEventPipe(io.Writer) (int, error) { return -1, nil }

func writeEventPipe(int, []byte, time.Time) error { return io.ErrClosedPipe }
