//go:build linux && rootintegration

package root_test

import (
	"bytes"
	"os"
	"testing"
)

func readHostResolver(t *testing.T) ([]byte, os.FileInfo) {
	t.Helper()
	contents, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		t.Fatalf("snapshot host resolver contents: %v", err)
	}
	info, err := os.Stat("/etc/resolv.conf")
	if err != nil {
		t.Fatalf("snapshot host resolver identity: %v", err)
	}
	return contents, info
}

func assertHostResolverUnchanged(t *testing.T, want []byte, identity os.FileInfo) {
	t.Helper()
	got, info := readHostResolver(t)
	if !bytes.Equal(got, want) || !os.SameFile(identity, info) {
		t.Fatalf("host resolver changed: identity_same=%t contents_same=%t",
			os.SameFile(identity, info), bytes.Equal(got, want))
	}
}
