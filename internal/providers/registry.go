// Package providers defines the proxy provider contract and the host that
// providers use to contribute capacity to the router.
//
// A provider is a self-contained unit (one package, one feature class):
//   - Name() identifies it for logs and refresh bookkeeping.
//   - Start() contributes proxies/pools through the Host, then returns.
//   - Refresh() (optional, via Refresher) is invoked periodically by the
//     bootstrap loop; the provider decides itself whether anything changed
//     (ETag, token expiry, static list => no-op).
//
// Adding a new provider requires one new package that implements Provider
// and registers itself via init() (Xray-style blank import), plus one import
// line in cmd/unroxy/main.go.
package providers

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
)

// Provider is the unit of proxy capacity.
type Provider interface {
	// Name returns a stable unique identifier.
	Name() string
	// Start initializes the provider and contributes proxies/pools via the
	// host. It may be called from a goroutine and must not block forever.
	Start(ctx context.Context, host *Host, logger *log.Logger) error
}

// Refresher is implemented by providers whose capacity degrades over time
// (list changes, token expiry) and need periodic re-fetching.
type Refresher interface {
	Refresh(ctx context.Context) error
}

var (
	mu     sync.RWMutex
	byName = make(map[string]Provider)
	order  []string
)

// Register adds a provider. Panics on duplicate names.
func Register(p Provider) {
	if p == nil || p.Name() == "" {
		panic("providers: Register requires a provider with a non-empty Name")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := byName[p.Name()]; dup {
		panic(fmt.Sprintf("providers: duplicate provider %q", p.Name()))
	}
	byName[p.Name()] = p
	order = append(order, p.Name())
	sort.Strings(order)
}

// All returns registered providers ordered by name.
func All() []Provider {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Provider, 0, len(order))
	for _, name := range order {
		out = append(out, byName[name])
	}
	return out
}
