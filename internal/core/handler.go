package core

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/idna"
	"unroxy/internal/core/rewriter"
)

type ProxyHandler struct {
	logger    *log.Logger
	transport http.RoundTripper
	router    *PoolRouter
	tcpProxy  string
}

func NewProxyHandler(logger *log.Logger, router *PoolRouter, tcpProxy string) *ProxyHandler {
	if logger == nil {
		logger = log.Default()
	}

	h := &ProxyHandler{
		logger: logger,
		router: router,
	}
	if router != nil {
		h.transport = router.Default()
	}

	h.tcpProxy = tcpProxy

	if h.transport != nil {
		h.transport = NewCFRetryTransport(h.transport, h.logger)
	}

	return h
}

func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodConnect:
		h.handleConnectTunnel(w, r)
	case r.URL.Host != "":
		h.handleForwardProxy(w, r)
	default:
		h.handleRewriteProxy(w, r)
	}
}

// errUnknownRegion is returned when a request authenticates with a region
// username that does not map to any configured pool. It must never silently
// fall back to the default (mixed-region) pool, otherwise a request like
// `id@host` would return an arbitrary non-Indonesia exit.
var errUnknownRegion = errors.New("unknown proxy region")

func (h *ProxyHandler) resolveTransport(r *http.Request) (http.RoundTripper, error) {
	username := AuthUsername(r)
	if username != "" && h.router != nil {
		if transport := h.router.Select(username); transport != nil {
			return NewCFRetryTransport(transport, h.logger), nil
		}
		return nil, errUnknownRegion
	}
	return h.transport, nil
}

func (h *ProxyHandler) handleRewriteProxy(w http.ResponseWriter, r *http.Request) {
	poolName, domain, path, query := h.parsePoolRequest(r)
	if domain == "" {
		h.writeIndexPage(w, r)
		return
	}

	if !isResolvable(domain) {
		http.NotFound(w, r)
		return
	}

	transport := h.transport
	if poolName != "" && h.router != nil {
		if t := h.router.Select(poolName); t != nil {
			transport = NewCFRetryTransport(t, h.logger)
		}
	}

	proxyBase := ""
	if poolName != "" {
		proxyBase = "/" + strings.ToLower(poolName)
	}
	proxy := h.createProxy("https", domain, path, query, transport, proxyBase)
	proxy.ServeHTTP(w, r)
}

func (h *ProxyHandler) writeIndexPage(w http.ResponseWriter, r *http.Request) {
	var buf strings.Builder

	buf.WriteString("Usage\n")
	buf.WriteString("─────\n")
	fmt.Fprintf(&buf, "  Rewrite   /ipwho.is/path\n")
	fmt.Fprintf(&buf, "            curl http://%s/ipwho.is\n", r.Host)
	fmt.Fprintf(&buf, "\n  Proxy     curl -x http://%s http://ipwho.is\n", r.Host)
	if h.tcpProxy != "" {
		fmt.Fprintf(&buf, "            curl -x http://%s https://ipwho.is\n", h.tcpProxy)
	}
	fmt.Fprintf(&buf, "\n  Region    curl http://%s/us/ipwho.is\n", r.Host)
	if h.tcpProxy != "" {
		fmt.Fprintf(&buf, "            curl -x http://us@%s https://ipwho.is\n", h.tcpProxy)
	}
	fmt.Fprintf(&buf, "\n  WARP      curl http://%s/warp/ipwho.is\n", r.Host)
	if h.tcpProxy != "" {
		fmt.Fprintf(&buf, "            curl -x http://warp@%s https://ipwho.is\n", h.tcpProxy)
	}

	if h.router != nil {
		stats := h.router.Stats()
		if len(stats.Pools) > 0 {
			buf.WriteString("\nPools\n")
			buf.WriteString("─────\n")
			const colWidth = 12
			for i, p := range stats.Pools {
				entry := fmt.Sprintf("%s(%d)", p.Name, p.ProxyCount)
				if i%5 == 0 {
					fmt.Fprintf(&buf, "  %-*s", -colWidth, entry)
				} else {
					fmt.Fprintf(&buf, "%-*s", colWidth, entry)
				}
				if i%5 == 4 || i == len(stats.Pools)-1 {
					buf.WriteString("\n")
				}
			}
			fmt.Fprintf(&buf, "\nTotal: %d proxies\n", stats.TotalProxies)
		}
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(buf.String()))
}

