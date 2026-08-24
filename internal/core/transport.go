package core

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type RotatingProxyTransport struct {
	logger         *log.Logger
	pool           *ProxyPool
	transport      http.RoundTripper
	dialTransports sync.Map
	warpTransport  *http.Transport

	warpEgressOnce sync.Once
	warpIP         string
	warpISP        string
}

func NewRotatingProxyTransport(pool *ProxyPool) *RotatingProxyTransport {
	logger := log.Default()
	if pool != nil && pool.logger != nil {
		logger = pool.logger
	}

	return &RotatingProxyTransport{
		logger:    logger,
		pool:      pool,
		transport: newProxyAwareTransport(),
	}
}

func (t *RotatingProxyTransport) SetWarpTransport(tr *http.Transport) {
	t.warpTransport = tr
}

func (t *RotatingProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.warpTransport != nil {
		resp, err := t.warpTransport.RoundTrip(req)
		if err == nil {
			t.setWarpEgressHeaders(resp)
		}
		return resp, err
	}

	body, hasBody, err := snapshotRequestBody(req)
	if err != nil {
		return nil, err
	}

	targetHost := requestTargetHost(req)
	return t.roundTripViaProxy(req, body, hasBody, targetHost)
}

func (t *RotatingProxyTransport) roundTripViaProxy(req *http.Request, body []byte, hasBody bool, targetHost string) (*http.Response, error) {
	if t.pool == nil {
		return nil, ErrNoUpstreamProxy
	}

	logger := t.transportLogger()
	targetLog := requestTargetLog(req)

	now := time.Now()
	candidates := t.pool.Candidates(now, targetHost)
	if len(candidates) == 0 {
		return nil, ErrNoUpstreamProxy
	}

	var lastErr error
	for _, candidate := range candidates {
		attemptReq := cloneRequestForProxy(req, candidate.URL, body, hasBody)
		// The candidate owns its dial (socks, psiphon, authed CONNECT): let
		// its DialContext establish the raw connection instead of the URL.
		if candidate.DialContext != nil {
			attemptReq = attemptReq.WithContext(context.WithValue(attemptReq.Context(), proxyDialerKey{}, true))
		}
		var resp *http.Response
		var err error

		if candidate.DialContext != nil {
			if isTunnelCandidate(candidate) {
				// Tunnel candidates (psiphon, tor) carry their own dialer;
				// a plain transport dials the real target through it. The
				// uTLS variant injects candidate.URL as an HTTP proxy which
				// raw tunnels cannot speak.
				v, _ := t.dialTransports.LoadOrStore("tunnel:"+candidate.Key, &http.Transport{
					DialContext:           candidate.DialContext,
					ForceAttemptHTTP2:     false,
					MaxIdleConns:          10,
					IdleConnTimeout:       90 * time.Second,
					TLSHandshakeTimeout:   10 * time.Second,
					ResponseHeaderTimeout: HeaderTimeout,
				})
				resp, err = v.(*http.Transport).RoundTrip(attemptReq)
			} else {
				v, _ := t.dialTransports.LoadOrStore(candidate.Key, NewUTLSTransport(candidate.DialContext))
				resp, err = v.(*http.Transport).RoundTrip(attemptReq)
			}
		} else {
			resp, err = t.transport.RoundTrip(attemptReq)
		}

		var ti *tunnelInfo
		if isPsiphonCandidate(candidate) {
			ti = TunnelInfoForHost(targetHost)
		}

		proto := candidateProtoPrefix(ti)
		if err != nil {
			if errors.Is(err, errPsiphonNotReady) {
				continue
			}
			if req.Context().Err() != nil {
				lastErr = err
				break
			}
			if isHostUnreachable(err) {
				if !isPsiphonCandidate(candidate) {
					t.pool.MarkFailure(candidate.Key, targetHost)
				}
				logger.Printf("[ERR]%s %s -> %s (%v)", proto, targetLog, candidateLogAddress(candidate, ti), err)
				lastErr = err
				break
			}
			if isPsiphonCandidate(candidate) {
				logger.Printf("[ERR]%s %s -> %s (%v)", proto, targetLog, candidateLogAddress(candidate, ti), err)
				lastErr = err
				continue
			}
			t.pool.MarkFailure(candidate.Key, targetHost)
			logger.Printf("[ERR]%s %s -> %s (%v)", proto, targetLog, candidateLogAddress(candidate, ti), err)
			lastErr = err
			continue
		}

		if shouldRetryStatus(resp.StatusCode) {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if !isPsiphonCandidate(candidate) {
				t.pool.MarkFailure(candidate.Key, targetHost)
			}
			logger.Printf("[RETRY]%s %s -> %s (%d)", proto, targetLog, candidateLogAddress(candidate, ti), resp.StatusCode)
			lastErr = fmt.Errorf("origin returned retriable status %d via %s", resp.StatusCode, candidate.Key)
			continue
		}

		t.pool.MarkSuccess(candidate.Key, targetHost)
		logger.Printf("[OK]%s %s -> %s (%d)", proto, targetLog, candidateLogAddress(candidate, ti), resp.StatusCode)
		t.setEgressHeaders(resp, candidate, targetHost)
		return resp, nil
	}

	if lastErr == nil {
		lastErr = ErrNoUpstreamProxy
	}

	return nil, lastErr
}

