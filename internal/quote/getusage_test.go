package quote_test

import (
	"context"
	"github.com/rfbatista/spillway/internal/domain"
	"github.com/rfbatista/spillway/internal/mocks"
	"github.com/rfbatista/spillway/internal/quote"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
)

func TestNewGetUsageService_NilRepo(t *testing.T) {
	broad := new(mocks.MockBroadcaster)
	// Passing nil for repository should return an error.
	_, err := quote.NewGetUsageService(nil, broad)
	if err == nil || !strings.Contains(err.Error(), "mising model repository") {
		t.Errorf("expected error for nil repository, got: %v", err)
	}
}

func TestNewGetUsageService_NilBroadcaster(t *testing.T) {
	repo := new(mocks.MockModelUsageRepository)
	// Passing nil for broadcaster should return an error.
	_, err := quote.NewGetUsageService(repo, nil)
	if err == nil || !strings.Contains(err.Error(), "mising broadcaster") {
		t.Errorf("expected error for nil broadcaster, got: %v", err)
	}
}

func TestExecute_ReturnsQuota_WithMocks(t *testing.T) {
	// Create a mock for the repository.
	repoMock := mocks.NewMockModelUsageRepository(t)
	// Create a mock for the broadcaster.
	brdMock := mocks.NewMockBroadcaster(t)

	id := "test-id"
	expectedQuota := &domain.ModelUsageQuota{
		ID:                id,
		RemainingTokens:   100,
		RemainingRequests: 50,
		Requested:         false,
	}

	// Set expectation: repo.Get will be called with any context and "test-id" and return expectedQuota and no error.
	repoMock.EXPECT().Get(mock.Anything, id).Return(expectedQuota, nil)

	// In this service, Execute does not call the broadcaster.
	// But if desired, you can set an expectation that PublishRemainingToken is not called:
	// brdMock.EXPECT().PublishRemainingToken(mock.Anything, mock.Anything).Maybe()

	// Create the service with the mocks.
	svc, err := quote.NewGetUsageService(repoMock, brdMock)
	if err != nil {
		t.Fatalf("failed to create service: %v", err)
	}

	// Execute the service.
	quota, err := svc.Execute(context.Background(), id)
	if err != nil {
		t.Fatalf("unexpected error during Execute: %v", err)
	}
	if quota == nil {
		t.Fatalf("expected non-nil quota")
	}

	// Verify the fields.
	if quota.ID != expectedQuota.ID {
		t.Errorf("expected quota ID %q; got %q", expectedQuota.ID, quota.ID)
	}
	if quota.RemainingTokens != expectedQuota.RemainingTokens {
		t.Errorf("expected RemainingTokens %d; got %d", expectedQuota.RemainingTokens, quota.RemainingTokens)
	}
	if quota.RemainingRequests != expectedQuota.RemainingRequests {
		t.Errorf("expected RemainingRequests %d; got %d", expectedQuota.RemainingRequests, quota.RemainingRequests)
	}
}
