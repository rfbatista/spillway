package ports

import (
	"context"
	"github.com/rfbatista/spillway/internal/domain"
)

type Broadcaster interface {
	PublishRemainingToken(context.Context, domain.ModelUsageQuota) error
}

// RequestPublisher broadcasts completed request information to subscribers,
// such as the live request dashboard. Implementations must be non-blocking so
// that a slow or absent subscriber never stalls request processing.
type RequestPublisher interface {
	PublishRequest(context.Context, domain.DetailedRequestInfo) error
}
