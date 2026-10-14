package configsource

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/rfbatista/spillway/internal/proxy"
)

// Watcher polls a ConfigSource on a fixed interval and calls onChange when the
// configuration actually changes. It logs warnings on transient errors and
// continues running — it never crashes the process.
//
// Change detection lives here rather than in a ConfigSource because it must not
// depend on any one source's HTTP semantics. The AWS AppConfig Agent, for
// example, returns HTTP 200 with the full document on every poll and never
// signals "unchanged", so a source-level check cannot be relied on. Rebuilding
// the backend pool on every poll previously leaked goroutines and memory.
type Watcher struct {
	source   ConfigSource
	interval time.Duration
	onChange func([]proxy.BackendConfig) error
	logger   *slog.Logger

	// lastApplied is the hash of the config most recently applied successfully.
	// Run is single-goroutine, so this needs no lock.
	lastApplied [sha256.Size]byte
	hasApplied  bool
}

func NewWatcher(source ConfigSource, interval time.Duration, onChange func([]proxy.BackendConfig) error) *Watcher {
	return &Watcher{
		source:   source,
		interval: interval,
		onChange: onChange,
		logger:   slog.With(slog.String("component", "ConfigWatcher")),
	}
}

// hashConfigs returns a content hash of cfgs. json.Marshal is deterministic for
// these types — struct fields marshal in declaration order and BackendConfig
// contains no maps — so equal configs always produce an equal hash.
func hashConfigs(cfgs []proxy.BackendConfig) ([sha256.Size]byte, error) {
	raw, err := json.Marshal(cfgs)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(raw), nil
}

// Seed records cfgs as already applied, so a config that is unchanged since
// boot does not trigger a redundant pool rebuild on the first tick. Call this
// right after the boot-time pool build succeeds, before Run starts polling.
//
// If cfgs fails to hash, Seed leaves the Watcher unseeded rather than guessing:
// a redundant rebuild on the first tick is strictly better than skipping a
// change that should have been applied.
func (w *Watcher) Seed(cfgs []proxy.BackendConfig) {
	sum, err := hashConfigs(cfgs)
	if err != nil {
		w.logger.Warn("watcher seed: failed to hash config, first poll may rebuild redundantly", slog.Any("error", err))
		return
	}
	w.lastApplied = sum
	w.hasApplied = true
}

// Run polls the source every interval until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.poll(ctx)
		}
	}
}

// poll performs a single load-and-maybe-apply cycle.
func (w *Watcher) poll(ctx context.Context) {
	cfgs, err := w.source.Load(ctx)
	if errors.Is(err, ErrNoChange) {
		w.logger.Debug("config poll: no change")
		return
	}
	if err != nil {
		w.logger.Warn("config poll: error fetching config", slog.Any("error", err))
		return
	}

	sum, err := hashConfigs(cfgs)
	if err != nil {
		w.logger.Warn("config poll: failed to hash config", slog.Any("error", err))
		return
	}
	if w.hasApplied && sum == w.lastApplied {
		w.logger.Debug("config poll: no change")
		return
	}

	if err := w.onChange(cfgs); err != nil {
		w.logger.Error("config poll: failed to apply new config", slog.Any("error", err))
		// lastApplied is deliberately NOT updated: a failed apply must be retried
		// on the next tick, never cached as if it had succeeded.
		return
	}

	w.lastApplied = sum
	w.hasApplied = true
	w.logger.Info("config poll: applied new config", slog.Int("backends", len(cfgs)))
}
