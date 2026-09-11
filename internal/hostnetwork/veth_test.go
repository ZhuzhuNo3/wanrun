package hostnetwork

import (
	"errors"
	"strings"
	"testing"
)

func TestVethCreationCarriesCompositeOwnerAndPeerNamespaceAtomically(t *testing.T) {
	value := testAllocation(t, firewallNFTables).transfers[0]
	links := &recordingVethCreation{}
	receipt, err := establishVethPair(links, value, 42)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.transfer != value.transfer || links.createCalls != 1 ||
		links.hostMAC != value.hostMAC || links.peerMAC != value.peerMAC || links.namespaceFD != 42 {
		t.Fatalf("atomic create = receipt %v calls %d host MAC %s peer MAC %s namespace %d",
			receipt, links.createCalls, links.hostMAC, links.peerMAC, links.namespaceFD)
	}
}

func TestVethCreateErrorNeverProducesReceiptOrAcceptsObservedMarker(t *testing.T) {
	value := testAllocation(t, firewallNFTables).transfers[0]
	for _, test := range []struct {
		name          string
		commitOnError bool
		err           error
	}{
		{name: "definite EEXIST", commitOnError: true, err: errors.New("EEXIST")},
		{name: "commit before ACK loss", commitOnError: true, err: errors.New("lost ACK")},
		{name: "failure before commit", err: errors.New("socket failure")},
	} {
		t.Run(test.name, func(t *testing.T) {
			links := &recordingVethCreation{createErr: test.err, commitOnError: test.commitOnError}
			receipt, err := establishVethPair(links, value, 42)
			if err == nil {
				t.Fatal("failed LinkAdd was accepted after observing the requested object")
			}
			if receipt != (linkReceipt{}) {
				t.Fatalf("failed LinkAdd produced receipt %#v", receipt)
			}
		})
	}
}

func TestLiveVethVerificationRequiresFreshReciprocalGraph(t *testing.T) {
	value := testAllocation(t, firewallNFTables).transfers[0]
	links := &recordingVethCreation{exists: true, hostMAC: value.hostMAC, peerMAC: value.peerMAC,
		hostPeerIndex: 12}
	err := verifyVethPair(links, value, 42, "eth0")
	if err == nil {
		t.Fatal("non-reciprocal live veth graph was accepted before activation")
	}
	if !strings.Contains(err.Error(), temporaryVethHostRequirement) {
		t.Fatalf("veth takeover error = %v", err)
	}
}

type recordingVethCreation struct {
	createCalls   int
	createErr     error
	commitOnError bool
	exists        bool
	hostName      string
	peerName      string
	hostMAC       linkMAC
	peerMAC       linkMAC
	namespaceFD   int
	hostPeerIndex int
	peerPeerIndex int
}

func (links *recordingVethCreation) CreateAtomic(hostName string, hostMAC linkMAC, peerName string,
	peerMAC linkMAC, _ int, namespaceFD int) error {
	links.createCalls++
	links.hostName, links.peerName = hostName, peerName
	links.hostMAC, links.peerMAC, links.namespaceFD = hostMAC, peerMAC, namespaceFD
	if links.createErr == nil || links.commitOnError {
		links.exists = true
	}
	return links.createErr
}

func (links *recordingVethCreation) HostLink(name string) (vethIdentity, error) {
	if !links.exists {
		return vethIdentity{}, errors.New("link not found")
	}
	peerIndex := links.hostPeerIndex
	if peerIndex == 0 {
		peerIndex = 11
	}
	return vethIdentity{name: name, mac: links.hostMAC, kind: "veth", index: 10, peerIndex: peerIndex}, nil
}

func (links *recordingVethCreation) NamespaceLink(_ int, name string) (vethIdentity, error) {
	if !links.exists {
		return vethIdentity{}, errors.New("link not found")
	}
	peerIndex := links.peerPeerIndex
	if peerIndex == 0 {
		peerIndex = 10
	}
	return vethIdentity{name: name, mac: links.peerMAC, kind: "veth", index: 11, peerIndex: peerIndex}, nil
}
