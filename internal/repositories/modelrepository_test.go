package repositories

import (
	"context"
	"sync"
	"testing"
	"time"

	"proxy/internal/domain"
)

func TestSaveAndGetModel(t *testing.T) {
	repo := NewInMemoryModelRepository()
	ctx := context.Background()

	quota := domain.ModelUsageQuota{
		ID:                "test-id",
		RemainingTokens:   100,
		RemainingRequests: 50,
		ResetTokens:       time.Minute,
		ResetRequests:     time.Minute,
		Requested:         true,
	}

	// Save the model quota.
	if err := repo.SaveModel(ctx, quota); err != nil {
		t.Fatalf("unexpected error saving model: %v", err)
	}

	// Retrieve the saved model quota.
	got, err := repo.Get(ctx, "test-id")
	if err != nil {
		t.Fatalf("unexpected error getting model: %v", err)
	}

	// Verify each field.
	if got.ID != "test-id" {
		t.Errorf("expected ID 'test-id'; got %s", got.ID)
	}
	if got.RemainingTokens != 100 {
		t.Errorf("expected RemainingTokens 100; got %d", got.RemainingTokens)
	}
	if got.RemainingRequests != 50 {
		t.Errorf("expected RemainingRequests 50; got %d", got.RemainingRequests)
	}
	if got.ResetTokens != time.Minute {
		t.Errorf("expected ResetTokens 1 minute; got %v", got.ResetTokens)
	}
	if got.ResetRequests != time.Minute {
		t.Errorf("expected ResetRequests 1 minute; got %v", got.ResetRequests)
	}
	if got.Requested != true {
		t.Errorf("expected Requested true; got %t", got.Requested)
	}
}

func TestGetNonExistingModel(t *testing.T) {
	repo := NewInMemoryModelRepository()
	ctx := context.Background()

	// Try to retrieve a model quota that wasn't saved.
	got, err := repo.Get(ctx, "non-existent")
	if err != nil {
		t.Fatalf("unexpected error getting model: %v", err)
	}

	// Expect zero-values from the in-memory map.
	if got.ID != "" {
		t.Errorf("expected empty ID; got %s", got.ID)
	}
	if got.RemainingTokens != 0 {
		t.Errorf("expected RemainingTokens 0; got %d", got.RemainingTokens)
	}
	if got.RemainingRequests != 0 {
		t.Errorf("expected RemainingRequests 0; got %d", got.RemainingRequests)
	}
	// The default boolean is false.
	if got.Requested != false {
		t.Errorf("expected Requested false; got %t", got.Requested)
	}
}

func TestConcurrentAccess(t *testing.T) {
	repo := NewInMemoryModelRepository()
	ctx := context.Background()
	var wg sync.WaitGroup
	numGoroutines := 10

	// Run multiple goroutines concurrently that save and retrieve the same model.
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			quota := domain.ModelUsageQuota{
				ID:                "concurrent",
				RemainingTokens:   int64(i),
				RemainingRequests: int64(i * 10),
				ResetTokens:       time.Duration(i) * time.Second,
				ResetRequests:     time.Duration(i) * time.Second,
				Requested:         i%2 == 0,
			}
			if err := repo.SaveModel(ctx, quota); err != nil {
				t.Errorf("goroutine %d encountered error on save: %v", i, err)
			}
			// Retrieve the quota to ensure no race conditions.
			if _, err := repo.Get(ctx, "concurrent"); err != nil {
				t.Errorf("goroutine %d encountered error on get: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
}
