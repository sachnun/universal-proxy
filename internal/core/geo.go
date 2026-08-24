package core

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// EgressInfo is the externally observed identity of a proxy's exit node.
type EgressInfo struct {
	IP      string
	ISP     string
	Country string
}

// egressTimeout bounds a single egress IP + ISP + country resolution.
const egressTimeout = 8 * time.Second

var (
	ispCacheMu   sync.Mutex
	ispCache     = make(map[string]string) // egress IP -> ISP
	countryCache = make(map[string]string) // egress IP -> ISO country code
)

// ipWhoisBase is the ipwho.is API base URL. Kept as a variable so tests can
// point it at a local server.
var ipWhoisBase = "https://ipwho.is"

// dialFuncFor returns the proxy's dial function, or nil when the proxy has
// no dialer at all.
func dialFuncFor(ps *ProxyState) func(ctx context.Context, network, addr string) (net.Conn, error) {
	if ps.DialContext != nil {
		return ps.DialContext
	}
	if ps.URL != nil {
		return func(ctx context.Context, network, addr string) (net.Conn, error) {
			return httpProxyConnect(ctx, ps.URL, addr)
		}
	}
	return nil
}

// dialProxy opens a connection to addr through ps, preferring the proxy's
// own DialContext and falling back to a URL-based HTTP CONNECT.
func dialProxy(ctx context.Context, ps *ProxyState, addr string) (net.Conn, error) {
	d := dialFuncFor(ps)
	if d == nil {
		return nil, errors.New("no proxy dialer")
	}
	return d(ctx, "tcp", addr)
}

// resolveEgress measures the public egress IP observed through the proxy
// and resolves its ISP and country. The ISP/country lookups are properties
// of the IP and are cached globally, so each is fetched at most once per
// unique IP.
func resolveEgress(ctx context.Context, ps *ProxyState) (EgressInfo, error) {
	d := dialFuncFor(ps)
	if d == nil {
		return EgressInfo{}, errors.New("no proxy dialer")
	}
	return EgressViaDial(ctx, d, egressTimeout)
}

// EgressViaDial performs a real HTTPS request through dial and resolves the
// exit IP plus its ISP and country.
func EgressViaDial(ctx context.Context, dial func(ctx context.Context, network, addr string) (net.Conn, error), timeout time.Duration) (EgressInfo, error) {
	transport := &http.Transport{
		DialContext:         dial,
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        1,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	client := &http.Client{Transport: transport, Timeout: timeout}

	egCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(egCtx, http.MethodGet, "https://api.ipify.org?format=json", nil)
	if err != nil {
		return EgressInfo{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return EgressInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return EgressInfo{}, errors.New("egress endpoint returned non-200")
	}

	var out struct {
		IP string `json:"ip"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return EgressInfo{}, err
	}
	if out.IP == "" {
		return EgressInfo{}, errors.New("empty egress IP")
	}

	isp, cc := whoisForIP(egCtx, out.IP)
	return EgressInfo{IP: out.IP, ISP: isp, Country: cc}, nil
}

// whoisForIP resolves the ISP and country for an egress IP with a single
// direct (non-proxied) ipwho.is lookup. Results are cached per field by IP,
// so a partial answer (ISP but no country) is refetched until complete.
func whoisForIP(ctx context.Context, ip string) (isp, cc string) {
	if ip == "" {
		return "", ""
	}

	ispCacheMu.Lock()
	isp, ispOK := ispCache[ip]
	cc, ccOK := countryCache[ip]
	ispCacheMu.Unlock()
	if ispOK && ccOK {
		return isp, cc
	}

	client := NewProviderHTTPClient()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ipWhoisBase+"/"+ip, nil)
	if err != nil {
		return "", ""
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}

	var out struct {
		Success     bool   `json:"success"`
		CountryCode string `json:"country_code"`
		Connection  struct {
			ISP string `json:"isp"`
			Org string `json:"org"`
		} `json:"connection"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", ""
	}

	isp = out.Connection.ISP
	if isp == "" {
		isp = out.Connection.Org
	}
	if out.Success {
		cc = strings.ToUpper(strings.TrimSpace(out.CountryCode))
		if len(cc) != 2 {
			cc = ""
		} else {
			for i := 0; i < len(cc); i++ {
				if cc[i] < 'A' || cc[i] > 'Z' {
					cc = ""
					break
				}
			}
		}
	}

	ispCacheMu.Lock()
	if isp != "" {
		ispCache[ip] = isp
	}
	if cc != "" {
		countryCache[ip] = cc
	}
	ispCacheMu.Unlock()
	return isp, cc
}

// ispForIP resolves the ISP for an egress IP. Results are cached by IP.
func ispForIP(ctx context.Context, ip string) string {
	isp, _ := whoisForIP(ctx, ip)
	return isp
}

// countryForIP resolves the ISO 3166-1 alpha-2 country code for an egress
// IP. Results are cached by IP.
func countryForIP(ctx context.Context, ip string) string {
	_, cc := whoisForIP(ctx, ip)
	return cc
}
