package cli

import (
	"slices"
	"testing"
)

func TestListCommandParsesAsList(t *testing.T) {
	for _, argv := range [][]string{
		{"list"},
		{"list", "--network", "192.0.2.2", "--probe", "--measure"},
	} {
		parsed, err := Parse(argv)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", argv, err)
		}
		if _, ok := parsed.(List); !ok {
			t.Fatalf("Parse(%q) = %T, want List", argv, parsed)
		}
	}
}

func TestRunCommandParsesAsRun(t *testing.T) {
	parsed, err := Parse([]string{"run", "--source", "/src", "--network", "192.0.2.2", "copy", "{}"})
	if err != nil {
		t.Fatalf("Parse(run) error = %v", err)
	}
	if _, ok := parsed.(Run); !ok {
		t.Fatalf("Parse(run) = %T, want Run", parsed)
	}
}

func TestRunChildArgumentsRemainOpaqueAcrossCommandBoundaries(t *testing.T) {
	for _, argv := range [][]string{
		{"run", "--source", "/src", "--network", "192.0.2.2", "--", "copy", "{}", "--network"},
		{"run", "--source", "/src", "--network", "192.0.2.2", "copy", "{}", "--network"},
	} {
		parsed, err := Parse(argv)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", argv, err)
		}
		want := []string{"copy", "{}", "--network"}
		if got := parsed.(Run).Request().ChildArgv(); !slices.Equal(got, want) {
			t.Fatalf("Parse(%q) child argv = %q, want %q", argv, got, want)
		}
	}
}

func TestHelpFlagsSelectTheirCommandPage(t *testing.T) {
	for _, test := range []struct {
		name string
		argv []string
		page HelpPage
	}{
		{name: "root short help", argv: []string{"-h"}, page: RootHelpPage},
		{name: "root long help", argv: []string{"--help"}, page: RootHelpPage},
		{name: "list short help", argv: []string{"list", "-h"}, page: ListHelpPage},
		{name: "list long help after incomplete semantics", argv: []string{"list", "--probe", "--help"}, page: ListHelpPage},
		{name: "run short help", argv: []string{"run", "-h"}, page: RunHelpPage},
		{name: "run long help after incomplete semantics", argv: []string{"run", "--source", "/src", "--help"}, page: RunHelpPage},
	} {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := Parse(test.argv)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", test.argv, err)
			}
			help, ok := parsed.(Help)
			if !ok || help.Page() != test.page {
				t.Fatalf("Parse(%q) = %#v, want help page %v", test.argv, parsed, test.page)
			}
		})
	}
}

func TestVersionIsOnlyAvailableAtRootCommand(t *testing.T) {
	parsed, err := Parse([]string{"--version"})
	if err != nil {
		t.Fatalf("Parse(--version) error = %v", err)
	}
	if _, ok := parsed.(Version); !ok {
		t.Fatalf("Parse(--version) = %T, want Version", parsed)
	}
	if _, err := Parse([]string{"list", "--version"}); err == nil {
		t.Fatal("list --version unexpectedly succeeded")
	}
}

func TestRunChildArgumentsCanContainInformationFlags(t *testing.T) {
	for _, argv := range [][]string{
		{"run", "--source", "/src", "--network", "192.0.2.2", "--", "copy", "{}", "--help", "--version", "-h"},
		{"run", "--source", "/src", "--network", "192.0.2.2", "copy", "{}", "--help", "--version", "-h"},
	} {
		parsed, err := Parse(argv)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", argv, err)
		}
		want := []string{"copy", "{}", "--help", "--version", "-h"}
		if got := parsed.(Run).Request().ChildArgv(); !slices.Equal(got, want) {
			t.Fatalf("Parse(%q) child argv = %q, want %q", argv, got, want)
		}
	}
}

func TestOptionValueIsNotParsedAsInformationFlag(t *testing.T) {
	if _, err := Parse([]string{"list", "--probe-url", "--help"}); err == nil {
		t.Fatal("an option value equal to --help was treated as a help flag")
	}
}
