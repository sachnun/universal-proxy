package core

import (
	"context"
	"log"
	"sync"
	"time"
)

// DefaultTrafficFailThreshold is the consecutive real-traffic failure count
// that demotes a ready proxy back to quarantine.
const DefaultTrafficFailThreshold = 3

// ValidatorConfig tunes the background proxy validator. Zero fields fall
// back to the defaults below; only tests override them.
type ValidatorConfig struct {
	// ProbeTimeout bounds a single probe (reachability + relay check).
	ProbeTimeout time.Duration
	// Concurrency is the number of parallel probe workers.
	Concurrency int
	// CheckInterval is how often the background scan runs.
	CheckInterval time.Duration
	// StaleAfter: ready proxies whose last check is older than this are
	// re-probed by the scan.
	StaleAfter time.Duration
	// MaxFails is the number of consecutive probe failures before a proxy
	// is evicted permanently.
	MaxFails int
	// BackoffBase and BackoffMax bound the quarantine retry delay.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// TrafficFailThreshold is the number of consecutive real-traffic
	// failures that demote a ready proxy back to quarantine.
	TrafficFailThreshold int
}

// quarantineEntry tracks a proxy waiting for a retry probe.
type quarantineEntry struct {
	state   *ProxyState
	fails   int
	nextTry time.Time
}

// ProxyValidator is the background service that gates every proxy before it
// becomes usable and keeps the ready set fresh afterwards.
//
// Lifecycle per proxy:
//
//	FETCHED -> PENDING (staged, not in rotation)
//	PENDING -> READY   (probe passed; graduated into the pools)
//	READY   -> QUARANTINE (probe or repeated traffic failure)
//	QUARANTINE -> READY (recovered on retry) | EVICTED (MaxFails reached)
type ProxyValidator struct {
	logger *log.Logger
	config ValidatorConfig

	mu         sync.Mutex
	pending    map[string]*ProxyState
	ready      map[string]*ProxyState
	quarantine map[string]*quarantineEntry
	graduate   func([]*ProxyState)
	started    bool

	jobs chan *ProxyState
}

// NewProxyValidator builds a validator with sane defaults for any zero
// config field.
func NewProxyValidator(logger *log.Logger, config ValidatorConfig) *ProxyValidator {
	if logger == nil {
		logger = log.Default()
	}
	if config.ProbeTimeout <= 0 {
		config.ProbeTimeout = DialTimeout
	}
	if config.Concurrency <= 0 {
		config.Concurrency = 100
	}
	if config.CheckInterval <= 0 {
		config.CheckInterval = time.Minute
	}
	if config.StaleAfter <= 0 {
		config.StaleAfter = 5 * time.Minute
	}
	if config.MaxFails <= 0 {
		config.MaxFails = 3
	}
	if config.BackoffBase <= 0 {
		config.BackoffBase = 15 * time.Second
	}
	if config.BackoffMax <= 0 {
		config.BackoffMax = 5 * time.Minute
	}
	if config.TrafficFailThreshold <= 0 {
		config.TrafficFailThreshold = DefaultTrafficFailThreshold
	}

	return &ProxyValidator{
		logger:     logger,
		config:     config,
		pending:    make(map[string]*ProxyState),
		ready:      make(map[string]*ProxyState),
		quarantine: make(map[string]*quarantineEntry),
	}
}

// SetGraduate registers the callback invoked whenever the ready set changes;
// it receives the full ready list and is responsible for pushing it into the
// pools (Host.ReplaceProxies).
func (v *ProxyValidator) SetGraduate(fn func([]*ProxyState)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.graduate = fn
}

// Start launches the probe workers and the background scan loop. Safe to
// call multiple times; the validator starts lazily on first Submit.
func (v *ProxyValidator) Start() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.startLocked()
}

// startLocked launches workers and the scan loop. Callers must hold v.mu.
func (v *ProxyValidator) startLocked() {
	if v.started {
		return
	}
	v.started = true
	v.jobs = make(chan *ProxyState, v.config.Concurrency*2)

	for i := 0; i < v.config.Concurrency; i++ {
		go v.worker()
	}
	go v.scanLoop()
}

// Submit stages a freshly fetched list for validation. Keys already READY
// are kept in rotation (the scan re-probes them); new keys and quarantined
// keys are (re)probed before they can be used.
func (v *ProxyValidator) Submit(states []*ProxyState) {
	v.mu.Lock()
	if !v.started {
		v.startLocked()
	}
	jobStates := make([]*ProxyState, 0, len(states))
	for _, ps := range states {
		if ps == nil || ps.Key == "" {
			continue
		}
		if _, ok := v.ready[ps.Key]; ok {
			// Already validated and in rotation; refresh its pointer so a
			// re-probe updates the current state.
			v.ready[ps.Key] = ps
			continue
		}
		delete(v.quarantine, ps.Key)
		v.pending[ps.Key] = ps
		jobStates = append(jobStates, ps)
	}
	v.mu.Unlock()

	for _, ps := range jobStates {
		v.jobs <- ps
	}
}

