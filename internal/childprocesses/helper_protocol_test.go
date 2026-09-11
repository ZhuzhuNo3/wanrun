package childprocesses

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/runid"
	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestHelperPayloadRoundTripsNonUTF8ExactArgv(t *testing.T) {
	id, _ := transfernumber.New(4)
	command, err := NewCommand(id, "/work", testResolverAccess(t),
		[]string{"/bin/tool", string([]byte{'a', 0xff, 'b'})}, []string{"TERM=x", "RAW=" + string([]byte{0xfe})})
	if err != nil {
		t.Fatal(err)
	}
	want := helperRequestFromCommand(command, 12345, inheritedIdentity{
		execChannel: descriptorIdentity{device: 23, inode: 42},
	}, true)
	encoded, err := encodeHelperRequest(want)
	if err != nil {
		t.Fatal(err)
	}
	var wireFields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wireFields); err != nil {
		t.Fatal(err)
	}
	if _, present := wireFields["transfer"]; !present {
		t.Fatal("helper identity wire field changed")
	}
	if _, present := wireFields["line"]; present {
		t.Fatal("helper wire retained the obsolete transfer alias")
	}
	got, err := decodeHelperRequest(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	if got.transfer != want.transfer || got.namespaceInode != want.namespaceInode || got.cwd != want.cwd ||
		got.execChannel != want.execChannel || got.view != want.view || got.interactive != want.interactive ||
		!slices.Equal(got.argv, want.argv) || !slices.Equal(got.environment, want.environment) {
		t.Fatalf("helper request = %#v, want %#v", got, want)
	}
}

func TestHelperPayloadRejectsUnknownOrTrailingData(t *testing.T) {
	unknown, _ := json.Marshal(map[string]any{"version": helperVersion, "type": helperType, "unknown": true})
	for name, encoded := range map[string][]byte{
		"unknown":  unknown,
		"trailing": append([]byte(`{"version":1}`), []byte(` {}`)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeHelperRequest(bytes.NewReader(encoded)); err == nil {
				t.Fatal("invalid helper payload was accepted")
			}
		})
	}
}

func TestHelperPayloadBindsViewToExactInheritedRootDescriptor(t *testing.T) {
	id, _ := transfernumber.New(2)
	run, _ := runid.Parse("00112233445566778899aabbccddeeff")
	viewPath, err := DescriptorBoundViewPath(run, id, "captured-source")
	if err != nil {
		t.Fatal(err)
	}
	request := helperRequest{transfer: id, namespaceInode: 71,
		view: viewDescriptorIdentity{root: descriptorIdentity{device: 10, inode: 21},
			runID: run, baseName: "captured-source"},
		cwd: viewPath, argv: []string{"copy", viewPath}, environment: []string{"PATH=/usr/bin"}}
	encoded, err := encodeHelperRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeHelperRequest(bytes.NewReader(encoded))
	if err != nil || decoded.view != request.view || decoded.cwd != viewPath {
		t.Fatalf("descriptor-bound view request = %#v, %v", decoded, err)
	}

	for name, mutate := range map[string]func(*helperRequest){
		"basename traversal":    func(value *helperRequest) { value.view.baseName = "../captured-source" },
		"string path mismatch":  func(value *helperRequest) { value.cwd = "/tmp/replacement/source" },
		"missing root identity": func(value *helperRequest) { value.view.root = descriptorIdentity{} },
		"missing run identity": func(value *helperRequest) {
			value.view.runID = runid.ID{}
		},
		"ambiguous capability": func(value *helperRequest) {
			value.execChannel = descriptorIdentity{device: 30, inode: 40}
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := request
			mutate(&invalid)
			if _, err := encodeHelperRequest(invalid); err == nil {
				t.Fatal("unsafe file-view capability was accepted")
			}
		})
	}
}
