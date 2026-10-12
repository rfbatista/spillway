package proxy

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"proxy/internal/apperr"
)

func TestReasoningBudget(t *testing.T) {
	tests := []struct {
		name       string
		effort     string
		maxTokens  int
		wantBudget int
		wantOn     bool
		wantErr    bool
	}{
		{name: "absent effort disables reasoning", effort: "", maxTokens: 4096},
		{name: "none disables reasoning", effort: "none", maxTokens: 4096},
		{name: "minimal disables reasoning", effort: "minimal", maxTokens: 4096},
		{name: "low", effort: "low", maxTokens: 4096, wantBudget: 1024, wantOn: true},
		{name: "medium", effort: "medium", maxTokens: 4096, wantBudget: 2048, wantOn: true},
		{name: "high", effort: "high", maxTokens: 8192, wantBudget: 4096, wantOn: true},
		{name: "unknown effort is rejected", effort: "extreme", maxTokens: 4096, wantErr: true},
		{name: "budget must fit in max_tokens", effort: "high", maxTokens: 4096, wantErr: true},
		{name: "budget equal to max_tokens is rejected", effort: "low", maxTokens: 1024, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			budget, on, err := reasoningBudget(tt.effort, tt.maxTokens)
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, apperr.ErrInvalidRequest)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantOn, on)
			assert.Equal(t, tt.wantBudget, budget)
		})
	}
}
