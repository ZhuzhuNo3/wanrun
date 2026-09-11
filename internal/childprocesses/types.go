package childprocesses

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/ZhuzhuNo3/transferlanes/internal/namespaceresolvers"
	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const maximumTerminalSize = 65535

const descriptorBoundViewAuthority = "/run/transferlanes"

// Command is one immutable exact-argv transfer prepared for the complete group owner.
type Command struct {
	transfer    transfernumber.Number
	cwd         string
	resolver    namespaceresolvers.ResolverAccess
	argv        []string
	environment []string
	execChannel *os.File
	view        ViewAccess
}

// ViewAccess is the descriptor capability required to bind one transfer's read-only view.
type ViewAccess interface {
	RunID() runid.ID
	TransferNumber() transfernumber.Number
	BaseName() string
	OpenChildDescriptor() (*os.File, error)
}

// NewCommandWithExecChannel prepares an exact command with one caller-owned descriptor that is
// handed through the namespace helper to the exact executable. ChildProcesses duplicates the
// descriptor and never closes the caller's file.
func NewCommandWithExecChannel(id transfernumber.Number, workingDirectory string, resolver namespaceresolvers.ResolverAccess,
	argv, environment []string, channel *os.File) (Command, error) {
	if channel == nil {
		return Command{}, fmt.Errorf("child exec channel is absent")
	}
	command, err := NewCommand(id, workingDirectory, resolver, argv, environment)
	if err != nil {
		return Command{}, err
	}
	command.execChannel = channel
	return command, nil
}

func NewCommand(id transfernumber.Number, workingDirectory string, resolver namespaceresolvers.ResolverAccess, argv, environment []string) (Command, error) {
	command := Command{transfer: id, cwd: strings.Clone(workingDirectory), resolver: resolver,
		argv: cloneStrings(argv), environment: cloneStrings(environment)}
	if err := validateCommand(command); err != nil {
		return Command{}, err
	}
	return command, nil
}

// NewViewCommand binds cwd and the already-substituted argv path to one non-owning FileViews
// access capability. ChildProcesses owns only the exact descriptor duplicates it requests.
func NewViewCommand(id transfernumber.Number, resolver namespaceresolvers.ResolverAccess, argv, environment []string,
	view ViewAccess,
) (Command, error) {
	if view == nil {
		return Command{}, fmt.Errorf("child file-view access is unavailable")
	}
	if view.TransferNumber() != id {
		return Command{}, fmt.Errorf("child file-view transfer identity is invalid")
	}
	viewPath, err := DescriptorBoundViewPath(view.RunID(), view.TransferNumber(), view.BaseName())
	if err != nil {
		return Command{}, err
	}
	command := Command{transfer: id, cwd: viewPath, resolver: resolver,
		argv: cloneStrings(argv), environment: cloneStrings(environment), view: view}
	if err := validateCommand(command); err != nil {
		return Command{}, err
	}
	viewArguments := 0
	for _, argument := range command.argv {
		if argument == viewPath {
			viewArguments++
		}
	}
	if viewArguments != 1 {
		return Command{}, fmt.Errorf("child command must contain its descriptor-bound view exactly once")
	}
	return command, nil
}

// DescriptorBoundViewPath returns the child-private mount alias for one captured source basename.
// The path is usable only after ChildProcesses installs the inherited descriptor-bound view.
func DescriptorBoundViewPath(run runid.ID, id transfernumber.Number, baseName string) (string, error) {
	if baseName == "" || baseName == "." || baseName == ".." || filepath.Base(baseName) != baseName ||
		strings.IndexByte(baseName, 0) >= 0 || id.Value() == 0 {
		return "", fmt.Errorf("child file-view basename is invalid")
	}
	parsed, err := runid.Parse(run.String())
	if err != nil || parsed != run {
		return "", fmt.Errorf("child file-view run identity is invalid")
	}
	return filepath.Join(descriptorBoundViewAuthority, "view-"+run.String(),
		fmt.Sprintf("transfer-%03d", id.Value()), baseName), nil
}

func (command Command) Transfer() transfernumber.Number { return command.transfer }
func (command Command) WorkingDirectory() string        { return command.cwd }
func (command Command) Argv() []string                  { return cloneStrings(command.argv) }
func (command Command) Environment() []string           { return cloneStrings(command.environment) }

func validateCommand(command Command) error {
	viewPath := ""
	if command.view != nil {
		viewPath, _ = DescriptorBoundViewPath(command.view.RunID(), command.view.TransferNumber(),
			command.view.BaseName())
	}
	if command.resolver == nil {
		return errors.New("child resolver capability is unavailable")
	}
	resolver, resolverErr := command.resolver.Open()
	if resolver != nil {
		_ = resolver.Close()
	}
	if resolverErr != nil {
		return fmt.Errorf("child resolver capability is invalid: %w", resolverErr)
	}
	return validateCommandFields(command, viewPath)
}

