package hostnetwork

import (
	"fmt"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

type linkReceipt struct {
	transfer transfernumber.Number
}

const temporaryVethHostRequirement = "privileged network managers must ignore Transfer Lanes temporary veths"

func ambiguousTemporaryVeth(detail string) error {
	return fmt.Errorf("%s; %s", detail, temporaryVethHostRequirement)
}

type linkReceipts map[transfernumber.Number]linkReceipt

func newLinkReceipts() linkReceipts { return make(linkReceipts) }

func (receipts linkReceipts) add(receipt linkReceipt) {
	if receipt.transfer.Value() != 0 {
		receipts[receipt.transfer] = receipt
	}
}

func (receipts linkReceipts) requireComplete(claim networkClaim) error {
	if len(receipts) != len(claim.transfers) {
		return fmt.Errorf("live link receipt set is incomplete")
	}
	for _, transfer := range claim.transfers {
		if receipt, exists := receipts[transfer.transfer]; !exists || receipt.transfer != transfer.transfer {
			return fmt.Errorf("live link receipt set is incomplete")
		}
	}
	return nil
}

type vethIdentity struct {
	name      string
	mac       linkMAC
	kind      string
	index     int
	peerIndex int
}

type vethCreation interface {
	CreateAtomic(hostName string, hostMAC linkMAC, peerName string, peerMAC linkMAC,
		mtu, namespaceFD int) error
	HostLink(string) (vethIdentity, error)
	NamespaceLink(namespaceFD int, name string) (vethIdentity, error)
}

func establishVethPair(links vethCreation, value transferAllocation, namespaceFD int) (linkReceipt, error) {
	if err := links.CreateAtomic(value.hostVeth, value.hostMAC, value.peerVeth, value.peerMAC,
		1500, namespaceFD); err != nil {
		return linkReceipt{}, fmt.Errorf("create atomic veth pair: %w", err)
	}
	receipt := linkReceipt{transfer: value.transfer}
	if err := verifyVethPair(links, value, namespaceFD, value.peerVeth); err != nil {
		return receipt, err
	}
	return receipt, nil
}

func verifyVethPair(links vethCreation, value transferAllocation, namespaceFD int,
	peerName string,
) error {
	host, err := links.HostLink(value.hostVeth)
	if err != nil {
		return fmt.Errorf("observe host veth: %w", err)
	}
	peer, err := links.NamespaceLink(namespaceFD, peerName)
	if err != nil {
		return fmt.Errorf("observe namespace veth: %w", err)
	}
	return verifyVethIdentityAndGraph(host, peer, value, peerName)
}

func verifyVethIdentityAndGraph(host, peer vethIdentity, value transferAllocation,
	peerName string,
) error {
	if host.name != value.hostVeth || host.mac != value.hostMAC || host.kind != "veth" || host.index <= 0 ||
		peer.name != peerName || peer.mac != value.peerMAC || peer.kind != "veth" || peer.index <= 0 ||
		host.peerIndex != peer.index || peer.peerIndex != host.index {
		return ambiguousTemporaryVeth(fmt.Sprintf(
			"veth owner marker or object graph mismatch: host=%+v peer=%+v", host, peer))
	}
	return nil
}
