//go:build linux && rootintegration && protocolacceptance

package root_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vishvananda/netns"
)

const (
	s3AccessKey = "transferlanes-acceptance"
	s3SecretKey = "transferlanes-acceptance-secret"
)

type s3Server struct {
	apiEndpoint    string
	apiAddress     string
	consoleAddress string
	endpoint       string
	proxyAddress   string
	namespace      string
	bucket         string
	command        *exec.Cmd
	wait           *processWait
	proxy          *http.Server
	proxyWait      chan error
	observed       receiverSourceLog
	writes         s3WriteLog
	gate           *s3RequestGate
	output         bytes.Buffer
	outputMu       sync.Mutex
	stopOnce       sync.Once
	stopErr        error
}

type s3Write struct {
	source netip.Addr
	path   string
}

type s3WriteLog struct {
	mu     sync.Mutex
	writes []s3Write
}

func (log *s3WriteLog) record(write s3Write) {
	log.mu.Lock()
	log.writes = append(log.writes, write)
	log.mu.Unlock()
}

func (log *s3WriteLog) snapshot() []s3Write {
	log.mu.Lock()
	defer log.mu.Unlock()
	return slices.Clone(log.writes)
}

func startS3Server(t *testing.T, fixture *hostNetworkFixture) *s3Server {
	return startS3ServerWithGate(t, fixture, nil)
}

func startGatedS3Server(t *testing.T, fixture *hostNetworkFixture,
	failing netip.Addr,
) *s3Server {
	t.Helper()
	return startS3ServerWithGate(t, fixture, newS3RequestGate(fixture.sources, failing))
}

