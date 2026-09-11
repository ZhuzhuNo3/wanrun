package hostnetwork

import (
	"net"
	"testing"
)

func TestLinkOwnerIDMapsEveryBitToHostNameAndMAC(t *testing.T) {
	owner := linkOwnerID{
		0x00, 0x01, 0x02, 0x03, 0x04,
		0x05, 0x06, 0x07, 0x08, 0x09,
		0x0a, 0x0b, 0x0c, 0x0d, 0x0e,
	}
	if got, want := owner.hostName(), "wAAECAwQFBgcICQ"; got != want {
		t.Fatalf("host name = %q, want %q", got, want)
	}
	if got, want := owner.hostMAC(), (linkMAC{0x02, 0x0a, 0x0b, 0x0c, 0x0d, 0x0e}); got != want {
		t.Fatalf("host MAC = %s, want %s", got, want)
	}
	if len(owner.hostName()) != 15 {
		t.Fatalf("host name length = %d, want 15", len(owner.hostName()))
	}
	if got := owner.hostMAC().hardwareAddr(); got[0]&0x03 != 0x02 {
		t.Fatalf("host MAC %s is not locally administered unicast", net.HardwareAddr(got))
	}
	decoded, err := linkOwnerIDFromHost(owner.hostName(), owner.hostMAC())
	if err != nil {
		t.Fatal(err)
	}
	if decoded != owner {
		t.Fatalf("decoded owner = %x, want %x", decoded, owner)
	}
}

func TestLinkOwnerIDDerivationSeparatesTransfers(t *testing.T) {
	first := mustTransferNumber(t, 1)
	second := mustTransferNumber(t, 2)
	firstOwner, err := deriveLinkOwnerID(testOwnerToken, first)
	if err != nil {
		t.Fatal(err)
	}
	secondOwner, err := deriveLinkOwnerID(testOwnerToken, second)
	if err != nil {
		t.Fatal(err)
	}
	again, err := deriveLinkOwnerID(testOwnerToken, first)
	if err != nil {
		t.Fatal(err)
	}
	if firstOwner == secondOwner {
		t.Fatal("two transfer identities derived the same link owner")
	}
	if firstOwner != again {
		t.Fatal("the same transfer did not retain its fixed link owner")
	}
}
