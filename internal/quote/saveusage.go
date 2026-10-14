package quote

import (
	"context"
	"errors"
	"github.com/rfbatista/spillway/internal/domain"
	"github.com/rfbatista/spillway/internal/ports"
)

type SaveUsage struct {
	repo ports.ModelUsageRepository
	brd  ports.Broadcaster
}

func NewSaveUsageService(
	repo ports.ModelUsageRepository,
	brd ports.Broadcaster,
) (*SaveUsage, error) {
	if repo == nil {
		return nil, errors.New("mising model repository")
	}
	if brd == nil {
		return nil, errors.New("mising broadcaster")
	}
	return &SaveUsage{
		repo: repo,
		brd:  brd,
	}, nil
}

func (s *SaveUsage) Execute(ctx context.Context, in domain.ModelUsageQuota) error {
	err := s.repo.SaveModel(ctx, in)
	go func(tokens domain.ModelUsageQuota) {
		s.brd.PublishRemainingToken(ctx, tokens)
	}(in)
	return err
}
