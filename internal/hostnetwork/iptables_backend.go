package hostnetwork

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

const iptablesOutputLimit = 1 << 20

type directArgvRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type iptablesFirewall struct {
	activeRulesCommand string
	activeSaveCommand  string
	runner             directArgvRunner
	legacyTables       legacyIPTablesTables
}

type systemArgvRunner struct{}

type boundedArgvOutput struct {
	mu       sync.Mutex
	contents bytes.Buffer
	limit    int
	cancel   context.CancelFunc
	exceeded bool
}

func (iptablesFirewall) Kind() firewallKind { return firewallIPTables }

func (firewall iptablesFirewall) Inspect(ctx context.Context) (firewallInventory, error) {
	if err := firewall.validDiscovery(); err != nil {
		return firewallInventory{}, err
	}
	frontend, err := firewall.inspectFrontend(ctx)
	if err != nil {
		return firewallInventory{}, err
	}
	plane, err := newIPTablesRulePlane(frontend, firewall.runner)
	if err != nil {
		return firewallInventory{}, err
	}
	if err := plane.verifyIdentity(ctx); err != nil {
		return firewallInventory{}, err
	}
	filter, nat, err := plane.readInventory(ctx, firewall.legacyTables)
	if err != nil {
		return firewallInventory{}, err
	}
	if err := plane.verifyCapabilities(ctx); err != nil {
		return firewallInventory{}, err
	}
	inventory, err := decodeIPTablesInventory(frontend, filter, nat)
	if err != nil {
		return firewallInventory{}, err
	}
	return inventory, nil
}

func decodeIPTablesInventory(frontend iptablesFrontend, filter, nat []byte) (firewallInventory, error) {
	inventory := firewallInventory{kind: firewallIPTables,
		transferlanesOwnerReferences: make(map[string]struct{}),
		iptablesFrontend:             frontend}
	if err := addIPTablesInventory(filter, inventory.transferlanesOwnerReferences); err != nil {
		return firewallInventory{}, fmt.Errorf("parse iptables filter inventory: %w", err)
	}
	if err := addIPTablesInventory(nat, inventory.transferlanesOwnerReferences); err != nil {
		return firewallInventory{}, fmt.Errorf("parse iptables nat inventory: %w", err)
	}
	active, err := iptablesForwardActive(filter)
	if err != nil {
		return firewallInventory{}, fmt.Errorf("inspect iptables FORWARD chain: %w", err)
	}
	inventory.iptablesForwardActive = active
	active, err = iptablesPostroutingActive(nat)
	if err != nil {
		return firewallInventory{}, fmt.Errorf("inspect iptables POSTROUTING chain: %w", err)
	}
	inventory.iptablesPostroutingActive = active
	return inventory, nil
}

func iptablesForwardActive(output []byte) (bool, error) {
	policy, found := "", false
	for _, raw := range strings.Split(string(output), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, ":FORWARD ") {
			fields := strings.Fields(line)
			if len(fields) < 2 || found {
				return false, fmt.Errorf("malformed or repeated FORWARD declaration")
			}
			policy, found = fields[1], true
		}
	}
	if !found {
		return false, fmt.Errorf("filter table has no FORWARD declaration")
	}
	lines, err := parseIPTablesLines(output)
	if err != nil {
		return false, err
	}
	for _, line := range lines {
		if len(line) >= 2 && line[0] == "-A" && line[1] == "FORWARD" {
			return true, nil
		}
	}
	return policy != "ACCEPT", nil
}

func iptablesPostroutingActive(output []byte) (bool, error) {
	found := false
	for _, raw := range strings.Split(string(output), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, ":POSTROUTING ") {
			if found || len(strings.Fields(line)) < 2 {
				return false, fmt.Errorf("malformed or repeated POSTROUTING declaration")
			}
			found = true
		}
	}
	if !found {
		return false, fmt.Errorf("nat table has no POSTROUTING declaration")
	}
	lines, err := parseIPTablesLines(output)
	if err != nil {
		return false, err
	}
	for _, line := range lines {
		if len(line) >= 2 && line[0] == "-A" && line[1] == "POSTROUTING" {
			return true, nil
		}
	}
	return false, nil
}

func (firewall iptablesFirewall) Preflight(ctx context.Context, claim networkClaim) error {
	if _, err := buildIPTablesProgram(claim); err != nil {
		return err
	}
	selected, err := newIPTablesRulePlane(claim.iptablesFrontend, firewall.runner)
	if err != nil {
		return err
	}
	return selected.preflightMutation(ctx)
}

