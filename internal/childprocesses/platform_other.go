//go:build !linux

package childprocesses

import (
	"context"
	"errors"

	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type ChildProcesses struct{}
type ProcessSet struct{}

func New() *ChildProcesses                { return &ChildProcesses{} }
func MaybeRunHelper([]string) (bool, int) { return false, 0 }
func (*ChildProcesses) StartInteractive(context.Context, *hostnetwork.Session, []Command, TerminalSize) (*ProcessSet, error) {
	return nil, errors.New("child processes require Linux")
}
func (*ChildProcesses) StartPlain(context.Context, *hostnetwork.Session, []Command) (*ProcessSet, error) {
	return nil, errors.New("child processes require Linux")
}
func (*ChildProcesses) RunHelpers(context.Context, *hostnetwork.Session, []Command) (HelperResult, *ProcessSet, error) {
	return HelperResult{}, nil, errors.New("child processes require Linux")
}
func (*ProcessSet) Output() <-chan Output   { return nil }
func (*ProcessSet) Statuses() <-chan Status { return nil }
func (*ProcessSet) WriteInput(transfernumber.Number, []byte) error {
	return errors.New("child processes require Linux")
}
func (*ProcessSet) Resize(transfernumber.Number, int, int) error {
	return errors.New("child processes require Linux")
}
func (*ProcessSet) Cancel(context.Context) error { return errors.New("child processes require Linux") }
func (*ProcessSet) Wait() (Result, error) {
	return Result{}, errors.New("child processes require Linux")
}
func (*ProcessSet) FinishStartFailure(context.Context) StartFailureResult {
	return StartFailureResult{SupervisionError: errors.New("child processes require Linux")}
}
func (*ProcessSet) ConfirmContainment(context.Context) error {
	return errors.New("child processes require Linux")
}
func (*ProcessSet) ContainmentDone() <-chan struct{} { return nil }
