package hostnetwork

import (
	"errors"
	"testing"
)

func TestWeakFIBReceiptsExistOnlyAfterPositiveAddResult(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	kernel := &recordingRoutingKernel{failOperation: "outbound-rule", commitOnFailure: true}
	receipts, err := installWeakFIB(kernel, claim)
	if err == nil {
		t.Fatal("uncertain outbound-rule add was accepted")
	}
	if len(receipts.returnRoutes) != 1 || len(receipts.outboundRules) != 0 || len(receipts.returnRules) != 0 {
		t.Fatalf("receipts after uncertain ACK = %#v", receipts)
	}
	if err := rollbackWeakFIB(kernel, claim, receipts); err != nil {
		t.Fatal(err)
	}
	if got, want := kernel.deletes, []string{"return-route:1"}; !equalStrings(got, want) {
		t.Fatalf("receipt-authorized deletes = %v, want %v", got, want)
	}
	if !kernel.committedWithoutReceipt {
		t.Fatal("test did not model kernel commit followed by lost ACK")
	}
}

func TestCompleteWeakFIBReceiptsCoverEveryClaimedTransfer(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	kernel := &recordingRoutingKernel{}
	receipts, err := installWeakFIB(kernel, claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := receipts.requireComplete(claim); err != nil {
		t.Fatalf("complete receipt set rejected: %v", err)
	}
	delete(receipts.returnRules, claim.transfers[0].transfer)
	if err := receipts.requireComplete(claim); err == nil {
		t.Fatal("incomplete receipt set was accepted")
	}
}

type recordingRoutingKernel struct {
	failOperation           string
	commitOnFailure         bool
	committedWithoutReceipt bool
	deletes                 []string
}

func (kernel *recordingRoutingKernel) addReturnRoute(_ networkClaim, transfer transferAllocation) error {
	return kernel.add("return-route", transfer.number)
}

func (kernel *recordingRoutingKernel) addOutboundRule(_ networkClaim, transfer transferAllocation) error {
	return kernel.add("outbound-rule", transfer.number)
}

func (kernel *recordingRoutingKernel) addReturnRule(_ networkClaim, transfer transferAllocation) error {
	return kernel.add("return-rule", transfer.number)
}

func (kernel *recordingRoutingKernel) deleteReturnRoute(_ networkClaim, transfer transferAllocation) error {
	kernel.deletes = append(kernel.deletes, operationName("return-route", transfer.number))
	return nil
}

func (kernel *recordingRoutingKernel) deleteOutboundRule(_ networkClaim, transfer transferAllocation) error {
	kernel.deletes = append(kernel.deletes, operationName("outbound-rule", transfer.number))
	return nil
}

func (kernel *recordingRoutingKernel) deleteReturnRule(_ networkClaim, transfer transferAllocation) error {
	kernel.deletes = append(kernel.deletes, operationName("return-rule", transfer.number))
	return nil
}

func (kernel *recordingRoutingKernel) add(kind string, transfer int) error {
	if kernel.failOperation != kind {
		return nil
	}
	kernel.committedWithoutReceipt = kernel.commitOnFailure
	return errors.New("injected ACK uncertainty")
}

func operationName(kind string, transfer int) string {
	return kind + ":" + string(rune('0'+transfer))
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
