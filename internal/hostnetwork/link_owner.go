package hostnetwork

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const linkOwnerBytes = 15

type linkOwnerID [linkOwnerBytes]byte
type linkMAC [6]byte

func deriveLinkOwnerID(ownerToken string, transfer transfernumber.Number) (linkOwnerID, error) {
	key, err := hex.DecodeString(ownerToken)
	if err != nil || len(key) != 32 {
		return linkOwnerID{}, fmt.Errorf("derive link owner from invalid network owner token")
	}
	digest := keyedLinkDigest(key, "transferlanes-link-owner-v1", transfer)
	var owner linkOwnerID
	copy(owner[:], digest[:linkOwnerBytes])
	return owner, nil
}

func derivePeerMAC(ownerToken string, transfer transfernumber.Number) (linkMAC, error) {
	key, err := hex.DecodeString(ownerToken)
	if err != nil || len(key) != 32 {
		return linkMAC{}, fmt.Errorf("derive peer MAC from invalid network owner token")
	}
	digest := keyedLinkDigest(key, "transferlanes-link-peer-v1", transfer)
	return linkMAC{0x06, digest[0], digest[1], digest[2], digest[3], digest[4]}, nil
}

func keyedLinkDigest(key []byte, domain string, transfer transfernumber.Number) [sha256.Size]byte {
	hash := hmac.New(sha256.New, key)
	_, _ = hash.Write([]byte(domain))
	_, _ = hash.Write([]byte{0, transfer.Value()})
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

func (owner linkOwnerID) String() string {
	return base64.RawURLEncoding.EncodeToString(owner[:])
}

func parseLinkOwnerID(value string) (linkOwnerID, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(decoded) != linkOwnerBytes {
		return linkOwnerID{}, fmt.Errorf("link owner ID is invalid")
	}
	var owner linkOwnerID
	copy(owner[:], decoded)
	if owner.String() != value {
		return linkOwnerID{}, fmt.Errorf("link owner ID is not canonical")
	}
	return owner, nil
}

func (owner linkOwnerID) hostName() string {
	return "w" + base64.RawURLEncoding.EncodeToString(owner[:10])
}

func (owner linkOwnerID) hostMAC() linkMAC {
	return linkMAC{0x02, owner[10], owner[11], owner[12], owner[13], owner[14]}
}

func linkOwnerIDFromHost(name string, mac linkMAC) (linkOwnerID, error) {
	if len(name) != 15 || name[0] != 'w' || mac[0] != 0x02 {
		return linkOwnerID{}, fmt.Errorf("host link owner marker is invalid")
	}
	nameBytes, err := base64.RawURLEncoding.Strict().DecodeString(name[1:])
	if err != nil || len(nameBytes) != 10 {
		return linkOwnerID{}, fmt.Errorf("host link owner name is invalid")
	}
	var owner linkOwnerID
	copy(owner[:10], nameBytes)
	copy(owner[10:], mac[1:])
	if owner.hostName() != name || owner.hostMAC() != mac {
		return linkOwnerID{}, fmt.Errorf("host link owner marker is not canonical")
	}
	return owner, nil
}

func (mac linkMAC) String() string { return net.HardwareAddr(mac[:]).String() }

func (mac linkMAC) hardwareAddr() net.HardwareAddr {
	return append(net.HardwareAddr(nil), mac[:]...)
}

func parseLinkMAC(value string) (linkMAC, error) {
	parsed, err := net.ParseMAC(value)
	if err != nil || len(parsed) != 6 {
		return linkMAC{}, fmt.Errorf("link MAC is invalid")
	}
	var mac linkMAC
	copy(mac[:], parsed)
	if mac.String() != value {
		return linkMAC{}, fmt.Errorf("link MAC is not canonical")
	}
	return mac, nil
}

func linkMACFromHardwareAddr(value net.HardwareAddr) (linkMAC, error) {
	if len(value) != 6 {
		return linkMAC{}, fmt.Errorf("link has invalid MAC length %d", len(value))
	}
	var mac linkMAC
	copy(mac[:], value)
	return mac, nil
}

func hardwareAddrEquals(value net.HardwareAddr, expected linkMAC) bool {
	actual, err := linkMACFromHardwareAddr(value)
	return err == nil && actual == expected
}
