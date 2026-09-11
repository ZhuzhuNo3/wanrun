package throughput

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSamplingDoesNotReplaceClosedPreparedConnections(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		if request.ContentLength == 1 {
			writer.Header().Set("Connection", "close")
		}
		_, _ = io.WriteString(writer, "ok")
	}))
	defer server.Close()
	target, _ := NewSourceBoundTarget(netip.MustParseAddr("127.0.0.1"))
	settings, _ := NewSettings(server.URL, minimumDuration)
	result := New().Observe(context.Background(), []Target{target}, settings)[0]
	var failure *Failure
	if !errors.As(result.Err(), &failure) || failure.Stage != SampleUpload ||
		!strings.Contains(failure.Cause.Error(), "cannot reconnect") {
		t.Fatalf("closed prepared connection result = %#v, %v", result, result.Err())
	}
}

func TestSamplingFailureCancelsSiblingStreamsAndDiscardsBytes(t *testing.T) {
	completed := make(chan struct{})
	streams := make([]*preparedStream, len(uploadRequestBytes))
	for index := range streams {
		streamIndex := index
		calls := &atomic.Int32{}
		streams[index] = &preparedStream{client: &http.Client{Transport: roundTripperFunc(
			func(request *http.Request) (*http.Response, error) {
				switch streamIndex {
				case 0:
					if calls.Add(1) == 1 {
						_, _ = io.Copy(io.Discard, request.Body)
						close(completed)
						return testResponse(http.StatusOK, "ok"), nil
					}
					<-request.Context().Done()
					return nil, request.Context().Err()
				case 1:
					<-completed
					return testResponse(http.StatusServiceUnavailable, "controlled"), nil
				default:
					<-request.Context().Done()
					return nil, request.Context().Err()
				}
			})}}
	}
	target, _ := NewCurrentNamespaceTarget(transferNumber(t, 1))
	settings, _ := NewSettings("https://measure.example/upload", time.Second)
	started := time.Now()
	result := sampleStreams(context.Background(), target, settings, streams)
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("sibling streams were not cancelled promptly: %v", elapsed)
	}
	if result.Bytes() != 0 || result.Mbps() != 0 ||
		result.Err() == nil || !strings.Contains(result.Err().Error(), "status 503") {
		t.Fatalf("failed sample retained evidence or lost cause: %#v, %v", result, result.Err())
	}
}

func TestSampleUploadCountsOnlyCompleteAcknowledgementsInsideWindow(t *testing.T) {
	target, _ := NewCurrentNamespaceTarget(transferNumber(t, 1))
	settings, _ := NewSettings("https://measure.example/upload", minimumDuration)
	t.Run("complete acknowledgements", func(t *testing.T) {
		calls := make([]atomic.Int32, len(uploadRequestBytes))
		streams := sampleTestStreams(func(index int, request *http.Request) (*http.Response, error) {
			if calls[index].Add(1) == 1 {
				_, _ = io.Copy(io.Discard, request.Body)
				return testResponse(http.StatusOK, "ok"), nil
			}
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		result := sampleStreams(context.Background(), target, settings, streams)
		var want uint64
		for _, size := range uploadRequestBytes {
			want += uint64(size)
		}
		if result.Err() != nil || result.Bytes() != want {
			t.Fatalf("complete result=%#v error=%v want=%d", result, result.Err(), want)
		}
	})
	t.Run("low speed remains usable", func(t *testing.T) {
		var first atomic.Bool
		streams := sampleTestStreams(func(index int, request *http.Request) (*http.Response, error) {
			if index == 0 && first.CompareAndSwap(false, true) {
				_, _ = io.Copy(io.Discard, request.Body)
				return testResponse(http.StatusOK, "ok"), nil
			}
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		result := sampleStreams(context.Background(), target, settings, streams)
		if result.Err() != nil || result.Bytes() != uint64(uploadRequestBytes[0]) || result.Mbps() <= 0 {
			t.Fatalf("low-speed result=%#v error=%v", result, result.Err())
		}
	})
	t.Run("oversized response", func(t *testing.T) {
		streams := sampleTestStreams(func(_ int, request *http.Request) (*http.Response, error) {
			_, _ = io.Copy(io.Discard, request.Body)
			return testResponse(http.StatusOK, strings.Repeat("x", maximumUploadResponseBytes+1)), nil
		})
		result := sampleStreams(context.Background(), target, settings, streams)
		if result.Err() == nil || result.Bytes() != 0 || !strings.Contains(result.Err().Error(), "too large") {
			t.Fatalf("oversized response result=%#v error=%v", result, result.Err())
		}
	})
	t.Run("redirect", func(t *testing.T) {
		streams := sampleTestStreams(func(_ int, _ *http.Request) (*http.Response, error) {
			return testResponse(http.StatusFound, "redirect"), nil
		})
		result := sampleStreams(context.Background(), target, settings, streams)
		if result.Err() == nil || result.Bytes() != 0 || !strings.Contains(result.Err().Error(), "status 302") {
			t.Fatalf("redirect result=%#v error=%v", result, result.Err())
		}
	})
	t.Run("early response", func(t *testing.T) {
		streams := sampleTestStreams(func(_ int, request *http.Request) (*http.Response, error) {
			buffer := make([]byte, 1024)
			_, _ = request.Body.Read(buffer)
			return testResponse(http.StatusOK, "ok"), nil
		})
		result := sampleStreams(context.Background(), target, settings, streams)
		if result.Err() == nil || result.Bytes() != 0 || !strings.Contains(result.Err().Error(), "complete upload") {
			t.Fatalf("early response result=%#v error=%v", result, result.Err())
		}
	})
	t.Run("completion after window", func(t *testing.T) {
		streams := sampleTestStreams(func(_ int, request *http.Request) (*http.Response, error) {
			_, _ = io.Copy(io.Discard, request.Body)
			time.Sleep(minimumDuration + 25*time.Millisecond)
			return testResponse(http.StatusOK, "ok"), nil
		})
		result := sampleStreams(context.Background(), target, settings, streams)
		if result.Err() == nil || result.Bytes() != 0 {
			t.Fatalf("late completion result=%#v error=%v", result, result.Err())
		}
	})
	t.Run("parent cancellation", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		streams := sampleTestStreams(func(_ int, request *http.Request) (*http.Response, error) {
			<-request.Context().Done()
			return nil, request.Context().Err()
		})
		result := sampleStreams(ctx, target, settings, streams)
		if result.Err() == nil || result.Bytes() != 0 || !errors.Is(result.Err(), context.Canceled) {
			t.Fatalf("cancelled result=%#v error=%v", result, result.Err())
		}
	})
}
