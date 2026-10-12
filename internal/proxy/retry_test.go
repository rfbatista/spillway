package proxy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"proxy/internal/domain"
	"proxy/internal/mocks"
	"strings"
	"testing"

	"github.com/stretchr/testify/mock"
)

type fakeRoundTripper struct {
	count     int
	responses []roundTripResult
}

type roundTripResult struct {
	resp *http.Response
	err  error
}

func (f *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.count < len(f.responses) {
		result := f.responses[f.count]
		f.count++
		return result.resp, result.err
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("default")),
		Header:     http.Header{},
	}, nil
}

func dummyResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Header:     http.Header{},
	}
}

func TestRetryTransport_SuccessFirstAttempt(t *testing.T) {
	fakeRT := &fakeRoundTripper{
		responses: []roundTripResult{
			{resp: dummyResponse(200, "ok"), err: nil},
		},
	}

	saveUsage := mocks.NewMockSaveUsageService(t)
	getUsage := mocks.NewMockGetUsageService(t)
	saveUsage.On("Execute", mock.Anything, mock.Anything).Return(nil)

	queueManager := NewQueueManager()
	requestPublisher := mocks.NewMockRequestPublisher(t)
	requestPublisher.On("PublishRequest", mock.Anything, mock.Anything).Return(nil).Maybe()
	tracker := NewRequestTracker(requestPublisher)
	retryTransport, err := NewRetryTransport("test", "test-model", saveUsage, getUsage, queueManager, tracker)
	if err != nil {
		t.Fatalf("Error creating RetryTransport: %v", err)
	}
	retryTransport.Base = fakeRT

	req := httptest.NewRequest("GET", "http://example.com", nil)
	resp, err := retryTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
	if fakeRT.count != 1 {
		t.Errorf("Expected 1 attempt, got %d", fakeRT.count)
	}
}

func TestRetryTransport_RetryThenSuccess(t *testing.T) {
	// simulate failure on first two attempts then success
	fakeRT := &fakeRoundTripper{
		responses: []roundTripResult{
			{resp: dummyResponse(500, "server error"), err: nil},
			{resp: nil, err: errors.New("temporary error")},
			{resp: dummyResponse(200, "ok on third attempt"), err: nil},
		},
	}

	saveUsage := mocks.NewMockSaveUsageService(t)
	getUsage := mocks.NewMockGetUsageService(t)
	saveUsage.On("Execute", mock.Anything, mock.Anything).Return(nil)
	getUsage.On("Execute", mock.Anything, mock.Anything).Return(&domain.ModelUsageQuota{
		ResetRequests: 0,
		ResetTokens:   0,
	}, nil)

	queueManager := NewQueueManager()
	requestPublisher := mocks.NewMockRequestPublisher(t)
	requestPublisher.On("PublishRequest", mock.Anything, mock.Anything).Return(nil).Maybe()
	tracker := NewRequestTracker(requestPublisher)
	retryTransport, err := NewRetryTransport("test", "test-model", saveUsage, getUsage, queueManager, tracker)
	if err != nil {
		t.Fatalf("Error creating RetryTransport: %v", err)
	}
	retryTransport.Base = fakeRT

	reqBody := "test body"
	req := httptest.NewRequest("POST", "http://example.com", bytes.NewBufferString(reqBody))
	resp, err := retryTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if resp.StatusCode != 200 {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}
	if fakeRT.count != 3 {
		t.Errorf("Expected 3 attempts, got %d", fakeRT.count)
	}
}

func TestRetryTransport_NoRetriesNonRetryStatus(t *testing.T) {
	// simulate a scenario with a response that should not trigger a retry (e.g., 404)
	fakeRT := &fakeRoundTripper{
		responses: []roundTripResult{
			{resp: dummyResponse(404, "not found"), err: nil},
		},
	}

	saveUsage := mocks.NewMockSaveUsageService(t)
	getUsage := mocks.NewMockGetUsageService(t)
	saveUsage.On("Execute", mock.Anything, mock.Anything).Return(nil)

	queueManager := NewQueueManager()
	requestPublisher := mocks.NewMockRequestPublisher(t)
	requestPublisher.On("PublishRequest", mock.Anything, mock.Anything).Return(nil).Maybe()
	tracker := NewRequestTracker(requestPublisher)
	retryTransport, err := NewRetryTransport("test", "test-model", saveUsage, getUsage, queueManager, tracker)
	if err != nil {
		t.Fatalf("Error creating RetryTransport: %v", err)
	}
	retryTransport.Base = fakeRT

	req := httptest.NewRequest("GET", "http://example.com", nil)
	resp, err := retryTransport.RoundTrip(req)
	if err != nil {
		t.Fatalf("Expected no error, got %v", err)
	}
	if resp.StatusCode != 404 {
		t.Errorf("Expected status 404, got %d", resp.StatusCode)
	}
	if fakeRT.count != 1 {
		t.Errorf("Expected 1 attempt, got %d", fakeRT.count)
	}
}

func TestRetryTransport_SaveUsage_CapturesLimitHeaders(t *testing.T) {
	saveUsage := mocks.NewMockSaveUsageService(t)
	getUsage := mocks.NewMockGetUsageService(t)

	var capturedQuota domain.ModelUsageQuota
	saveUsage.On("Execute", mock.Anything, mock.MatchedBy(func(q domain.ModelUsageQuota) bool {
		capturedQuota = q
		return true
	})).Return(nil)

	queueManager := NewQueueManager()
	requestPublisher := mocks.NewMockRequestPublisher(t)
	requestPublisher.On("PublishRequest", mock.Anything, mock.Anything).Return(nil).Maybe()
	tracker := NewRequestTracker(requestPublisher)
	rt, err := NewRetryTransport("test-backend", "test-model", saveUsage, getUsage, queueManager, tracker)
	if err != nil {
		t.Fatalf("NewRetryTransport: %v", err)
	}

	resp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("")),
		Header: http.Header{
			"X-Ratelimit-Limit-Requests":     []string{"9600"},
			"X-Ratelimit-Limit-Tokens":       []string{"150000000"},
			"X-Ratelimit-Remaining-Requests": []string{"9000"},
			"X-Ratelimit-Remaining-Tokens":   []string{"140000000"},
			"X-Request-Id":                   []string{"ef803e6a-6cea-cf9e-aca0-e94bef8900b1"},
		},
	}

	rt.SaveUsage(resp, "test-request-id")

	if capturedQuota.LimitRequests != 9600 {
		t.Errorf("LimitRequests: want 9600, got %d", capturedQuota.LimitRequests)
	}
	if capturedQuota.LimitTokens != 150000000 {
		t.Errorf("LimitTokens: want 150000000, got %d", capturedQuota.LimitTokens)
	}
	if capturedQuota.RemainingRequests != 9000 {
		t.Errorf("RemainingRequests: want 9000, got %d", capturedQuota.RemainingRequests)
	}
	if capturedQuota.RemainingTokens != 140000000 {
		t.Errorf("RemainingTokens: want 140000000, got %d", capturedQuota.RemainingTokens)
	}
}

func TestSaturationPct(t *testing.T) {
	cases := []struct {
		limit, remaining int64
		want             float64
	}{
		{9600, 9600, 0},
		{9600, 0, 100},
		{9600, 4800, 50},
		{0, 100, 0},   // unknown limit
		{100, 150, 0}, // remaining > limit (clamp to 0)
	}
	for _, tc := range cases {
		got := saturationPct(tc.limit, tc.remaining)
		if got != tc.want {
			t.Errorf("saturationPct(%d, %d) = %f, want %f", tc.limit, tc.remaining, got, tc.want)
		}
	}
}
