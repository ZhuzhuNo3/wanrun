package hostnetwork

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"
)

func TestIPTablesLifecycleRejectsNonCurrentClaimsBeforeCommands(t *testing.T) {
	operations := map[string]func(iptablesFirewall, networkClaim) error{
		"preflight install": func(backend iptablesFirewall, claim networkClaim) error {
			return backend.Preflight(context.Background(), claim)
		},
		"verify": func(backend iptablesFirewall, claim networkClaim) error {
			return backend.Verify(context.Background(), claim, true)
		},
		"remove": func(backend iptablesFirewall, claim networkClaim) error {
			return backend.Recover(context.Background(), claim)
		},
		"verify absent": func(backend iptablesFirewall, claim networkClaim) error {
			return backend.VerifyAbsent(context.Background(), claim)
		},
	}
	for _, version := range []int{1, 2} {
		for name, operation := range operations {
			t.Run(fmt.Sprintf("version-%d/%s", version, name), func(t *testing.T) {
				claim := iptablesFixtureClaim(t, "2f012233445566778899aabbccddeeff")
				claim.version = version
				claim.iptablesFrontend = ""
				runner := &scriptedArgvRunner{}
				backend := testIPTablesFirewall(runner)
				if err := operation(backend, claim); err == nil {
					t.Fatal("operation through a non-current claim unexpectedly succeeded")
				}
				if len(runner.calls) != 0 {
					t.Fatalf("non-current claim invoked commands: %q", runner.calls)
				}
			})
		}
	}
}

func TestIPTablesLifecycleRefusesAFrontendDifferentFromTheClaim(t *testing.T) {
	claim := iptablesFixtureClaim(t, "2f112233445566778899aabbccddeeff")
	frontends := map[string][]argvResponse{
		"changed": {
			{output: []byte("iptables v1.8.9 (legacy)\n")},
			{output: []byte("iptables-save v1.8.9 (legacy)\n")},
		},
		"unidentified": {
			{output: []byte("iptables v1.6.0\n")},
			{output: []byte("iptables-save v1.6.0\n")},
		},
		"inconsistent commands": {
			{output: []byte("iptables v1.8.9 (nf_tables)\n")},
			{output: []byte("iptables-save v1.8.9 (legacy)\n")},
		},
	}
	operations := map[string]func(iptablesFirewall, networkClaim) error{
		"preflight install": func(backend iptablesFirewall, claim networkClaim) error {
			return backend.Preflight(context.Background(), claim)
		},
		"verify": func(backend iptablesFirewall, claim networkClaim) error {
			return backend.Verify(context.Background(), claim, true)
		},
		"remove": func(backend iptablesFirewall, claim networkClaim) error {
			return backend.Recover(context.Background(), claim)
		},
		"verify absent": func(backend iptablesFirewall, claim networkClaim) error {
			return backend.VerifyAbsent(context.Background(), claim)
		},
	}
	for frontendName, responses := range frontends {
		for operationName, operation := range operations {
			t.Run(frontendName+"/"+operationName, func(t *testing.T) {
				runner := &scriptedArgvRunner{responses: append([]argvResponse(nil), responses...)}
				backend := testIPTablesFirewall(runner)
				if err := operation(backend, claim); err == nil {
					t.Fatal("operation through a different frontend unexpectedly succeeded")
				}
				if len(runner.calls) != 2 || runner.calls[0].name != iptablesNFTRulesCommand ||
					runner.calls[1].name != iptablesNFTSaveCommand ||
					!slices.Equal(runner.calls[0].argv, []string{"--version"}) ||
					!slices.Equal(runner.calls[1].argv, []string{"--version"}) {
					t.Fatalf("frontend mismatch reached ruleset access or mutation: %q", runner.calls)
				}
			})
		}
	}
}