func (firewall iptablesFirewall) Install(ctx context.Context, claim networkClaim) (firewallReceipts, error) {
	receipts := newFirewallReceipts(firewallIPTables)
	program, err := buildIPTablesProgram(claim)
	if err != nil {
		return receipts, err
	}
	mutations, err := buildIPTablesInstallMutations(claim)
	if err != nil {
		return receipts, err
	}
	observation, err := firewall.observe(ctx, claim, program, true)
	if err != nil {
		return receipts, err
	}
	if observation.count() != 0 {
		return receipts, fmt.Errorf("iptables claim owner already has %d rules before install", observation.count())
	}
	plane, err := newIPTablesRulePlane(claim.iptablesFrontend, firewall.runner)
	if err != nil {
		return receipts, err
	}
	for index, mutation := range mutations {
		if _, err := plane.runner.Run(ctx, plane.rulesCommand, mutation.argv...); err != nil {
			return receipts, fmt.Errorf("apply iptables command %q: %w", mutation.argv, err)
		}
		receipts.add(mutation.owner)
		observation, err = firewall.observe(ctx, claim, program, true)
		if err != nil {
			return receipts, fmt.Errorf("verify iptables command %q: %w", mutation.argv, err)
		}
		if observation.count() != index+1 {
			return receipts, fmt.Errorf("iptables command %q did not create exactly one owned rule", mutation.argv)
		}
	}
	return receipts, nil
}

func (firewall iptablesFirewall) Verify(ctx context.Context, claim networkClaim, allowAbsent bool) error {
	program, err := buildIPTablesProgram(claim)
	if err != nil {
		return err
	}
	_, err = firewall.observe(ctx, claim, program, allowAbsent)
	return err
}

func (firewall iptablesFirewall) ObservePublished(ctx context.Context,
	claims []networkClaim,
) ([]bool, error) {
	if len(claims) == 0 {
		return nil, nil
	}
	frontend := claims[0].iptablesFrontend
	programs := make([]iptablesProgram, 0, len(claims))
	for _, claim := range claims {
		if claim.iptablesFrontend != frontend {
			return nil, fmt.Errorf("published iptables claims name different frontends %q and %q",
				frontend, claim.iptablesFrontend)
		}
		program, err := buildIPTablesProgram(claim)
		if err != nil {
			return nil, err
		}
		programs = append(programs, program)
	}
	plane, err := newIPTablesRulePlane(frontend, firewall.runner)
	if err != nil {
		return nil, err
	}
	if err := plane.verifyIdentity(ctx); err != nil {
		return nil, err
	}
	filter, nat, err := plane.readInventory(ctx, firewall.legacyTables)
	if err != nil {
		return nil, err
	}
	return observePublishedIPTablesPrograms(filter, nat, programs)
}

func (firewall iptablesFirewall) Remove(ctx context.Context, claim networkClaim,
	receipts firewallReceipts) error {
	if receipts.kind != firewallIPTables {
		return errors.New("live iptables receipts name another backend")
	}
	return firewall.remove(ctx, claim, &receipts)
}

func (firewall iptablesFirewall) Recover(ctx context.Context, claim networkClaim) error {
	return firewall.remove(ctx, claim, nil)
}

func (firewall iptablesFirewall) remove(ctx context.Context, claim networkClaim,
	receipts *firewallReceipts) error {
	program, err := buildIPTablesProgram(claim)
	if err != nil {
		return err
	}
	observation, err := firewall.observe(ctx, claim, program, true)
	if err != nil {
		return err
	}
	commands := buildIPTablesRemoveCommandsFor(program, observation, receipts)
	plane, err := newIPTablesRulePlane(claim.iptablesFrontend, firewall.runner)
	if err != nil {
		return err
	}
	remaining := observation.count()
	for _, command := range commands {
		if _, err := plane.runner.Run(ctx, plane.rulesCommand, command...); err != nil {
			return fmt.Errorf("remove exact iptables object with %q: %w", command, err)
		}
		remaining--
		observation, err = firewall.observe(ctx, claim, program, true)
		if err != nil {
			return fmt.Errorf("verify removed iptables object %q: %w", command, err)
		}
		if observation.count() != remaining {
			return fmt.Errorf("iptables delete %q did not remove exactly one owned rule", command)
		}
	}
	return nil
}

