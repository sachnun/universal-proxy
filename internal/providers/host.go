package providers

import (
	"log"
	"sort"
	"strings"
	"sync"

	"unroxy/internal/core"
)

// Host is what a Provider receives to contribute capacity. It owns the
// router, the default proxy pool and lazily-created country pools, and it
// reapplies psiphon primaries whenever a pool's contents are replaced. All
// fetched proxy lists pass through a background validator (Submit) before
// they reach the pools.
type Host struct {
	logger *log.Logger
	router *core.PoolRouter
	pool   *core.ProxyPool

	validator     *core.ProxyValidator
	validatorOnce sync.Once

	mu                   sync.Mutex
	countries            map[string]*core.ProxyPool
	primaries            []*core.ProxyState
	trafficFailThreshold int
}

// NewHost builds a router with an empty default pool and no countries, and
// wires up the background validator that gates every submitted proxy list.
func NewHost(logger *log.Logger) *Host {
	if logger == nil {
		logger = log.Default()
	}
	h := &Host{
		logger:    logger,
		countries: make(map[string]*core.ProxyPool),
	}
	validator := core.NewProxyValidator(logger, core.ValidatorConfig{})
	validator.SetGraduate(h.ReplaceProxies)
	h.validator = validator
	h.trafficFailThreshold = core.DefaultTrafficFailThreshold

	h.pool = h.newPool()
	h.router = core.NewPoolRouter(nil, core.NewRotatingProxyTransport(h.pool))
	return h
}

// newPool builds a pool wired to the background validator's traffic-failure
// hook. Must be called after h.validator is set.
func (h *Host) newPool() *core.ProxyPool {
	pool := core.NewProxyPool(h.logger, nil)
	if h.validator != nil {
		pool.SetTrafficFailureHook(h.trafficFailThreshold, h.validator.OnTrafficFailure)
	}
	return pool
}

// Submit routes a freshly fetched proxy list through the background
// validator. Only proxies that pass probing are graduated into the pools;
// everything else is quarantined with backoff and retried, then evicted
// after repeated failures.
func (h *Host) Submit(proxies []*core.ProxyState) {
	h.validatorOnce.Do(h.validator.Start)
	h.validator.Submit(proxies)
}

// Router exposes the pool router (for named pools, e.g. WARP variants).
func (h *Host) Router() *core.PoolRouter { return h.router }

// Country returns the pool for a country code, creating and registering it
// on the router on first use.
func (h *Host) Country(code string) *core.ProxyPool {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		code = "XX"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	pool := h.countryLocked(code)
	h.applyPrimariesLocked()
	return pool
}

// AddNamed registers a named pool with a custom transport (e.g. WARP).
func (h *Host) AddNamed(name string, pool *core.ProxyPool, transport *core.RotatingProxyTransport) {
	h.router.Add(&core.NamedPool{
		Name:      name,
		Username:  name,
		Pool:      pool,
		Transport: transport,
	})
}

// AddPrimary records a tunnel-based primary (psiphon) and applies it to the
// default pool and any matching country pool.
func (h *Host) AddPrimary(ps *core.ProxyState) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.primaries = append(h.primaries, ps)
	h.applyPrimariesLocked()
}

// ReplaceProxies replaces the default pool and all country pools with a
// freshly fetched list, then reapplies registered primaries.
func (h *Host) ReplaceProxies(proxies []*core.ProxyState) {
	h.mu.Lock()
	defer h.mu.Unlock()

	groups := core.GroupProxiesByCountry(proxies)
	codes := make([]string, 0, len(groups))
	for code := range groups {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		pool := h.countryLocked(code)
		pool.Replace(groups[code])
	}
	h.pool.Replace(proxies)
	h.applyPrimariesLocked()
}

func (h *Host) countryLocked(code string) *core.ProxyPool {
	if pool, ok := h.countries[code]; ok {
		return pool
	}
	pool := h.newPool()
	h.countries[code] = pool
	h.router.Add(&core.NamedPool{
		Name:      code,
		Username:  code,
		Pool:      pool,
		Transport: core.NewRotatingProxyTransport(pool),
	})
	return pool
}

// applyPrimariesLocked re-adds psiphon primaries to the default pool and to
// country pools whose code matches the primary's region exactly (case
// preserved, matching the historical behavior).
func (h *Host) applyPrimariesLocked() {
	for _, ps := range h.primaries {
		h.pool.SetPrimary(ps)
		if pool, ok := h.countries[ps.Country]; ok {
			pool.SetPrimary(ps)
		}
	}
}
