package hostnetwork

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

const testOwnerToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestNetworkClaimHasNoMutationProgressAndUsesTransferVocabulary(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	encoded, err := encodeClaim(claim)
	if err != nil {
		t.Fatalf("encode claim: %v", err)
	}
	text := string(encoded)
	if !strings.Contains(text, `"transfers"`) {
		t.Fatalf("claim has no transfers field: %s", text)
	}
	for _, forbidden := range []string{`"lines"`, `"created"`, `"firewall_created"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("immutable claim contains progress field %s: %s", forbidden, text)
		}
	}
	decoded, err := decodeClaim(bytes.NewReader(encoded), claim.runID)
	if err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if !claimsEqual(decoded, claim) {
		t.Fatalf("claim round trip mismatch:\n got %#v\nwant %#v", decoded, claim)
	}
}

func TestTrafficActivationBindsTheExactImmutableClaim(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	encoded, err := encodeTrafficActivation(claim)
	if err != nil {
		t.Fatalf("encode traffic activation: %v", err)
	}
	if err := decodeTrafficActivation(bytes.NewReader(encoded), claim); err != nil {
		t.Fatalf("decode matching traffic activation: %v", err)
	}

	changed := claim
	changed.returnTable++
	if err := decodeTrafficActivation(bytes.NewReader(encoded), changed); err == nil {
		t.Fatal("activation accepted a different claim")
	}
}

func TestNetworkClaimProtocolMustBeInAllocatorRange(t *testing.T) {
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	for _, test := range []struct {
		protocol uint8
		valid    bool
	}{
		{protocol: 0},
		{protocol: firstClaimProtocol - 1},
		{protocol: firstClaimProtocol, valid: true},
		{protocol: lastClaimProtocol, valid: true},
		{protocol: lastClaimProtocol + 1},
		{protocol: ^uint8(0)},
	} {
		t.Run(fmt.Sprintf("protocol_%d", test.protocol), func(t *testing.T) {
			encoded := claimEncodingWithProtocol(t, claim, test.protocol)
			_, err := decodeClaim(bytes.NewReader(encoded), claim.runID)
			if (err == nil) != test.valid {
				t.Fatalf("decode protocol %d error = %v, valid = %t",
					test.protocol, err, test.valid)
			}
		})
	}
}

func TestOwnerTokensAreIndependentFullEntropyIdentities(t *testing.T) {
	first, err := newOwnerToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := newOwnerToken()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || !validOwnerToken(first) || !validOwnerToken(second) {
		t.Fatalf("owner tokens are not distinct canonical identities: %q %q", first, second)
	}
}

func testAllocation(t *testing.T, kind firewallKind) networkAllocation {
	t.Helper()
	inventory := emptyInventory()
	if kind == firewallIPTables {
		inventory = emptyIPTablesInventory()
	}
	allocation, err := allocateClaimNetwork(mustRunID(t, "f0112233445566778899aabbccddeeff"),
		testOwnerToken, testSelections(), inventory)
	if err != nil {
		t.Fatal(err)
	}
	return allocation
}

func claimEncodingWithProtocol(t *testing.T, claim networkClaim, protocol uint8) []byte {
	t.Helper()
	encoded, err := encodeClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	var document claimDocument
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	document.Protocol = protocol
	encoded, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}

func writeClaimEncoding(t *testing.T, root *os.File, encoded []byte) {
	t.Helper()
	directory, _, err := openNetworkDirectory(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	file, err := openTestEvidenceFile(directory, claimFileName)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}
