//go:build !linux

package runsupervisor

import (
	"context"
	"errors"
	"io"

	"github.com/ZhuzhuNo3/transferlanes/internal/sourcefiles"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type RunSupervisorClient struct{}

func Launch(context.Context, string, []byte, *sourcefiles.SourceRoot) (*RunSupervisorClient, error) {
	return nil, errors.New("supervisor requires Linux")
}

func MaybeRun([]string, Work) (bool, int) { return false, 0 }
func (*RunSupervisorClient) PID() int     { return 0 }
func (*RunSupervisorClient) Cancel(CancelReason) error {
	return errors.New("supervisor requires Linux")
}
func (*RunSupervisorClient) Resize(transfernumber.Number, uint16, uint16) error {
	return errors.New("supervisor requires Linux")
}
func (*RunSupervisorClient) WriteInput(transfernumber.Number, []byte) error {
	return errors.New("supervisor requires Linux")
}
func (*RunSupervisorClient) Next() (Event, error) { return Event{}, io.EOF }
func (*RunSupervisorClient) CloseLifeline() error { return errors.New("supervisor requires Linux") }
func (*RunSupervisorClient) Wait() error          { return errors.New("supervisor requires Linux") }
