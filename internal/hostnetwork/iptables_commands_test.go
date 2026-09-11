package hostnetwork

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestIPTablesProgramUsesDirectOwnerMarkedRulesOnly(t *testing.T) {
	claim := iptablesFixtureClaim(t, "29112233445566778899aabbccddeeff")
	commands, err := buildIPTablesInstallCommands(claim)
	if err != nil {
		t.Fatal(err)
	}
	owner := "transferlanes:" + claim.runID.String() + ":" + claim.ownerToken + ":1"
	transfer := claim.transfers[0]
	assertArgvPresent(t, commands, []string{"-w", "5", "-t", "filter", "-I", "FORWARD", "1",
		"-s", transfer.subnet.String(), "-i", transfer.hostVeth, "-o", transfer.providerName,
		"-m", "comment", "--comment", owner + ":forward-out", "-j", "ACCEPT"})
	assertArgvPresent(t, commands, []string{"-w", "5", "-t", "filter", "-I", "FORWARD", "1",
		"-d", transfer.subnet.String(), "-i", transfer.providerName, "-o", transfer.hostVeth,
		"-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED",
		"-m", "comment", "--comment", owner + ":return", "-j", "ACCEPT"})
	assertArgvPresent(t, commands, []string{"-w", "5", "-t", "nat", "-I", "POSTROUTING", "1",
		"-s", transfer.namespaceIP.String() + "/32", "-o", transfer.providerName,
		"-m", "comment", "--comment", owner + ":snat", "-j", "SNAT", "--to-source", transfer.source.String()})
	for _, command := range commands {
		joined := strings.Join(command, " ")
		if len(command) < 7 || command[0] != "-w" || command[1] != "5" || command[2] != "-t" ||
			!slices.Contains(command, "-I") || slices.Contains(command, "-N") || slices.Contains(command, "-A") ||
			strings.Contains(joined, "WRF_") || strings.Contains(joined, "WRN_") || strings.Contains(joined, "MARK") {
			t.Fatalf("iptables program contains a non-direct or unowned mutation: %q", command)
		}
	}
}

func TestIPTablesBackendUsesDirectBoundedCommands(t *testing.T) {
	runner := &scriptedArgvRunner{responses: []argvResponse{
		{output: []byte("iptables v1.8.9 (nf_tables)\n")},
		{output: []byte("iptables-save v1.8.9 (nf_tables)\n")},
		{output: []byte("iptables v1.8.9 (nf_tables)\n")},
		{output: []byte("iptables-save v1.8.9 (nf_tables)\n")},
		{output: []byte("*filter\n:FORWARD ACCEPT [0:0]\nCOMMIT\n")},
		{output: []byte("*nat\n:POSTROUTING ACCEPT [0:0]\nCOMMIT\n")},
		{}, {}, {},
	}}
	backend := testIPTablesFirewall(runner)
	inventory, err := backend.Inspect(context.Background())
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if inventory.kind != firewallIPTables || inventory.iptablesFrontend != iptablesFrontendNFT ||
		len(runner.calls) != 9 {
		t.Fatalf("iptables inventory/calls = %#v/%q", inventory, runner.calls)
	}
	for index, call := range runner.calls {
		if index < 2 {
			if call.name != []string{"iptables", "iptables-save"}[index] ||
				!slices.Equal(call.argv, []string{"--version"}) {
				t.Fatalf("unexpected active frontend discovery call %d: %s %q", index, call.name, call.argv)
			}
			continue
		}
		if call.name != "iptables-nft" && call.name != "iptables-nft-save" {
			t.Fatalf("inventory escaped the selected nft rule plane: %s %q", call.name, call.argv)
		}
		if call.name == "iptables-nft-save" {
			if slices.Equal(call.argv, []string{"--version"}) {
				continue
			}
			if len(call.argv) != 2 || call.argv[0] != "-t" {
				t.Fatalf("unexpected read-only iptables-save argv: %q", call.argv)
			}
			continue
		}
		if slices.Equal(call.argv, []string{"--version"}) {
			continue
		}
		if len(call.argv) < 2 || call.argv[0] != "-w" || call.argv[1] != "5" {
			t.Fatalf("iptables capability command lacks xtables wait: %q", call.argv)
		}
	}
}

func TestIPTablesLifecycleUsesOnlyTheClaimedRulePlaneCommands(t *testing.T) {
	claim := iptablesFixtureClaim(t, "2e212233445566778899aabbccddeeff")
	responses := []argvResponse{
		{output: []byte("iptables v1.8.9 (nf_tables)\n")},
		{output: []byte("iptables-save v1.8.9 (nf_tables)\n")},
		{}, {},
	}
	runner := &scriptedArgvRunner{responses: responses}
	backend := testIPTablesFirewall(runner)
	if err := backend.Recover(context.Background(), claim); err != nil {
		t.Fatalf("remove claimed nft rule plane: %v", err)
	}
	for _, call := range runner.calls {
		if call.name != "iptables-nft" && call.name != "iptables-nft-save" {
			t.Fatalf("claimed lifecycle used mutable alternatives command: %s %q", call.name, call.argv)
		}
	}
}

func TestIPTablesLifecycleDoesNotRequireTheUnselectedRulePlane(t *testing.T) {
	for _, frontend := range []iptablesFrontend{iptablesFrontendNFT, iptablesFrontendLegacy} {
		t.Run(string(frontend), func(t *testing.T) {
			claim := iptablesFixtureClaim(t, "2e712233445566778899aabbccddeeff")
			claim.iptablesFrontend = frontend
			preflightRunner := &scriptedArgvRunner{responses: append(
				fixedFrontendResponses(frontend), argvResponse{}, argvResponse{}, argvResponse{})}
			if err := testIPTablesFirewall(preflightRunner).Preflight(context.Background(), claim); err != nil {
				t.Fatalf("preflight with only the selected rule plane: %v", err)
			}
			assertOnlyFixedFrontendCommands(t, preflightRunner.calls, frontend)

			absentResponses := fixedFrontendResponses(frontend)
			if frontend == iptablesFrontendNFT {
				absentResponses = append(absentResponses, argvResponse{}, argvResponse{})
			}
			absentRunner := &scriptedArgvRunner{responses: absentResponses}
			if err := testIPTablesFirewall(absentRunner).Recover(context.Background(), claim); err != nil {
				t.Fatalf("remove with only the selected rule plane: %v", err)
			}
			assertOnlyFixedFrontendCommands(t, absentRunner.calls, frontend)

			absentRunner = &scriptedArgvRunner{responses: append([]argvResponse(nil), absentResponses...)}
			if err := testIPTablesFirewall(absentRunner).VerifyAbsent(context.Background(), claim); err != nil {
				t.Fatalf("verify absence with only the selected rule plane: %v", err)
			}
			assertOnlyFixedFrontendCommands(t, absentRunner.calls, frontend)
		})
	}
}

func TestDirectArgvOutputIsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	output := &boundedArgvOutput{limit: 4, cancel: cancel}
	if written, err := output.Write([]byte("abcdef")); err != nil || written != 6 {
		t.Fatalf("bounded write = %d, %v", written, err)
	}
	if !output.wasExceeded() || string(output.bytes()) != "abcd" || ctx.Err() == nil {
		t.Fatalf("bounded output = exceeded %v, %q, context %v",
			output.wasExceeded(), output.bytes(), ctx.Err())
	}
}
