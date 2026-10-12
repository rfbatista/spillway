// Package apperr Application errors
package apperr

import "errors"

// ErrRetryQueueFull is returned when the per-backend retry queue cannot accept
// another request (buffer saturated) or when a deferred re-enqueue fails for the same reason.
var ErrRetryQueueFull = errors.New("retry queue full")

// ErrInvalidRequest marks a request the proxy refuses to forward because the
// caller's parameters cannot be mapped to the backend API. Errors wrapping it
// are answered with 400 and their message, so the caller can fix the request
// instead of debugging a downstream failure.
var ErrInvalidRequest = errors.New("invalid request")
