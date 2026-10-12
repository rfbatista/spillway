package ports

import (
	"context"
	"proxy/internal/domain"
)

type GetUsageService interface {
	Execute(ctx context.Context, id string) (*domain.ModelUsageQuota, error)
}

type SaveUsageService interface {
	Execute(ctx context.Context, in domain.ModelUsageQuota) error
}

type UpdateUsageService interface {
	Execute(ctx context.Context, opts ...domain.ModelUsageQuotaOption) error
}
