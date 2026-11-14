package releasefetch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestClassifyStableErrorEnvelope(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		header     http.Header
		code       string
		retryable  bool
		nextAction string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, code: "release_access_denied", nextAction: "configure_release_read_credential"},
		{name: "forbidden", status: http.StatusForbidden, header: http.Header{"X-RateLimit-Remaining": []string{"12"}}, code: "release_access_denied", nextAction: "configure_release_read_credential"},
		{name: "too many requests", status: http.StatusTooManyRequests, header: http.Header{"Retry-After": []string{"7"}}, code: "release_rate_limited", retryable: true, nextAction: "wait_or_configure_release_read_credential"},
		{name: "rate limit without wait", status: http.StatusTooManyRequests, code: "release_rate_limited", retryable: false, nextAction: "wait_or_configure_release_read_credential"},
		{name: "upstream", status: http.StatusBadGateway, code: "release_network_error", retryable: true, nextAction: "retry_update"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err, ok := classify(&http.Response{StatusCode: tc.status, Header: tc.header}).(*Error)
			if !ok || err.Code != tc.code || err.Retryable != tc.retryable || err.NextAction != tc.nextAction || err.HTTPStatus != tc.status {
				t.Fatalf("classified error = %#v", err)
			}
			payload, marshalErr := json.Marshal(err)
			if marshalErr != nil || !strings.Contains(string(payload), `"http_status"`) || !strings.Contains(string(payload), `"request_id"`) || !strings.Contains(string(payload), `"retry_after_seconds"`) {
				t.Fatalf("unstable error envelope: %s (%v)", payload, marshalErr)
			}
		})
	}
}

func TestMetadataFromEndpointAnonymousAndPATHeaders(t *testing.T) {
	var gotAuth, gotAccept, gotAgent string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAccept, gotAgent = r.Header.Get("Authorization"), r.Header.Get("Accept"), r.Header.Get("User-Agent")
		w.Header().Set("X-GitHub-Request-Id", "req-metadata")
		_, _ = io.WriteString(w, `{"tag_name":"v1.4.24","immutable":true}`)
	}))
	defer server.Close()

	response, err := metadataFromEndpoint(context.Background(), server.Client(), server.URL, "")
	if err != nil {
		t.Fatalf("anonymous metadata: %v", err)
	}
	response.Body.Close()
	if gotAuth != "" || gotAccept != "application/vnd.github+json" || gotAgent != "agent-vm-deploy" {
		t.Fatalf("anonymous headers auth=%q accept=%q agent=%q", gotAuth, gotAccept, gotAgent)
	}

	response, err = metadataFromEndpoint(context.Background(), server.Client(), server.URL, "test-token")
	if err != nil {
		t.Fatalf("PAT metadata: %v", err)
	}
	response.Body.Close()
	if gotAuth != "Bearer test-token" {
		t.Fatalf("PAT header = %q", gotAuth)
	}
}

func TestDownloadUsesBoundedAuthenticatedAssetRequest(t *testing.T) {
	var gotAuth, gotAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAccept = r.Header.Get("Authorization"), r.Header.Get("Accept")
		_, _ = io.WriteString(w, "bundle")
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "bundle.tar")
	if err := download(context.Background(), server.Client(), server.URL, path, "test-token", 1<<20); err != nil {
		t.Fatalf("download: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "bundle" {
		t.Fatalf("downloaded content = %q, err=%v", content, err)
	}
	if gotAuth != "Bearer test-token" || gotAccept != "application/octet-stream" {
		t.Fatalf("asset headers auth=%q accept=%q", gotAuth, gotAccept)
	}
}