func (h *ProxyHandler) handleForwardProxy(w http.ResponseWriter, r *http.Request) {
	scheme := r.URL.Scheme
	if scheme != "http" && scheme != "https" {
		http.Error(w, "Unsupported scheme", http.StatusBadRequest)
		return
	}

	domain := r.URL.Host
	path := r.URL.Path
	if path == "" {
		path = "/"
	}

	transport, err := h.resolveTransport(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	proxy := h.createProxy(scheme, domain, path, r.URL.RawQuery, transport, "")
	proxy.ServeHTTP(w, r)
}

func (h *ProxyHandler) handleConnectTunnel(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	if target == "" {
		target = r.URL.Host
	}
	if target == "" {
		http.Error(w, "Missing target host", http.StatusBadRequest)
		return
	}

	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "443")
	}

	transport, err := h.resolveTransport(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	rt, ok := transport.(*RotatingProxyTransport)
	if !ok {
		if cr, isCF := transport.(*CFRetryTransport); isCF {
			rt, ok = cr.base.(*RotatingProxyTransport)
		}
	}
	if !ok || rt == nil {
		http.Error(w, "Transport not available", http.StatusInternalServerError)
		return
	}

	var targetConn net.Conn
	if AuthUsername(r) != "" {
		targetConn, err = rt.DialContextStrict(r.Context(), "tcp", target)
	} else {
		targetConn, err = rt.DialContext(r.Context(), "tcp", target)
	}
	if err != nil {
		h.logger.Printf("[ERR] CONNECT %s: %v", target, err)
		http.Error(w, "Failed to connect to target", http.StatusServiceUnavailable)
		return
	}
	defer targetConn.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}

	clientConn, bufReader, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer clientConn.Close()

	if _, err := clientConn.Write([]byte("HTTP/1.1 200 OK\r\n\r\n")); err != nil {
		h.logger.Printf("[ERR] CONNECT %s: failed to send 200: %v", target, err)
		return
	}

	h.logger.Printf("[OK] CONNECT tunnel %s established", target)

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if n := bufReader.Reader.Buffered(); n > 0 {
			io.CopyN(targetConn, bufReader, int64(n))
		}
		io.Copy(targetConn, clientConn)
		targetConn.Close()
	}()

	go func() {
		defer wg.Done()
		io.Copy(clientConn, targetConn)
		clientConn.Close()
	}()

	wg.Wait()
}

func (h *ProxyHandler) parsePoolRequest(r *http.Request) (pool, domain, path, query string) {
	fullPath := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(fullPath, "/", 3)

	if len(parts) < 1 || parts[0] == "" {
		return "", "", "", ""
	}

	first := parts[0]

	if h.router != nil && !strings.Contains(first, ".") && h.router.Has(strings.ToUpper(first)) {
		pool = strings.ToUpper(first)
		if len(parts) > 1 {
			second := parts[1]
			compoundKey := strings.ToUpper(first + "/" + second)
			if !strings.Contains(second, ".") && h.router.Has(compoundKey) {
				pool = compoundKey
				if len(parts) > 2 {
					dp := strings.SplitN(parts[2], "/", 2)
					domain = dp[0]
					path = "/"
					if len(dp) > 1 {
						path = "/" + dp[1]
					}
				}
			} else {
				domain = second
				path = "/"
				if len(parts) > 2 {
					path = "/" + strings.Join(parts[2:], "/")
				}
			}
		}
	} else {
		domain = first
		path = "/"
		if len(parts) > 1 {
			path = "/" + strings.Join(parts[1:], "/")
		}
	}

	if domain != "" && !isValidDomain(domain) {
		return "", "", "", ""
	}

	query = r.URL.RawQuery
	return pool, domain, path, query
}

func isValidDomain(s string) bool {
	ascii, err := idna.ToASCII(s)
	if err != nil || ascii == "" {
		return false
	}
	parts := strings.Split(ascii, ".")
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if len(part) == 0 {
			return false
		}
	}
	return true
}

