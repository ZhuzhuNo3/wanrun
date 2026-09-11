package throughput

import (
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

func assertFailureStage(t *testing.T, observation Observation, want FailureStage) {
	t.Helper()
	var failure *Failure
	if !errors.As(observation.Err(), &failure) || failure.Stage != want {
		t.Fatalf("failure = %v, want stage %s", observation.Err(), want)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func testResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)),
		Header: make(http.Header)}
}

func sampleTestStreams(roundTrip func(int, *http.Request) (*http.Response, error)) []*preparedStream {
	streams := make([]*preparedStream, len(uploadRequestBytes))
	for index := range streams {
		streamIndex := index
		streams[index] = &preparedStream{client: &http.Client{Transport: roundTripperFunc(
			func(request *http.Request) (*http.Response, error) { return roundTrip(streamIndex, request) })}}
	}
	return streams
}

func newTLSBlackhole(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var connectionsMu sync.Mutex
	var connections []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			connectionsMu.Lock()
			connections = append(connections, connection)
			connectionsMu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		connectionsMu.Lock()
		for _, connection := range connections {
			_ = connection.Close()
		}
		connectionsMu.Unlock()
		<-done
	})
	return "https://" + listener.Addr().String() + "/upload"
}

func transferNumber(t *testing.T, value int) transfernumber.Number {
	t.Helper()
	number, err := transfernumber.New(value)
	if err != nil {
		t.Fatal(err)
	}
	return number
}
