//go:build linux && rootintegration && protocolacceptance

package root_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPublicRsyncOverSSHUsesEverySelectedSource(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "public-rsync-ssh")
	server := startSSHServer(t, fixture)
	processMarker := "rsync-" + filepath.Base(scenario.markers)
	t.Setenv("TRANSFERLANES_PROTOCOL_RUN", processMarker)
	destination := filepath.Join(scenario.markers, "ssh-destination")
	logDirectory := filepath.Join(scenario.markers, "rsync-transfer-logs")
	if err := os.Mkdir(destination, 0o700); err != nil {
		t.Fatal(err)
	}

	arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv), "--log-dir", logDirectory}
	for index, source := range fixture.sources {
		weight := []string{"@1.5", "", "@0.5"}[index]
		arguments = append(arguments, "--network", source.String()+weight)
	}
	arguments = append(arguments, "--", "/usr/bin/rsync", "-a", "--itemize-changes",
		"--out-format=TRANSFERLANES_FILE:%n", "-e", server.clientShell(), "{}",
		"root@"+fixture.remote.String()+":"+destination+"/")
	output, err := runPublicTransferLanesPTY(t, arguments, nil)
	if err != nil {
		t.Fatalf("public rsync over SSH failed: %v\n%s\nsshd:\n%s", err, output, server.output())
	}
	assertInteractiveSuccessSnapshotTitles(t, output, fixture.sources)
	assertPublicRsyncTree(t, destination, os.Getenv(supervisorTransferEnv))
	assertRsyncTransferManifest(t, logDirectory, filepath.Base(os.Getenv(supervisorTransferEnv)),
		len(fixture.sources))
	for _, source := range fixture.sources {
		if !strings.Contains(server.output(), "Connection from "+source.String()+" port") {
			t.Errorf("sshd did not observe selected source %s:\n%s", source, server.output())
		}
	}
	assertNoMarkedProcesses(t, "TRANSFERLANES_PROTOCOL_RUN="+processMarker)
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("rsync over SSH left run roots: before=%v after=%v", runsBefore, after)
	}
}

func TestPublicS3ClientUsesEverySelectedSource(t *testing.T) {
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	fixture.newRunSupervisorScenario(t, "public-s3")
	server := startS3Server(t, fixture)
	processMarker := "s3-" + filepath.Base(os.Getenv(supervisorMarkerEnv))
	t.Setenv("TRANSFERLANES_PROTOCOL_RUN", processMarker)
	for _, variable := range awsEnvironment() {
		name, value, _ := strings.Cut(variable, "=")
		t.Setenv(name, value)
	}

	arguments := []string{"run", "--source", os.Getenv(supervisorTransferEnv)}
	for _, source := range fixture.sources {
		arguments = append(arguments, "--network", source.String())
	}
	arguments = append(arguments, "--", "/usr/local/bin/aws", "--endpoint-url", server.endpoint,
		"s3", "cp", "--recursive", "--no-progress", "--only-show-errors",
		"{}", "s3://"+server.bucket+"/run/")
	output, err := runPublicTransferLanesPTY(t, arguments, nil)
	if err != nil {
		t.Fatalf("public S3 transfer failed: %v\n%s\nMinIO:\n%s", err, output, server.logs())
	}
	assertInteractiveSuccessSnapshotTitles(t, output, fixture.sources)
	assertS3Objects(t, server, protocolEntriesFromContents(transferSourceContents()))
	assertProtocolSources(t, server.observed.snapshot(), fixture.sources)
	assertNoMarkedProcesses(t, "TRANSFERLANES_PROTOCOL_RUN="+processMarker)
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("S3 transfer left run roots: before=%v after=%v", runsBefore, after)
	}
}

func assertS3Objects(t *testing.T, server *s3Server, expected map[string]protocolEntry) {
	t.Helper()
	got := readS3ProtocolEntries(t, server)
	want := protocolRegularEntries(expected)
	if !maps.Equal(got, want) {
		t.Fatalf("S3 object manifest=%s, want=%s",
			describeProtocolManifest(got), describeProtocolManifest(want))
	}
	writes := make(map[string][]netip.Addr, len(want))
	for _, write := range server.writes.snapshot() {
		writes[write.path] = append(writes[write.path], write.source)
	}
	assertS3WriteUnion(t, server.bucket, want, writes)
}

