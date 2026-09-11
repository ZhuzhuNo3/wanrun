package childprocesses

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/namespaceresolvers"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestCommandOwnsExactArgvAndEnvironment(t *testing.T) {
	id, _ := transfernumber.New(2)
	argv := []string{"/bin/transfer", "argument with spaces", "$(not-a-shell)", string([]byte{0xff, 'x'})}
	environment := []string{"PATH=/bin:/usr/bin", "TERM=xterm-256color", "VALUE=a b"}
	command, err := NewCommand(id, "/work/view", testResolverAccess(t), argv, environment)
	if err != nil {
		t.Fatal(err)
	}
	argv[0], environment[0] = "changed", "PATH=/changed"
	if !slices.Equal(command.Argv(), []string{"/bin/transfer", "argument with spaces", "$(not-a-shell)", string([]byte{0xff, 'x'})}) ||
		!slices.Equal(command.Environment(), []string{"PATH=/bin:/usr/bin", "TERM=xterm-256color", "VALUE=a b"}) {
		t.Fatalf("command did not preserve exact values: argv=%q env=%q", command.Argv(), command.Environment())
	}
}

type testResolverCapability struct{ path string }

func (resolver testResolverCapability) Open() (*os.File, error) { return os.Open(resolver.path) }

func testResolverAccess(t *testing.T) testResolverCapability {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("nameserver 192.0.2.53\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return testResolverCapability{path: path}
}

func TestCommandRejectsUnsafeProcessInputs(t *testing.T) {
	id, _ := transfernumber.New(1)
	cases := map[string]struct {
		cwd       string
		argv, env []string
	}{
		"relative cwd": {"relative", []string{"/bin/true"}, []string{"TERM=x"}},
		"empty argv":   {"/work", nil, []string{"TERM=x"}},
		"nul argv":     {"/work", []string{"/bin/true", "a\x00b"}, []string{"TERM=x"}},
		"bad env":      {"/work", []string{"/bin/true"}, []string{"TERM=x", "TERM=y"}},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			command := Command{transfer: id, cwd: test.cwd, argv: test.argv, environment: test.env}
			if err := validateCommandFields(command, ""); err == nil {
				t.Fatal("unsafe process inputs were accepted")
			}
		})
	}
}

func TestCommandRejectsUnavailableResolverCapability(t *testing.T) {
	id, _ := transfernumber.New(1)
	for name, resolver := range map[string]namespaceresolvers.ResolverAccess{
		"absent": nil,
		"closed": testResolverCapability{path: filepath.Join(t.TempDir(), "missing-resolver")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewCommand(id, "/work", resolver, []string{"/bin/true"}, nil); err == nil {
				t.Fatal("unavailable resolver capability was accepted")
			}
		})
	}
}

func TestGroupResultKeepsSiblingFailuresIndependent(t *testing.T) {
	transfer1, _ := transfernumber.New(1)
	transfer2, _ := transfernumber.New(2)
	result, err := newResult([]TransferResult{{Transfer: transfer2, ExitCode: 0}, {Transfer: transfer1, ExitCode: 7}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Cancelled || len(result.Transfers) != 2 || result.Transfers[0].Transfer != transfer1 ||
		result.Transfers[0].ExitCode != 7 || result.Transfers[1].Transfer != transfer2 || result.Transfers[1].ExitCode != 0 {
		t.Fatalf("group result = %#v", result)
	}
}

func TestGroupResultRejectsIncompleteTransferSet(t *testing.T) {
	first, _ := transfernumber.New(1)
	third, _ := transfernumber.New(3)
	if _, err := newResult([]TransferResult{{Transfer: first}, {Transfer: third}}, false); err == nil {
		t.Fatal("incomplete transfer result set was accepted")
	}
}

func TestHelperResultOwnsCompleteSortedOutput(t *testing.T) {
	transfer1, _ := transfernumber.New(1)
	transfer2, _ := transfernumber.New(2)
	outputs := map[transfernumber.Number][]byte{transfer2: []byte("second\n"), transfer1: []byte("first\n")}
	processes, err := newResult([]TransferResult{{Transfer: transfer2, ExitCode: 0}, {Transfer: transfer1, ExitCode: 0}}, false)
	if err != nil {
		t.Fatal(err)
	}
	result, err := newHelperResult(processes, outputs)
	if err != nil {
		t.Fatal(err)
	}
	outputs[transfer1][0] = 'X'
	if len(result.Transfers) != 2 || result.Transfers[0].Transfer != transfer1 || string(result.Transfers[0].Output) != "first\n" ||
		result.Transfers[1].Transfer != transfer2 || string(result.Transfers[1].Output) != "second\n" {
		t.Fatalf("helper result = %#v", result)
	}
}

func TestHelperResultRejectsMissingTransferOutput(t *testing.T) {
	transfer1, _ := transfernumber.New(1)
	processes, err := newResult([]TransferResult{{Transfer: transfer1, ExitCode: 0}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newHelperResult(processes, map[transfernumber.Number][]byte{}); err == nil {
		t.Fatal("incomplete helper output was accepted")
	}
}
