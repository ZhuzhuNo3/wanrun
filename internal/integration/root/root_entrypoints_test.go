//go:build linux && rootintegration

package root_test

import (
	"slices"
	"testing"
)

func TestRootSupervisorScenarioParsersOwnOnlyTheirDomain(t *testing.T) {
	if scenario, ok := parseTransferRootScenario("transfer-manual"); !ok || scenario != transferManual {
		t.Fatalf("transfer parser = %q, %v", scenario, ok)
	}
	if scenario, ok := parsePlainSupervisorScenario("plain"); !ok || scenario != plainSupervisorPipes {
		t.Fatalf("plain parser = %q, %v", scenario, ok)
	}
	if scenario, ok := parseMeasurementSupervisorScenario("measure"); !ok || scenario != measurementSupervisorMeasure {
		t.Fatalf("measurement parser = %q, %v", scenario, ok)
	}
	if scenario, ok := parseDiagnosticSupervisorScenario("exec-fail"); !ok || scenario != diagnosticSupervisorExec {
		t.Fatalf("diagnostic parser = %q, %v", scenario, ok)
	}
	if scenario, ok := parseContainmentSupervisorScenario("signals"); !ok || scenario != containmentSupervisorSignals {
		t.Fatalf("containment parser = %q, %v", scenario, ok)
	}

	for name, parserOwns := range map[string]func(string) bool{
		"transfer": func(value string) bool { _, ok := parseTransferRootScenario(value); return ok },
		"plain":    func(value string) bool { _, ok := parsePlainSupervisorScenario(value); return ok },
		"measurement": func(value string) bool {
			_, ok := parseMeasurementSupervisorScenario(value)
			return ok
		},
		"diagnostic": func(value string) bool { _, ok := parseDiagnosticSupervisorScenario(value); return ok },
		"containment": func(value string) bool {
			_, ok := parseContainmentSupervisorScenario(value)
			return ok
		},
	} {
		t.Run(name+" rejects unknown", func(t *testing.T) {
			if parserOwns("unknown-root-scenario") {
				t.Fatal("unknown scenario was accepted")
			}
		})
	}
}

func TestRootChildEntrypointsRejectUnknownArgvWithinTheirDomain(t *testing.T) {
	for name, run := range map[string]func([]string) (bool, int){
		"transfer":    maybeRunTransferChild,
		"plain":       maybeRunPlainChild,
		"measurement": maybeRunMeasurementChild,
		"diagnostic":  maybeRunDiagnosticChild,
		"containment": maybeRunContainmentChild,
	} {
		t.Run(name, func(t *testing.T) {
			handled, code := run([]string{map[string]string{
				"transfer": transferChildArgument, "plain": plainChildArgument,
				"measurement": measurementChildArgument, "diagnostic": diagnosticChildArgument,
				"containment": containmentChildArgument,
			}[name], "unknown-action"})
			if !handled || code != 2 {
				t.Fatalf("unknown argv result = handled %v, code %d", handled, code)
			}
			if handled, code := run([]string{"--other-domain", "unknown-action"}); handled || code != 0 {
				t.Fatalf("other-domain argv result = handled %v, code %d", handled, code)
			}
		})
	}
}

func TestRootDomainCommandBuildersPreserveExactArgv(t *testing.T) {
	for name, got := range map[string][]string{
		"transfer":     transferChildCommand("/test", transferChildView, "one", "two"),
		"plain":        plainChildCommandLine("/test", plainChildPTY, "one", "two"),
		"plain resize": plainChildCommandLine("/test", plainChildPTYResize, "one", "two"),
		"measurement":  measurementChildCommand("/test", "one", "two"),
		"diagnostic":   diagnosticChildCommand("/test", diagnosticChildExit, "one", "two"),
		"containment":  containmentChildCommand("/test", containmentChildSignals, "one", "two"),
	} {
		t.Run(name, func(t *testing.T) {
			if len(got) != 5 || got[0] != "/test" || !slices.Equal(got[len(got)-2:], []string{"one", "two"}) {
				t.Fatalf("command argv = %q", got)
			}
		})
	}
}
