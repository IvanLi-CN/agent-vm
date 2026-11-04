package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/observe"
)

func TestPublicStatusIsAnonymousAndSanitized(t *testing.T) {
	authority, store, cfg := testAuthority(t)
	now := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	authority.Clock = func() time.Time { return now }
	authority.Snapshots = observe.NewSnapshotStore(t.TempDir())
	if err := authority.Snapshots.Publish("capacity", map[string]any{
		"observed_at":     now.Add(-10 * time.Second).Format(time.RFC3339Nano),
		"up":              true,
		"admission_ready": true,
		"draining":        false,
		"resources": map[string]any{
			"remaining": map[string]any{"cpu": 2, "memory_mib": 1024},
		},
		"ct_cpu":      map[string]any{"capacity_vcpu": 4},
		"task_states": map[string]any{"active": 2},
	}); err != nil {
		t.Fatal(err)
	}
	server, err := NewUnifiedServer(UnifiedOptions{
		Config: cfg, Authority: authority, Store: store, Snapshots: authority.Snapshots,
		Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}, Clock: authority.Clock,
	})
	if err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("public status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "public, max-age=15, stale-while-revalidate=45" {
		t.Fatalf("cache-control=%q", got)
	}
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["state"] != "available" || value["active_sessions"] != float64(2) || value["observation_state"] != "available" {
		t.Fatalf("public status=%#v", value)
	}
	for _, forbidden := range []string{"capacity", "pressure", "resource_version", "issuer", "subject", "capabilities"} {
		if _, ok := value[forbidden]; ok {
			t.Fatalf("public status leaked %q: %#v", forbidden, value)
		}
	}

	managementRequest := httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil)
	managementRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(managementRecorder, managementRequest)
	if managementRecorder.Code != http.StatusUnauthorized || !strings.Contains(managementRecorder.Body.String(), "authentication_required") {
		t.Fatalf("unauthenticated overview status/body=%d %s", managementRecorder.Code, managementRecorder.Body.String())
	}
	methodRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(methodRecorder, httptest.NewRequest(http.MethodPost, "/api/v1/public/status", strings.NewReader(`{}`)))
	if methodRecorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("public status method status=%d body=%s", methodRecorder.Code, methodRecorder.Body.String())
	}
}
