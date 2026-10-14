package quote

import (
	"context"
	"errors"
	"github.com/rfbatista/spillway/internal/domain"
	"github.com/rfbatista/spillway/internal/ports"
)

type GetUsage struct {
	repo ports.ModelUsageRepository
	brd  ports.Broadcaster
}

func NewGetUsageService(
	repo ports.ModelUsageRepository,
	brd ports.Broadcaster,
) (*GetUsage, error) {
	if repo == nil {
		return nil, errors.New("mising model repository")
	}
	if brd == nil {
		return nil, errors.New("mising broadcaster")
	}
	return &GetUsage{
		repo: repo,
		brd:  brd,
	}, nil
}

func (s *GetUsage) Execute(ctx context.Context, id string) (*domain.ModelUsageQuota, error) {
	return s.repo.Get(ctx, id)
}
