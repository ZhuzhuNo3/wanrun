//go:build linux

package runsupervisor

import (
	"errors"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

// RunSupervisorClient is the parent-side control and event capability for one run supervisor.
type RunSupervisorClient struct {
	controls *controlSender
	events   *eventReceiver
	process  *supervisorProcess
}

func (client *RunSupervisorClient) PID() int {
	if client == nil {
		return 0
	}
	return client.process.PID()
}

func (client *RunSupervisorClient) Cancel(reason CancelReason) error {
	return client.controls.cancel(reason)
}

func (client *RunSupervisorClient) Resize(id transfernumber.Number, cols, rows uint16) error {
	return client.controls.resize(id, cols, rows)
}

func (client *RunSupervisorClient) WriteInput(id transfernumber.Number, input []byte) error {
	return client.controls.writeInput(id, input)
}

// Next reads one validated raw-output, transfer-status, or final event.
func (client *RunSupervisorClient) Next() (Event, error) {
	return client.events.next(client.controls)
}

// CloseLifeline closes the dedicated parent-liveness capability and requests whole-run cancellation
// by EOF. The ordinary control stream remains owned until Wait reaps the supervisor.
func (client *RunSupervisorClient) CloseLifeline() error { return client.controls.closeLifeline() }

// Wait reaps the supervisor process after its final event or forced termination.
func (client *RunSupervisorClient) Wait() error {
	client.controls.stop()
	outcome := client.process.waitForExit(supervisorReapLimit)
	var timeoutErr error
	if outcome.timedOut {
		timeoutErr = errors.New("supervisor did not exit after final before timeout")
	}
	return errors.Join(timeoutErr, outcome.killErr, outcome.waitErr,
		client.controls.teardown(), client.events.close())
}