type dnsEntry struct {
	ok bool
	at time.Time
}

// resolvCache caches LookupHost results for the rewrite path. isResolvable
// previously did one blocking DNS lookup per request (2s timeout) — at high
// QPS that doubles as a per-request latency tax and resolver load spike.
// Positive results are cached 60s, negatives 10s so a transient failure is
// retried quickly. The map is capped and old entries purged on insert.
var resolvCache = struct {
	mu      sync.Mutex
	entries map[string]dnsEntry
	max     int
	ttlOK   time.Duration
	ttlFail time.Duration
}{
	entries: make(map[string]dnsEntry, 1024),
	max:     4096,
	ttlOK:   60 * time.Second,
	ttlFail: 10 * time.Second,
}

const dnsLookupTimeout = 2 * time.Second

func isResolvable(domain string) bool {
	resolvCache.mu.Lock()
	if e, ok := resolvCache.entries[domain]; ok {
		ttl := resolvCache.ttlOK
		if !e.ok {
			ttl = resolvCache.ttlFail
		}
		if time.Since(e.at) < ttl {
			resolvCache.mu.Unlock()
			return e.ok
		}
		delete(resolvCache.entries, domain)
	}
	resolvCache.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), dnsLookupTimeout)
	defer cancel()
	_, err := net.DefaultResolver.LookupHost(ctx, domain)
	resolved := err == nil

	resolvCache.mu.Lock()
	if len(resolvCache.entries) >= resolvCache.max {
		now := time.Now()
		for k, e := range resolvCache.entries {
			ttl := resolvCache.ttlOK
			if !e.ok {
				ttl = resolvCache.ttlFail
			}
			if now.Sub(e.at) >= ttl {
				delete(resolvCache.entries, k)
			}
		}
		if len(resolvCache.entries) >= resolvCache.max {
			clear(resolvCache.entries)
		}
	}
	resolvCache.entries[domain] = dnsEntry{ok: resolved, at: time.Now()}
	resolvCache.mu.Unlock()
	return resolved
}

func (h *ProxyHandler) createProxy(scheme, domain, path, query string, transport http.RoundTripper, proxyBase string) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		ErrorLog:  log.New(io.Discard, "", 0),
		Transport: transport,
		Director: func(req *http.Request) {
			req.URL.Scheme = scheme
			req.URL.Host = domain
			req.URL.Path = path
			req.URL.RawQuery = query

			rewriter.RewriteRequestHeaders(req, domain)
		},
	}

	// Rewrite mode (proxyBase != "") rewrites the response so all URLs route
	// through the proxy base; the forward proxy passes responses through.
	if proxyBase != "" {
		rp.ModifyResponse = func(resp *http.Response) error {
			resp.Header.Set("Cache-Control", "no-store")
			resp.Header.Set("Pragma", "no-cache")
			resp.Header.Set("Expires", "0")

			rewriter.RewriteHeaders(resp, domain, proxyBase)

			contentType := resp.Header.Get("Content-Type")
			needsRewrite := strings.Contains(contentType, "text/html") ||
				strings.Contains(contentType, "text/css") ||
				strings.Contains(contentType, "javascript")

			if !needsRewrite {
				return nil
			}

			body, err := h.readResponseBody(resp)
			if err != nil {
				return err
			}

			var newBody []byte
			switch {
			case strings.Contains(contentType, "text/html"):
				newBody = rewriter.RewriteHTML(body, domain, proxyBase)
			case strings.Contains(contentType, "text/css"):
				newBody = rewriter.RewriteCSS(body, domain, proxyBase)
			case strings.Contains(contentType, "javascript"):
				newBody = rewriter.RewriteJS(body, domain, proxyBase)
			default:
				newBody = body
			}

			resp.Body = io.NopCloser(bytes.NewReader(newBody))
			resp.ContentLength = int64(len(newBody))
			resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
			resp.Header.Del("Content-Encoding")

			return nil
		}
	}

	return rp
}

func (h *ProxyHandler) readResponseBody(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body

	if resp.Header.Get("Content-Encoding") == "gzip" {
		gzipReader, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gzipReader.Close()
		reader = gzipReader
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()

	return body, nil
}
