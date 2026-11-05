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
		"observed_at":            now.Add(-10 * time.Second).Format(time.RFC3339Nano),
		"up":                     true,
		"admission_ready":        true,
		"draining":               false,
		"observation_consistent": true,
		"default_admission":      map[string]any{"ready": true},
		"resources": map[string]any{
			"remaining": map[string]any{"cpu": 2, "memory_mib": 1024},
		},
		"ct_cpu":      map[string]any{"capacity_vcpu": 4},
		"task_states": completeTaskStates(2),
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

func TestPublicStatusReturnsUnknownWhenCapacitySnapshotIsUnavailable(t *testing.T) {
	authority, store, cfg := testAuthority(t)
	authority.Clock = func() time.Time { return time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC) }
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Store: store, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}, Clock: authority.Clock})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("missing public snapshot status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["state"] != "unknown" || value["observation_state"] != "unverified" || value["active_sessions"] != nil || value["observed_at"] != nil {
		t.Fatalf("missing public snapshot=%#v", value)
	}
}

func completeTaskStates(active int) map[string]any {
	return map[string]any{"active": active, "preparing": 0, "cached": 0, "closing": 0, "deleting": 0, "failed": 0, "absent": 0}
}

func TestPublicStatusDoesNotReportMalformedTaskStatesAsHealthy(t *testing.T) {
	authority, store, cfg := testAuthority(t)
	now := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	authority.Clock = func() time.Time { return now }
	authority.Snapshots = observe.NewSnapshotStore(t.TempDir())
	if err := authority.Snapshots.Publish("capacity", map[string]any{
		"observed_at":     now.Add(-10 * time.Second).Format(time.RFC3339Nano),
		"up":              true,
		"admission_ready": true,
		"task_states":     map[string]any{"active": "not-a-count", "preparing": 0, "cached": 0, "closing": 0, "deleting": 0, "failed": 0, "absent": 0},
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

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("public status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["state"] != "unknown" || value["active_sessions"] != nil || value["observation_state"] != "unverified" {
		t.Fatalf("malformed public status=%#v", value)
	}
}

func TestPublicStatusRedactsActiveSessionsFromStaleSnapshots(t *testing.T) {
	authority, store, cfg := testAuthority(t)
	now := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	authority.Clock = func() time.Time { return now }
	authority.Snapshots = observe.NewSnapshotStore(t.TempDir())
	if err := authority.Snapshots.Publish("capacity", map[string]any{
		"observed_at":            now.Add(-2 * time.Minute).Format(time.RFC3339Nano),
		"up":                     true,
		"admission_ready":        true,
		"draining":               false,
		"observation_consistent": true,
		"task_states":            completeTaskStates(2),
	}); err != nil {
		t.Fatal(err)
	}
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Store: store, Snapshots: authority.Snapshots, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}, Clock: authority.Clock})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil))
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["state"] != "unknown" || value["active_sessions"] != nil || value["observation_state"] != "stale" {
		t.Fatalf("stale public status=%#v", value)
	}
}

func TestPublicStatusNullsMalformedObservedAt(t *testing.T) {
	authority, store, cfg := testAuthority(t)
	authority.Clock = func() time.Time { return time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC) }
	authority.Snapshots = observe.NewSnapshotStore(t.TempDir())
	if err := authority.Snapshots.Publish("capacity", map[string]any{
		"observed_at":            "not-a-timestamp",
		"up":                     true,
		"admission_ready":        true,
		"draining":               false,
		"observation_consistent": true,
		"task_states":            completeTaskStates(2),
	}); err != nil {
		t.Fatal(err)
	}
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Store: store, Snapshots: authority.Snapshots, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}, Clock: authority.Clock})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil))
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["observed_at"] != nil || value["state"] != "unknown" || value["active_sessions"] != nil {
		t.Fatalf("malformed observed_at public status=%#v", value)
	}
}

func TestPublicStatusNullsFutureObservedAt(t *testing.T) {
	authority, store, cfg := testAuthority(t)
	now := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	authority.Clock = func() time.Time { return now }
	authority.Snapshots = observe.NewSnapshotStore(t.TempDir())
	if err := authority.Snapshots.Publish("capacity", map[string]any{
		"observed_at":            now.Add(time.Second).Format(time.RFC3339Nano),
		"up":                     true,
		"admission_ready":        true,
		"draining":               false,
		"observation_consistent": true,
		"task_states":            completeTaskStates(2),
	}); err != nil {
		t.Fatal(err)
	}
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Store: store, Snapshots: authority.Snapshots, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}, Clock: authority.Clock})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil))
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["observed_at"] != nil || value["state"] != "unknown" || value["active_sessions"] != nil {
		t.Fatalf("future observed_at public status=%#v", value)
	}
}

