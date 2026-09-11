package childprocesses

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestHelperHandoffHasOnlyPreparedAndExecFailureFrames(t *testing.T) {
	for _, want := range []helperHandoff{
		{kind: helperPrepared},
		{kind: helperExecFailed, diagnostic: "exec exact child argv: executable disappeared"},
	} {
		var wire bytes.Buffer
		if err := writeHelperHandoff(&wire, want); err != nil {
			t.Fatal(err)
		}
		got, err := readHelperHandoff(&wire)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("handoff = %#v, want %#v", got, want)
		}
	}

	invalidKind := []byte{helperVersion, 0xff, 0, 0}
	preparedPayload := []byte{helperVersion, byte(helperPrepared), 0, 1, 'x'}
	emptyFailure := []byte{helperVersion, byte(helperExecFailed), 0, 0}
	invalidUTF8 := []byte{helperVersion, byte(helperExecFailed), 0, 1, 0xff}
	oversized := []byte{helperVersion, byte(helperExecFailed), 0, 0}
	binary.BigEndian.PutUint16(oversized[2:], maximumHelperFailureBytes+1)
	for name, encoded := range map[string][]byte{
		"kind": invalidKind, "prepared payload": preparedPayload, "empty failure": emptyFailure,
		"invalid UTF-8": invalidUTF8, "oversized": oversized,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readHelperHandoff(bytes.NewReader(encoded)); err == nil {
				t.Fatal("invalid helper handoff was accepted")
			}
		})
	}
}

func TestHelperExecFailureDiagnosticIsBoundedOnWrite(t *testing.T) {
	var wire bytes.Buffer
	if err := writeHelperHandoff(&wire, helperHandoff{kind: helperExecFailed,
		diagnostic: strings.Repeat("界", maximumHelperFailureBytes)}); err != nil {
		t.Fatal(err)
	}
	got, err := readHelperHandoff(&wire)
	if err != nil {
		t.Fatal(err)
	}
	if got.kind != helperExecFailed || len(got.diagnostic) > maximumHelperFailureBytes || got.diagnostic == "" {
		t.Fatalf("bounded failure = %#v", got)
	}
}