func startS3ServerWithGate(t *testing.T, fixture *hostNetworkFixture,
	gate *s3RequestGate,
) *s3Server {
	t.Helper()
	for _, executable := range []string{"aws", "minio"} {
		if _, err := exec.LookPath(executable); err != nil {
			t.Fatalf("default protocol dependency %s: %v", executable, err)
		}
	}
	apiAddress := net.JoinHostPort(fixture.sources[0].String(), "19000")
	consoleAddress := net.JoinHostPort(fixture.sources[0].String(), "19001")
	proxyAddress := net.JoinHostPort(fixture.remote.String(), "19000")
	server := &s3Server{apiEndpoint: "http://" + apiAddress, apiAddress: apiAddress,
		consoleAddress: consoleAddress, endpoint: "http://" + proxyAddress,
		proxyAddress: proxyAddress, namespace: fixture.router, bucket: "transferlanes-acceptance",
		gate: gate}
	if err := server.requireListenersAvailable(); err != nil {
		t.Fatalf("S3 fixture listeners are unavailable before start: %v", err)
	}
	data := filepath.Join(t.TempDir(), "data")
	server.command = exec.Command("minio", "server", "--quiet", "--address",
		apiAddress, "--console-address", consoleAddress, data)
	server.command.Env = append(os.Environ(),
		"MINIO_ROOT_USER="+s3AccessKey, "MINIO_ROOT_PASSWORD="+s3SecretKey)
	server.command.Stdout = lockedWriter{buffer: &server.output, mutex: &server.outputMu}
	server.command.Stderr = lockedWriter{buffer: &server.output, mutex: &server.outputMu}
	if err := server.command.Start(); err != nil {
		t.Fatal(err)
	}
	server.wait = waitForProcess(server.command)
	t.Cleanup(func() {
		if err := server.stop(); err != nil {
			t.Errorf("stop S3 fixture: %v", err)
		} else {
			t.Logf("released MinIO API %s, MinIO console %s, and S3 proxy %s listeners",
				server.apiAddress, server.consoleAddress, server.proxyAddress)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := waitForS3Ready(ctx, server.apiEndpoint, server.wait, server.command.Process.Pid,
		[]string{server.apiAddress, server.consoleAddress}); err != nil {
		stopErr := server.stop()
		t.Fatalf("start MinIO: %v\nstop: %v\n%s", err, stopErr, server.logs())
	}
	t.Logf("MinIO pid %d owns API %s and console %s listeners",
		server.command.Process.Pid, server.apiAddress, server.consoleAddress)
	server.startProxy(t)
	server.runAWS(t, "s3api", "create-bucket", "--bucket", server.bucket)
	return server
}

func (server *s3Server) requireListenersAvailable() error {
	return errors.Join(
		server.requireMinIOListenersAvailable(),
		requireAvailableListener("S3 proxy", func() (net.Listener, error) {
			return listenTCPInNamespace(server.namespace, server.proxyAddress)
		}),
	)
}

func (server *s3Server) requireMinIOListenersAvailable() error {
	return errors.Join(
		requireAvailableListener("MinIO API", func() (net.Listener, error) {
			return net.Listen("tcp4", server.apiAddress)
		}),
		requireAvailableListener("MinIO console", func() (net.Listener, error) {
			return net.Listen("tcp4", server.consoleAddress)
		}),
	)
}

func (server *s3Server) startProxy(t *testing.T) {
	t.Helper()
	target, err := url.Parse(server.apiEndpoint)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := listenTCPInNamespace(server.namespace, server.proxyAddress)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	server.proxy = &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		host, _, splitErr := net.SplitHostPort(request.RemoteAddr)
		address, parseErr := netip.ParseAddr(host)
		if splitErr == nil && parseErr == nil {
			server.observed.record(address)
			if request.Method == http.MethodPut {
				if server.gate != nil {
					decision := server.gate.await(request.Context(), address)
					defer server.gate.finished(address, decision, request.Context())
					switch decision {
					case s3GateFail:
						writeS3AccessDenied(writer)
						return
					case s3GateCancelled:
						return
					}
				}
			}
		}
		status := &responseStatus{ResponseWriter: writer}
		proxy.ServeHTTP(status, request)
		if request.Method == http.MethodPut && status.code >= http.StatusOK &&
			status.code < http.StatusMultipleChoices {
			server.writes.record(s3Write{source: address, path: request.URL.Path})
			if server.gate != nil {
				server.gate.completed(address)
			}
		}
	})}
	server.proxyWait = make(chan error, 1)
	go func() {
		err := server.proxy.Serve(listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		server.proxyWait <- err
	}()
}

func (server *s3Server) stop() error {
	server.stopOnce.Do(func() {
		server.stopErr = errors.Join(server.stopProxy(), server.stopMinIO())
	})
	return server.stopErr
}

func (server *s3Server) stopProxy() error {
	if server.proxy == nil {
		return nil
	}
	closeErr := server.proxy.Close()
	waitErr := waitForServerExit(server.proxyWait, 5*time.Second, "S3 proxy")
	releaseErr := waitForNamedListenerRelease("S3 proxy", 5*time.Second, func() (net.Listener, error) {
		return listenTCPInNamespace(server.namespace, server.proxyAddress)
	})
	return errors.Join(closeErr, waitErr, releaseErr)
}

func (server *s3Server) stopMinIO() error {
	if server.command == nil || server.command.Process == nil || server.wait == nil {
		return nil
	}
	return errors.Join(server.stopMinIOProcess(), server.waitForMinIOListenersReleased())
}

func (server *s3Server) stopMinIOProcess() error {
	if server.wait.exited() {
		return fmt.Errorf("MinIO exited before fixture stop: %w", server.wait.result())
	}
	if err := server.command.Process.Signal(os.Interrupt); err != nil {
		return fmt.Errorf("interrupt MinIO: %w", err)
	}
	if waitForProcessExit(server.wait, 5*time.Second) {
		return nil
	}
	killErr := server.command.Process.Kill()
	killed := waitForProcessExit(server.wait, 5*time.Second)
	if !killed {
		return errors.Join(errors.New("MinIO did not exit after kill"), killErr)
	}
	return errors.Join(errors.New("MinIO required kill after interrupt"), killErr)
}

func (server *s3Server) waitForMinIOListenersReleased() error {
	return errors.Join(
		waitForNamedListenerRelease("MinIO API", 5*time.Second, func() (net.Listener, error) {
			return net.Listen("tcp4", server.apiAddress)
		}),
		waitForNamedListenerRelease("MinIO console", 5*time.Second, func() (net.Listener, error) {
			return net.Listen("tcp4", server.consoleAddress)
		}),
	)
}

func (server *s3Server) runAWS(t *testing.T, arguments ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	commandArguments := append([]string{"--endpoint-url", server.apiEndpoint}, arguments...)
	command := exec.CommandContext(ctx, "aws", commandArguments...)
	command.Env = append(os.Environ(), awsEnvironment()...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("aws %q: %v: %s\nMinIO:\n%s", arguments, err, output, server.logs())
	}
	return string(output)
}

func (server *s3Server) logs() string {
	server.outputMu.Lock()
	defer server.outputMu.Unlock()
	return server.output.String()
}

func awsEnvironment() []string {
	return []string{"AWS_ACCESS_KEY_ID=" + s3AccessKey, "AWS_SECRET_ACCESS_KEY=" + s3SecretKey,
		"AWS_DEFAULT_REGION=us-east-1", "AWS_EC2_METADATA_DISABLED=true",
		"AWS_MAX_ATTEMPTS=1", "AWS_RETRY_MODE=standard", "NO_PROXY=*", "no_proxy=*"}
}

type responseStatus struct {
	http.ResponseWriter
	code int
}

func (writer *responseStatus) WriteHeader(code int) {
	writer.code = code
	writer.ResponseWriter.WriteHeader(code)
}

func (writer *responseStatus) Write(content []byte) (int, error) {
	if writer.code == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(content)
}

func (writer *responseStatus) Unwrap() http.ResponseWriter { return writer.ResponseWriter }

func writeS3AccessDenied(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "application/xml")
	writer.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(writer,
		`<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>selected test source rejected</Message></Error>`)
}

