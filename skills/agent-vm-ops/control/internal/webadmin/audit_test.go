package webadmin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

func TestActorValueWrapsHistoricalActor(t *testing.T) {
	got := actorValue("legacy-admin")
	want := map[string]any{"kind": "legacy_unknown", "raw_actor": "legacy-admin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("actorValue() = %#v, want %#v", got, want)
	}
	if got := actorValue(rejectedActor("unauthorized")); !reflect.DeepEqual(got, map[string]any{"kind": "unauthorized"}) {
		t.Fatalf("rejected actorValue() = %#v", got)
	}
}

func TestUnifiedUnauthenticatedRequestRecordsTypedAudit(t *testing.T) {
	authority, store, cfg := testAuthority(t)
	lifecycleService := lifecycle.New(store, testLifecycleConfig(cfg), lifecycle.Options{})
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Lifecycle: lifecycleService, Store: store, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	audit, err := store.ListManagementAudit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 || audit[0].Actor != rejectedActor("unauthenticated") || audit[0].Outcome != "rejected" || audit[0].Detail["code"] != "authentication_required" {
		t.Fatalf("audit = %#v", audit)
	}
	if got := auditValue(audit[0])["actor"]; !reflect.DeepEqual(got, map[string]any{"kind": "unauthenticated"}) {
		t.Fatalf("audit actor = %#v", got)
	}
}

func TestUnifiedUnauthorizedRequestRecordsTypedAudit(t *testing.T) {
	authority, store, cfg := testAuthority(t)
	lifecycleService := lifecycle.New(store, testLifecycleConfig(cfg), lifecycle.Options{})
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Lifecycle: lifecycleService, Store: store, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}})
	if err != nil {
		t.Fatal(err)
	}
	sessionRaw := "session-secret-for-audit-test"
	now := server.now()
	session := state.BrowserSession{
		SessionDigest: digestString(sessionRaw), Issuer: cfg.AdminPrincipal.Issuer, Subject: cfg.AdminPrincipal.Subject,
		CSRFSecretDigest: digestString(server.OIDC.csrfToken(digestString(sessionRaw))), Capabilities: []string{"read"},
		CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(15 * time.Minute).Format(time.RFC3339Nano),
	}
	if err := store.InsertBrowserSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil)
	request.AddCookie(&http.Cookie{Name: managementSessionCookie, Value: sessionRaw})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	audit, err := store.ListManagementAudit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 || audit[0].Actor != rejectedActor("unauthorized") || audit[0].Outcome != "rejected" || audit[0].Detail["code"] != "permission_denied" {
		t.Fatalf("audit = %#v", audit)
	}
	if got := auditValue(audit[0])["actor"]; !reflect.DeepEqual(got, map[string]any{"kind": "unauthorized"}) {
		t.Fatalf("audit actor = %#v", got)
	}
	if strings.Contains(recorder.Body.String(), sessionRaw) {
		t.Fatal("rejection response leaked the session value")
	}
}

func TestUnifiedRejectedVMActionAuditPreservesTargetAndAction(t *testing.T) {
	authority, store, cfg := testAuthority(t)
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Store: store, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}})
	if err != nil {
		t.Fatal(err)
	}
	sessionRaw := "session-secret-for-vm-audit-test"
	now := server.now()
	session := state.BrowserSession{
		SessionDigest: digestString(sessionRaw), Issuer: cfg.AdminPrincipal.Issuer, Subject: cfg.AdminPrincipal.Subject,
		CSRFSecretDigest: digestString(server.OIDC.csrfToken(digestString(sessionRaw))), Capabilities: []string{"read"},
		CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(15 * time.Minute).Format(time.RFC3339Nano),
	}
	if err := store.InsertBrowserSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	taskID := "avm-audit-task"
	request := httptest.NewRequest(http.MethodPost, "/api/v1/vms/"+taskID+"/actions", strings.NewReader(`{"action":"restart","expected_generation":"1","target_version":"vm:`+taskID+`:revision-1","parameters":{}}`))
	request.AddCookie(&http.Cookie{Name: managementSessionCookie, Value: sessionRaw})
	request.Header.Set("Origin", cfg.ExternalOrigin)
	request.Header.Set("X-CSRF-Token", server.OIDC.csrfToken(session.SessionDigest))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	audit, err := store.ListManagementAudit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 || audit[0].Action != "restart" || audit[0].Target["kind"] != "vm" || audit[0].Target["id"] != taskID {
		t.Fatalf("audit = %#v", audit)
	}
}

func TestRejectedAuditDoesNotInvalidateMaintenancePreview(t *testing.T) {
	authority, store, cfg := testAuthority(t)
	authority.Management.Options.CurrentSystemVersion = func(ctx context.Context) (string, error) {
		epoch, err := store.StateEpoch(ctx)
		if err != nil {
			return "", err
		}
		return "system:" + epoch, nil
	}
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Store: store, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}})
	if err != nil {
		t.Fatal(err)
	}
	initialEpoch, err := store.StateEpoch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rejected := httptest.NewRecorder()
	server.Handler().ServeHTTP(rejected, httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil))
	if rejected.Code != http.StatusUnauthorized {
		t.Fatalf("rejected status=%d body=%s", rejected.Code, rejected.Body.String())
	}
	afterRejectedEpoch, err := store.StateEpoch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if afterRejectedEpoch != initialEpoch {
		t.Fatalf("rejected audit changed resource epoch: %q -> %q", initialEpoch, afterRejectedEpoch)
	}

	request := httptest.NewRequest(http.MethodPost, "/internal/v1/management/system/maintenance/preview", strings.NewReader(`{"action":"reconcile","target_version":"system:`+initialEpoch+`","parameters":{}}`))
	request = request.WithContext(WithPeerUID(request.Context(), cfg.WebUID))
	request.Header.Set(forwardedActorHeader, "admin@example")
	request.Header.Set("Content-Type", "application/json")
	preview := httptest.NewRecorder()
	authority.Handler().ServeHTTP(preview, request)
	if preview.Code != http.StatusOK {
		t.Fatalf("maintenance preview became stale after rejection: status=%d body=%s", preview.Code, preview.Body.String())
	}
}
