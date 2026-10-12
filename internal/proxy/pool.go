package proxy

import (
	"errors"
	"fmt"
	"log/slog"
	"proxy/internal/ports"
	"sync"
)

type BackendPool struct {
	backend      []*Backend
	configs      []BackendConfig
	mux          sync.RWMutex
	current      int
	aliasCursor  map[string]int // per-alias round-robin cursor for model routing
	queueManager *QueueManager
	logger       *slog.Logger
}

func (b *BackendPool) getLogger() *slog.Logger {
	if b != nil && b.logger != nil {
		return b.logger
	}
	return slog.Default()
}

// NewBackendPool builds a pool of backends from cfg.
//
// qm is owned by the caller and shared across pool generations: the config
// watcher rebuilds the pool whenever configuration changes, and a QueueManager
// created per-pool would strand its worker goroutines every rebuild (they are
// GC roots, so they pin their queues forever). Sharing it means a rebuild reuses
// the existing per-backend queues and starts no new goroutines.
//
// This is safe because a QueueWorker never performs the upstream call — it only
// gates on the backend's rate-limit window and signals readiness back over the
// response channel — so a queue is a per-backend-ID rate-limit gate with no
// coupling to any one pool generation. Backend IDs are derived from the
// backend's identity (see backendID), so a reload that reorders entries keeps
// each backend attached to its own queue and usage window; queues for backends
// a reload removed are pruned from the manager once the build succeeds.
func NewBackendPool(
	save ports.SaveUsageService,
	get ports.GetUsageService,
	cfg []BackendConfig,
	tracker *RequestTracker,
	qm *QueueManager,
) (*BackendPool, error) {
	if qm == nil {
		return nil, errors.New("missing QueueManager")
	}

	pool := BackendPool{
		backend:      make([]*Backend, 0, len(cfg)),
		configs:      cfg,
		queueManager: qm,
		logger:       slog.With(slog.String("component", "BackendPool")),
	}
	pool.logger.Info("loading backends",
		slog.Int("configured_backends", len(cfg)),
	)
	seen := make(map[string]int, len(cfg))
	active := make(map[string]struct{}, len(cfg))
	for _, bcfg := range cfg {
		id := backendID(bcfg, seen)
		active[id] = struct{}{}
		summarizer := newLogBodySummarizer(bcfg)
		retry, err := NewRetryTransport(id, bcfg.Model, save, get, qm, tracker,
			WithLogBodySummarizer(summarizer))
		if err != nil {
			return nil, err
		}
		retry.ClientTagHeader = bcfg.ClientTagHeader
		mapper, err := NewMapper(bcfg)
		if err != nil {
			return nil, fmt.Errorf("backend %s: %w", id, err)
		}
		prx := NewProxy(bcfg.GetURL(), bcfg, mapper, retry)
		bc, err := NewBackend(id, get, bcfg, prx)
		if err != nil {
			return nil, err
		}
		pool.backend = append(pool.backend, bc)
		pool.logger.Info("backend loaded",
			slog.String("backend_id", bc.ID),
			slog.String("backend_url", bc.cfg.Host),
			slog.String("mapper", mapperName(bcfg.Mapper)),
			slog.Bool("log_body_summary", summarizer.active()),
			slog.Int("loaded_backends", len(pool.backend)),
		)
	}
	qm.Prune(active)
	return &pool, nil
}

// backendID derives the pool-facing identifier for a backend: the configured
// Name when set, otherwise the fields that name its upstream ("model@host") —
// never its position in the config. Queues and rate-limit usage are keyed by
// this ID and outlive a config reload (see NewBackendPool), so the ID must
// follow the backend when entries are added, removed, or reordered — an index
// would silently hand one backend's queue and usage window to another; for the
// same reason, renaming a backend resets its queue and usage window. seen
// counts prior occurrences so duplicate entries (same name, or same model and
// host, e.g. for weighting) stay unique; their "#n" suffix is assigned in
// config order.
func backendID(cfg BackendConfig, seen map[string]int) string {
	id := cfg.Host
	if cfg.Model != "" {
		id = cfg.Model + "@" + cfg.Host
	}
	if cfg.Name != "" {
		id = cfg.Name
	}
	seen[id]++
	if n := seen[id]; n > 1 {
		id = fmt.Sprintf("%s#%d", id, n)
	}
	return id
}

