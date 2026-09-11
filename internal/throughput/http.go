package throughput

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

const (
	uploadChunkBytes           = 64 * 1024
	uploadReadBufferBytes      = 32 * 1024
	maximumUploadResponseBytes = 64 * 1024
)

var uploadRequestBytes = [...]int64{uploadChunkBytes, 256 * 1024, 512 * 1024, 1024 * 1024, 2 * 1024 * 1024, 4 * 1024 * 1024}

type Tester struct {
	resolver *net.Resolver
	connect  endpointConnector
	tlsRoots *x509.CertPool
}

type endpointConnector func(context.Context, Target, []netip.Addr, string) (net.Conn, error)

func New() *Tester { return &Tester{resolver: net.DefaultResolver, connect: dialAddresses} }

// Observe runs every target independently. Preparation is excluded from each
// target's equal-duration sample and a failed target never cancels siblings.
func (tester *Tester) Observe(ctx context.Context, targets []Target, settings Settings) []Observation {
	results := make([]Observation, len(targets))
	if tester == nil || tester.resolver == nil || tester.connect == nil || ctx == nil || len(targets) == 0 || !settings.Valid() {
		for index, target := range targets {
			results[index] = failed(target, settings.duration, ResolveEndpoint, errors.New("throughput inputs are invalid"))
		}
		return results
	}
	seen := make(map[Target]struct{}, len(targets))
	for _, target := range targets {
		if !target.valid() {
			for index, value := range targets {
				results[index] = failed(value, settings.duration, ResolveEndpoint, errors.New("throughput target is invalid"))
			}
			return results
		}
		if _, duplicate := seen[target]; duplicate {
			for index, value := range targets {
				results[index] = failed(value, settings.duration, ResolveEndpoint, errors.New("throughput target is repeated"))
			}
			return results
		}
		seen[target] = struct{}{}
	}
	var wait sync.WaitGroup
	wait.Add(len(targets))
	for index, target := range targets {
		go func() { defer wait.Done(); results[index] = tester.observeTarget(ctx, target, settings) }()
	}
	wait.Wait()
	return results
}

func (tester *Tester) observeTarget(parent context.Context, target Target, settings Settings) Observation {
	prepareCtx, cancel := context.WithTimeout(parent, settings.preparationTimeout)
	defer cancel()
	addresses, err := tester.resolveEndpoint(prepareCtx, target, settings.endpoint.Hostname())
	if err != nil {
		return failed(target, settings.duration, ResolveEndpoint, err)
	}
	streams := make([]*preparedStream, len(uploadRequestBytes))
	stage, err := prepareStreams(prepareCtx, target, settings, addresses, streams, tester.connect, tester.tlsRoots)
	if err != nil {
		closeStreams(streams)
		return failed(target, settings.duration, stage, err)
	}
	defer closeStreams(streams)
	return sampleStreams(parent, target, settings, streams)
}

func (tester *Tester) resolveEndpoint(ctx context.Context, target Target, hostname string) ([]netip.Addr, error) {
	if parsed, err := netip.ParseAddr(hostname); err == nil {
		if parsed.Is4() {
			return []netip.Addr{parsed}, nil
		}
		return nil, errors.New("measure endpoint did not resolve to IPv4")
	}
	addresses, err := tester.resolver.LookupNetIP(ctx, "ip4", hostname)
	if err != nil {
		return nil, err
	}
	result := make([]netip.Addr, 0, len(addresses))
	for _, address := range addresses {
		if address.Is4() {
			result = append(result, address)
		}
	}
	if len(result) == 0 {
		return nil, errors.New("measure endpoint did not resolve to IPv4")
	}
	return result, nil
}

