package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

func TestResultContractAcceptsOnePassPerExpectedTarget(t *testing.T) {
	expected := map[string]map[string]int{"example/pkg": {"TestOne": 0}}
	events := strings.Join([]string{
		`{"Action":"run","Package":"example/pkg","Test":"TestOne"}`,
		`{"Action":"run","Package":"example/pkg","Test":"TestOne/case"}`,
		`{"Action":"pass","Package":"example/pkg","Test":"TestOne/case"}`,
		`{"Action":"pass","Package":"example/pkg","Test":"TestOne"}`,
		`{"Action":"pass","Package":"example/pkg"}`,
	}, "\n")
	if err := checkEvents(expected, bufio.NewScanner(strings.NewReader(events))); err != nil {
		t.Fatal(err)
	}
}

func TestResultContractRejectsMissingTarget(t *testing.T) {
	assertRejectedEvents(t, "", "pass count=0")
}

func TestResultContractRejectsSkippedSubtest(t *testing.T) {
	assertRejectedEvents(t,
		`{"Action":"skip","Package":"example/pkg","Test":"TestOne/case"}`+"\n"+
			`{"Action":"pass","Package":"example/pkg","Test":"TestOne"}`,
		"skip: example/pkg TestOne/case")
}

func TestResultContractRejectsFailedTarget(t *testing.T) {
	assertRejectedEvents(t,
		`{"Action":"fail","Package":"example/pkg","Test":"TestOne"}`,
		"fail: example/pkg TestOne")
}

func TestResultContractRejectsUnexpectedTarget(t *testing.T) {
	assertRejectedEvents(t,
		`{"Action":"pass","Package":"example/pkg","Test":"TestOther"}`,
		"unexpected selected target")
}

func TestExpectedTargetMappingMustNotBeEmpty(t *testing.T) {
	path := t.TempDir() + "/expected"
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readExpected(path); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error=%v, want empty mapping rejection", err)
	}
}

func assertRejectedEvents(t *testing.T, events, fragment string) {
	t.Helper()
	expected := map[string]map[string]int{"example/pkg": {"TestOne": 0}}
	err := checkEvents(expected, bufio.NewScanner(strings.NewReader(events)))
	if err == nil || !strings.Contains(err.Error(), fragment) {
		t.Fatalf("error=%v, want fragment %q", err, fragment)
	}
}
