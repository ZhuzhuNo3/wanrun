//go:build linux

package childprocesses

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func TestHelperStderrFloodHasBoundedDiagnostic(t *testing.T) {
	id, _ := transfernumber.New(1)
	cancellations := 0
	evidence := newHelperEvidence([]Command{{transfer: id}}, func() { cancellations++ })
	chunk := bytes.Repeat([]byte{0xff}, 512)

	evidence.acceptOutput(Output{Transfer: id, Stream: StreamStderr, Bytes: chunk})
	if cancellations != 1 {
		t.Fatalf("first stderr chunk triggered %d cancellations, want 1", cancellations)
	}
	for range 128 {
		evidence.acceptOutput(Output{Transfer: id, Stream: StreamStderr, Bytes: chunk})
	}

	diagnostic := evidence.diagnosticError().Error()
	if len(diagnostic) > 2048 {
		t.Fatalf("helper stderr diagnostic grew to %d bytes", len(diagnostic))
	}
	if !strings.Contains(diagnostic, "stderr diagnostic exceeds limit") {
		t.Fatalf("bounded helper stderr diagnostic does not report truncation: %q", diagnostic)
	}
	if cancellations != 1 {
		t.Fatalf("stderr drain triggered %d cancellations, want 1", cancellations)
	}
}
