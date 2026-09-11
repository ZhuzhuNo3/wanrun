package hostnetwork

import (
	"errors"
	"fmt"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type weakFIBKernel interface {
	addReturnRoute(networkClaim, transferAllocation) error
	addOutboundRule(networkClaim, transferAllocation) error
	addReturnRule(networkClaim, transferAllocation) error
	deleteReturnRoute(networkClaim, transferAllocation) error
	deleteOutboundRule(networkClaim, transferAllocation) error
	deleteReturnRule(networkClaim, transferAllocation) error
}

type returnRouteReceipt struct{ transfer transfernumber.Number }
type outboundRuleReceipt struct{ transfer transfernumber.Number }
type returnRuleReceipt struct{ transfer transfernumber.Number }

type weakFIBReceipts struct {
	returnRoutes  map[transfernumber.Number]returnRouteReceipt
	outboundRules map[transfernumber.Number]outboundRuleReceipt
	returnRules   map[transfernumber.Number]returnRuleReceipt
}

func newWeakFIBReceipts() weakFIBReceipts {
	return weakFIBReceipts{
		returnRoutes:  make(map[transfernumber.Number]returnRouteReceipt),
		outboundRules: make(map[transfernumber.Number]outboundRuleReceipt),
		returnRules:   make(map[transfernumber.Number]returnRuleReceipt),
	}
}

func installWeakFIB(kernel weakFIBKernel, claim networkClaim) (weakFIBReceipts, error) {
	receipts := newWeakFIBReceipts()
	for _, transfer := range claim.transfers {
		if err := installTransferFIB(kernel, claim, transfer, &receipts); err != nil {
			return receipts, fmt.Errorf("install transfer %d routing: %w", transfer.number, err)
		}
	}
	return receipts, nil
}

func installTransferFIB(kernel weakFIBKernel, claim networkClaim, transfer transferAllocation,
	receipts *weakFIBReceipts,
) error {
	if err := kernel.addReturnRoute(claim, transfer); err != nil {
		return err
	}
	receipts.returnRoutes[transfer.transfer] = returnRouteReceipt{transfer: transfer.transfer}
	if err := kernel.addOutboundRule(claim, transfer); err != nil {
		return err
	}
	receipts.outboundRules[transfer.transfer] = outboundRuleReceipt{transfer: transfer.transfer}
	if err := kernel.addReturnRule(claim, transfer); err != nil {
		return err
	}
	receipts.returnRules[transfer.transfer] = returnRuleReceipt{transfer: transfer.transfer}
	return nil
}

func (receipts weakFIBReceipts) requireComplete(claim networkClaim) error {
	if len(receipts.returnRoutes) != len(claim.transfers) ||
		len(receipts.outboundRules) != len(claim.transfers) || len(receipts.returnRules) != len(claim.transfers) {
		return errors.New("routing receipt set is incomplete")
	}
	for _, transfer := range claim.transfers {
		if _, exists := receipts.returnRoutes[transfer.transfer]; !exists {
			return fmt.Errorf("return-route receipt is missing for transfer %d", transfer.number)
		}
		if _, exists := receipts.outboundRules[transfer.transfer]; !exists {
			return fmt.Errorf("outbound-rule receipt is missing for transfer %d", transfer.number)
		}
		if _, exists := receipts.returnRules[transfer.transfer]; !exists {
			return fmt.Errorf("return-rule receipt is missing for transfer %d", transfer.number)
		}
	}
	return nil
}

func rollbackWeakFIB(kernel weakFIBKernel, claim networkClaim, receipts weakFIBReceipts) error {
	var cleanupErrors []error
	for index := len(claim.transfers) - 1; index >= 0; index-- {
		transfer := claim.transfers[index]
		if _, exists := receipts.returnRules[transfer.transfer]; exists {
			cleanupErrors = appendDeleteError(cleanupErrors, kernel.deleteReturnRule(claim, transfer))
		}
		if _, exists := receipts.outboundRules[transfer.transfer]; exists {
			cleanupErrors = appendDeleteError(cleanupErrors, kernel.deleteOutboundRule(claim, transfer))
		}
		if _, exists := receipts.returnRoutes[transfer.transfer]; exists {
			cleanupErrors = appendDeleteError(cleanupErrors, kernel.deleteReturnRoute(claim, transfer))
		}
	}
	return errors.Join(cleanupErrors...)
}

func appendDeleteError(found []error, err error) []error {
	if err != nil {
		return append(found, err)
	}
	return found
}