func (t *RotatingProxyTransport) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.dialContext(ctx, network, addr, false)
}

// DialContextStrict is like DialContext but never falls back to a direct
// connection. Explicit region requests use it so an empty or exhausted pool
// returns an error instead of leaking the server's own egress IP.
func (t *RotatingProxyTransport) DialContextStrict(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.dialContext(ctx, network, addr, true)
}

func (t *RotatingProxyTransport) dialContext(ctx context.Context, network, addr string, strict bool) (net.Conn, error) {
	if t.warpTransport != nil && t.warpTransport.DialContext != nil {
		return t.warpTransport.DialContext(ctx, network, addr)
	}

	conn, ok := t.dialThroughPool(ctx, network, addr)
	if ok {
		return conn, nil
	}

	if strict {
		return nil, ErrNoUpstreamProxy
	}
	t.transportLogger().Printf("[DIRECT] CONNECT %s (no proxy)", addr)
	return (&net.Dialer{Timeout: DialTimeout}).DialContext(ctx, network, addr)
}

// dialThroughPool tries every candidate in the pool and returns the first
// successful connection. The boolean is false when no candidate succeeded.
func (t *RotatingProxyTransport) dialThroughPool(ctx context.Context, network, addr string) (net.Conn, bool) {
	targetHost := extractHost(addr)
	logger := t.transportLogger()

	candidates := t.pool.Candidates(time.Now(), targetHost)
	for _, candidate := range candidates {
		var conn net.Conn
		var err error
		if candidate.DialContext != nil {
			conn, err = candidate.DialContext(ctx, network, addr)
		} else {
			conn, err = httpProxyConnect(ctx, candidate.URL, addr)
		}

		var ti *tunnelInfo
		if isPsiphonCandidate(candidate) {
			host, _, _ := net.SplitHostPort(addr)
			ti = TunnelInfoForHost(host)
		}

		proto := candidateProtoPrefix(ti)
		if err != nil {
			if errors.Is(err, errPsiphonNotReady) {
				continue
			}
			if ctx.Err() != nil {
				break
			}
			if isHostUnreachable(err) {
				if !isPsiphonCandidate(candidate) {
					t.pool.MarkFailure(candidate.Key, targetHost)
				}
				logger.Printf("[ERR]%s CONNECT %s -> %s (%v)", proto, addr, candidateLogAddress(candidate, ti), err)
				break
			}
			if isPsiphonCandidate(candidate) {
				logger.Printf("[ERR]%s CONNECT %s -> %s (%v)", proto, addr, candidateLogAddress(candidate, ti), err)
				continue
			}
			t.pool.MarkFailure(candidate.Key, targetHost)
			logger.Printf("[ERR]%s CONNECT %s -> %s (%v)", proto, addr, candidateLogAddress(candidate, ti), err)
			continue
		}

		t.pool.MarkSuccess(candidate.Key, targetHost)
		logger.Printf("[OK]%s CONNECT %s -> %s", proto, addr, candidateLogAddress(candidate, ti))
		return conn, true
	}

	return nil, false
}

func (t *RotatingProxyTransport) transportLogger() *log.Logger {
	logger := t.logger
	if logger == nil {
		logger = log.Default()
		if t.pool != nil && t.pool.logger != nil {
			logger = t.pool.logger
		}
	}

	return logger
}

func candidateLogAddress(c ProxyCandidate, ti *tunnelInfo) string {
	if isPsiphonCandidate(c) && c.Psiphon != nil {
		if ti != nil && ti.ip != "" {
			return fmt.Sprintf("%s (%s)", ti.ip, ti.region)
		}
		return "tunnel"
	}

	host := c.URL.Hostname()
	if host == "" {
		host = c.URL.Host
	}

	if c.Country != "" {
		return fmt.Sprintf("%s (%s)", host, c.Country)
	}

	return host
}

func candidateProtoPrefix(ti *tunnelInfo) string {
	if ti != nil && ti.protocol != "" {
		return "[TUN]"
	}
	return ""
}

// setEgressHeaders writes the externally observed exit identity onto an HTTP
// response. For psiphon candidates the egress IP is read from the per-host
// tunnel map (populated by the dialer); other candidates carry the IP/ISP
// resolved during validation.
func (t *RotatingProxyTransport) setEgressHeaders(resp *http.Response, c ProxyCandidate, targetHost string) {
	if resp == nil {
		return
	}
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}

	ip, isp := c.IP, c.ISP
	if isPsiphonCandidate(c) {
		if ti := TunnelInfoForHost(targetHost); ti != nil && ti.ip != "" {
			ip = ti.ip
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			isp = ispForIP(ctx, ti.ip)
			cancel()
		}
	}

	if ip != "" {
		resp.Header.Set("x-unroxy-ip", ip)
	}
	if isp != "" {
		resp.Header.Set("x-unroxy-isp", isp)
	}
}

