package throughput

import (
	"testing"
	"time"
)

func TestEndpointPortUsesSchemeDefault(t *testing.T) {
	if got := endpointPort("http", ""); got != "80" {
		t.Fatalf("HTTP default port = %s", got)
	}
	if got := endpointPort("https", ""); got != "443" {
		t.Fatalf("HTTPS default port = %s", got)
	}
	if got := endpointPort("https", "8443"); got != "8443" {
		t.Fatalf("explicit port = %s", got)
	}
}

func TestSettingsRejectInvalidEndpointOrDuration(t *testing.T) {
	for _, input := range []struct {
		endpoint string
		duration time.Duration
	}{
		{endpoint: "", duration: minimumDuration},
		{endpoint: "ftp://measure.example/upload", duration: minimumDuration},
		{endpoint: "http:///upload", duration: minimumDuration},
		{endpoint: "http://user@measure.example/upload", duration: minimumDuration},
		{endpoint: "http://measure.example/upload#fragment", duration: minimumDuration},
		{endpoint: "http://measure.example/upload", duration: minimumDuration - time.Nanosecond},
		{endpoint: "http://measure.example/upload", duration: maximumDuration + time.Nanosecond},
	} {
		if _, err := NewSettings(input.endpoint, input.duration); err == nil {
			t.Fatalf("invalid settings accepted: endpoint=%q duration=%v", input.endpoint, input.duration)
		}
	}
}
