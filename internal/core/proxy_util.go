package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ProbeTarget is the control endpoint the validator dials through each
// proxy to prove it can relay traffic.
const ProbeTarget = "1.1.1.1:80"

const (
	// DialTimeout bounds a single dial attempt to a proxy or origin.
	DialTimeout = 5 * time.Second
	// HeaderTimeout bounds the wait for response headers through a proxy.
	HeaderTimeout = 20 * time.Second
	// HealthTimeout bounds each connectivity probe during health checks.
	HealthTimeout = 3 * time.Second
	// ProviderFetchTimeout bounds provider list / API fetches.
	ProviderFetchTimeout = 30 * time.Second
	// HealthCheckConcurrency caps parallel health probes.
	HealthCheckConcurrency = 300
	// FailureTTL is how long a failed proxy stays deprioritized per host.
	FailureTTL = 10 * time.Minute
)

// ErrNoUpstreamProxy is returned when a request has no proxy candidates.
var ErrNoUpstreamProxy = errors.New("no upstream proxies available")

// NewProviderHTTPClient returns a client used to fetch proxy provider data.
func NewProviderHTTPClient() *http.Client {
	dialer := &net.Dialer{
		Timeout:   DialTimeout,
		KeepAlive: 30 * time.Second,
	}
	return &http.Client{
		Timeout:   ProviderFetchTimeout,
		Transport: NewUTLSTransport(dialer.DialContext),
	}
}

// ProbeProxy verifies a proxy end-to-end against the control target and
// returns the measured latency. A custom ProbeFunc takes precedence (for
// tests and special providers); otherwise the proxy's own DialContext (or a
// plain HTTP CONNECT for URL-based proxies) is used, and the tunnel is
// exercised with a real HTTP request to prove it actually relays traffic
// rather than merely accepting TCP connections.
func ProbeProxy(ctx context.Context, ps *ProxyState) (time.Duration, bool) {
	if ps == nil {
		return 0, false
	}

	if ps.ProbeFunc != nil {
		lat, err := ps.ProbeFunc(ctx)
		if err != nil {
			return 0, false
		}
		return lat, true
	}

	start := time.Now()
	conn, err := dialProxy(ctx, ps, ProbeTarget)
	if err != nil {
		return 0, false
	}
	defer conn.Close()

	// Exercise the tunnel with a minimal GET through it. Any HTTP status
	// line proves the proxy relays origin traffic.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(HealthTimeout)
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, false
	}
	host, _, err := net.SplitHostPort(ProbeTarget)
	if err != nil {
		host = ProbeTarget
	}
	req := fmt.Sprintf("GET /cdn-cgi/trace HTTP/1.1\r\nHost: %s\r\nUser-Agent: unroxy-probe\r\nConnection: close\r\n\r\n", host)
	if _, err := conn.Write([]byte(req)); err != nil {
		return 0, false
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		return 0, false
	}
	if !bytes.Contains(buf[:n], []byte("HTTP/")) {
		return 0, false
	}

	// Relay works. Resolve the real egress identity once so responses can
	// carry x-unroxy-ip / x-unroxy-isp. Best-effort: a proxy that relays
	// but whose egress lookup fails still graduates, and the lookup is
	// retried on the next re-probe until both fields are populated.
	egCtx, cancel := context.WithTimeout(context.Background(), egressTimeout)
	defer cancel()

	if ps.IP == "" {
		if e, err := resolveEgress(egCtx, ps); err == nil && e.IP != "" {
			ps.IP = e.IP
			ps.ISP = e.ISP
			// The provider-declared country can be wrong (shared infra,
			// stale lists). Prefer the observed egress country so the proxy
			// lands in the pool that matches where it actually exits.
			if e.Country != "" {
				ps.Country = e.Country
				ps.CountryVerified = true
			}
		}
	}
	if ps.IP != "" {
		isp, cc := whoisForIP(egCtx, ps.IP)
		if ps.ISP == "" {
			ps.ISP = isp
		}
		// Retry country verification if the first egress resolution did not
		// observe it, so a mislabeled proxy is eventually re-routed to its
		// actual region instead of staying wrong forever.
		if !ps.CountryVerified && cc != "" {
			ps.Country = cc
			ps.CountryVerified = true
		}
	}

	return time.Since(start), true
}