func (t *RotatingProxyTransport) setWarpEgressHeaders(resp *http.Response) {
	if resp == nil {
		return
	}
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}

	ip, isp := t.warpEgress()
	if ip != "" {
		resp.Header.Set("x-unroxy-ip", ip)
	}
	if isp != "" {
		resp.Header.Set("x-unroxy-isp", isp)
	}
}

// warpEgress resolves the WARP exit identity once per transport, lazily on
// first use, by issuing a real HTTPS request through the WARP dialer.
func (t *RotatingProxyTransport) warpEgress() (string, string) {
	t.warpEgressOnce.Do(func() {
		if t.warpTransport == nil || t.warpTransport.DialContext == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), egressTimeout)
		defer cancel()
		if e, err := EgressViaDial(ctx, t.warpTransport.DialContext, egressTimeout); err == nil {
			t.warpIP = e.IP
			t.warpISP = e.ISP
		}
	})
	return t.warpIP, t.warpISP
}

type proxyContextKey struct{}

type proxyDialerKey struct{}

func newProxyAwareTransport() http.RoundTripper {
	dialer := &net.Dialer{
		Timeout:   DialTimeout,
		KeepAlive: 30 * time.Second,
	}

	return NewUTLSTransport(dialer.DialContext)
}

func snapshotRequestBody(req *http.Request) ([]byte, bool, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, false, nil
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, true, err
	}
	if err := req.Body.Close(); err != nil {
		return nil, true, err
	}

	setRequestBody(req, body)
	return body, true, nil
}

// setRequestBody restores a buffered body onto req so it can be replayed
// across retry attempts.
func setRequestBody(req *http.Request, body []byte) {
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	req.ContentLength = int64(len(body))
}

func cloneRequestForProxy(req *http.Request, proxyURL *url.URL, body []byte, hasBody bool) *http.Request {
	ctx := req.Context()
	if proxyURL != nil {
		ctx = context.WithValue(ctx, proxyContextKey{}, proxyURL)
	}

	attemptReq := req.Clone(ctx)

	if hasBody {
		setRequestBody(attemptReq, body)
	} else {
		attemptReq.Body = nil
		attemptReq.GetBody = nil
	}

	return attemptReq
}

func requestTargetHost(req *http.Request) string {
	if req == nil || req.URL == nil {
		return ""
	}

	host := strings.ToLower(req.URL.Hostname())
	if host != "" {
		return host
	}

	return strings.ToLower(req.URL.Host)
}

func requestTargetLog(req *http.Request) string {
	if req == nil || req.URL == nil {
		return "-"
	}

	host := req.URL.Host
	if hostname := req.URL.Hostname(); hostname != "" {
		host = hostname
	}
	if host == "" {
		host = "-"
	}

	path := req.URL.EscapedPath()
	if path == "" || path == "/" {
		path = ""
	}
	if req.URL.RawQuery != "" {
		path += "?" + req.URL.RawQuery
	}

	return strings.ToLower(host) + path
}

func extractHost(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return strings.ToLower(host)
}

func shouldRetryStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests
}

func isHostUnreachable(err error) bool {
	return strings.Contains(err.Error(), "host unreachable")
}

func isPsiphonCandidate(c ProxyCandidate) bool {
	return c.URL != nil && c.URL.Scheme == "psiphon"
}

// isTunnelCandidate reports whether the candidate is a raw tunnel dialer
// (embedded protocol) rather than an HTTP/SOCKS proxy endpoint.
func isTunnelCandidate(c ProxyCandidate) bool {
	return c.URL != nil && (c.URL.Scheme == "psiphon" || c.URL.Scheme == "tor")
}

func httpProxyConnect(ctx context.Context, proxyURL *url.URL, target string) (net.Conn, error) {
	d := &net.Dialer{Timeout: DialTimeout}
	conn, err := d.DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return nil, err
	}

	if proxyURL.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         proxyURL.Hostname(),
			InsecureSkipVerify: true, // provider certs are frequently stale
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("proxy tls handshake: %w", err)
		}
		conn = tlsConn
	}

	req := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if proxyURL.User != nil {
		user := proxyURL.User.Username()
		pass, _ := proxyURL.User.Password()
		req += fmt.Sprintf("Proxy-Authorization: Basic %s\r\n",
			base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	}
	req += "\r\n"

	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		return nil, err
	}
	resp := buf[:n]
	if !bytes.Contains(resp, []byte("200")) {
		conn.Close()
		firstLine, _, _ := strings.Cut(string(resp), "\r\n")
		return nil, fmt.Errorf("proxy rejected CONNECT: %s", firstLine)
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

// HTTPProxyConnect dials a target through an HTTP(S) proxy using CONNECT,
// honoring basic auth from the URL userinfo. Exported for providers whose
// credentials rotate (Turbo static creds, Urban per-server tokens).
func HTTPProxyConnect(ctx context.Context, proxyURL *url.URL, target string) (net.Conn, error) {
	return httpProxyConnect(ctx, proxyURL, target)
}
