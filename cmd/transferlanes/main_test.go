package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/cli"
)

func TestCompositionRootFallsThroughToPublicCLI(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--help"}, strings.NewReader(""), &stdout, &stderr); code != 0 ||
		stdout.String() != cli.RootHelpPage.Text() || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

func TestCompositionRootRejectsForgedAndLegacyHiddenArgumentsAsPublicUsage(t *testing.T) {
	for _, argv := range [][]string{
		{"--transferlanes-internal-run-supervisor", "forged"},
		{"--transferlanes-internal-child-process", "forged"},
		{"--transferlanes-internal-throughput-helper", "forged"},
		{"--list-networks"}, {"--source", "/source"}, {"probe"}, {"measure"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(argv, strings.NewReader(""), &stdout, &stderr); code != 2 ||
			stdout.Len() != 0 || !strings.Contains(stderr.String(), "transferlanes:") {
			t.Fatalf("argv=%q exit=%d stdout=%q stderr=%q", argv, code,
				stdout.String(), stderr.String())
		}
	}
}