func validateCommandFields(command Command, descriptorViewPath string) error {
	validWorkingDirectory := cleanAbsolutePath(command.cwd) ||
		descriptorViewPath != "" && command.cwd == descriptorViewPath
	if command.transfer.Value() == 0 || !validWorkingDirectory || len(command.argv) == 0 {
		return fmt.Errorf("child command identity or paths are invalid")
	}
	if command.execChannel != nil && command.view != nil {
		return fmt.Errorf("child command has conflicting inherited capabilities")
	}
	for _, argument := range command.argv {
		if strings.IndexByte(argument, 0) >= 0 {
			return fmt.Errorf("child command argv contains NUL")
		}
	}
	seen := make(map[string]struct{}, len(command.environment))
	for _, entry := range command.environment {
		name, _, found := strings.Cut(entry, "=")
		if !found || name == "" || strings.IndexByte(entry, 0) >= 0 || strings.Contains(name, "=") {
			return fmt.Errorf("child command environment is invalid")
		}
		if _, duplicate := seen[name]; duplicate {
			return fmt.Errorf("child command environment contains duplicate name %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func validateCommands(commands []Command) error {
	if len(commands) == 0 || len(commands) > transfernumber.Maximum {
		return fmt.Errorf("child process group size is invalid")
	}
	seen := make(map[transfernumber.Number]struct{}, len(commands))
	for _, command := range commands {
		if err := validateCommand(command); err != nil {
			return err
		}
		if _, duplicate := seen[command.transfer]; duplicate {
			return fmt.Errorf("child process group contains duplicate transfer %d", command.transfer.Value())
		}
		seen[command.transfer] = struct{}{}
	}
	for expected := 1; expected <= len(commands); expected++ {
		number, _ := transfernumber.New(expected)
		if _, exists := seen[number]; !exists {
			return fmt.Errorf("child process group is missing transfer %d", expected)
		}
	}
	return nil
}

func cleanAbsolutePath(value string) bool {
	return value != "" && filepath.IsAbs(value) && filepath.Clean(value) == value && strings.IndexByte(value, 0) < 0
}

func cloneStrings(values []string) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = strings.Clone(value)
	}
	return result
}

// TerminalSize is the validated initial PTY size for the complete group.
type TerminalSize struct{ cols, rows uint16 }

func NewTerminalSize(cols, rows int) (TerminalSize, error) {
	if cols < 1 || rows < 1 || cols > maximumTerminalSize || rows > maximumTerminalSize {
		return TerminalSize{}, fmt.Errorf("terminal size is invalid")
	}
	return TerminalSize{cols: uint16(cols), rows: uint16(rows)}, nil
}

func (size TerminalSize) Values() (uint16, uint16) { return size.cols, size.rows }

// OutputStream identifies the child descriptor that produced one byte-exact chunk.
type OutputStream uint8

const (
	StreamPTY OutputStream = iota + 1
	StreamStdout
	StreamStderr
)

func (stream OutputStream) String() string {
	switch stream {
	case StreamPTY:
		return "pty"
	case StreamStdout:
		return "stdout"
	case StreamStderr:
		return "stderr"
	default:
		return "invalid"
	}
}

// Output contains byte-exact output from one transfer and one real child stream.
type Output struct {
	Transfer transfernumber.Number
	Stream   OutputStream
	Bytes    []byte
}

// StatusKind is the fixed running/exited process status vocabulary.
type StatusKind uint8

const (
	StatusRunning StatusKind = iota + 1
	StatusExited
)

// Status reports a process transition without exposing a PID.
type Status struct {
	Transfer transfernumber.Number
	Kind     StatusKind
	ExitCode int
	Signal   int
}

// TransferResult is one direct command's terminal wait result.
type TransferResult struct {
	Transfer transfernumber.Number
	ExitCode int
	Signal   int
}

// Result is the complete group result; a transfer failure does not imply whole-group cancellation.
type Result struct {
	Transfers []TransferResult
	Cancelled bool
}

// StartFailureResult separates new supervision evidence obtained while finishing a failed start
// from the independent proof that no descendant remains.
type StartFailureResult struct {
	SupervisionError error
	ContainmentError error
}

// HelperTransfer is the complete bounded output and outcome from one jointly run helper.
type HelperTransfer struct {
	Transfer transfernumber.Number
	Output   []byte
	ExitCode int
	Signal   int
}

// HelperResult is returned only after the complete helper group and its descendants are reaped.
type HelperResult struct {
	Transfers []HelperTransfer
}

func newResult(transfers []TransferResult, cancelled bool) (Result, error) {
	ordered := append([]TransferResult(nil), transfers...)
	seen := make(map[transfernumber.Number]struct{}, len(ordered))
	for _, result := range ordered {
		if result.Transfer.Value() == 0 || result.Signal < 0 || result.Signal > 255 ||
			result.Signal == 0 && (result.ExitCode < 0 || result.ExitCode > 255) ||
			result.Signal != 0 && result.ExitCode != -1 {
			return Result{}, fmt.Errorf("child transfer result is invalid")
		}
		if _, duplicate := seen[result.Transfer]; duplicate {
			return Result{}, fmt.Errorf("child result contains duplicate transfer")
		}
		seen[result.Transfer] = struct{}{}
	}
	sort.Slice(ordered, func(left, right int) bool {
		return ordered[left].Transfer.Value() < ordered[right].Transfer.Value()
	})
	for index, result := range ordered {
		if result.Transfer.Value() != uint8(index+1) {
			return Result{}, fmt.Errorf("child result set is missing transfer %d", index+1)
		}
	}
	return Result{Transfers: ordered, Cancelled: cancelled}, nil
}

func cloneResult(result Result) Result {
	result.Transfers = slices.Clone(result.Transfers)
	return result
}

func newHelperResult(processes Result, outputs map[transfernumber.Number][]byte) (HelperResult, error) {
	if len(processes.Transfers) != len(outputs) {
		return HelperResult{}, fmt.Errorf("helper result set is incomplete")
	}
	result := HelperResult{Transfers: make([]HelperTransfer, len(processes.Transfers))}
	for index, process := range processes.Transfers {
		output, found := outputs[process.Transfer]
		if !found {
			return HelperResult{}, fmt.Errorf("transfer %d helper output is absent", process.Transfer.Value())
		}
		result.Transfers[index] = HelperTransfer{Transfer: process.Transfer, Output: append([]byte(nil), output...),
			ExitCode: process.ExitCode, Signal: process.Signal}
	}
	return result, nil
}