func waitForS3Ready(ctx context.Context, endpoint string, process *processWait, pid int,
	requiredListeners []string,
) error {
	if len(requiredListeners) == 0 {
		return errors.New("MinIO readiness requires at least one listener")
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	client := &http.Client{Timeout: time.Second}
	var missingListeners []string
	for {
		if process.exited() {
			return fmt.Errorf("MinIO exited before readiness: %w", process.result())
		}
		request, _ := http.NewRequestWithContext(ctx, http.MethodGet,
			endpoint+"/minio/health/ready", nil)
		response, err := client.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				var ownershipErr error
				missingListeners, ownershipErr = missingProcessTCPListeners(pid, requiredListeners)
				if ownershipErr != nil {
					return fmt.Errorf("verify MinIO listeners: %w", ownershipErr)
				}
				if len(missingListeners) == 0 && !process.exited() {
					return nil
				}
			}
		}
		select {
		case <-process.done:
			return fmt.Errorf("MinIO exited before readiness: %w", process.result())
		case <-ctx.Done():
			if len(missingListeners) > 0 {
				return fmt.Errorf("MinIO does not own required listeners %s: %w",
					strings.Join(missingListeners, ", "), ctx.Err())
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func requireAvailableListener(name string, open func() (net.Listener, error)) error {
	listener, err := open()
	if err != nil {
		return fmt.Errorf("%s listener: %w", name, err)
	}
	if err := listener.Close(); err != nil {
		return fmt.Errorf("close %s availability listener: %w", name, err)
	}
	return nil
}

func missingProcessTCPListeners(pid int, addresses []string) ([]string, error) {
	missing := make([]string, 0, len(addresses))
	for _, address := range addresses {
		owned, err := processOwnsTCPListener(pid, address)
		if err != nil {
			return nil, fmt.Errorf("listener %s: %w", address, err)
		}
		if !owned {
			missing = append(missing, address)
		}
	}
	return missing, nil
}

func processOwnsTCPListener(pid int, address string) (bool, error) {
	inodes, err := processSocketInodes(pid)
	if err != nil {
		return false, err
	}
	want, err := procTCPAddress(address)
	if err != nil {
		return false, err
	}
	contents, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/tcp", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 9 && fields[1] == want && fields[3] == "0A" {
			if _, owned := inodes[fields[9]]; owned {
				return true, nil
			}
		}
	}
	return false, nil
}

func processSocketInodes(pid int) (map[string]struct{}, error) {
	directory := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(directory)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]struct{}{}, nil
		}
		return nil, err
	}
	result := make(map[string]struct{})
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(directory, entry.Name()))
		if err != nil {
			continue
		}
		if strings.HasPrefix(target, "socket:[") && strings.HasSuffix(target, "]") {
			result[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = struct{}{}
		}
	}
	return result, nil
}

