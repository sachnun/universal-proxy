// Package urban implements the Urban VPN proxy provider.
//
// Flow (verified live): register an anonymous account -> mint a 1h security
// token ("accs") -> fetch the country/server list from stats.urban-vpn.com
// (each server carries a signature) -> mint a per-server token ("accs-proxy")
// on demand for each CONNECT. Every proxy is `http://<host>:8081` with HTTP
// Basic auth, username = fresh accs-proxy token, password ignored.
//
// The accs-proxy API is aggressively rate-limited and tokens are short-lived
// (they have failed after a few minutes in practice), so tokens are minted
// lazily per connection with a 60s memoization, a paced rate limiter, and
// immediate invalidation when a proxy answers 407.
package urban

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"unroxy/internal/core"
	"unroxy/internal/providers"
)

const (
	apiBase      = "https://api-pro.urban-vpn.com/rest/v1"
	anonEndpoint = apiBase + "/registrations/clientApps/URBAN_VPN_BROWSER_EXTENSION/users/anonymous"
	accsEndpoint = apiBase + "/security/tokens/accs"
	accsProxyEP  = apiBase + "/security/tokens/accs-proxy"
	countriesURL = "https://stats.urban-vpn.com/api/rest/v2/entrypoints/countries"
	clientApp    = "URBAN_VPN_BROWSER_EXTENSION"
	defaultPort  = 8081
	proxyPass    = "1" // ignored by the proxy; only the username matters

	anonRetries   = 3
	tokenCacheTTL = 60 * time.Second
	mintRate      = 4.0 // tokens per second
	mintBurst     = 8
)

type proxyServer struct {
	host      string
	port      int
	signature string
}

type countryEntry struct {
	iso2    string
	servers []proxyServer
}

// Provider mints Urban tokens on demand and maintains the country pools.
type Provider struct {
	host   *providers.Host
	logger *log.Logger

	mu      sync.Mutex
	anon    string
	sec     string
	secExp  time.Time
	limiter *rate.Limiter
	cache   map[string]proxyToken // server signature -> memoized token
}

type proxyToken struct {
	value string
	exp   time.Time
}

func init() {
	providers.Register(&Provider{})
}

func (p *Provider) Name() string { return "Urban" }

func (p *Provider) Start(ctx context.Context, host *providers.Host, logger *log.Logger) error {
	p.host = host
	p.logger = logger
	p.limiter = rate.NewLimiter(mintRate, mintBurst)
	p.cache = make(map[string]proxyToken)

	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.mintSessionLocked(); err != nil {
		logger.Printf("Urban: session setup failed: %v", err)
		return err
	}
	countries, err := p.fetchCountriesLocked()
	if err != nil {
		logger.Printf("Urban: country list failed: %v", err)
		return err
	}
	p.distributeLocked(countries)
	return nil
}

// Refresh re-fetches the server list and rebuilds the pools. Tokens are not
// minted here; they are minted per connection by the DialContext closures.
func (p *Provider) Refresh(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.secExpiredLocked() {
		if err := p.mintSessionLocked(); err != nil {
			return err
		}
	}
	countries, err := p.fetchCountriesLocked()
	if err != nil {
		return err
	}
	p.distributeLocked(countries)
	return nil
}

func (p *Provider) secExpiredLocked() bool {
	return p.sec == "" || time.Until(p.secExp) < 5*time.Minute
}

func (p *Provider) mintSessionLocked() error {
	var anon struct {
		Value string `json:"value"`
	}
	if p.anon == "" {
		var lastErr error
		for i := 0; i < anonRetries; i++ {
			lastErr = postJSON(anonEndpoint, "", map[string]interface{}{
				"clientApp": map[string]string{"name": clientApp, "browser": "CHROME"},
			}, &anon)
			if lastErr == nil {
				break
			}
			time.Sleep(time.Second)
		}
		if lastErr != nil {
			return fmt.Errorf("urban anonymous register: %w", lastErr)
		}
		p.anon = anon.Value
	}

	var accs struct {
		Value          string `json:"value"`
		ExpirationTime int64  `json:"expirationTime"`
	}
	if err := postJSON(accsEndpoint, p.anon, map[string]interface{}{
		"type":      "accs",
		"clientApp": map[string]string{"name": clientApp, "browser": "CHROME"},
	}, &accs); err != nil {
		return fmt.Errorf("urban accs token: %w", err)
	}
	p.sec = accs.Value
	p.secExp = time.UnixMilli(accs.ExpirationTime)
	return nil
}

