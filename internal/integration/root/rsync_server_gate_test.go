//go:build linux && rootintegration

package root_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
)

const (
	rsyncGatePass      = byte('P')
	rsyncGateFail      = byte('F')
	rsyncGateCompleted = byte('S')
	rsyncGateCancelled = byte('C')
)

type rsyncProtocolGate struct {
	listener net.Listener
	failing  netip.Addr
	expected map[netip.Addr]struct{}

	mu          sync.Mutex
	active      int
	seen        map[netip.Addr]struct{}
	sourceCount map[netip.Addr]int
	completed   map[netip.Addr]struct{}
	cancelled   map[netip.Addr]struct{}
	connections map[net.Conn]struct{}
	work        sync.WaitGroup
	changed     chan struct{}
	ready       chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func startGatedSSHServer(t *testing.T, fixture *hostNetworkFixture,
	failing netip.Addr,
) (*sshServer, *rsyncProtocolGate) {
	t.Helper()
	gate := newRsyncProtocolGate(t, fixture.sources, failing)
	forceCommand := strings.Join([]string{
		strconv.Quote(os.Args[0]), transferChildArgument,
		transferChildRsyncServerGate, strconv.Quote(gate.listener.Addr().String()),
	}, " ")
	return startSSHServerWithForceCommand(t, fixture, forceCommand), gate
}

func newRsyncProtocolGate(t *testing.T, sources []netip.Addr,
	failing netip.Addr,
) *rsyncProtocolGate {
	t.Helper()
	socket := fmt.Sprintf("%s/gate.sock", t.TempDir())
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	gate := &rsyncProtocolGate{listener: listener, failing: failing,
		expected:    make(map[netip.Addr]struct{}, len(sources)),
		seen:        make(map[netip.Addr]struct{}, len(sources)),
		sourceCount: make(map[netip.Addr]int, len(sources)),
		completed:   make(map[netip.Addr]struct{}, len(sources)),
		cancelled:   make(map[netip.Addr]struct{}, len(sources)),
		connections: make(map[net.Conn]struct{}), changed: make(chan struct{}),
		ready: make(chan struct{}), release: make(chan struct{})}
	for _, source := range sources {
		gate.expected[source] = struct{}{}
	}
	gate.work.Add(1)
	go gate.accept()
	t.Cleanup(func() {
		gate.Release()
		_ = listener.Close()
		gate.mu.Lock()
		for connection := range gate.connections {
			_ = connection.Close()
		}
		gate.mu.Unlock()
		gate.work.Wait()
	})
	return gate
}

func (gate *rsyncProtocolGate) accept() {
	defer gate.work.Done()
	for {
		connection, err := gate.listener.Accept()
		if err != nil {
			return
		}
		gate.work.Add(1)
		go gate.serve(connection)
	}
}

func (gate *rsyncProtocolGate) serve(connection net.Conn) {
	defer gate.work.Done()
	registered := false
	gate.mu.Lock()
	gate.connections[connection] = struct{}{}
	gate.mu.Unlock()
	defer func() {
		_ = connection.Close()
		gate.mu.Lock()
		delete(gate.connections, connection)
		if registered {
			gate.active--
		}
		gate.signalChangedLocked()
		gate.mu.Unlock()
	}()

	reader := bufio.NewReader(connection)
	line, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	source, err := netip.ParseAddr(strings.TrimSpace(line))
	if err != nil || !gate.start(source) {
		return
	}
	registered = true
	result := make(chan byte, 1)
	go func() {
		value, readErr := reader.ReadByte()
		if readErr != nil {
			value = rsyncGateCancelled
		}
		result <- value
	}()
	select {
	case <-gate.ready:
	case value := <-result:
		gate.record(source, value)
		return
	}
	if source == gate.failing {
		if _, err := connection.Write([]byte{rsyncGateFail}); err != nil {
			gate.record(source, rsyncGateCancelled)
			return
		}
		gate.record(source, <-result)
		return
	}
	select {
	case <-gate.release:
		if _, err := connection.Write([]byte{rsyncGatePass}); err != nil {
			gate.record(source, rsyncGateCancelled)
			return
		}
		gate.record(source, <-result)
	case value := <-result:
		gate.record(source, value)
	}
}

func (gate *rsyncProtocolGate) start(source netip.Addr) bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if _, ok := gate.expected[source]; !ok {
		return false
	}
	gate.active++
	gate.seen[source] = struct{}{}
	gate.sourceCount[source]++
	if len(gate.seen) == len(gate.expected) {
		select {
		case <-gate.ready:
		default:
			close(gate.ready)
		}
	}
	gate.signalChangedLocked()
	return true
}

