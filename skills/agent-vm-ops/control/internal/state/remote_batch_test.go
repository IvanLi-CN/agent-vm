package state

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestRemoteQueryBatchRequiresOneStrongTransactionAndCompleteResults(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		wantError      bool
	}{
		{"complete", `{"results":[{"columns":["value"],"types":["text"],"values":[["first"]]},{"columns":["value"],"types":["text"]}]}`, false},
		{"truncated", `{"results":[{"columns":["value"],"types":["text"]}]}`, true},
		{"statement-error", `{"results":[{"columns":["value"],"types":["text"]},{"error":"no such table"}]}`, true},
		{"null-values", `{"results":[{"columns":["value"],"types":["text"]},{"columns":["value"],"types":["text"],"values":null}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/db/query" || r.URL.Query().Get("level") != "strong" || !r.URL.Query().Has("transaction") {
					t.Errorf("unsafe batch request: %s %s", r.Method, r.URL)
				}
				var statements []string
				if err := json.NewDecoder(r.Body).Decode(&statements); err != nil || len(statements) != 2 || statements[1] != "SELECT * FROM second" {
					t.Errorf("statements=%v err=%v", statements, err)
				}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(tc.response))
			}))
			defer server.Close()
			endpoint, _ := url.Parse(server.URL)
			backend := &remoteBackend{base: endpoint, client: server.Client()}
			result, err := backend.queryBatch(context.Background(), []string{"SELECT * FROM first", "SELECT * FROM second"})
			if (err != nil) != tc.wantError {
				t.Fatalf("result=%v err=%v", result, err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
			if !tc.wantError && (result[0].Values[0][0] != "first" || result[1].Values == nil) {
				t.Fatalf("invalid complete batch: %v", result)
			}
		})
	}
}