func TestPublicStatusFailsClosedWhenNativeUpIsMalformed(t *testing.T) {
	authority, store, cfg := testAuthority(t)
	now := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	authority.Clock = func() time.Time { return now }
	authority.Snapshots = observe.NewSnapshotStore(t.TempDir())
	if err := authority.Snapshots.Publish("capacity", map[string]any{
		"observed_at":            now.Add(-10 * time.Second).Format(time.RFC3339Nano),
		"admission_ready":        true,
		"draining":               false,
		"observation_consistent": true,
		"task_states":            completeTaskStates(2),
	}); err != nil {
		t.Fatal(err)
	}
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Store: store, Snapshots: authority.Snapshots, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}, Clock: authority.Clock})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil))
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["state"] != "unknown" || value["observation_state"] != "unverified" || value["active_sessions"] != nil {
		t.Fatalf("malformed native up public status=%#v", value)
	}
}

func TestPublicStatusFailsClosedForMalformedDefaultAdmission(t *testing.T) {
	for name, admission := range map[string]any{"missing": nil, "wrong_type": "ready", "missing_ready": map[string]any{}} {
		t.Run(name, func(t *testing.T) {
			authority, store, cfg := testAuthority(t)
			now := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
			authority.Clock = func() time.Time { return now }
			authority.Snapshots = observe.NewSnapshotStore(t.TempDir())
			metrics := map[string]any{"observed_at": now.Add(-10 * time.Second).Format(time.RFC3339Nano), "up": true, "admission_ready": true, "draining": false, "observation_consistent": true, "task_states": completeTaskStates(2)}
			if admission != nil {
				metrics["default_admission"] = admission
			}
			if err := authority.Snapshots.Publish("capacity", metrics); err != nil {
				t.Fatal(err)
			}
			server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Store: store, Snapshots: authority.Snapshots, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}, Clock: authority.Clock})
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil))
			var value map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
				t.Fatal(err)
			}
			if value["state"] != "unknown" || value["observation_state"] != "unverified" || value["active_sessions"] != nil {
				t.Fatalf("malformed default admission=%#v", value)
			}
		})
	}
}

func TestPublicStatusFailsClosedForInvalidDrainingObservation(t *testing.T) {
	for name, draining := range map[string]any{"missing": nil, "wrong_type": "false"} {
		t.Run(name, func(t *testing.T) {
			authority, store, cfg := testAuthority(t)
			now := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
			authority.Clock = func() time.Time { return now }
			authority.Snapshots = observe.NewSnapshotStore(t.TempDir())
			metrics := map[string]any{
				"observed_at":     now.Add(-10 * time.Second).Format(time.RFC3339Nano),
				"up":              true,
				"admission_ready": true,
				"task_states":     completeTaskStates(2),
			}
			if draining != nil {
				metrics["draining"] = draining
			}
			if err := authority.Snapshots.Publish("capacity", metrics); err != nil {
				t.Fatal(err)
			}
			server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Store: store, Snapshots: authority.Snapshots, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}, Clock: authority.Clock})
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil))
			var value map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
				t.Fatal(err)
			}
			if value["state"] != "unknown" || value["active_sessions"] != nil || value["observation_state"] != "unverified" {
				t.Fatalf("invalid draining public status=%#v", value)
			}
		})
	}
}

func TestPublicStatusDoesNotReportIncompleteTaskStatesAsHealthy(t *testing.T) {
	authority, store, cfg := testAuthority(t)
	authority.Clock = func() time.Time { return time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC) }
	authority.Snapshots = observe.NewSnapshotStore(t.TempDir())
	if err := authority.Snapshots.Publish("capacity", map[string]any{
		"observed_at": authority.Clock().Add(-10 * time.Second).Format(time.RFC3339Nano), "up": true, "admission_ready": true,
		"task_states": map[string]any{"active": 2},
	}); err != nil {
		t.Fatal(err)
	}
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Store: store, Snapshots: authority.Snapshots, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}, Clock: authority.Clock})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil))
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["state"] != "unknown" || value["active_sessions"] != nil || value["observation_state"] != "unverified" {
		t.Fatalf("incomplete public status=%#v", value)
	}
}

func TestPublicStatusDoesNotAcceptOutOfRangeTaskStateCount(t *testing.T) {
	authority, store, cfg := testAuthority(t)
	authority.Clock = func() time.Time { return time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC) }
	authority.Snapshots = observe.NewSnapshotStore(t.TempDir())
	if err := authority.Snapshots.Publish("capacity", map[string]any{
		"observed_at": authority.Clock().Add(-10 * time.Second).Format(time.RFC3339Nano), "up": true, "admission_ready": true,
		"task_states": map[string]any{"active": float64(9223372036854775808), "preparing": 0, "cached": 0, "closing": 0, "deleting": 0, "failed": 0, "absent": 0},
	}); err != nil {
		t.Fatal(err)
	}
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Store: store, Snapshots: authority.Snapshots, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}, Clock: authority.Clock})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil))
	var value map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value["state"] != "unknown" || value["active_sessions"] != nil || value["observation_state"] != "unverified" {
		t.Fatalf("out-of-range public status=%#v", value)
	}
}
