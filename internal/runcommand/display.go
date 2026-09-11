package runcommand

import (
	"errors"
	"io"
	"net/netip"
	"os"
	"sync"

	"github.com/ZhuzhuNo3/transferlanes/internal/plainoutput"
	"github.com/ZhuzhuNo3/transferlanes/internal/runoutput"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/terminal"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type commandControl interface {
	Resize(transfernumber.Number, uint16, uint16) error
	WriteInput(transfernumber.Number, []byte) error
}

type finalSnapshotProjection struct {
	final                 *runsupervisor.Final
	displayEventsComplete bool
}

type displayDriver interface {
	mode() DisplayMode
	display() runoutput.Display
	commandIO() (int, int, bool)
	start(commandControl, <-chan struct{}, <-chan struct{}, *terminationInbox)
	stopCommands()
	done() <-chan struct{}
	result() error
	abort() error
}

type finalSnapshotDriver interface {
	complete(finalSnapshotProjection)
}

type plainDisplayDriver struct {
	output *plainoutput.PlainTextOutput
	doneCh chan struct{}
	err    error
}

func newPlainDisplayDriver(request Request, stdout, stderr io.Writer) (*plainDisplayDriver, error) {
	selections := displayNetworks(request)
	transfers := make([]plainoutput.Transfer, len(selections))
	for index, selection := range selections {
		transfer, err := plainoutput.NewTransfer(selection.number, selection.localIP)
		if err != nil {
			return nil, err
		}
		transfers[index] = transfer
	}
	output, err := plainoutput.New(stdout, stderr, transfers)
	if err != nil {
		return nil, err
	}
	return &plainDisplayDriver{output: output, doneCh: make(chan struct{})}, nil
}

func (driver *plainDisplayDriver) mode() DisplayMode           { return DisplayPlain }
func (driver *plainDisplayDriver) display() runoutput.Display  { return driver.output }
func (driver *plainDisplayDriver) commandIO() (int, int, bool) { return 0, 0, false }
func (driver *plainDisplayDriver) stopCommands()               {}
func (driver *plainDisplayDriver) done() <-chan struct{}       { return driver.doneCh }
func (driver *plainDisplayDriver) result() error               { return driver.err }
func (driver *plainDisplayDriver) abort() error                { return driver.output.Close() }

func (driver *plainDisplayDriver) start(_ commandControl, outputDone <-chan struct{}, _ <-chan struct{},
	_ *terminationInbox,
) {
	go func() {
		defer close(driver.doneCh)
		<-outputDone
		driver.err = driver.output.Close()
	}()
}

type interactiveDisplayDriver struct {
	program   *terminal.Program
	transfers []transfernumber.Number
	doneCh    chan struct{}
	err       error
	mu        sync.Mutex
}

func newInteractiveDisplayDriver(request Request, stdin io.Reader, stdout io.Writer,
	columns, rows int,
) (*interactiveDisplayDriver, error) {
	input, inputOK := stdin.(*os.File)
	output, outputOK := stdout.(*os.File)
	if !inputOK || !outputOK {
		return nil, errors.New("interactive run requires file-backed terminal descriptors")
	}
	selections := displayNetworks(request)
	transfers := make([]terminal.Transfer, len(selections))
	numbers := make([]transfernumber.Number, len(selections))
	for index, selection := range selections {
		transfer, err := terminal.NewTransfer(selection.number, selection.localIP)
		if err != nil {
			return nil, err
		}
		transfers[index], numbers[index] = transfer, selection.number
	}
	mouse := terminal.MouseScroll
	if request.Display().Mouse() {
		mouse = terminal.MouseTarget
	}
	display, err := terminal.NewInteractiveTerminal(transfers, columns, rows, mouse)
	if err != nil {
		return nil, err
	}
	program, err := terminal.NewProgram(display, input, output)
	if err != nil {
		return nil, err
	}
	return &interactiveDisplayDriver{program: program, transfers: numbers,
		doneCh: make(chan struct{})}, nil
}

func (driver *interactiveDisplayDriver) mode() DisplayMode          { return DisplayInteractive }
func (driver *interactiveDisplayDriver) display() runoutput.Display { return driver.program.Display() }
func (driver *interactiveDisplayDriver) done() <-chan struct{}      { return driver.doneCh }
func (driver *interactiveDisplayDriver) abort() error               { return driver.program.Close() }

func (driver *interactiveDisplayDriver) commandIO() (int, int, bool) {
	columns, rows := driver.program.TargetSize()
	return columns, rows, true
}

func (driver *interactiveDisplayDriver) start(control commandControl, outputDone <-chan struct{},
	resize <-chan struct{}, inbox *terminationInbox,
) {
	go func() {
		defer close(driver.doneCh)
		err := driver.program.Run(control, outputDone, resize, func(request terminal.TerminationRequest) {
			inbox.publish(terminationRequest{reason: request.Reason(), controllerLost: request.ControllerLost()})
		})
		driver.mu.Lock()
		driver.err = err
		driver.mu.Unlock()
		if err != nil {
			inbox.publish(terminationRequest{reason: runsupervisor.CancelInternal})
		}
	}()
}

func (driver *interactiveDisplayDriver) stopCommands() { driver.program.StopCommands() }

func (driver *interactiveDisplayDriver) complete(projection finalSnapshotProjection) {
	if projection.final == nil || !projection.displayEventsComplete ||
		!completeTransferResults(projection.final.Transfers, driver.transfers) {
		driver.program.Finish(nil, false)
		return
	}
	driver.program.Finish(projection.final, true)
}

func (driver *interactiveDisplayDriver) result() error {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.err
}

func completeTransferResults(results []runsupervisor.TransferResult,
	expected []transfernumber.Number,
) bool {
	if len(results) != len(expected) {
		return false
	}
	seen := make(map[transfernumber.Number]struct{}, len(results))
	for _, result := range results {
		seen[result.Transfer] = struct{}{}
	}
	for _, number := range expected {
		if _, exists := seen[number]; !exists {
			return false
		}
	}
	return len(seen) == len(expected)
}

type displayNetwork struct {
	number  transfernumber.Number
	localIP netip.Addr
}

func displayNetworks(request Request) []displayNetwork {
	networks := request.Networks()
	manual, automatic := networks.Manual(), networks.Automatic()
	result := make([]displayNetwork, 0, len(manual)+len(automatic))
	for index, selected := range manual {
		number, _ := transfernumber.New(index + 1)
		result = append(result, displayNetwork{number: number, localIP: selected.LocalIP()})
	}
	for index, localIP := range automatic {
		number, _ := transfernumber.New(index + 1)
		result = append(result, displayNetwork{number: number, localIP: localIP})
	}
	return result
}
