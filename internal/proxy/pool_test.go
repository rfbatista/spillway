package proxy

import (
	"context"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"proxy/internal/domain"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestBackendPool_Configs(t *testing.T) {
	cfgs := []BackendConfig{
		{Host: "api.openai.com", Model: "gpt-4o"},
	}
	pool, err := NewBackendPoolFromConfigs(cfgs)
	require.NoError(t, err)
	got := pool.Configs()
	require.Len(t, got, 1)
	assert.Equal(t, "api.openai.com", got[0].Host)
	assert.Equal(t, "gpt-4o", got[0].Model)
}

// fakeDeadUsage reports a saturated backend whose reset window is in the future,
// so IsAlive returns false.
type fakeDeadUsage struct{}

func (fakeDeadUsage) Execute(ctx context.Context, id string) (*domain.ModelUsageQuota, error) {
	return &domain.ModelUsageQuota{ResetAt: time.Now().Add(time.Hour)}, nil
}

func testBackend(id, alias string, usage interface {
	Execute(context.Context, string) (*domain.ModelUsageQuota, error)
}) *Backend {
	return &Backend{
		ID:       id,
		cfg:      BackendConfig{Alias: alias},
		getusage: usage,
		logger:   slog.Default(),
	}
}

func testPool(backends ...*Backend) *BackendPool {
	return &BackendPool{backend: backends, logger: slog.Default()}
}

func TestGetNextBackendForModel_RoutesByAlias(t *testing.T) {
	pool := testPool(
		testBackend("0", "claude", fakeGetUsage{}),
		testBackend("1", "gpt", fakeGetUsage{}),
	)
	for range 4 {
		back, err := pool.GetNextBackendForModel("gpt")
		require.NoError(t, err)
		assert.Equal(t, "1", back.ID, "gpt alias must always route to backend 1")
	}
}

func TestGetNextBackendForModel_RoundRobinsWithinAlias(t *testing.T) {
	pool := testPool(
		testBackend("0", "claude", fakeGetUsage{}),
		testBackend("1", "claude", fakeGetUsage{}),
		testBackend("2", "gpt", fakeGetUsage{}),
	)
	var ids []string
	for range 4 {
		back, err := pool.GetNextBackendForModel("claude")
		require.NoError(t, err)
		ids = append(ids, back.ID)
	}
	assert.Equal(t, []string{"0", "1", "0", "1"}, ids, "claude alias must round-robin between its two backends")
}

func TestGetNextBackendForModel_SkipsSaturatedWithinAlias(t *testing.T) {
	pool := testPool(
		testBackend("0", "claude", fakeDeadUsage{}),
		testBackend("1", "claude", fakeGetUsage{}),
	)
	back, err := pool.GetNextBackendForModel("claude")
	require.NoError(t, err)
	assert.Equal(t, "1", back.ID, "must skip the saturated backend in the alias group")
}

func TestGetNextBackendForModel_FallsBackToRoundRobinOnUnknownAlias(t *testing.T) {
	pool := testPool(
		testBackend("0", "claude", fakeGetUsage{}),
		testBackend("1", "gpt", fakeGetUsage{}),
	)
	back, err := pool.GetNextBackendForModel("unknown")
	require.NoError(t, err)
	require.NotNil(t, back)
	assert.Contains(t, []string{"0", "1"}, back.ID)
}

func TestGetNextBackendForModel_EmptyAliasUsesRoundRobin(t *testing.T) {
	pool := testPool(
		testBackend("0", "claude", fakeGetUsage{}),
		testBackend("1", "gpt", fakeGetUsage{}),
	)
	back, err := pool.GetNextBackendForModel("")
	require.NoError(t, err)
	require.NotNil(t, back)
}

// fakeAliveUsage reports a backend with capacity remaining.
type fakeAliveUsage struct{}

func (fakeAliveUsage) Execute(ctx context.Context, id string) (*domain.ModelUsageQuota, error) {
	return &domain.ModelUsageQuota{RemainingRequests: 100, RemainingTokens: 100}, nil
}

// fakeSaveUsage discards usage writes.
type fakeSaveUsage struct{}

func (fakeSaveUsage) Execute(ctx context.Context, in domain.ModelUsageQuota) error { return nil }

// fakePublisher discards published requests.
type fakePublisher struct{}

func (fakePublisher) PublishRequest(ctx context.Context, info domain.DetailedRequestInfo) error {
	return nil
}

// settle gives freshly-spawned goroutines a moment to reach their park point so
// runtime.NumGoroutine() is stable before we sample it.
func settle() {
	for i := 0; i < 10; i++ {
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
}

// This is the regression test for the production memory leak. Before the fix,
// every NewBackendPool built its own QueueManager and started a QueueWorker
// goroutine per backend that nothing ever stopped. The config watcher rebuilt
// the pool every 30s, so the proxy leaked ~4MB/hour until the ECS task hit its
// 1024MB limit. With a shared QueueManager, rebuilds must add zero goroutines.
func TestBackendPool_RebuildDoesNotLeakGoroutines(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	cfgs := []BackendConfig{
		{Host: "api.openai.com", Model: "gpt-4o"},
		{Host: "api.anthropic.com", Model: "claude-sonnet-5", Mapper: "anthropic"},
	}
	save := fakeSaveUsage{}
	get := fakeAliveUsage{}
	tracker := NewRequestTracker(fakePublisher{})

	qm := NewQueueManager()
	defer qm.StopAll()

	// First build: this legitimately starts one worker per backend.
	_, err := NewBackendPool(save, get, cfgs, tracker, qm)
	require.NoError(t, err)
	settle()
	baseline := runtime.NumGoroutine()

	// Rebuild the way the config watcher does. Each rebuild must reuse the
	// existing per-backend queues and start no new goroutines.
	const rebuilds = 50
	for i := 0; i < rebuilds; i++ {
		_, err := NewBackendPool(save, get, cfgs, tracker, qm)
		require.NoError(t, err)
	}
	settle()

	grown := runtime.NumGoroutine() - baseline
	assert.LessOrEqual(t, grown, 2,
		"rebuilding the pool %d times leaked %d goroutines; a shared QueueManager must reuse queues",
		rebuilds, grown)
}

// Queues and usage windows are keyed by backend ID and outlive a config
// reload, so the ID must follow the backend's identity, not its position:
// reordering entries must not hand one backend's queue to another.
func TestBackendID_FollowsIdentityNotPosition(t *testing.T) {
	azure := BackendConfig{Host: "azure.example.com", Model: "gpt-4o"}
	bedrock := BackendConfig{Host: "bedrock.example.com", Model: "claude"}

	ids := func(cfgs ...BackendConfig) []string {
		seen := make(map[string]int)
		var out []string
		for _, c := range cfgs {
			out = append(out, backendID(c, seen))
		}
		return out
	}

	assert.Equal(t, []string{"gpt-4o@azure.example.com", "claude@bedrock.example.com"}, ids(azure, bedrock))
	assert.Equal(t, []string{"claude@bedrock.example.com", "gpt-4o@azure.example.com"}, ids(bedrock, azure),
		"reordering must keep each backend's ID")

	assert.Equal(t, []string{"gpt-4o@azure.example.com", "gpt-4o@azure.example.com#2"}, ids(azure, azure),
		"duplicate entries stay unique via an ordinal suffix")

	assert.Equal(t, []string{"plain.example.com"}, ids(BackendConfig{Host: "plain.example.com"}),
		"a model-less (passthrough) backend is identified by host alone")

	named := BackendConfig{Name: "azure-main", Host: "azure.example.com", Model: "gpt-4o"}
	assert.Equal(t, []string{"azure-main"}, ids(named),
		"a configured name replaces the derived model@host ID")

	assert.Equal(t, []string{"azure-main", "azure-main#2"}, ids(named, named),
		"duplicate names stay unique via the same ordinal suffix")

	assert.Equal(t, []string{"azure-main", "gpt-4o@azure.example.com"}, ids(named, azure),
		"a named backend does not collide with an unnamed one on the same upstream")
}