func prepareStreams(ctx context.Context, target Target, settings Settings, addresses []netip.Addr,
	streams []*preparedStream, connect endpointConnector, tlsRoots *x509.CertPool) (FailureStage, error) {
	contextWithCause, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stages := make([]FailureStage, len(streams))
	errorsByStream := make([]error, len(streams))
	var wait sync.WaitGroup
	wait.Add(len(streams))
	for index := range streams {
		go func() {
			defer wait.Done()
			stream, stage, err := prepareStream(contextWithCause, target, settings, addresses, connect, tlsRoots)
			streams[index], stages[index], errorsByStream[index] = stream, stage, err
			if err != nil {
				cancel(&streamPreparationFailure{stage: stage, cause: err})
			}
		}()
	}
	wait.Wait()
	var preparationFailure *streamPreparationFailure
	if errors.As(context.Cause(contextWithCause), &preparationFailure) {
		return preparationFailure.stage, preparationFailure.cause
	}
	for index, err := range errorsByStream {
		if err != nil {
			return stages[index], err
		}
	}
	return 0, nil
}

type preparedStream struct {
	client       *http.Client
	transport    *http.Transport
	allowDial    atomic.Bool
	failureStage atomic.Uint32
}

type streamPreparationFailure struct {
	stage FailureStage
	cause error
}

func (failure *streamPreparationFailure) Error() string { return failure.cause.Error() }
func (failure *streamPreparationFailure) Unwrap() error { return failure.cause }

func prepareStream(ctx context.Context, target Target, settings Settings, addresses []netip.Addr,
	connect endpointConnector, tlsRoots *x509.CertPool) (*preparedStream, FailureStage, error) {
	stream := &preparedStream{}
	stream.allowDial.Store(true)
	stream.failureStage.Store(uint32(ConnectEndpoint))
	dial := func(ctx context.Context, network, _ string) (net.Conn, error) {
		if !stream.allowDial.Load() {
			return nil, errors.New("prepared throughput stream cannot reconnect during sampling")
		}
		connection, err := connect(ctx, target, addresses,
			endpointPort(settings.endpoint.Scheme, settings.endpoint.Port()))
		if err == nil && settings.endpoint.Scheme == "http" {
			stream.failureStage.Store(uint32(ValidateEndpoint))
		}
		return connection, err
	}
	transport := &http.Transport{Proxy: nil, DialContext: dial, ForceAttemptHTTP2: false,
		MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1, IdleConnTimeout: settings.preparationTimeout + settings.duration}
	if settings.endpoint.Scheme == "https" {
		transport.DialTLSContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			connection, err := dial(ctx, network, address)
			if err != nil {
				return nil, err
			}
			stream.failureStage.Store(uint32(NegotiateTLS))
			tlsConnection := tls.Client(connection, &tls.Config{ServerName: settings.endpoint.Hostname(),
				MinVersion: tls.VersionTLS12, RootCAs: tlsRoots})
			if err := tlsConnection.HandshakeContext(ctx); err != nil {
				_ = connection.Close()
				stream.failureStage.Store(uint32(NegotiateTLS))
				return nil, err
			}
			stream.failureStage.Store(uint32(ValidateEndpoint))
			return tlsConnection, nil
		}
	}
	stream.transport = transport
	stream.client = &http.Client{Transport: transport, CheckRedirect: rejectRedirect}
	_, complete, err := upload(ctx, stream.client, settings.Endpoint(), 1)
	if err != nil {
		stream.close()
		if stage := FailureStage(stream.failureStage.Load()); stage != 0 {
			return nil, stage, err
		}
		var sibling *streamPreparationFailure
		if errors.As(err, &sibling) {
			return nil, sibling.stage, sibling.cause
		}
		return nil, ValidateEndpoint, err
	}
	if !complete {
		stream.close()
		return nil, ValidateEndpoint, errors.New("measure endpoint did not consume validation upload")
	}
	stream.allowDial.Store(false)
	return stream, 0, nil
}

func dialAddresses(ctx context.Context, target Target, addresses []netip.Addr, rawPort string) (net.Conn, error) {
	var failures []error
	for _, address := range addresses {
		dialer := net.Dialer{KeepAlive: 30 * time.Second}
		if target.HasLocalIP() {
			dialer.LocalAddr = &net.TCPAddr{IP: net.IP(target.LocalIP().AsSlice())}
		}
		connection, err := dialer.DialContext(ctx, "tcp4", net.JoinHostPort(address.String(), rawPort))
		if err == nil {
			return connection, nil
		}
		failures = append(failures, err)
	}
	return nil, errors.Join(failures...)
}

