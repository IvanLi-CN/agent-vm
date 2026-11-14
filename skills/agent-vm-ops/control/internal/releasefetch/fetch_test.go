package releasefetch

import (
	"net/http"
	"testing"
)

func TestClassifyAnonymousRateLimit(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"X-Ratelimit-Remaining": []string{"0"}, "X-Ratelimit-Reset": []string{"4102444800"}, "X-Github-Request-Id": []string{"req-1"}}}
	err, ok := classify(response).(*Error)
	if !ok || err.Code != "release_rate_limited" || !err.Retryable || err.RequestID != "req-1" || err.RetryAfterSeconds == nil {
		t.Fatalf("rate limit error = %#v", classify(response))
	}
}

func TestClassifyAccessDeniedDoesNotRetry(t *testing.T) {
	response := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"X-Ratelimit-Remaining": []string{"12"}}}
	err, ok := classify(response).(*Error)
	if !ok || err.Code != "release_access_denied" || err.Retryable || err.NextAction != "configure_release_read_credential" {
		t.Fatalf("access denied error = %#v", classify(response))
	}
}

func TestClassifyNotFound(t *testing.T) {
	err, ok := classify(&http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header)}).(*Error)
	if !ok || err.Code != "release_not_found" || err.Retryable || err.NextAction != "check_version" {
		t.Fatalf("not found error = %#v", classify(&http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header)}))
	}
}
