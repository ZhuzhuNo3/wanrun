package hostnetwork

import (
	"context"
	"errors"
	"fmt"
)

const (
	iptablesNFTRulesCommand    = "iptables-nft"
	iptablesNFTSaveCommand     = "iptables-nft-save"
	iptablesLegacyRulesCommand = "iptables-legacy"
	iptablesLegacySaveCommand  = "iptables-legacy-save"
)

type iptablesRulePlane struct {
	frontend     iptablesFrontend
	rulesCommand string
	saveCommand  string
	runner       directArgvRunner
}

func newIPTablesRulePlane(frontend iptablesFrontend, runner directArgvRunner) (iptablesRulePlane, error) {
	if runner == nil {
		return iptablesRulePlane{}, fmt.Errorf("iptables rule plane requires a command runner")
	}
	switch frontend {
	case iptablesFrontendNFT:
		return iptablesRulePlane{frontend: frontend, rulesCommand: iptablesNFTRulesCommand,
			saveCommand: iptablesNFTSaveCommand, runner: runner}, nil
	case iptablesFrontendLegacy:
		return iptablesRulePlane{frontend: frontend, rulesCommand: iptablesLegacyRulesCommand,
			saveCommand: iptablesLegacySaveCommand, runner: runner}, nil
	default:
		return iptablesRulePlane{}, fmt.Errorf("unsupported iptables frontend %q", frontend)
	}
}

func (plane iptablesRulePlane) verifyIdentity(ctx context.Context) error {
	rulesOutput, rulesErr := plane.runner.Run(ctx, plane.rulesCommand, "--version")
	saveOutput, saveErr := plane.runner.Run(ctx, plane.saveCommand, "--version")
	if rulesErr != nil || saveErr != nil {
		return joinCommandIdentityErrors(plane.rulesCommand, rulesErr, plane.saveCommand, saveErr)
	}
	rulesFrontend, rulesParseErr := parseIPTablesRulePlaneFrontend(plane.rulesCommand, rulesOutput)
	saveFrontend, saveParseErr := parseIPTablesRulePlaneFrontend(plane.saveCommand, saveOutput)
	if rulesParseErr != nil || saveParseErr != nil {
		return fmt.Errorf("verify iptables rule plane identity: %w", errors.Join(rulesParseErr, saveParseErr))
	}
	if rulesFrontend != saveFrontend {
		return fmt.Errorf("verify iptables rule plane identity: %s uses %s but %s uses %s",
			plane.rulesCommand, rulesFrontend, plane.saveCommand, saveFrontend)
	}
	actual := rulesFrontend
	if actual != plane.frontend {
		return fmt.Errorf("iptables rule plane command identifies %s, want %s", actual, plane.frontend)
	}
	return nil
}

func joinCommandIdentityErrors(first string, firstErr error, second string, secondErr error) error {
	return errors.Join(commandIdentityError(first, firstErr), commandIdentityError(second, secondErr))
}

func (plane iptablesRulePlane) readInventory(ctx context.Context,
	legacyTables legacyIPTablesTables) ([]byte, []byte, error) {
	if plane.frontend == iptablesFrontendLegacy {
		return plane.readLegacyInventory(ctx, legacyTables)
	}
	return plane.readTables(ctx, true, true)
}

func (plane iptablesRulePlane) readLegacyInventory(ctx context.Context,
	legacyTables legacyIPTablesTables) ([]byte, []byte, error) {
	if legacyTables == nil {
		return nil, nil, fmt.Errorf("legacy iptables table reader is unavailable")
	}
	names, err := legacyTables.Names()
	if err != nil {
		return nil, nil, err
	}
	_, filterExists := names["filter"]
	_, natExists := names["nat"]
	filter, nat, err := plane.readTables(ctx, filterExists, natExists)
	if err == nil {
		if !filterExists {
			filter = emptyLegacyFilterInventory()
		}
		if !natExists {
			nat = emptyLegacyNATInventory()
		}
	}
	return filter, nat, err
}

func (plane iptablesRulePlane) readTables(ctx context.Context,
	filterExists, natExists bool) ([]byte, []byte, error) {
	var filter, nat []byte
	var err error
	if filterExists {
		filter, err = plane.readTable(ctx, "filter")
		if err != nil {
			return nil, nil, err
		}
	}
	if natExists {
		nat, err = plane.readTable(ctx, "nat")
		if err != nil {
			return nil, nil, err
		}
	}
	return filter, nat, nil
}

func (plane iptablesRulePlane) readTable(ctx context.Context, table string) ([]byte, error) {
	output, err := plane.runner.Run(ctx, plane.saveCommand, "-t", table)
	if err != nil {
		return nil, fmt.Errorf("inspect iptables %s table: %w", table, err)
	}
	return output, nil
}

func (plane iptablesRulePlane) verifyCapabilities(ctx context.Context) error {
	for _, probe := range [][]string{
		{"-w", "5", "-m", "comment", "--help"},
		{"-w", "5", "-m", "conntrack", "--help"},
		{"-w", "5", "-t", "nat", "-j", "SNAT", "--help"},
	} {
		if _, err := plane.runner.Run(ctx, plane.rulesCommand, probe...); err != nil {
			return fmt.Errorf("probe iptables capability %q: %w", probe, err)
		}
	}
	return nil
}

func (plane iptablesRulePlane) preflightMutation(ctx context.Context) error {
	if err := plane.verifyIdentity(ctx); err != nil {
		return err
	}
	return plane.verifyCapabilities(ctx)
}

func (plane iptablesRulePlane) observe(ctx context.Context, program iptablesProgram,
	allowPartial bool, legacyTables legacyIPTablesTables) (iptablesObservation, error) {
	if err := plane.verifyIdentity(ctx); err != nil {
		return iptablesObservation{}, err
	}
	filter, nat, err := plane.readInventory(ctx, legacyTables)
	if err != nil {
		return iptablesObservation{}, err
	}
	return observeIPTablesProgram(filter, nat, program, allowPartial)
}