func (p *Provider) fetchCountriesLocked() ([]countryEntry, error) {
	req, err := http.NewRequest(http.MethodGet, countriesURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.sec)
	resp, err := core.NewProviderHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("urban countries status %d", resp.StatusCode)
	}

	var payload struct {
		Countries struct {
			Elements []struct {
				Code struct {
					ISO2 string `json:"iso2"`
				} `json:"code"`
				Servers struct {
					Elements []struct {
						Address struct {
							Primary struct {
								Host string `json:"host"`
								Port int    `json:"port"`
							} `json:"primary"`
						} `json:"address"`
						Signature string `json:"signature"`
					} `json:"elements"`
				} `json:"servers"`
			} `json:"elements"`
		} `json:"countries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("urban countries decode: %w", err)
	}

	out := make([]countryEntry, 0)
	for _, c := range payload.Countries.Elements {
		if c.Code.ISO2 == "" {
			continue
		}
		entry := countryEntry{iso2: strings.ToUpper(c.Code.ISO2)}
		for _, s := range c.Servers.Elements {
			if s.Address.Primary.Host == "" || s.Signature == "" {
				continue
			}
			port := s.Address.Primary.Port
			if port == 0 {
				port = defaultPort
			}
			entry.servers = append(entry.servers, proxyServer{
				host:      s.Address.Primary.Host,
				port:      port,
				signature: s.Signature,
			})
		}
		if len(entry.servers) > 0 {
			out = append(out, entry)
		}
	}
	return out, nil
}

func (p *Provider) distributeLocked(countries []countryEntry) {
	states := make([]*core.ProxyState, 0, len(countries))
	for _, c := range countries {
		for _, s := range c.servers {
			addr := net.JoinHostPort(s.host, fmt.Sprintf("%d", s.port))
			u := &url.URL{Scheme: "http", Host: addr}
			state := &core.ProxyState{
				Key:      "urban://" + addr,
				URL:      u,
				Country:  c.iso2,
				Priority: 1,
			}
			sig := s.signature
			state.DialContext = func(ctx context.Context, network, target string) (net.Conn, error) {
				return p.connect(ctx, sig, u, target)
			}
			states = append(states, state)
		}
	}
	p.host.Submit(states)
	p.logger.Printf("Urban: %d proxies queued for validation", len(states))
}

// connect mints (or reuses) a token and establishes a CONNECT tunnel.
func (p *Provider) connect(ctx context.Context, sig string, u *url.URL, target string) (net.Conn, error) {
	token, err := p.tokenFor(ctx, sig)
	if err != nil {
		return nil, err
	}

	auth := *u
	auth.User = url.UserPassword(token, proxyPass)
	conn, err := core.HTTPProxyConnect(ctx, &auth, target)
	if err != nil && strings.Contains(err.Error(), "407") {
		p.invalidate(sig, token) // single-use / revoked token
	}
	return conn, err
}

func (p *Provider) invalidate(sig, token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if t, ok := p.cache[sig]; ok && t.value == token {
		delete(p.cache, sig)
	}
}

// tokenFor returns a memoized token or mints a fresh one, paced by the
// rate limiter to respect the API quota.
func (p *Provider) tokenFor(ctx context.Context, sig string) (string, error) {
	p.mu.Lock()
	if t, ok := p.cache[sig]; ok && time.Until(t.exp) > 5*time.Second {
		p.mu.Unlock()
		return t.value, nil
	}
	p.mu.Unlock()

	if err := p.limiter.Wait(ctx); err != nil {
		return "", err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.secExpiredLocked() {
		if err := p.mintSessionLocked(); err != nil {
			return "", err
		}
	}
	if t, ok := p.cache[sig]; ok && time.Until(t.exp) > 5*time.Second {
		return t.value, nil
	}

	var out struct {
		Value          string `json:"value"`
		ExpirationTime int64  `json:"expirationTime"`
	}
	body := map[string]interface{}{
		"type":      "accs-proxy",
		"clientApp": map[string]string{"name": clientApp, "browser": "CHROME"},
		"signature": sig,
	}
	if err := postJSON(accsProxyEP, p.sec, body, &out); err != nil {
		return "", fmt.Errorf("urban accs-proxy: %w", err)
	}

	p.cache[sig] = proxyToken{value: out.Value, exp: time.UnixMilli(out.ExpirationTime)}
	return out.Value, nil
}

func postJSON(endpoint, bearer string, body, out interface{}) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := core.NewProviderHTTPClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("urban api rate limited")
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("post %s: status %d", endpoint, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