func (firewall iptablesFirewall) VerifyAbsent(ctx context.Context, claim networkClaim) error {
	program, err := buildIPTablesProgram(claim)
	if err != nil {
		return err
	}
	observation, err := firewall.observe(ctx, claim, program, true)
	if err != nil {
		return err
	}
	return requireIPTablesOwnershipAbsent(claim.iptablesFrontend, observation)
}

func (firewall iptablesFirewall) observe(ctx context.Context, claim networkClaim, program iptablesProgram,
	allowPartial bool) (iptablesObservation, error) {
	plane, err := newIPTablesRulePlane(claim.iptablesFrontend, firewall.runner)
	if err != nil {
		return iptablesObservation{}, err
	}
	return plane.observe(ctx, program, allowPartial, firewall.legacyTables)
}

func requireIPTablesOwnershipAbsent(frontend iptablesFrontend, observation iptablesObservation) error {
	if observation.count() != 0 {
		return fmt.Errorf("claimed iptables ownership remains in %s rule plane", frontend)
	}
	return nil
}

func (firewall iptablesFirewall) validDiscovery() error {
	if firewall.activeRulesCommand == "" || firewall.activeSaveCommand == "" || firewall.runner == nil ||
		firewall.legacyTables == nil {
		return errors.New("iptables backend dependencies are incomplete")
	}
	return nil
}

func addIPTablesInventory(output []byte, owners map[string]struct{}) error {
	for _, raw := range strings.Split(string(output), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "*") || line == "COMMIT" {
			continue
		}
		if strings.HasPrefix(line, ":") {
			fields := strings.Fields(line[1:])
			if len(fields) < 2 || fields[0] == "" {
				return fmt.Errorf("malformed iptables chain declaration %q", line)
			}
			continue
		}
		argv, err := splitIPTablesLine(line)
		if err != nil {
			return err
		}
		if len(argv) < 2 || argv[0] != "-A" {
			return fmt.Errorf("unexpected iptables-save statement %q", line)
		}
		comment := iptablesOption(argv[2:], "--comment")
		if id, valid := runIDFromIPTablesComment(comment); valid {
			owners[id] = struct{}{}
		}
	}
	return nil
}

func runIDFromIPTablesComment(comment string) (string, bool) {
	const prefix = "transferlanes:"
	if !strings.HasPrefix(comment, prefix) {
		return "", false
	}
	remainder := strings.TrimPrefix(comment, prefix)
	if separator := strings.IndexByte(remainder, ':'); separator >= 0 {
		remainder = remainder[:separator]
	}
	if len(remainder) != 32 {
		return "", false
	}
	for _, character := range remainder {
		if !strings.ContainsRune("0123456789abcdef", character) {
			return "", false
		}
	}
	return remainder, true
}

func (systemArgvRunner) Run(ctx context.Context, name string, argv ...string) ([]byte, error) {
	commandContext, cancel := context.WithCancel(ctx)
	defer cancel()
	output := &boundedArgvOutput{limit: iptablesOutputLimit, cancel: cancel}
	command := exec.CommandContext(commandContext, name, argv...)
	command.Stdout, command.Stderr = output, output
	err := command.Run()
	if output.wasExceeded() {
		return nil, fmt.Errorf("direct argv command %s output exceeded %d bytes", name, iptablesOutputLimit)
	}
	if err != nil {
		return nil, fmt.Errorf("direct argv command %s %q failed: %w: %s",
			name, argv, err, strings.TrimSpace(output.text()))
	}
	return output.bytes(), nil
}

func (output *boundedArgvOutput) Write(contents []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	written := len(contents)
	if output.exceeded {
		return written, nil
	}
	remaining := output.limit - output.contents.Len()
	if remaining > 0 {
		_, _ = output.contents.Write(contents[:min(remaining, len(contents))])
	}
	if len(contents) > remaining {
		output.exceeded = true
		output.cancel()
	}
	return written, nil
}

func (output *boundedArgvOutput) wasExceeded() bool {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.exceeded
}

func (output *boundedArgvOutput) bytes() []byte {
	output.mu.Lock()
	defer output.mu.Unlock()
	return append([]byte(nil), output.contents.Bytes()...)
}

func (output *boundedArgvOutput) text() string {
	output.mu.Lock()
	defer output.mu.Unlock()
	return output.contents.String()
}