func readS3ProtocolEntries(t *testing.T, server *s3Server) map[string]protocolEntry {
	t.Helper()
	output := server.runAWS(t, "s3api", "list-objects-v2", "--bucket", server.bucket,
		"--prefix", "run/", "--output", "json")
	var listing struct {
		Contents []struct {
			Key string `json:"Key"`
		} `json:"Contents"`
	}
	if err := json.Unmarshal([]byte(output), &listing); err != nil {
		t.Fatalf("decode S3 listing: %v: %s", err, output)
	}
	got := make(map[string]protocolEntry, len(listing.Contents))
	downloads := t.TempDir()
	for index, object := range listing.Contents {
		if !strings.HasPrefix(object.Key, "run/") {
			t.Fatalf("unexpected S3 object key %q", object.Key)
		}
		relative := strings.TrimPrefix(object.Key, "run/")
		path := filepath.Join(downloads, strconv.Itoa(index))
		server.runAWS(t, "s3api", "get-object", "--bucket", server.bucket,
			"--key", object.Key, path)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("downloaded S3 object %q has mode %s", object.Key, info.Mode())
		}
		hash, err := pathSHA256(path)
		if err != nil {
			t.Fatal(err)
		}
		got[relative] = protocolEntry{kind: protocolRegularFile, size: info.Size(), sha256: hash}
	}
	return got
}

func assertS3WriteUnion(t *testing.T, bucket string, objects map[string]protocolEntry,
	writes map[string][]netip.Addr) {
	t.Helper()
	owners := make(map[string]netip.Addr, len(objects))
	transferSizes := make(map[netip.Addr]int)
	for object := range objects {
		path := "/" + bucket + "/run/" + object
		if len(writes[path]) != 1 {
			t.Errorf("S3 object %s was written from %v, want exactly one source", object, writes[path])
		} else {
			owners[object] = writes[path][0]
			transferSizes[writes[path][0]]++
		}
		delete(writes, path)
	}
	if len(writes) != 0 {
		t.Errorf("unexpected S3 writes: %v", writes)
	}
	t.Logf("S3 receiver verified unique owners=%v and per-transfer objects=%v", owners, transferSizes)
}

func assertRsyncTransferManifest(t *testing.T, logDirectory, sourceBase string, transferCount int) {
	t.Helper()
	want := make(map[string]int, len(transferSourceContents()))
	for relative := range transferSourceContents() {
		want[filepath.ToSlash(filepath.Join(sourceBase, relative))] = 1
	}
	seen := make(map[string]int, len(want))
	owners := make(map[string]int, len(want))
	for number := 1; number <= transferCount; number++ {
		path := filepath.Join(logDirectory, fmt.Sprintf("transfer-%02d.ptylog", number))
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read rsync transfer %d log: %v", number, err)
		}
		transferFiles := 0
		for _, outputLine := range strings.Split(strings.ReplaceAll(string(content), "\r\n", "\n"), "\n") {
			marker := strings.Index(outputLine, "TRANSFERLANES_FILE:")
			if marker < 0 {
				continue
			}
			relative := strings.TrimSpace(outputLine[marker+len("TRANSFERLANES_FILE:"):])
			if _, expected := want[relative]; !expected {
				if relative == sourceBase+"/" || relative == sourceBase+"/nested/" {
					continue
				}
				t.Errorf("transfer %d reported unexpected rsync member %q", number, relative)
				continue
			}
			seen[relative]++
			if previous, duplicate := owners[relative]; duplicate && previous != number {
				t.Errorf("rsync member %q appeared on transfers %d and %d", relative, previous, number)
			}
			owners[relative] = number
			transferFiles++
		}
		if transferFiles == 0 {
			t.Errorf("rsync transfer %d transferred no regular file", number)
		}
	}
	if !maps.Equal(seen, want) {
		t.Errorf("rsync transfer events=%v, want each member once=%v", seen, want)
	}
}

func TestPublicProtocolsKeepSiblingsRunningAfterOneTransferFails(t *testing.T) {
	t.Run("rsync", func(t *testing.T) {
		runSiblingFailureAcceptance(t, "rsync", func(fixture *hostNetworkFixture,
			scenario supervisorRootScenario) protocolLifecycleAcceptance {
			return newRsyncAcceptance(t, fixture, scenario, liveProtocolFailure)
		})
	})
	t.Run("s3", func(t *testing.T) {
		runSiblingFailureAcceptance(t, "s3", func(fixture *hostNetworkFixture,
			scenario supervisorRootScenario) protocolLifecycleAcceptance {
			return newS3Acceptance(t, fixture, scenario, liveProtocolFailure)
		})
	})
}