// OnTrafficFailure is invoked by the pools when a ready proxy accumulates
// TrafficFailThreshold consecutive failures from real traffic. It demotes
// the proxy out of rotation into the quarantine for a fresh probe.
func (v *ProxyValidator) OnTrafficFailure(key string) {
	v.mu.Lock()
	defer v.mu.Unlock()

	ps, ok := v.ready[key]
	if !ok {
		return
	}
	delete(v.ready, key)
	v.quarantine[key] = &quarantineEntry{
		state:   ps,
		fails:   1,
		nextTry: time.Now().Add(v.backoff(1)),
	}
	v.logger.Printf("[VALIDATOR] %s demoted (%d consecutive traffic failures)", key, v.config.TrafficFailThreshold)
	v.graduateLocked()
}

func (v *ProxyValidator) worker() {
	for ps := range v.jobs {
		lat, ok := v.probe(ps)
		v.handleResult(ps, lat, ok)
	}
}

func (v *ProxyValidator) scanLoop() {
	ticker := time.NewTicker(v.config.CheckInterval)
	defer ticker.Stop()
	for range ticker.C {
		v.scan()
	}
}

// scan re-queues stale ready proxies for re-probing and quarantined proxies
// whose backoff has elapsed.
func (v *ProxyValidator) scan() {
	v.mu.Lock()
	now := time.Now()
	jobs := make([]*ProxyState, 0, 8)
	for _, ps := range v.ready {
		if now.Sub(ps.LastChecked) >= v.config.StaleAfter {
			jobs = append(jobs, ps)
		}
	}
	for _, entry := range v.quarantine {
		if !entry.nextTry.After(now) {
			jobs = append(jobs, entry.state)
		}
	}
	v.mu.Unlock()

	for _, ps := range jobs {
		v.jobs <- ps
	}
}

func (v *ProxyValidator) probe(ps *ProxyState) (time.Duration, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), v.config.ProbeTimeout)
	defer cancel()
	return ProbeProxy(ctx, ps)
}

// handleResult routes a probe outcome to the proxy's current stage. It must
// be called with the validator mutex held.
func (v *ProxyValidator) handleResult(ps *ProxyState, lat time.Duration, ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()

	key := ps.Key

	if entry, inQuarantine := v.quarantine[key]; inQuarantine {
		if !ok {
			entry.fails++
			if entry.fails >= v.config.MaxFails {
				delete(v.quarantine, key)
				v.logger.Printf("[VALIDATOR] %s evicted (%d consecutive probe failures)", key, entry.fails)
			} else {
				entry.nextTry = time.Now().Add(v.backoff(entry.fails))
			}
			return
		}
		delete(v.quarantine, key)
		entry.state.Latency = lat
		entry.state.LastChecked = time.Now()
		v.ready[key] = entry.state
		v.logger.Printf("[VALIDATOR] %s recovered (%.0fms)", key, float64(lat)/float64(time.Millisecond))
		v.graduateLocked()
		return
	}

	if cur, inReady := v.ready[key]; inReady {
		if !ok {
			delete(v.ready, key)
			v.quarantine[key] = &quarantineEntry{
				state:   cur,
				fails:   1,
				nextTry: time.Now().Add(v.backoff(1)),
			}
			v.logger.Printf("[VALIDATOR] %s demoted (probe failed)", key)
			v.graduateLocked()
			return
		}
		cur.Latency = lat
		cur.LastChecked = time.Now()
		v.ready[key] = cur
		return
	}

	// Pending (initial validation) or unknown (already evicted).
	cur, inPending := v.pending[key]
	if !ok {
		if inPending {
			delete(v.pending, key)
			v.quarantine[key] = &quarantineEntry{
				state:   cur,
				fails:   1,
				nextTry: time.Now().Add(v.backoff(1)),
			}
		}
		return
	}
	if !inPending {
		return // replaced or evicted between enqueue and probe
	}
	delete(v.pending, key)
	cur.Latency = lat
	cur.LastChecked = time.Now()
	v.ready[key] = cur
	v.logger.Printf("[VALIDATOR] %s ready (%.0fms)", key, float64(lat)/float64(time.Millisecond))
	v.graduateLocked()
}

// graduateLocked pushes the current ready set into the pools. Callers must
// hold the validator mutex; the graduate callback (Host.ReplaceProxies)
// takes its own locks and never re-enters the validator.
func (v *ProxyValidator) graduateLocked() {
	if v.graduate == nil {
		return
	}
	ready := make([]*ProxyState, 0, len(v.ready))
	for _, ps := range v.ready {
		ready = append(ready, ps)
	}
	v.graduate(ready)
}

func (v *ProxyValidator) backoff(fails int) time.Duration {
	d := v.config.BackoffBase
	for i := 1; i < fails && d < v.config.BackoffMax; i++ {
		d *= 2
	}
	if d > v.config.BackoffMax {
		d = v.config.BackoffMax
	}
	return d
}
