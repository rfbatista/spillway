package repositories

import (
	"context"
	"proxy/internal/domain"
	"sync"
)

type ModelRepositoryInMemory struct {
	mu   sync.Mutex
	data map[string]domain.ModelUsageQuota
}

func NewInMemoryModelRepository() *ModelRepositoryInMemory {
	return &ModelRepositoryInMemory{
		mu:   sync.Mutex{},
		data: make(map[string]domain.ModelUsageQuota),
	}
}

func (repo *ModelRepositoryInMemory) SaveModel(ctx context.Context, m domain.ModelUsageQuota) error {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	repo.data[m.ID] = m
	return nil
}

func (repo *ModelRepositoryInMemory) Get(ctx context.Context, id string) (*domain.ModelUsageQuota, error) {
	repo.mu.Lock()
	defer repo.mu.Unlock()
	res := repo.data[id]
	return &res, nil
}
