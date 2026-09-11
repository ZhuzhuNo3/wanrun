//go:build linux && rootintegration

package root_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/runsupervisor"
	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
)

var labSequence atomic.Uint32

func TestMain(m *testing.M) {
	if handled, code := maybeRunTransferEntrypoint(os.Args[1:]); handled {
		os.Exit(code)
	}
	if handled, code := maybeRunPlainPTYChildEntrypoint(os.Args[1:]); handled {
		os.Exit(code)
	}
	if handled, code := maybeRunMeasurementEntrypoint(os.Args[1:]); handled {
		os.Exit(code)
	}
	if handled, code := maybeRunDiagnosticEntrypoint(os.Args[1:]); handled {
		os.Exit(code)
	}
	if handled, code := maybeRunContainmentEntrypoint(os.Args[1:]); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func maybeRunTransferEntrypoint(argv []string) (bool, int) {
	if handled, code := maybeRunTransferChild(argv); handled {
		return true, code
	}
	scenario, ok := parseTransferRootScenario(os.Getenv(supervisorScenarioEnv))
	if !ok {
		return false, 0
	}
	return runsupervisor.MaybeRun(argv, func(ctx context.Context,
		wire *runsupervisor.RunSupervisor) runsupervisor.Final {
		if err := markSupervisorWorkEntered(); err != nil {
			return internalRunSupervisorFinal(err)
		}
		return runTransferRootFixture(ctx, wire, scenario)
	})
}

func maybeRunPlainPTYChildEntrypoint(argv []string) (bool, int) {
	if handled, code := maybeRunPlainChild(argv); handled {
		return true, code
	}
	if handled, code := childprocesses.MaybeRunHelper(argv); handled {
		return true, code
	}
	scenario, ok := parsePlainSupervisorScenario(os.Getenv(supervisorScenarioEnv))
	if !ok {
		return false, 0
	}
	return runsupervisor.MaybeRun(argv, func(ctx context.Context,
		wire *runsupervisor.RunSupervisor) runsupervisor.Final {
		return runPlainSupervisorFixture(ctx, wire, scenario)
	})
}

func maybeRunMeasurementEntrypoint(argv []string) (bool, int) {
	if handled, code := maybeRunMeasurementChild(argv); handled {
		return true, code
	}
	if handled, code := throughput.MaybeRunHelper(argv); handled {
		return true, code
	}
	scenario, ok := parseMeasurementSupervisorScenario(os.Getenv(supervisorScenarioEnv))
	if !ok {
		return false, 0
	}
	return runsupervisor.MaybeRun(argv, func(ctx context.Context,
		wire *runsupervisor.RunSupervisor) runsupervisor.Final {
		return runMeasurementSupervisorFixture(ctx, wire, scenario)
	})
}

func maybeRunDiagnosticEntrypoint(argv []string) (bool, int) {
	if handled, code := maybeRunDiagnosticChild(argv); handled {
		return true, code
	}
	if handled, code := maybeRunNonRootListing(argv); handled {
		return true, code
	}
	scenario, ok := parseDiagnosticSupervisorScenario(os.Getenv(supervisorScenarioEnv))
	if !ok {
		return false, 0
	}
	return runsupervisor.MaybeRun(argv, func(ctx context.Context,
		wire *runsupervisor.RunSupervisor) runsupervisor.Final {
		return runDiagnosticSupervisorFixture(ctx, wire, scenario)
	})
}

func maybeRunContainmentEntrypoint(argv []string) (bool, int) {
	if handled, code := maybeRunContainmentChild(argv); handled {
		return true, code
	}
	scenario, ok := parseContainmentSupervisorScenario(os.Getenv(supervisorScenarioEnv))
	if !ok {
		return false, 0
	}
	return runsupervisor.MaybeRun(argv, func(ctx context.Context,
		wire *runsupervisor.RunSupervisor) runsupervisor.Final {
		return runContainmentSupervisorFixture(ctx, wire, scenario)
	})
}

func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Fatalf("rootintegration requires euid 0, got %d", os.Geteuid())
	}
}

func requireRootTools(t *testing.T) {
	t.Helper()
	for _, name := range []string{"ip", "nft", "iptables", "iptables-save", "ip6tables-save", "sysctl"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("rootintegration requires %s: %v", name, err)
		}
	}
}

func cleanupOutputIsAbsent(output string) bool {
	for _, fragment := range []string{"No such", "Cannot find", "does not exist"} {
		if strings.Contains(output, fragment) {
			return true
		}
	}
	return false
}