func runSiblingFailureAcceptance(t *testing.T, name string,
	create func(*hostNetworkFixture, supervisorRootScenario) protocolLifecycleAcceptance,
) {
	t.Helper()
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "public-"+name+"-transfer-failure")
	acceptance := create(fixture, scenario)
	defer acceptance.release()
	facts := acceptance.facts()
	processMarker := name + "-fail-" + filepath.Base(scenario.markers)
	t.Setenv("TRANSFERLANES_PROTOCOL_RUN", processMarker)
	output, err := runPublicTransferLanesPTY(t, facts.arguments,
		func(_ context.Context, _ *exec.Cmd, _ *os.File, capture *boundedPTYCapture) error {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := acceptance.waitUntilActive(ctx); err != nil {
				return err
			}
			if err := capture.waitForText(ctx, "EXIT 1"); err != nil {
				return err
			}
			acceptance.release()
			return nil
		})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("transfer failure exit=%v output=%q", err, output)
	}
	succeeded, failed := protocolTransferResults(output, len(fixture.sources))
	if failed != 1 || succeeded != len(fixture.sources)-1 {
		t.Fatalf("transfer failure did not preserve siblings: %q", output)
	}
	if strings.Contains(finalInteractiveOutput(t, output), "SOURCE ") {
		t.Fatalf("%s transfer failure retained source summary: %q", name, output)
	}
	acceptance.assertStopped(t)
	acceptance.assertSiblingCompletion(t)
	assertProtocolSources(t, facts.observed(), fixture.sources)
	assertNoMarkedProcesses(t, "TRANSFERLANES_PROTOCOL_RUN="+processMarker)
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("%s transfer failure left run roots: before=%v after=%v", name, runsBefore, after)
	}
}

func protocolTransferResults(output string, transferCount int) (succeeded, failed int) {
	tail := output
	if index := strings.LastIndex(output, "\x1b[?1049l"); index >= 0 {
		tail = output[index+len("\x1b[?1049l"):]
	}
	for number := 1; number <= transferCount; number++ {
		prefix := "[transfer " + strconv.Itoa(number) + " |"
		var title string
		for line := range strings.SplitSeq(strings.ReplaceAll(tail, "\r\n", "\n"), "\n") {
			if strings.Contains(line, prefix) {
				title = line
				break
			}
		}
		if strings.Contains(title, "exit=0 signal=0]") {
			succeeded++
		} else if strings.Contains(title, "exit=") {
			failed++
		}
	}
	return succeeded, failed
}

func TestPublicProtocolsCancelAllTransfersAndCleanOwners(t *testing.T) {
	t.Run("rsync", func(t *testing.T) {
		runCancellationAcceptance(t, "rsync", func(fixture *hostNetworkFixture,
			scenario supervisorRootScenario) protocolLifecycleAcceptance {
			return newRsyncAcceptance(t, fixture, scenario, liveProtocolCancellation)
		})
	})
	t.Run("s3", func(t *testing.T) {
		runCancellationAcceptance(t, "s3", func(fixture *hostNetworkFixture,
			scenario supervisorRootScenario) protocolLifecycleAcceptance {
			return newS3Acceptance(t, fixture, scenario, liveProtocolCancellation)
		})
	})
}

func runCancellationAcceptance(t *testing.T, name string,
	create func(*hostNetworkFixture, supervisorRootScenario) protocolLifecycleAcceptance,
) {
	t.Helper()
	fixture := newHostNetworkFixture(t)
	fixture.install()
	baseline := fixture.hostSurface()
	runsBefore := authorityEntries(t)
	scenario := fixture.newRunSupervisorScenario(t, "public-"+name+"-cancel")
	acceptance := create(fixture, scenario)
	defer acceptance.release()
	facts := acceptance.facts()
	processMarker := name + "-cancel-" + filepath.Base(scenario.markers)
	t.Setenv("TRANSFERLANES_PROTOCOL_RUN", processMarker)
	output, err := runPublicTransferLanesPTY(t, facts.arguments,
		func(_ context.Context, command *exec.Cmd, _ *os.File, _ *boundedPTYCapture) error {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := acceptance.waitUntilActive(ctx); err != nil {
				return err
			}
			return command.Process.Signal(os.Interrupt)
		})
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 130 {
		t.Fatalf("cancel exit=%v output=%q", err, output)
	}
	if strings.Contains(finalInteractiveOutput(t, output), "SOURCE ") {
		t.Fatalf("%s cancellation retained source summary: %q", name, output)
	}
	// Resume server-side handlers after the public process has reaped its
	// clients so the real transports report cancellation rather than success.
	acceptance.release()
	acceptance.assertStopped(t)
	acceptance.assertCancellation(t)
	assertProtocolSources(t, facts.observed(), fixture.sources)
	assertNoMarkedProcesses(t, "TRANSFERLANES_PROTOCOL_RUN="+processMarker)
	fixture.assertSurface(baseline)
	if after := authorityEntries(t); !slices.Equal(after, runsBefore) {
		t.Fatalf("%s cancel left run roots: before=%v after=%v", name, runsBefore, after)
	}
}

func assertProtocolSources(t *testing.T, observations []netip.Addr, sources []netip.Addr) {
	t.Helper()
	observed := make(map[netip.Addr]bool, len(sources))
	for _, source := range observations {
		observed[source] = true
	}
	for _, source := range sources {
		if !observed[source] {
			t.Errorf("protocol endpoint did not observe selected source %s; observed=%v",
				source, observed)
		}
	}
}
