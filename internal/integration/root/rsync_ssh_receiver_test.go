//go:build linux && rootintegration && protocolacceptance

package root_test

import (
	"fmt"
	"io/fs"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type rsyncSSHReceiver struct {
	server      *sshServer
	gate        *rsyncProtocolGate
	destination string
}

func startRsyncSSHReceiver(t *testing.T, fixture *hostNetworkFixture) *rsyncSSHReceiver {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "by-source")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	gate := newRsyncProtocolGate(t, fixture.sources, netip.Addr{})
	forceCommand := strings.Join([]string{
		strconv.Quote(os.Args[0]), transferChildArgument,
		transferChildRsyncServerGate, strconv.Quote(gate.listener.Addr().String()),
		strconv.Quote(destination),
	}, " ")
	return &rsyncSSHReceiver{server: startSSHServerWithForceCommand(t, fixture, forceCommand),
		gate: gate, destination: destination}
}

func (receiver *rsyncSSHReceiver) assertTransfer(t *testing.T, sourceRoot string,
	want map[string]protocolEntry, sources []netip.Addr,
) {
	t.Helper()
	assertRsyncReceiverSources(t, receiver.destination, sources)
	got := make(map[string]protocolEntry, len(want))
	owners := make(map[string]netip.Addr, len(want))
	base := filepath.Base(sourceRoot)
	transferSizes := make(map[netip.Addr]int, len(sources))
	for _, source := range sources {
		entries, err := readRsyncProtocolEntries(filepath.Join(receiver.destination,
			source.String(), base))
		if err != nil {
			t.Fatalf("read rsync transfer from %s: %v", source, err)
		}
		if len(entries) == 0 {
			t.Errorf("rsync transfer from %s has an empty manifest", source)
		}
		transferSizes[source] = len(entries)
		for member, entry := range entries {
			if previous, duplicate := owners[member]; duplicate {
				t.Errorf("rsync member %q received from both %s and %s", member, previous, source)
				continue
			}
			owners[member], got[member] = source, entry
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("rsync receiver manifest=%s, want=%s",
			describeProtocolManifest(got), describeProtocolManifest(want))
	}
	counts := receiver.gate.ConnectionCounts()
	for _, source := range sources {
		if counts[source] != 1 {
			t.Errorf("rsync receiver connections from %s=%d, want 1", source, counts[source])
		}
		if !strings.Contains(receiver.server.output(), "Connection from "+source.String()+" port") {
			t.Errorf("sshd log does not correlate source %s:\n%s", source, receiver.server.output())
		}
	}
	t.Logf("rsync receiver accepted connections=%v and verified per-transfer entries=%v, union=%d",
		counts, transferSizes, len(got))
}

func assertRsyncReceiverSources(t *testing.T, destination string, sources []netip.Addr) {
	t.Helper()
	entries, err := os.ReadDir(destination)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			t.Fatalf("unexpected rsync receiver root member %q", entry.Name())
		}
		got[entry.Name()] = true
	}
	want := make(map[string]bool, len(sources))
	for _, source := range sources {
		want[source.String()] = true
	}
	if !maps.Equal(got, want) {
		t.Fatalf("rsync receiver sources=%v, want=%v", got, want)
	}
}

func readRsyncProtocolEntries(root string) (map[string]protocolEntry, error) {
	result := make(map[string]protocolEntry)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil || relative == "." {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			children, readErr := os.ReadDir(path)
			if readErr == nil && len(children) == 0 {
				result[relative] = protocolEntry{kind: protocolEmptyDirectory}
			}
			return readErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unexpected rsync member %q with mode %s", relative, info.Mode())
		}
		hash, err := pathSHA256(path)
		if err == nil {
			result[relative] = protocolEntry{kind: protocolRegularFile, size: info.Size(), sha256: hash}
		}
		return err
	})
	return result, err
}
