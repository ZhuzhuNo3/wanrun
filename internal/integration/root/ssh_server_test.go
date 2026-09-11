//go:build linux && rootintegration

package root_test

import (
	"bufio"
	"bytes"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type sshServer struct {
	command  *exec.Cmd
	logs     bytes.Buffer
	logMu    sync.Mutex
	observed receiverSourceLog
}

func startSSHServer(t *testing.T, fixture *hostNetworkFixture) *sshServer {
	return startSSHServerWithForceCommand(t, fixture, "")
}

func startSSHServerWithForceCommand(t *testing.T, fixture *hostNetworkFixture,
	forceCommand string,
) *sshServer {
	t.Helper()
	for _, executable := range []string{"ssh", "sshd", "ssh-keygen", "rsync"} {
		if _, err := exec.LookPath(executable); err != nil {
			t.Fatalf("default rsync/SSH dependency %s: %v", executable, err)
		}
	}
	root := t.TempDir()
	hostKey := filepath.Join(root, "host-key")
	clientKey := filepath.Join(root, "client-key")
	fixture.run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", hostKey)
	fixture.run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", clientKey)
	configurationLines := []string{
		"Port 2222",
		"ListenAddress " + fixture.remote.String(),
		"HostKey " + hostKey,
		"AuthorizedKeysFile " + clientKey + ".pub",
		"PidFile " + filepath.Join(root, "sshd.pid"),
		"PermitRootLogin prohibit-password",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"UsePAM no",
		"StrictModes no",
		"LogLevel VERBOSE",
	}
	if forceCommand != "" {
		configurationLines = append(configurationLines, "ForceCommand "+forceCommand)
	}
	configuration := strings.Join(configurationLines, "\n") + "\n"
	configPath := filepath.Join(root, "sshd_config")
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("ip", "netns", "exec", fixture.router,
		"/usr/sbin/sshd", "-D", "-e", "-f", configPath)
	stderr, err := command.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	server := &sshServer{command: command}
	ready := make(chan struct{})
	readDone := make(chan struct{})
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	go func() {
		defer close(readDone)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			server.logMu.Lock()
			server.logs.WriteString(line)
			server.logs.WriteByte('\n')
			server.logMu.Unlock()
			if strings.Contains(line, "Server listening") {
				select {
				case <-ready:
				default:
					close(ready)
				}
			}
			if source, ok := sshConnectionSource(line); ok {
				server.observed.record(source)
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		_ = command.Process.Kill()
		_ = command.Wait()
		<-readDone
		t.Fatalf("sshd did not become ready: %s", server.output())
	}
	t.Cleanup(func() {
		_ = command.Process.Signal(os.Interrupt)
		_ = command.Wait()
		<-readDone
	})
	return server
}

func sshConnectionSource(line string) (netip.Addr, bool) {
	const prefix = "Connection from "
	start := strings.Index(line, prefix)
	if start < 0 {
		return netip.Addr{}, false
	}
	value := line[start+len(prefix):]
	host, _, found := strings.Cut(value, " port ")
	if !found {
		return netip.Addr{}, false
	}
	address, err := netip.ParseAddr(host)
	return address, err == nil
}

func (server *sshServer) clientShell() string {
	return fmt.Sprintf("/usr/bin/ssh -i %s -p 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR",
		filepath.Join(filepath.Dir(server.command.Args[len(server.command.Args)-1]), "client-key"))
}

func (server *sshServer) output() string {
	server.logMu.Lock()
	defer server.logMu.Unlock()
	return server.logs.String()
}
