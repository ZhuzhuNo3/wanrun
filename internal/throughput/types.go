package throughput

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ZhuzhuNo3/transferlanes/internal/transfernumber"
)

const (
	DefaultEndpointURL        = "https://speed.cloudflare.com/__up"
	minimumDuration           = 100 * time.Millisecond
	maximumDuration           = 60 * time.Second
	defaultPreparationTimeout = 10 * time.Second
	maximumEndpointBytes      = 2048
)

type Settings struct {
	endpoint           url.URL
	duration           time.Duration
	preparationTimeout time.Duration
}

func NewSettings(rawURL string, duration time.Duration) (Settings, error) {
	if len(rawURL) == 0 || len(rawURL) > maximumEndpointBytes || !utf8.ValidString(rawURL) || strings.IndexByte(rawURL, 0) >= 0 {
		return Settings{}, errors.New("measure endpoint length or encoding is invalid")
	}
	endpoint, err := url.Parse(rawURL)
	if err != nil {
		return Settings{}, fmt.Errorf("parse measure endpoint: %w", err)
	}
	if endpoint.Scheme != "http" && endpoint.Scheme != "https" || endpoint.Host == "" ||
		endpoint.User != nil || endpoint.Fragment != "" || endpoint.RawFragment != "" {
		return Settings{}, errors.New("measure endpoint requires HTTP, a host, and no userinfo or fragment")
	}
	if duration < minimumDuration || duration > maximumDuration {
		return Settings{}, fmt.Errorf("measure duration %s is outside %s..%s", duration, minimumDuration, maximumDuration)
	}
	return Settings{endpoint: *endpoint, duration: duration, preparationTimeout: defaultPreparationTimeout}, nil
}

func DefaultSettings(duration time.Duration) (Settings, error) {
	return NewSettings(DefaultEndpointURL, duration)
}
func (settings Settings) Valid() bool {
	return settings.endpoint.Host != "" && settings.duration >= minimumDuration && settings.duration <= maximumDuration && settings.preparationTimeout > 0
}
func (settings Settings) Endpoint() string        { copyURL := settings.endpoint; return copyURL.String() }
func (settings Settings) Duration() time.Duration { return settings.duration }

type Target struct {
	localIP  netip.Addr
	transfer transfernumber.Number
}

func NewSourceBoundTarget(localIP netip.Addr) (Target, error) {
	if !localIP.Is4() {
		return Target{}, errors.New("source-bound throughput target is invalid")
	}
	return Target{localIP: localIP}, nil
}

func NewCurrentNamespaceTarget(id transfernumber.Number) (Target, error) {
	if id.Value() == 0 {
		return Target{}, errors.New("namespace throughput target is invalid")
	}
	return Target{transfer: id}, nil
}

func (target Target) LocalIP() netip.Addr             { return target.localIP }
func (target Target) HasLocalIP() bool                { return target.localIP.Is4() }
func (target Target) Transfer() transfernumber.Number { return target.transfer }
func (target Target) HasTransfer() bool               { return target.transfer.Value() != 0 }
func (target Target) valid() bool                     { return target.localIP.Is4() != (target.transfer.Value() != 0) }

type FailureStage uint8

const (
	ResolveEndpoint FailureStage = iota + 1
	ConnectEndpoint
	NegotiateTLS
	ValidateEndpoint
	SampleUpload
)

func (stage FailureStage) String() string {
	switch stage {
	case ResolveEndpoint:
		return "resolve endpoint"
	case ConnectEndpoint:
		return "connect endpoint"
	case NegotiateTLS:
		return "negotiate TLS"
	case ValidateEndpoint:
		return "validate endpoint"
	case SampleUpload:
		return "sample upload"
	default:
		return "unknown throughput stage"
	}
}

type Failure struct {
	Stage FailureStage
	Cause error
}

func (failure *Failure) Error() string { return fmt.Sprintf("%s: %v", failure.Stage, failure.Cause) }
func (failure *Failure) Unwrap() error { return failure.Cause }

type Observation struct {
	target   Target
	bytes    uint64
	duration time.Duration
	err      error
}

func failed(target Target, duration time.Duration, stage FailureStage, err error) Observation {
	return Observation{target: target, duration: duration, err: &Failure{Stage: stage, Cause: err}}
}

func (observation Observation) Target() Target          { return observation.target }
func (observation Observation) Bytes() uint64           { return observation.bytes }
func (observation Observation) Duration() time.Duration { return observation.duration }
func (observation Observation) Err() error              { return observation.err }
func (observation Observation) Mbps() float64 {
	if observation.err != nil || observation.bytes == 0 || observation.duration <= 0 {
		return 0
	}
	return float64(observation.bytes) * 8 / observation.duration.Seconds() / 1_000_000
}

type Weight struct {
	target Target
	value  uint64
}

func (weight Weight) Target() Target { return weight.target }
func (weight Weight) Value() uint64  { return weight.value }