func endpointPort(scheme, explicit string) string {
	if explicit != "" {
		return explicit
	}
	if scheme == "https" {
		return "443"
	}
	return "80"
}

func sampleStreams(parent context.Context, target Target, settings Settings, streams []*preparedStream) Observation {
	window, stopWindow := context.WithTimeout(parent, settings.duration)
	defer stopWindow()
	ctx, cancelSiblings := context.WithCancelCause(window)
	defer cancelSiblings(nil)
	completed := make([]uint64, len(streams))
	errorsByStream := make([]error, len(streams))
	var wait sync.WaitGroup
	wait.Add(len(streams))
	for index, stream := range streams {
		go func() {
			defer wait.Done()
			completed[index], errorsByStream[index] = sampleStream(ctx, stream.client, settings.Endpoint(), uploadRequestBytes[index])
			if err := errorsByStream[index]; err != nil &&
				!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				cancelSiblings(err)
			}
		}()
	}
	wait.Wait()
	if parent.Err() != nil {
		return failed(target, settings.duration, SampleUpload, parent.Err())
	}
	if cause := context.Cause(ctx); cause != nil &&
		!errors.Is(cause, context.Canceled) && !errors.Is(cause, context.DeadlineExceeded) {
		return failed(target, settings.duration, SampleUpload, cause)
	}
	var total uint64
	for index, err := range errorsByStream {
		if err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return failed(target, settings.duration, SampleUpload, err)
		}
		if total > math.MaxUint64-completed[index] {
			return failed(target, settings.duration, SampleUpload, errors.New("measured byte count overflows"))
		}
		total += completed[index]
	}
	if total == 0 {
		return failed(target, settings.duration, SampleUpload, errors.New("no upload completed during measure window"))
	}
	return Observation{target: target, bytes: total, duration: settings.duration}
}

func sampleStream(ctx context.Context, client *http.Client, endpoint string, size int64) (uint64, error) {
	var completed uint64
	for {
		consumed, complete, err := upload(ctx, client, endpoint, size)
		if err != nil {
			return completed, err
		}
		if !complete || consumed != size {
			return completed, errors.New("measure endpoint did not consume complete upload")
		}
		if ctx.Err() != nil {
			return completed, ctx.Err()
		}
		if completed > math.MaxUint64-uint64(consumed) {
			return 0, errors.New("measured byte count overflows")
		}
		completed += uint64(consumed)
	}
}

func upload(ctx context.Context, client *http.Client, endpoint string, size int64) (int64, bool, error) {
	body := &countingZeroReader{remaining: size}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return 0, false, err
	}
	request.ContentLength = size
	request.Header.Set("Content-Type", "application/octet-stream")
	response, err := client.Do(request)
	if err != nil {
		return body.consumed.Load(), false, err
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return body.consumed.Load(), false, fmt.Errorf("measure endpoint returned status %d", response.StatusCode)
	}
	count, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, maximumUploadResponseBytes+1))
	closeErr := response.Body.Close()
	if count > maximumUploadResponseBytes {
		return body.consumed.Load(), false, errors.New("measure response is too large")
	}
	if err := errors.Join(readErr, closeErr); err != nil {
		return body.consumed.Load(), false, err
	}
	return body.consumed.Load(), body.consumed.Load() == size, nil
}

type countingZeroReader struct {
	remaining int64
	consumed  atomic.Int64
}

func (body *countingZeroReader) Read(buffer []byte) (int, error) {
	if body.remaining == 0 {
		return 0, io.EOF
	}
	count := min(len(buffer), uploadReadBufferBytes, int(body.remaining))
	clear(buffer[:count])
	body.remaining -= int64(count)
	body.consumed.Add(int64(count))
	return count, nil
}
func (body *countingZeroReader) Close() error { return nil }
func (stream *preparedStream) close() {
	if stream != nil && stream.transport != nil {
		stream.transport.CloseIdleConnections()
	}
}
func closeStreams(streams []*preparedStream) {
	for _, stream := range streams {
		stream.close()
	}
}
func rejectRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
