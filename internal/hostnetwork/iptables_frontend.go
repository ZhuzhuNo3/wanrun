package hostnetwork

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

func (firewall iptablesFirewall) inspectFrontend(ctx context.Context) (iptablesFrontend, error) {
	iptablesOutput, iptablesErr := firewall.runner.Run(ctx, firewall.activeRulesCommand, "--version")
	saveOutput, saveErr := firewall.runner.Run(ctx, firewall.activeSaveCommand, "--version")
	iptablesMissing := errors.Is(iptablesErr, exec.ErrNotFound)
	saveMissing := errors.Is(saveErr, exec.ErrNotFound)
	if iptablesMissing && saveMissing {
		return "", fmt.Errorf("%w: iptables and iptables-save executables are absent", errFirewallUnavailable)
	}
	if iptablesErr != nil || saveErr != nil {
		return "", errors.Join(commandIdentityError(firewall.activeRulesCommand, iptablesErr),
			commandIdentityError(firewall.activeSaveCommand, saveErr))
	}
	frontend, err := matchingIPTablesCommandFrontends(firewall.activeRulesCommand, iptablesOutput,
		firewall.activeSaveCommand, saveOutput)
	if err != nil {
		return "", fmt.Errorf("verify iptables frontend identity: %w", err)
	}
	return frontend, nil
}

func matchingIPTablesCommandFrontends(rulesCommand string, rulesOutput []byte,
	saveCommand string, saveOutput []byte,
) (iptablesFrontend, error) {
	rulesFrontend, err := parseConfiguredIPTablesFrontend(rulesCommand, rulesOutput)
	if err != nil {
		return "", err
	}
	saveFrontend, err := parseConfiguredIPTablesFrontend(saveCommand, saveOutput)
	if err != nil {
		return "", err
	}
	if rulesFrontend != saveFrontend {
		return "", fmt.Errorf("%s uses %s but %s uses %s",
			rulesCommand, rulesFrontend, saveCommand, saveFrontend)
	}
	return rulesFrontend, nil
}

func parseConfiguredIPTablesFrontend(command string, output []byte) (iptablesFrontend, error) {
	switch command {
	case "iptables", "iptables-save":
		return parseIPTablesFrontend(command, output)
	default:
		return parseIPTablesRulePlaneFrontend(command, output)
	}
}

func commandIdentityError(command string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("read %s frontend identity: %w", command, err)
}

func matchingIPTablesFrontend(iptablesOutput, saveOutput []byte) (iptablesFrontend, error) {
	iptablesValue, err := parseIPTablesFrontend("iptables", iptablesOutput)
	if err != nil {
		return "", err
	}
	saveValue, err := parseIPTablesFrontend("iptables-save", saveOutput)
	if err != nil {
		return "", err
	}
	if iptablesValue != saveValue {
		return "", fmt.Errorf("iptables uses %s but iptables-save uses %s", iptablesValue, saveValue)
	}
	return iptablesValue, nil
}

func parseIPTablesFrontend(command string, output []byte) (iptablesFrontend, error) {
	return parseIPTablesFrontendNames(command, []string{command}, output)
}

func parseIPTablesRulePlaneFrontend(command string, output []byte) (iptablesFrontend, error) {
	var names []string
	switch command {
	case iptablesNFTRulesCommand:
		names = []string{"iptables", iptablesNFTRulesCommand}
	case iptablesNFTSaveCommand:
		names = []string{"iptables-save", iptablesNFTSaveCommand}
	case iptablesLegacyRulesCommand:
		names = []string{"iptables", iptablesLegacyRulesCommand}
	case iptablesLegacySaveCommand:
		names = []string{"iptables-save", iptablesLegacySaveCommand}
	default:
		return "", fmt.Errorf("unsupported fixed iptables command %q", command)
	}
	return parseIPTablesFrontendNames(command, names, output)
}

func parseIPTablesFrontendNames(command string, names []string, output []byte) (iptablesFrontend, error) {
	fields := strings.Fields(strings.TrimSpace(string(output)))
	if len(fields) != 3 || !slices.Contains(names, fields[0]) || len(fields[1]) < 2 || fields[1][0] != 'v' {
		return "", fmt.Errorf("%s version output does not identify a supported frontend", command)
	}
	switch fields[2] {
	case "(nf_tables)":
		return iptablesFrontendNFT, nil
	case "(legacy)":
		return iptablesFrontendLegacy, nil
	default:
		return "", fmt.Errorf("%s version output names unsupported frontend %q", command, fields[2])
	}
}