func procTCPAddress(address string) (string, error) {
	value, err := netip.ParseAddrPort(address)
	if err != nil {
		return "", fmt.Errorf("parse IPv4 listener %q: %w", address, err)
	}
	if !value.Addr().Is4() {
		return "", fmt.Errorf("listener %q is not IPv4", address)
	}
	bytes := value.Addr().As4()
	return fmt.Sprintf("%02X%02X%02X%02X:%04X", bytes[3], bytes[2], bytes[1], bytes[0],
		value.Port()), nil
}

func waitForProcessExit(process *processWait, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-process.done:
		return true
	case <-timer.C:
		return false
	}
}

func waitForServerExit(done <-chan error, timeout time.Duration, name string) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s serve: %w", name, err)
		}
		return nil
	case <-timer.C:
		return fmt.Errorf("%s did not stop within %s", name, timeout)
	}
}

func waitForListenerRelease(timeout time.Duration, open func() (net.Listener, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		listener, err := open()
		if err == nil {
			return listener.Close()
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("listener was not released within %s: %w", timeout, err)
		}
		delay := 20 * time.Millisecond
		if remaining < delay {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		<-timer.C
	}
}

func waitForNamedListenerRelease(name string, timeout time.Duration,
	open func() (net.Listener, error),
) error {
	if err := waitForListenerRelease(timeout, open); err != nil {
		return fmt.Errorf("%s listener: %w", name, err)
	}
	return nil
}

type processWait struct {
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func waitForProcess(command *exec.Cmd) *processWait {
	wait := &processWait{done: make(chan struct{})}
	go func() {
		err := command.Wait()
		wait.mu.Lock()
		wait.err = err
		wait.mu.Unlock()
		close(wait.done)
	}()
	return wait
}

func (wait *processWait) result() error {
	wait.mu.Lock()
	defer wait.mu.Unlock()
	return wait.err
}

func (wait *processWait) exited() bool {
	select {
	case <-wait.done:
		return true
	default:
		return false
	}
}

func listenTCPInNamespace(namespace, address string) (net.Listener, error) {
	result := make(chan struct {
		listener net.Listener
		err      error
	}, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		original, err := netns.Get()
		if err != nil {
			result <- struct {
				listener net.Listener
				err      error
			}{err: err}
			return
		}
		defer original.Close()
		target, err := netns.GetFromName(namespace)
		if err == nil {
			defer target.Close()
			err = netns.Set(target)
		}
		var listener net.Listener
		if err == nil {
			listener, err = net.Listen("tcp4", address)
		}
		restoreErr := netns.Set(original)
		joinedErr := errors.Join(err, restoreErr)
		if joinedErr != nil && listener != nil {
			joinedErr = errors.Join(joinedErr, listener.Close())
			listener = nil
		}
		result <- struct {
			listener net.Listener
			err      error
		}{listener: listener, err: joinedErr}
	}()
	value := <-result
	return value.listener, value.err
}

type lockedWriter struct {
	buffer *bytes.Buffer
	mutex  *sync.Mutex
}

func (writer lockedWriter) Write(content []byte) (int, error) {
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.buffer.Write(content)
}