func (gate *rsyncProtocolGate) record(source netip.Addr, value byte) {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	switch value {
	case rsyncGateCompleted:
		gate.completed[source] = struct{}{}
	case rsyncGateCancelled:
		gate.cancelled[source] = struct{}{}
	}
	gate.signalChangedLocked()
}

func (gate *rsyncProtocolGate) signalChangedLocked() {
	close(gate.changed)
	gate.changed = make(chan struct{})
}

func (gate *rsyncProtocolGate) WaitUntilActive(ctx context.Context) error {
	select {
	case <-gate.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (gate *rsyncProtocolGate) Release() {
	gate.releaseOnce.Do(func() { close(gate.release) })
}

func (gate *rsyncProtocolGate) WaitUntilStopped(ctx context.Context) error {
	for {
		gate.mu.Lock()
		if gate.active == 0 && len(gate.seen) == len(gate.expected) {
			gate.mu.Unlock()
			return nil
		}
		changed := gate.changed
		gate.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (gate *rsyncProtocolGate) CompletedSources() []netip.Addr {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return addressSet(gate.completed)
}

func (gate *rsyncProtocolGate) CancelledSources() []netip.Addr {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return addressSet(gate.cancelled)
}

func (gate *rsyncProtocolGate) ConnectionCounts() map[netip.Addr]int {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	result := make(map[netip.Addr]int, len(gate.sourceCount))
	for source, count := range gate.sourceCount {
		result[source] = count
	}
	return result
}

func addressSet(values map[netip.Addr]struct{}) []netip.Addr {
	result := make([]netip.Addr, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	return result
}

func runRsyncServerGate(argv []string) int {
	if len(argv) < 1 || len(argv) > 2 {
		return 2
	}
	original := os.Getenv("SSH_ORIGINAL_COMMAND")
	connectionSource, ok := sshEnvironmentSource(os.Getenv("SSH_CONNECTION"))
	if original == "" || !ok {
		return 3
	}
	server := exec.Command("/bin/sh", "-c", original)
	if len(argv) == 2 {
		workingDirectory := filepath.Join(argv[1], connectionSource.String())
		if err := os.MkdirAll(workingDirectory, 0o700); err != nil {
			return 4
		}
		server.Dir = workingDirectory
	}
	server.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	serverInput, err := server.StdinPipe()
	if err != nil {
		return 4
	}
	serverOutput, err := server.StdoutPipe()
	if err != nil {
		return 4
	}
	server.Stderr = os.Stderr
	if err := server.Start(); err != nil {
		return 5
	}
	stop := func() error {
		_ = syscall.Kill(-server.Process.Pid, syscall.SIGKILL)
		return server.Wait()
	}
	gate, err := net.Dial("unix", argv[0])
	if err != nil {
		_ = stop()
		return 6
	}
	defer gate.Close()
	if _, err := fmt.Fprintln(gate, connectionSource); err != nil {
		_ = stop()
		return 6
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	instruction := make(chan byte, 1)
	go func() {
		value := make([]byte, 1)
		if _, readErr := io.ReadFull(gate, value); readErr != nil {
			value[0] = rsyncGateFail
		}
		instruction <- value[0]
	}()
	select {
	case value := <-instruction:
		if value == rsyncGatePass {
			return completeRsyncServer(gate, server, serverInput, serverOutput)
		}
		_ = stop()
		_, _ = gate.Write([]byte{rsyncGateFail})
		return 7
	case <-signals:
		_ = stop()
		_, _ = gate.Write([]byte{rsyncGateCancelled})
		return 8
	}
}

func completeRsyncServer(gate net.Conn, server *exec.Cmd, input io.WriteCloser,
	output io.Reader,
) int {
	go func() {
		_, _ = io.Copy(input, os.Stdin)
		_ = input.Close()
	}()
	_, copyErr := io.Copy(os.Stdout, output)
	waitErr := server.Wait()
	if copyErr != nil || waitErr != nil {
		_, _ = gate.Write([]byte{rsyncGateFail})
		return 9
	}
	_, _ = gate.Write([]byte{rsyncGateCompleted})
	return 0
}

func sshEnvironmentSource(value string) (netip.Addr, bool) {
	fields := strings.Fields(value)
	if len(fields) != 4 {
		return netip.Addr{}, false
	}
	source, err := netip.ParseAddr(fields[0])
	return source, err == nil
}
