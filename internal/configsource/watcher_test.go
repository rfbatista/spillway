package configsource_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rfbatista/spillway/internal/configsource"
	"github.com/rfbatista/spillway/internal/proxy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSource replays a fixed sequence of Load results, then returns ErrNoChange forever.
type mockSource struct {
	calls []mockCall
	idx   int
}

type mockCall struct {
	cfgs []proxy.BackendConfig
	err  error
}

func (m *mockSource) Load(_ context.Context) ([]proxy.BackendConfig, error) {
	if m.idx >= len(m.calls) {
		return nil, configsource.ErrNoChange
	}
	r := m.calls[m.idx]
	m.idx++
	return r.cfgs, r.err
}

func TestWatcher_CallsOnChangeOnNewConfig(t *testing.T) {
	cfg := []proxy.BackendConfig{{Host: "api.openai.com"}}
	src := &mockSource{calls: []mockCall{{cfgs: cfg}}}
	called := make(chan []proxy.BackendConfig, 1)

	w := configsource.NewWatcher(src, 10*time.Millisecond, func(cfgs []proxy.BackendConfig) error {
		called <- cfgs
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	go w.Run(ctx)

	select {
	case got := <-called:
		require.Equal(t, cfg, got)
	case <-ctx.Done():
		t.Fatal("onChange was not called within timeout")
	}
}

func TestWatcher_SkipsOnErrNoChange(t *testing.T) {
	src := &mockSource{calls: []mockCall{{err: configsource.ErrNoChange}}}
	called := make(chan struct{}, 1)

	w := configsource.NewWatcher(src, 10*time.Millisecond, func(_ []proxy.BackendConfig) error {
		called <- struct{}{}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go w.Run(ctx)
	<-ctx.Done()

	assert.Empty(t, called, "onChange must not be called when ErrNoChange is returned")
}

func TestWatcher_SkipsOnNetworkError(t *testing.T) {
	src := &mockSource{calls: []mockCall{{err: errors.New("connection refused")}}}
	called := make(chan struct{}, 1)

	w := configsource.NewWatcher(src, 10*time.Millisecond, func(_ []proxy.BackendConfig) error {
		called <- struct{}{}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	go w.Run(ctx)
	<-ctx.Done()

	assert.Empty(t, called, "onChange must not be called on transient network errors")
}

func TestWatcher_LogsOnChangeError(t *testing.T) {
	cfg := []proxy.BackendConfig{{Host: "api.openai.com"}}
	src := &mockSource{calls: []mockCall{{cfgs: cfg}}}
	w := configsource.NewWatcher(src, 10*time.Millisecond, func(_ []proxy.BackendConfig) error {
		return errors.New("pool build failed")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	assert.NotPanics(t, func() {
		go w.Run(ctx)
		<-ctx.Done()
	})
}

func TestWatcher_StopsOnContextCancel(t *testing.T) {
	src := &mockSource{}
	w := configsource.NewWatcher(src, 10*time.Millisecond, func(_ []proxy.BackendConfig) error { return nil })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Watcher.Run did not stop after context cancellation")
	}
}

// repeatSource returns the same config on every Load, forever — this is how the
// real AWS AppConfig Agent behaves (HTTP 200 with the full document on every
// poll, never a 204). Load calls are counted so tests can assert polling
// actually happened.
type repeatSource struct {
	mu    sync.Mutex
	cfgs  []proxy.BackendConfig
	loads int
}

func (r *repeatSource) Load(_ context.Context) ([]proxy.BackendConfig, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loads++
	return r.cfgs, nil
}

func (r *repeatSource) set(cfgs []proxy.BackendConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfgs = cfgs
}

func (r *repeatSource) loadCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loads
}

// counter is a race-safe tally of onChange invocations.
type counter struct {
	mu sync.Mutex
	n  int
}

func (c *counter) inc() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
}

func (c *counter) get() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// This is the regression test for the production memory leak: the AppConfig
// agent returns the same config on every poll, and the Watcher used to rebuild
// the whole backend pool each time.
func TestWatcher_DoesNotReapplyUnchangedConfig(t *testing.T) {
	src := &repeatSource{cfgs: []proxy.BackendConfig{{Host: "api.openai.com", Model: "gpt-4o"}}}
	var applied counter

	w := configsource.NewWatcher(src, 5*time.Millisecond, func(_ []proxy.BackendConfig) error {
		applied.inc()
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	assert.GreaterOrEqual(t, src.loadCount(), 5, "the source should have been polled repeatedly")
	assert.Equal(t, 1, applied.get(), "onChange must fire exactly once for a config that never changes")
}

// Regression test for the redundant boot-time rebuild: main builds the pool
// once at startup, then starts the Watcher. Without seeding, the first poll
// tick has no lastApplied and rebuilds even though the config never changed.
func TestWatcher_Seed_SkipsRebuildForUnchangedBootConfig(t *testing.T) {
	bootCfg := []proxy.BackendConfig{{Host: "api.openai.com", Model: "gpt-4o"}}
	src := &repeatSource{cfgs: bootCfg}
	var applied counter

	w := configsource.NewWatcher(src, 5*time.Millisecond, func(_ []proxy.BackendConfig) error {
		applied.inc()
		return nil
	})
	w.Seed(bootCfg)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	assert.GreaterOrEqual(t, src.loadCount(), 5, "the source should have been polled repeatedly")
	assert.Equal(t, 0, applied.get(), "a seeded config that never changes must never trigger onChange")
}

func TestWatcher_Seed_StillAppliesWhenConfigChanges(t *testing.T) {
	bootCfg := []proxy.BackendConfig{{Host: "api.openai.com", Model: "gpt-4o"}}
	src := &repeatSource{cfgs: bootCfg}
	var applied counter

	w := configsource.NewWatcher(src, 5*time.Millisecond, func(_ []proxy.BackendConfig) error {
		applied.inc()
		return nil
	})
	w.Seed(bootCfg)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	time.Sleep(100 * time.Millisecond)
	src.set([]proxy.BackendConfig{{Host: "api.anthropic.com", Model: "claude-sonnet-5"}})
	<-done

	assert.Equal(t, 1, applied.get(), "onChange must fire once for the genuine change, after the seeded config was skipped")
}

func TestWatcher_ReappliesWhenConfigChanges(t *testing.T) {
	src := &repeatSource{cfgs: []proxy.BackendConfig{{Host: "api.openai.com", Model: "gpt-4o"}}}
	var applied counter

	w := configsource.NewWatcher(src, 5*time.Millisecond, func(_ []proxy.BackendConfig) error {
		applied.inc()
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	// Let the first config settle, then genuinely change it.
	time.Sleep(100 * time.Millisecond)
	src.set([]proxy.BackendConfig{{Host: "api.anthropic.com", Model: "claude-sonnet-5"}})
	<-done

	assert.Equal(t, 2, applied.get(), "onChange must fire once per distinct config")
}

// A failed pool build must NOT be recorded as applied, or a transient failure
// would wedge the process on stale config forever.
func TestWatcher_RetriesAfterOnChangeFailure(t *testing.T) {
	src := &repeatSource{cfgs: []proxy.BackendConfig{{Host: "api.openai.com"}}}
	var attempts counter

	w := configsource.NewWatcher(src, 5*time.Millisecond, func(_ []proxy.BackendConfig) error {
		attempts.inc()
		return errors.New("pool build failed")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	w.Run(ctx)

	assert.Greater(t, attempts.get(), 1, "a failed onChange must be retried on the next tick, not cached as applied")
}