// mapperName returns a stable label for logging ("passthrough" when unset).
func mapperName(m string) string {
	if m == "" {
		return "passthrough"
	}
	return m
}

// GetNextBackendForModel selects a backend whose configured alias matches the
// requested model (typically the X-Request-Model header). It round-robins within
// the matching alias group and prefers backends with remaining capacity. When
// the alias is empty or no backend declares it, selection falls back to
// round-robin across the whole pool.
func (b *BackendPool) GetNextBackendForModel(alias string) (*Backend, error) {
	if alias == "" {
		return b.GetNextBackend()
	}

	group := b.backendsForAlias(alias)
	if len(group) == 0 {
		b.getLogger().Info("requested model alias not configured; falling back to round-robin",
			slog.String("requested_model", alias),
			slog.Int("pool_size", len(b.backend)),
		)
		return b.GetNextBackend()
	}

	start := b.nextAliasCursor(alias, len(group))
	chosen := group[start]
	for i := range group {
		cand := group[(start+i)%len(group)]
		if cand.IsAlive() {
			chosen = cand
			break
		}
	}

	b.getLogger().Info("selected backend for model alias",
		slog.String("requested_model", alias),
		slog.String("backend_id", chosen.ID),
		slog.Int("alias_group_size", len(group)),
	)
	return chosen, nil
}

// backendsForAlias returns the pool backends whose alias equals alias, in pool
// order.
func (b *BackendPool) backendsForAlias(alias string) []*Backend {
	b.mux.RLock()
	defer b.mux.RUnlock()
	var group []*Backend
	for _, back := range b.backend {
		if back.Alias() == alias {
			group = append(group, back)
		}
	}
	return group
}

// nextAliasCursor returns the current round-robin offset for alias within a
// group of groupSize and advances it by one.
func (b *BackendPool) nextAliasCursor(alias string, groupSize int) int {
	b.mux.Lock()
	defer b.mux.Unlock()
	if b.aliasCursor == nil {
		b.aliasCursor = make(map[string]int)
	}
	start := b.aliasCursor[alias] % groupSize
	b.aliasCursor[alias] = (start + 1) % groupSize
	return start
}

func (b *BackendPool) GetNextBackend() (*Backend, error) {
	logger := b.getLogger()
	logger.Info("looking for backend",
		slog.Int("pool_size", len(b.backend)),
		slog.Int("current_index", b.current),
	)
	if len(b.backend) == 0 {
		logger.Warn("no backend available",
			slog.Int("pool_size", len(b.backend)),
		)
		return nil, errors.New("no backend registred")
	}
	back := b.GetNextValidPeer()
	if back != nil {
		logger.Info("selected healthy backend",
			slog.String("backend_id", back.ID),
		)
		return back, nil
	}
	logger.Warn("no healthy backend found, returning current backend",
		slog.Int("current_index", b.current),
	)
	return b.GetCurrentBackend(), nil
}

func (s *BackendPool) GetServerPoolSize() int {
	return len(s.backend)
}

func (s *BackendPool) GetCurrentBackend() *Backend {
	s.mux.Lock()
	defer s.mux.Unlock()
	return s.backend[s.current]
}

func (s *BackendPool) SetCurrentBackend(cur int) {
	s.mux.Lock()
	s.current = cur
	s.mux.Unlock()
}

func (s *BackendPool) Rotate() *Backend {
	s.mux.Lock()
	s.current = (s.current + 1) % s.GetServerPoolSize()
	s.mux.Unlock()
	return s.backend[s.current]
}

func (s *BackendPool) GetNextValidPeer() *Backend {
	for i := 0; i < s.GetServerPoolSize(); i++ {
		nextPeer := s.Rotate()
		if nextPeer.IsAlive() {
			return nextPeer
		}
	}
	return nil
}

func (b *BackendPool) GetBackends() ([]*Backend, error) {
	return b.backend, nil
}

// Configs returns the BackendConfig slice this pool was built from.
func (b *BackendPool) Configs() []BackendConfig {
	return b.configs
}

// NewBackendPoolFromConfigs creates a minimal BackendPool for testing purposes.
// It does not create Backend instances; only Configs() is populated.
func NewBackendPoolFromConfigs(cfgs []BackendConfig) (*BackendPool, error) {
	return &BackendPool{configs: cfgs}, nil
}