func TestIPTablesInstallAndRemoveObserveEveryDirectRuleMutation(t *testing.T) {
	claim := iptablesFixtureClaim(t, "2f212233445566778899aabbccddeeff")
	runner := newMutableIPTablesRulePlane(iptablesFrontendNFT)
	backend := testIPTablesFirewall(runner)
	receipts, err := backend.Install(context.Background(), claim)
	if err != nil {
		t.Fatalf("install direct rules: %v", err)
	}
	program, err := buildIPTablesProgram(claim)
	if err != nil {
		t.Fatal(err)
	}
	if runner.mutations != len(program.rules) || runner.observations != len(program.rules)+1 {
		t.Fatalf("install mutations/observations = %d/%d, want %d/%d",
			runner.mutations, runner.observations, len(program.rules), len(program.rules)+1)
	}
	if err := backend.Verify(context.Background(), claim, false); err != nil {
		t.Fatalf("verify direct rules: %v", err)
	}
	before := append([][]string(nil), runner.foreign...)
	runner.observations = 0
	if err := backend.Remove(context.Background(), claim, receipts); err != nil {
		t.Fatalf("remove direct rules: %v", err)
	}
	if runner.observations != len(program.rules)+1 || !reflect.DeepEqual(runner.foreign, before) {
		t.Fatalf("cleanup observations/foreign rules = %d/%q, want %d/%q",
			runner.observations, runner.foreign, len(program.rules)+1, before)
	}
}

func TestIPTablesUncertainMutationHasNoReceiptButRemainsStaleRecoverable(t *testing.T) {
	claim := iptablesFixtureClaim(t, "2f292233445566778899aabbccddeeff")
	runner := newUncertainSecondInsertRulePlane(iptablesFrontendNFT)
	backend := testIPTablesFirewall(runner)
	receipts, err := backend.Install(context.Background(), claim)
	if err == nil {
		t.Fatal("commit-before-exit iptables mutation was accepted")
	}
	if len(receipts.owners) != 1 {
		t.Fatalf("uncertain mutation receipts = %v, want only the prior successful rule", receipts.owners)
	}
	program, err := buildIPTablesProgram(claim)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := backend.observe(context.Background(), claim, program, true)
	if err != nil || observation.count() != 2 {
		t.Fatalf("post-failure observation = %#v, %v; want two exact rules", observation, err)
	}
	if err := backend.Remove(context.Background(), claim, receipts); err != nil {
		t.Fatalf("live receipt cleanup: %v", err)
	}
	observation, err = backend.observe(context.Background(), claim, program, true)
	if err != nil || observation.count() != 1 {
		t.Fatalf("live cleanup touched uncertain rule: %#v, %v", observation, err)
	}
	if err := backend.Recover(context.Background(), claim); err != nil {
		t.Fatalf("stale marker cleanup: %v", err)
	}
	if err := backend.VerifyAbsent(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	remaining := append(cloneArgvLines(runner.filter), runner.nat...)
	if !reflect.DeepEqual(remaining, runner.foreign) {
		t.Fatalf("foreign iptables rules changed: %q, want %q", remaining, runner.foreign)
	}
}

func TestIPTablesSuccessfulMutationKeepsReceiptWhenObservationFails(t *testing.T) {
	claim := iptablesFixtureClaim(t, "2f302233445566778899aabbccddeeff")
	runner := newSecondFilterObservationFailureRulePlane(iptablesFrontendNFT)
	receipts, err := testIPTablesFirewall(runner).Install(context.Background(), claim)
	if err == nil {
		t.Fatal("post-mutation observation failure was accepted")
	}
	program, buildErr := buildIPTablesProgram(claim)
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	firstMutation, buildErr := buildIPTablesInstallMutations(claim)
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	if len(receipts.owners) != 1 || !receipts.has(firstMutation[0].owner) {
		t.Fatalf("successful mutation receipts = %v, want %q", receipts.owners, firstMutation[0].owner)
	}
	if len(program.rules) == 0 || runner.mutations != 1 {
		t.Fatalf("fixture mutations = %d, program rules = %d", runner.mutations, len(program.rules))
	}
}

func TestIPTablesRemoveRefusesConflictingOwnerShapeBeforeMutation(t *testing.T) {
	claim := iptablesFixtureClaim(t, "2f312233445566778899aabbccddeeff")
	program, err := buildIPTablesProgram(claim)
	if err != nil {
		t.Fatal(err)
	}
	runner := newMutableIPTablesRulePlane(iptablesFrontendNFT)
	conflicting := append([]string(nil), iptablesExpectedTableLines(program, "filter")[0]...)
	conflicting[2] = "203.0.113.99/32"
	runner.filter = append(runner.filter, conflicting)
	if err := testIPTablesFirewall(runner).Recover(context.Background(), claim); err == nil {
		t.Fatal("mismatched iptables ownership was removed")
	}
	if runner.mutations != 0 {
		t.Fatalf("mismatched cleanup issued %d mutations", runner.mutations)
	}
}
