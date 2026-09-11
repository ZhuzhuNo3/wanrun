package throughput

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZhuzhuNo3/transferlanes/internal/childprocesses"
	"github.com/ZhuzhuNo3/transferlanes/internal/hostnetwork"
	"github.com/ZhuzhuNo3/transferlanes/internal/namespaceresolvers"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const (
	helperArgumentPrefix    = "--transferlanes-internal-throughput="
	helperWireVersion       = 1
	helperRequestType       = "transferlanes.throughput.request"
	helperResultType        = "transferlanes.throughput.result"
	maximumHelperWireBytes  = 4096
	maximumHelperDiagnostic = 1024
)

// NamespaceExecution is the process-placement adapter for one throughput
// target. Upload semantics remain owned by Tester in both host and namespace.
type NamespaceExecution struct {
	target      Target
	cwd         string
	resolver    namespaceresolvers.ResolverAccess
	environment []string
}

func NewNamespaceExecution(id transfernumber.Number, workingDirectory string,
	resolver namespaceresolvers.ResolverAccess, environment []string) (NamespaceExecution, error) {
	target, err := NewCurrentNamespaceTarget(id)
	if err != nil || resolver == nil || !filepath.IsAbs(workingDirectory) || filepath.Clean(workingDirectory) != workingDirectory {
		return NamespaceExecution{}, errors.Join(errors.New("namespace throughput execution is invalid"), err)
	}
	if file, accessErr := resolver.Open(); accessErr != nil {
		return NamespaceExecution{}, accessErr
	} else {
		_ = file.Close()
	}
	return NamespaceExecution{target: target, cwd: strings.Clone(workingDirectory), resolver: resolver,
		environment: append([]string(nil), environment...)}, nil
}

