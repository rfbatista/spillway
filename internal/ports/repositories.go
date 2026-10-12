package ports

import (
	"context"

	"proxy/internal/domain"
)

type ModelUsageRepository interface {
	SaveModel(ctx context.Context, m domain.ModelUsageQuota) error
	Get(ctx context.Context, id string) (*domain.ModelUsageQuota, error)
}
