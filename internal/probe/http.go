package probe

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"time"
)

const httpConnectTimeout = 5 * time.Second

func newBoundTransport(localIP netip.Addr, resolver *net.Resolver) *http.Transport {
	dialer := &net.Dialer{Timeout: httpConnectTimeout, KeepAlive: 30 * time.Second,
		LocalAddr: &net.TCPAddr{IP: net.IP(localIP.AsSlice())}, Resolver: resolver}
	return &http.Transport{Proxy: nil, DialContext: dialer.DialContext,
		ForceAttemptHTTP2: true, TLSHandshakeTimeout: httpConnectTimeout}
}

func noRedirect(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }

func readBoundedBody(body io.ReadCloser, limit int64) ([]byte, error) {
	defer body.Close()
	content, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if int64(len(content)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return content, nil
}
