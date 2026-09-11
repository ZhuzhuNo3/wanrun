package throughput

import (
	"context"
	"net/netip"
	"testing"
	"time"
)

func TestRelativeWeightsCanIgnoreFailedTargets(t *testing.T) {
	first, _ := NewSourceBoundTarget(netip.MustParseAddr("192.0.2.1"))
	second, _ := NewSourceBoundTarget(netip.MustParseAddr("192.0.2.2"))
	third, _ := NewSourceBoundTarget(netip.MustParseAddr("192.0.2.3"))
	results := []Observation{{target: first, bytes: 2_000_000, duration: time.Second},
		failed(second, time.Second, ConnectEndpoint, context.DeadlineExceeded),
		{target: third, bytes: 1_000_000, duration: time.Second}}
	weights, err := RelativeWeights(results, true)
	if err != nil || len(weights) != 2 || weights[0].Target() != first || weights[0].Value() != 2 ||
		weights[1].Target() != third || weights[1].Value() != 1 {
		t.Fatalf("successful weights = %#v, %v", weights, err)
	}
	if _, err := RelativeWeights(results, false); err == nil {
		t.Fatal("complete-run weights accepted a failed target")
	}
}
