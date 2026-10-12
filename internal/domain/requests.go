package domain

import (
	"fmt"
	"time"
)

// RequestInfo contém todas as informações sobre uma request processada
type RequestInfo struct {
	ID        string `json:"id"`
	BackendID string `json:"backend_id"`
	// Model is the backend's configured upstream model that served this request
	// (from BackendConfig.Model), not the model field of the client payload.
	Model             string            `json:"model"`
	Method            string            `json:"method"`
	URL               string            `json:"url"`
	Path              string            `json:"path"`
	Headers           map[string]string `json:"headers"`
	RequestBody       string            `json:"request_body"`
	ResponseBody      string            `json:"response_body"`
	StatusCode        int               `json:"status_code"`
	ResponseHeaders   map[string]string `json:"response_headers"`
	StartTime         time.Time         `json:"start_time"`
	EndTime           time.Time         `json:"end_time"`
	Duration          time.Duration     `json:"duration"`
	DurationFormatted string            `json:"formatted_duration"`
	RetryCount        int               `json:"retry_count"`
	Error             string            `json:"error,omitempty"`
	ClientIP          string            `json:"client_ip"`
	UserAgent         string            `json:"user_agent"`
	// ClientTag is the value of the configured client_tag_header request header
	// (BackendConfig.ClientTagHeader), or the literal "untagged" when there is
	// none — no client_tag_header configured, or the client sent it empty. It is
	// never empty and has no omitempty, so this field is always readable and
	// always says something, here and on the request_trace log lines alike.
	ClientTag string `json:"client_tag"`
}

// FormattedDuration returns the duration formatted as "XXsXXms"
func (r *RequestInfo) FormattedDuration() string {
	if r.Duration == 0 {
		return "0ms"
	}

	seconds := int(r.Duration.Seconds())
	milliseconds := int((r.Duration % time.Second).Nanoseconds() / 1000000)

	if seconds > 0 {
		return fmt.Sprintf("%ds%03dms", seconds, milliseconds)
	}
	return fmt.Sprintf("%dms", milliseconds)
}

// RequestAttempt representa uma tentativa individual de uma request
type RequestAttempt struct {
	AttemptNumber     int           `json:"attempt_number"`
	StartTime         time.Time     `json:"start_time"`
	EndTime           time.Time     `json:"end_time"`
	Duration          time.Duration `json:"duration"`
	DurationFormatted string        `json:"formatted_duration"`
	StatusCode        int           `json:"status_code"`
	Error             string        `json:"error,omitempty"`
	ResponseBody      string        `json:"response_body"`
}

// FormattedDuration returns the duration formatted as "XXsXXms"
func (r *RequestAttempt) FormattedDuration() string {
	if r.Duration == 0 {
		return "0ms"
	}

	seconds := int(r.Duration.Seconds())
	milliseconds := int((r.Duration % time.Second).Nanoseconds() / 1000000)

	if seconds > 0 {
		return fmt.Sprintf("%ds%03dms", seconds, milliseconds)
	}
	return fmt.Sprintf("%dms", milliseconds)
}

// DetailedRequestInfo contém informações detalhadas incluindo tentativas individuais
type DetailedRequestInfo struct {
	RequestInfo
	Attempts []RequestAttempt `json:"attempts"`
}