type helperRequest struct {
	Version    int    `json:"version"`
	Type       string `json:"type"`
	Transfer   uint8  `json:"transfer"`
	Endpoint   string `json:"endpoint"`
	DurationNS int64  `json:"durationNs"`
}
type helperResult struct {
	Version    int    `json:"version"`
	Type       string `json:"type"`
	Transfer   uint8  `json:"transfer"`
	Bytes      uint64 `json:"bytes,omitempty"`
	DurationNS int64  `json:"durationNs,omitempty"`
	Stage      uint8  `json:"stage,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ObserveNamespaces runs one helper per already-created namespace. A known
// endpoint failure is returned as Observation while helper/protocol failures
// remain group errors.
func (tester *Tester) ObserveNamespaces(ctx context.Context, owner *childprocesses.ChildProcesses,
	network *hostnetwork.Session, executions []NamespaceExecution, settings Settings) ([]Observation, *childprocesses.ProcessSet, error) {
	if tester == nil || ctx == nil || owner == nil || network == nil || len(executions) == 0 || !settings.Valid() {
		return nil, nil, errors.New("namespace throughput inputs are incomplete")
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("locate throughput helper executable: %w", err)
	}
	commands := make([]childprocesses.Command, len(executions))
	for index, execution := range executions {
		request := helperRequest{Version: helperWireVersion, Type: helperRequestType,
			Transfer: execution.target.transfer.Value(), Endpoint: settings.Endpoint(), DurationNS: int64(settings.duration)}
		document, err := json.Marshal(request)
		if err != nil || len(document) > maximumHelperWireBytes {
			return nil, nil, errors.Join(errors.New("encode throughput helper request"), err)
		}
		argument := helperArgumentPrefix + base64.RawURLEncoding.EncodeToString(document)
		commands[index], err = childprocesses.NewCommand(execution.target.transfer, execution.cwd,
			execution.resolver, []string{executable, argument}, execution.environment)
		if err != nil {
			return nil, nil, err
		}
	}
	group, uncontained, err := owner.RunHelpers(ctx, network, commands)
	if err != nil {
		return nil, uncontained, err
	}
	results, err := decodeHelperResults(group, executions, settings)
	return results, nil, err
}

func MaybeRunHelper(argv []string) (bool, int) {
	if len(argv) != 1 || !strings.HasPrefix(argv[0], helperArgumentPrefix) {
		return false, 0
	}
	request, err := decodeHelperRequest(argv[0])
	if err != nil {
		return true, writeHelperProtocolFailure(err)
	}
	id, _ := transfernumber.New(int(request.Transfer))
	settings, _ := NewSettings(request.Endpoint, time.Duration(request.DurationNS))
	observation := New().Observe(context.Background(), []Target{{transfer: id}}, settings)[0]
	result := helperResult{Version: helperWireVersion, Type: helperResultType, Transfer: request.Transfer,
		Bytes: observation.Bytes(), DurationNS: int64(observation.Duration())}
	if observation.Err() != nil {
		var failure *Failure
		if !errors.As(observation.Err(), &failure) {
			return true, writeHelperProtocolFailure(observation.Err())
		}
		result.Stage, result.Error = uint8(failure.Stage), boundedDiagnostic(failure.Cause)
	}
	document, err := json.Marshal(result)
	if err != nil || len(document) > maximumHelperWireBytes {
		return true, 1
	}
	if _, err := os.Stdout.Write(document); err != nil {
		return true, 1
	}
	return true, 0
}

func decodeHelperRequest(argument string) (helperRequest, error) {
	encoded := strings.TrimPrefix(argument, helperArgumentPrefix)
	document, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(document) == 0 || len(document) > maximumHelperWireBytes {
		return helperRequest{}, errors.New("decode throughput helper request")
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	var request helperRequest
	if err := decoder.Decode(&request); err != nil {
		return helperRequest{}, err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return helperRequest{}, errors.New("throughput helper request has trailing data")
	}
	id, idErr := transfernumber.New(int(request.Transfer))
	settings, settingsErr := NewSettings(request.Endpoint, time.Duration(request.DurationNS))
	if request.Version != helperWireVersion || request.Type != helperRequestType || idErr != nil || !settings.Valid() || id.Value() != request.Transfer {
		return helperRequest{}, errors.Join(errors.New("throughput helper request is invalid"), idErr, settingsErr)
	}
	return request, nil
}

func decodeHelperResults(group childprocesses.HelperResult, executions []NamespaceExecution,
	settings Settings) ([]Observation, error) {
	if len(group.Transfers) != len(executions) {
		return nil, errors.New("throughput helper result set is incomplete")
	}
	byTransfer := make(map[transfernumber.Number]childprocesses.HelperTransfer, len(group.Transfers))
	for _, result := range group.Transfers {
		if _, duplicate := byTransfer[result.Transfer]; duplicate {
			return nil, errors.New("throughput helper result is repeated")
		}
		byTransfer[result.Transfer] = result
	}
	observations := make([]Observation, len(executions))
	for index, execution := range executions {
		help, exists := byTransfer[execution.target.transfer]
		if !exists || help.ExitCode != 0 || help.Signal != 0 {
			return nil, fmt.Errorf("transfer %d throughput helper failed: exit=%d signal=%d", execution.target.transfer.Value(), help.ExitCode, help.Signal)
		}
		if len(help.Output) == 0 || len(help.Output) > maximumHelperWireBytes {
			return nil, fmt.Errorf("transfer %d throughput helper output is invalid", help.Transfer.Value())
		}
		decoder := json.NewDecoder(bytes.NewReader(help.Output))
		decoder.DisallowUnknownFields()
		var result helperResult
		if err := decoder.Decode(&result); err != nil {
			return nil, err
		}
		if decoder.Decode(&struct{}{}) != io.EOF {
			return nil, errors.New("throughput helper result has trailing data")
		}
		if result.Version != helperWireVersion || result.Type != helperResultType || result.Transfer != help.Transfer.Value() || time.Duration(result.DurationNS) != settings.duration {
			return nil, errors.New("throughput helper result identity is invalid")
		}
		if result.Error != "" {
			stage := FailureStage(result.Stage)
			if stage < ResolveEndpoint || stage > SampleUpload {
				return nil, errors.New("throughput helper failure stage is invalid")
			}
			observations[index] = failed(execution.target, settings.duration, stage, errors.New(result.Error))
			continue
		}
		if result.Stage != 0 || result.Bytes == 0 {
			return nil, errors.New("throughput helper success is invalid")
		}
		observations[index] = Observation{target: execution.target, bytes: result.Bytes, duration: settings.duration}
	}
	return observations, nil
}

func writeHelperProtocolFailure(cause error) int {
	_, _ = fmt.Fprintf(os.Stderr, "transferlanes throughput helper: %s", boundedDiagnostic(cause))
	return 1
}

func boundedDiagnostic(cause error) string {
	if cause == nil {
		return "unknown failure"
	}
	value := strings.ToValidUTF8(cause.Error(), "?")
	if len(value) <= maximumHelperDiagnostic {
		return value
	}
	value = value[:maximumHelperDiagnostic]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
