//go:build linux && rootintegration

package hostnetwork

import (
	"bytes"
	"os"
	"os/exec"
	"testing"
)

func TestNFTInstallRefusesForeignTableThatWinsThePreflightRace(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatalf("root integration requires uid 0, got %d", os.Geteuid())
	}
	if _, err := exec.LookPath("nft"); err != nil {
		t.Fatalf("root integration requires nft: %v", err)
	}
	claim := claimFromAllocation(testAllocation(t, firewallNFTables))
	runNFTTestCommand(t, "add", "table", "ip", claim.firewallTable)
	t.Cleanup(func() { runNFTTestCommand(t, "delete", "table", "ip", claim.firewallTable) })
	before := runNFTTestCommand(t, "list", "table", "ip", claim.firewallTable)

	program, err := buildNFTProgram(claim)
	if err != nil {
		t.Fatal(err)
	}
	if err := installNFT(program); err == nil {
		t.Fatal("nft install accepted a foreign table created after preflight")
	}
	after := runNFTTestCommand(t, "list", "table", "ip", claim.firewallTable)
	if !bytes.Equal(after, before) {
		t.Fatalf("failed exclusive nft install changed foreign table:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func runNFTTestCommand(t *testing.T, argv ...string) []byte {
	t.Helper()
	output, err := exec.Command("nft", argv...).CombinedOutput()
	if err != nil {
		t.Fatalf("nft %q: %v: %s", argv, err, output)
	}
	return output
}
