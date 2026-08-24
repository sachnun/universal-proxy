package core

import (
	"encoding/base64"
	"net/http"
	"strings"
	"sync"
)

type NamedPool struct {
	Name      string
	Username  string
	Pool      *ProxyPool
	Transport *RotatingProxyTransport
}

type PoolRouter struct {
	mu               sync.RWMutex
	pools            []*NamedPool
	defaultTransport http.RoundTripper
}

func NewPoolRouter(pools []*NamedPool, defaultTransport http.RoundTripper) *PoolRouter {
	if pools == nil {
		pools = []*NamedPool{}
	}
	return &PoolRouter{
		pools:            pools,
		defaultTransport: defaultTransport,
	}
}

func (r *PoolRouter) Add(p *NamedPool) {
	if r == nil || p == nil {
		return
	}
	r.mu.Lock()
	r.pools = append(r.pools, p)
	r.mu.Unlock()
}

func (r *PoolRouter) Select(username string) *RotatingProxyTransport {
	if username == "" || r == nil {
		return nil
	}

	upper := strings.ToUpper(username)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.pools {
		if strings.ToUpper(p.Username) == upper {
			return p.Transport
		}
	}
	return nil
}

func (r *PoolRouter) Get(name string) *NamedPool {
	if r == nil || name == "" {
		return nil
	}
	upper := strings.ToUpper(name)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range r.pools {
		if strings.ToUpper(p.Name) == upper {
			return p
		}
	}
	return nil
}

func (r *PoolRouter) Has(name string) bool {
	return r.Get(name) != nil
}

func (r *PoolRouter) Default() http.RoundTripper {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultTransport
}

func (r *PoolRouter) Names() []string {
	if r == nil {
		return nil
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.pools))
	for _, p := range r.pools {
		names = append(names, p.Name)
	}
	return names
}

type PoolInfo struct {
	Name       string
	ProxyCount int
}

type SystemStats struct {
	Pools        []PoolInfo
	TotalProxies int
}

func (r *PoolRouter) Stats() SystemStats {
	if r == nil {
		return SystemStats{}
	}

	r.mu.RLock()
	defer r.mu.RUnlock()

	pools := make([]PoolInfo, 0, len(r.pools))
	total := 0
	for _, p := range r.pools {
		if strings.HasPrefix(strings.ToUpper(p.Name), "WARP") {
			continue
		}
		count := 0
		if p.Pool != nil {
			count = p.Pool.Count()
		}
		pools = append(pools, PoolInfo{Name: p.Name, ProxyCount: count})
		total += count
	}
	return SystemStats{Pools: pools, TotalProxies: total}
}

func AuthUsername(r *http.Request) string {
	user, _, ok := r.BasicAuth()
	if ok {
		return user
	}

	pa := r.Header.Get("Proxy-Authorization")
	if pa == "" {
		return ""
	}

	if !strings.HasPrefix(pa, "Basic ") {
		return ""
	}

	payload, err := base64.StdEncoding.DecodeString(strings.TrimSpace(pa[6:]))
	if err != nil {
		return ""
	}

	pair := strings.SplitN(string(payload), ":", 2)
	if len(pair) == 0 || pair[0] == "" {
		return ""
	}

	return pair[0]
}
