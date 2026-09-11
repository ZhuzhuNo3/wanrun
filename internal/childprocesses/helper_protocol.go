package childprocesses

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const (
	helperVersion        = uint8(5)
	helperType           = "child-process"
	maximumHelperPayload = 1024 * 1024
)

type helperPayload struct {
	Version          uint8    `json:"version"`
	Type             string   `json:"type"`
	Transfer         uint8    `json:"transfer"`
	NamespaceInode   uint64   `json:"namespaceInode"`
	ExecChannelDev   uint64   `json:"execChannelDev,omitempty"`
	ExecChannelInode uint64   `json:"execChannelInode,omitempty"`
	ViewDev          uint64   `json:"viewDev,omitempty"`
	ViewInode        uint64   `json:"viewInode,omitempty"`
	ViewRunID        []byte   `json:"viewRunID,omitempty"`
	ViewBaseName     []byte   `json:"viewBaseName,omitempty"`
	Interactive      bool     `json:"interactive"`
	WorkingDir       []byte   `json:"workingDirectory"`
	Argv             [][]byte `json:"argv"`
	Environment      [][]byte `json:"environment"`
}

type descriptorIdentity struct {
	device uint64
	inode  uint64
}

type inheritedIdentity struct {
	execChannel descriptorIdentity
	view        viewDescriptorIdentity
}

type viewDescriptorIdentity struct {
	root     descriptorIdentity
	runID    runid.ID
	baseName string
}

type helperRequest struct {
	transfer       transfernumber.Number
	namespaceInode uint64
	execChannel    descriptorIdentity
	view           viewDescriptorIdentity
	interactive    bool
	cwd            string
	argv           []string
	environment    []string
}

func helperRequestFromCommand(command Command, namespaceInode uint64,
	inherited inheritedIdentity, interactive bool) helperRequest {
	return helperRequest{transfer: command.transfer, namespaceInode: namespaceInode,
		execChannel: inherited.execChannel, view: inherited.view,
		interactive: interactive, cwd: command.cwd,
		argv: command.Argv(), environment: command.Environment()}
}

func encodeHelperRequest(request helperRequest) ([]byte, error) {
	if err := validateHelperRequest(request); err != nil {
		return nil, err
	}
	var viewRunID []byte
	if request.view.runID != (runid.ID{}) {
		viewRunID = []byte(request.view.runID.String())
	}
	payload := helperPayload{Version: helperVersion, Type: helperType, Transfer: request.transfer.Value(),
		NamespaceInode: request.namespaceInode, ExecChannelDev: request.execChannel.device,
		ExecChannelInode: request.execChannel.inode, WorkingDir: []byte(request.cwd),
		ViewDev: request.view.root.device, ViewInode: request.view.root.inode,
		ViewRunID: viewRunID, ViewBaseName: []byte(request.view.baseName),
		Interactive: request.interactive,
		Argv:        stringsToBytes(request.argv), Environment: stringsToBytes(request.environment)}
	encoded, err := json.Marshal(payload)
	if err != nil || len(encoded) > maximumHelperPayload {
		return nil, fmt.Errorf("encode child helper request")
	}
	return encoded, nil
}

func decodeHelperRequest(reader io.Reader) (helperRequest, error) {
	limited := io.LimitReader(reader, maximumHelperPayload+1)
	encoded, err := io.ReadAll(limited)
	if err != nil || len(encoded) > maximumHelperPayload {
		return helperRequest{}, fmt.Errorf("read child helper request: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var payload helperPayload
	if err := decoder.Decode(&payload); err != nil {
		return helperRequest{}, fmt.Errorf("decode child helper request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return helperRequest{}, fmt.Errorf("child helper request has trailing data")
	}
	id, err := transfernumber.New(int(payload.Transfer))
	if err != nil || payload.Version != helperVersion || payload.Type != helperType {
		return helperRequest{}, fmt.Errorf("child helper protocol identity is invalid")
	}
	viewRunID, viewRunErr := runid.Parse(string(payload.ViewRunID))
	if len(payload.ViewRunID) == 0 {
		viewRunID, viewRunErr = runid.ID{}, nil
	}
	if viewRunErr != nil {
		return helperRequest{}, fmt.Errorf("child file-view run identity is invalid")
	}
	request := helperRequest{transfer: id, namespaceInode: payload.NamespaceInode,
		execChannel: descriptorIdentity{device: payload.ExecChannelDev, inode: payload.ExecChannelInode},
		view: viewDescriptorIdentity{
			root:     descriptorIdentity{device: payload.ViewDev, inode: payload.ViewInode},
			runID:    viewRunID,
			baseName: string(append([]byte(nil), payload.ViewBaseName...)),
		},
		interactive: payload.Interactive,
		cwd:         string(append([]byte(nil), payload.WorkingDir...)), argv: bytesToStrings(payload.Argv),
		environment: bytesToStrings(payload.Environment)}
	if err := validateHelperRequest(request); err != nil {
		return helperRequest{}, err
	}
	return request, nil
}

func validateHelperRequest(request helperRequest) error {
	command := Command{transfer: request.transfer, cwd: request.cwd,
		argv: request.argv, environment: request.environment}
	if request.namespaceInode == 0 {
		return fmt.Errorf("child helper namespace identity is invalid")
	}
	if (request.execChannel.device == 0) != (request.execChannel.inode == 0) {
		return fmt.Errorf("child exec channel identity is incomplete")
	}
	rootPresent := request.view.root.device != 0 && request.view.root.inode != 0
	if (request.view.root.device == 0) != (request.view.root.inode == 0) ||
		rootPresent != (request.view.runID != (runid.ID{})) || rootPresent != (request.view.baseName != "") {
		return fmt.Errorf("child file-view identity is incomplete")
	}
	if request.execChannel.inode != 0 && rootPresent {
		return fmt.Errorf("child inherited capability identity is ambiguous")
	}
	viewPath := ""
	if rootPresent {
		var err error
		viewPath, err = DescriptorBoundViewPath(request.view.runID, request.transfer, request.view.baseName)
		if err != nil {
			return err
		}
		if request.cwd != viewPath {
			return fmt.Errorf("child file-view working directory is not descriptor-bound")
		}
		viewArguments := 0
		for _, argument := range request.argv {
			if argument == viewPath {
				viewArguments++
			}
		}
		if viewArguments != 1 {
			return fmt.Errorf("child file-view argv binding is invalid")
		}
	} else if descriptorViewPath(request.cwd) || slices.ContainsFunc(request.argv, descriptorViewPath) {
		return fmt.Errorf("child file-view path lacks inherited identity")
	}
	return validateCommandFields(command, viewPath)
}

func descriptorViewPath(value string) bool {
	return strings.HasPrefix(value, descriptorBoundViewAuthority+"/view-")
}

func stringsToBytes(values []string) [][]byte {
	result := make([][]byte, len(values))
	for index, value := range values {
		result[index] = append([]byte(nil), value...)
	}
	return result
}

func bytesToStrings(values [][]byte) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = string(append([]byte(nil), value...))
	}
	return result
}
