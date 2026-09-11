package transfer

import (
	"errors"
	"fmt"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/namespaceresolvers"
	"github.com/ZhuzhuNo3/transferlanes/internal/networkcatalog"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

// TransferExecution is the immutable, non-owning join of one transfer's final facts.
type TransferExecution struct {
	number   transfernumber.Number
	shortcut byte
	egress   networkcatalog.Egress
	view     childprocesses.ViewAccess
	viewPath string
	argv     []string
}

func prepareExecutions(request Request,
	egresses map[transfernumber.Number]networkcatalog.Egress, views activeFileViews,
) ([]TransferExecution, []fileViewLease, error) {
	selectedNetworks := request.selectedNetworks()
	if err := validateEgressSet(selectedNetworks, egresses); err != nil {
		return nil, nil, err
	}
	executions := make([]TransferExecution, 0, len(selectedNetworks))
	leases := make([]fileViewLease, 0, len(selectedNetworks))
	for _, selected := range selectedNetworks {
		lease, openErr := views.openView(selected.transfer)
		if openErr != nil {
			return nil, leases, fmt.Errorf("open transfer %d file view: %w", selected.transfer.Value(), openErr)
		}
		leases = append(leases, lease)
		access := lease.access()
		if access.TransferNumber() != selected.transfer {
			return nil, leases, fmt.Errorf("transfer %d received mismatched file-view access",
				selected.transfer.Value())
		}
		execution, executionErr := newTransferExecution(selected, egresses[selected.transfer], access, request.argv)
		if executionErr != nil {
			return nil, leases, executionErr
		}
		executions = append(executions, execution)
	}
	return executions, leases, nil
}

func newTransferExecution(selection selectedNetwork, egress networkcatalog.Egress,
	view childprocesses.ViewAccess, argv []string,
) (TransferExecution, error) {
	if selection.transfer.Value() == 0 || view.TransferNumber() != selection.transfer {
		return TransferExecution{}, errors.New("transfer execution facts do not share one identity")
	}
	viewPath, err := childprocesses.DescriptorBoundViewPath(view.RunID(), view.TransferNumber(),
		view.BaseName())
	if err != nil {
		return TransferExecution{}, err
	}
	exactArgv, err := replaceViewArgument(argv, viewPath)
	if err != nil {
		return TransferExecution{}, err
	}
	shortcut, err := transferShortcut(selection.transfer)
	if err != nil {
		return TransferExecution{}, err
	}
	return TransferExecution{number: selection.transfer, shortcut: shortcut, egress: egress,
		view: view, viewPath: viewPath, argv: exactArgv}, nil
}

func transferShortcut(number transfernumber.Number) (byte, error) {
	value := number.Value()
	switch {
	case value >= 1 && value <= 9:
		return '0' + value, nil
	case value >= 10 && value <= transfernumber.Maximum:
		return 'a' + value - 10, nil
	default:
		return 0, fmt.Errorf("transfer shortcut identity is invalid")
	}
}

func replaceViewArgument(template []string, path string) ([]string, error) {
	if !cleanAbsolutePath(path) {
		return nil, errors.New("descriptor-bound file-view path is invalid")
	}
	result := cloneText(template)
	replaced := 0
	for index, argument := range result {
		if argument == viewPlaceholder {
			result[index] = path
			replaced++
		}
	}
	if replaced != 1 {
		return nil, errors.New("command template does not contain one standalone view argument")
	}
	return result, nil
}

func (execution TransferExecution) Number() transfernumber.Number { return execution.number }
func (execution TransferExecution) Shortcut() byte                { return execution.shortcut }
func (execution TransferExecution) Egress() networkcatalog.Egress { return execution.egress }
func (execution TransferExecution) ViewPath() string              { return execution.viewPath }
func (execution TransferExecution) Argv() []string                { return cloneText(execution.argv) }

func (execution TransferExecution) command(resolver namespaceresolvers.ResolverAccess, environment []string) (childprocesses.Command, error) {
	return childprocesses.NewViewCommand(execution.number, resolver, execution.argv, environment,
		execution.view)
}
