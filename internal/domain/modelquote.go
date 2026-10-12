package domain

import (
	"strconv"
	"time"
)

type ModelUsageQuota struct {
	ID                string        `json:"id"`
	LimitRequests     int64         `json:"limit_requests"`
	LimitTokens       int64         `json:"limit_tokens"`
	RemainingTokens   int64         `json:"remaining_tokens"`
	RemainingRequests int64         `json:"remaining_requests"`
	ResetTokens       time.Duration `json:"reset_tokens"`
	ResetRequests     time.Duration `json:"reset_requests"`
	Requested         bool          `json:"requested"`
	ResetAt           time.Time     `json:"reset_at,omitempty"`
}

// ModelUsageQuotaOption defines an option function for modifying a ModelUsageQuota.
type ModelUsageQuotaOption func(*ModelUsageQuota)

// NewModelUsageQuota is a factory function for ModelUsageQuota using the optional pattern.
// It requires an id and accepts an arbitrary number of options.
func NewModelUsageQuota(id string, opts ...ModelUsageQuotaOption) ModelUsageQuota {
	quota := ModelUsageQuota{
		ID:                id,
		RemainingTokens:   0,
		RemainingRequests: 0,
		ResetTokens:       0,
		ResetRequests:     0,
		Requested:         true,
	}
	for _, opt := range opts {
		opt(&quota)
	}
	return quota
}

// WithRemainingTokens sets the RemainingTokens field.
func WithRemainingTokens(tokens int64) ModelUsageQuotaOption {
	return func(m *ModelUsageQuota) {
		m.RemainingTokens = tokens
	}
}

func WithRequested(isRequested bool) ModelUsageQuotaOption {
	return func(m *ModelUsageQuota) {
		m.Requested = isRequested
	}
}

// WithRemainingRequests sets the RemainingRequests field.
func WithRemainingRequests(requests int64) ModelUsageQuotaOption {
	return func(m *ModelUsageQuota) {
		m.RemainingRequests = requests
	}
}

// WithResetTokens sets the ResetTokens field.
// It now accepts a string (e.g. "6m0s") which is parsed into a time.Duration.
func WithResetTokens(durationStr string) ModelUsageQuotaOption {
	return func(m *ModelUsageQuota) {
		d, err := time.ParseDuration(durationStr)
		if err != nil {
			if durationStr != "" {
				durationInt, _ := strconv.ParseInt(durationStr, 10, 0)
				d = time.Duration(durationInt) * time.Second
			} else {
				d = 0
			}
		}
		m.ResetTokens = d
		if d > 0 {
			newResetAt := time.Now().Add(d)
			if m.ResetAt.IsZero() || newResetAt.Before(m.ResetAt) {
				m.ResetAt = newResetAt
			}
		}
	}
}

// WithResetRequests sets the ResetRequests field.
// It now accepts a string (e.g. "6m0s") which is parsed into a time.Duration.
func WithResetRequests(durationStr string) ModelUsageQuotaOption {
	return func(m *ModelUsageQuota) {
		d, err := time.ParseDuration(durationStr)
		if err != nil {
			if durationStr != "" {
				durationInt, _ := strconv.ParseInt(durationStr, 10, 0)
				d = time.Duration(durationInt) * time.Second
			} else {
				d = 0
			}
		}
		m.ResetRequests = d
		if d > 0 {
			newResetAt := time.Now().Add(d)
			if m.ResetAt.IsZero() || newResetAt.Before(m.ResetAt) {
				m.ResetAt = newResetAt
			}
		}
	}
}

// WithLimitRequests sets the LimitRequests field.
func WithLimitRequests(limit int64) ModelUsageQuotaOption {
	return func(m *ModelUsageQuota) {
		m.LimitRequests = limit
	}
}

// WithLimitTokens sets the LimitTokens field.
func WithLimitTokens(limit int64) ModelUsageQuotaOption {
	return func(m *ModelUsageQuota) {
		m.LimitTokens = limit
	}
}
