package transferstart

import (
	"encoding/hex"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/ZhuzhuNo3/transferlanes/internal/throughput"
)

func TestV5CanonicalModeRecordsDoNotEncodeDerivedTransferNumbers(t *testing.T) {
	settings, err := throughput.NewSettings("https://m.example/u", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		request Request
		want    string
	}{
		{name: "manual", request: mustManualRequest(t, Pipes(), true),
			want: "5754525105060000000000000000000000000201000300022f73000000093139322e302e322e31000000000000000300093139322e302e322e3200000000000000020007312e312e312e3100016300027b7d000278ff"},
		{name: "automatic", request: mustAutomaticRequest(t, Pipes(), settings),
			want: "57545251050300000000000000003b9aca000200000200022f73001368747470733a2f2f6d2e6578616d706c652f7500093139322e302e322e3100093139322e302e322e3200016300027b7d"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := Encode(test.request)
			if err != nil {
				t.Fatal(err)
			}
			if got := hex.EncodeToString(payload); got != test.want {
				t.Fatalf("canonical payload=%s", got)
			}
		})
	}
}

func TestV5RoundTripPreservesManualAutomaticPipesPTYAndOpaqueArgv(t *testing.T) {
	settings, err := throughput.NewSettings("https://m.example/u", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pty, err := NewPTY(100, 30)
	if err != nil {
		t.Fatal(err)
	}
	requests := []Request{
		mustManualRequest(t, Pipes(), true),
		mustManualRequest(t, pty, false),
		mustAutomaticRequest(t, Pipes(), settings),
		mustAutomaticRequest(t, pty, settings),
	}
	for _, request := range requests {
		payload, err := Encode(request)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := Decode(payload)
		if err != nil {
			t.Fatal(err)
		}
		if decoded.SourceLabel() != request.SourceLabel() || decoded.AutomaticWeight() != request.AutomaticWeight() ||
			decoded.FollowSymlinks() != request.FollowSymlinks() || decoded.CommandIO() != request.CommandIO() ||
			!slices.Equal(decoded.ManualNetworks(), request.ManualNetworks()) ||
			!slices.Equal(decoded.AutomaticNetworks(), request.AutomaticNetworks()) ||
			!slices.Equal(decoded.DNS(), request.DNS()) || !slices.Equal(decoded.ChildArgv(), request.ChildArgv()) {
			t.Fatalf("round trip mismatch: got=%#v want=%#v", decoded, request)
		}
	}
}

func TestDecodeRejectsV4UnknownFlagsTruncationTrailingCountsAndContradictoryIO(t *testing.T) {
	payload, err := Encode(mustManualRequest(t, Pipes(), false))
	if err != nil {
		t.Fatal(err)
	}
	mutations := map[string][]byte{
		"truncated":     append([]byte(nil), payload[:len(payload)-1]...),
		"trailing":      append(append([]byte(nil), payload...), 0),
		"bad magic":     append([]byte("BAD!"), payload[4:]...),
		"old V4":        mutateByte(payload, 4, 4),
		"unknown flags": mutateByte(payload, 5, payload[5]|0x80),
		"zero networks": mutateByte(payload, 18, 0),
		"zero argv":     mutateByte(payload, 21, 0),
		"pipes with PTY": mutateByte(
			mutateByte(payload, 6, 1), 7, 1),
	}
	for name, mutation := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(mutation); err == nil {
				t.Fatal("invalid payload decoded")
			}
		})
	}
}

func TestEncodedSizeMatchesCanonicalEncoding(t *testing.T) {
	settings, err := throughput.NewSettings("https://m.example/u", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	pty, err := NewPTY(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	for _, request := range []Request{
		mustManualRequest(t, Pipes(), false),
		mustManualRequest(t, pty, true),
		mustAutomaticRequest(t, Pipes(), settings),
		mustAutomaticRequest(t, pty, settings),
	} {
		size, err := EncodedSize(request)
		if err != nil {
			t.Fatal(err)
		}
		payload, err := Encode(request)
		if err != nil {
			t.Fatal(err)
		}
		if size != len(payload) {
			t.Fatalf("EncodedSize=%d Encode=%d", size, len(payload))
		}
	}
}

func mustManualRequest(t *testing.T, commandIO CommandIO, follow bool) Request {
	t.Helper()
	first, err := NewManualNetwork(netip.MustParseAddr("192.0.2.1"), 3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewManualNetwork(netip.MustParseAddr("192.0.2.2"), 2)
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewManualRequest("/s", []ManualNetwork{first, second},
		[]netip.Addr{netip.MustParseAddr("1.1.1.1")}, []string{"c", "{}", string([]byte{'x', 0xff})},
		commandIO, follow)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func mustAutomaticRequest(t *testing.T, commandIO CommandIO, settings throughput.Settings) Request {
	t.Helper()
	request, err := NewAutomaticRequest("/s",
		[]netip.Addr{netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("192.0.2.2")}, settings,
		nil, []string{"c", "{}"}, commandIO, false)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func mutateByte(payload []byte, offset int, value byte) []byte {
	result := append([]byte(nil), payload...)
	result[offset] = value
	return result
}
