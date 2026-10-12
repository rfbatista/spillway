package proxy

import (
	"fmt"

	"proxy/internal/apperr"
)

// reasoningBudgets maps the OpenAI reasoning_effort levels onto Anthropic
// thinking budgets, in tokens. 1024 is the smallest budget Anthropic accepts.
var reasoningBudgets = map[string]int{
	"low":    1024,
	"medium": 2048,
	"high":   4096,
}

// reasoningBudget resolves an OpenAI reasoning_effort value into an Anthropic
// thinking budget. enabled is false when the caller did not ask for reasoning:
// "", "none" and "minimal" all leave it off.
//
// Anthropic requires max_tokens to be strictly greater than the thinking budget,
// since the budget is spent from the same allowance as the answer. An effort we
// cannot honour within maxTokens — or one we do not recognise — is reported as
// apperr.ErrInvalidRequest rather than silently downgraded, so a caller asking
// for reasoning never gets a non-reasoning answer back.
func reasoningBudget(effort string, maxTokens int) (budget int, enabled bool, err error) {
	switch effort {
	case "", "none", "minimal":
		return 0, false, nil
	}

	budget, ok := reasoningBudgets[effort]
	if !ok {
		return 0, false, fmt.Errorf("%w: unknown reasoning_effort %q", apperr.ErrInvalidRequest, effort)
	}
	if maxTokens <= budget {
		return 0, false, fmt.Errorf(
			"%w: reasoning_effort %q needs a thinking budget of %d tokens, so max_tokens must be greater than %d (got %d)",
			apperr.ErrInvalidRequest, effort, budget, budget, maxTokens,
		)
	}
	return budget, true, nil
}
