package webadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/management"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

func TestUnifiedTokenAPIIsIdempotentAndRevocable(t *testing.T) {
	authority, store, cfg := testAuthority(t)
	lifecycleService := lifecycle.New(store, testLifecycleConfig(cfg), lifecycle.Options{})
	lifecycleService.MarkReady()
	authority.Lifecycle = lifecycleService
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Lifecycle: lifecycleService, Store: store, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}})
	if err != nil {
		t.Fatal(err)
	}
	sessionRaw := "session-secret-for-unified-test"
	now := server.now()
	session := state.BrowserSession{
		SessionDigest: digestString(sessionRaw), Issuer: cfg.AdminPrincipal.Issuer, Subject: cfg.AdminPrincipal.Subject,
		CSRFSecretDigest: digestString(server.OIDC.csrfToken(digestString(sessionRaw))), Capabilities: cfg.AdminPrincipal.Capabilities,
		CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(15 * time.Minute).Format(time.RFC3339Nano),
	}
	if err := store.InsertBrowserSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: managementSessionCookie, Value: sessionRaw}
	makeRequest := func(method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.AddCookie(cookie)
		request.Header.Set("Origin", cfg.ExternalOrigin)
		request.Header.Set("X-CSRF-Token", server.OIDC.csrfToken(session.SessionDigest))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder
	}
	createBody := `{"operation_id":"50e8400e-ccf8-4c7b-8d1d-14f66c7fb126","scopes":["agent:read"],"expires_at":"` + now.Add(5*time.Minute).UTC().Format(time.RFC3339Nano) + `"}`
	created := makeRequest(http.MethodPost, "/api/v1/tokens", createBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var createdValue map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &createdValue); err != nil {
		t.Fatal(err)
	}
	rawToken, ok := createdValue["token"].(string)
	if !ok || len(rawToken) < 32 {
		t.Fatalf("created token = %#v", createdValue["token"])
	}
	metadata := createdValue["metadata"].(map[string]any)
	tokenID := metadata["token_id"].(string)
	targetVersion := metadata["resource_version"].(string)
	replayed := makeRequest(http.MethodPost, "/api/v1/tokens", createBody)
	if replayed.Code != http.StatusOK || strings.Contains(replayed.Body.String(), rawToken) || strings.Contains(replayed.Body.String(), `"token"`) {
		t.Fatalf("replay status/body=%d %s", replayed.Code, replayed.Body.String())
	}
	validAgent := httptest.NewRequest(http.MethodPost, "/agent/v1/control", strings.NewReader(`{"protocol":4,"action":"status","session_id":"agent-test"}`))
	validAgent.Header.Set("Authorization", "Bearer "+rawToken)
	validAgent.Header.Set("Content-Type", "application/json")
	validAgentRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(validAgentRecorder, validAgent)
	if validAgentRecorder.Code == http.StatusUnauthorized {
		t.Fatalf("valid Agent Token was rejected: %s", validAgentRecorder.Body.String())
	}
	preview := makeRequest(http.MethodPost, "/api/v1/tokens/"+tokenID+"/revoke/preview", `{"target_version":"`+targetVersion+`"}`)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	var previewValue struct {
		Confirmation string `json:"confirmation_token"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &previewValue); err != nil || previewValue.Confirmation == "" {
		t.Fatalf("preview=%s err=%v", preview.Body.String(), err)
	}
	revokeBody := `{"operation_id":"50e8400e-ccf8-4c7b-8d1d-14f66c7fb127","target_version":"` + targetVersion + `","confirmation_token":"` + previewValue.Confirmation + `"}`
	revoked := makeRequest(http.MethodPost, "/api/v1/tokens/"+tokenID+"/revoke", revokeBody)
	if revoked.Code != http.StatusAccepted {
		t.Fatalf("revoke status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	deniedAgent := httptest.NewRequest(http.MethodPost, "/agent/v1/control", strings.NewReader(`{"protocol":4,"action":"status","session_id":"agent-test"}`))
	deniedAgent.Header.Set("Authorization", "Bearer "+rawToken)
	deniedAgent.Header.Set("Content-Type", "application/json")
	deniedRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(deniedRecorder, deniedAgent)
	if deniedRecorder.Code != http.StatusUnauthorized || !strings.Contains(deniedRecorder.Body.String(), "token_revoked") {
		t.Fatalf("revoked Agent Token status/body=%d %s", deniedRecorder.Code, deniedRecorder.Body.String())
	}
}

func TestAgentSuccessResponseRedactsNativeDiagnostics(t *testing.T) {
	value := map[string]any{
		"result": map[string]any{
			"last_error": `{"phase":"native-init","cause":"/private/path and command output"}`,
			"nested":     []any{map[string]any{"last_error": "native_failed: private stderr"}},
		},
	}
	safe := safeAgentResponse(value).(map[string]any)
	encoded := jsonString(safe)
	if strings.Contains(encoded, "private") || strings.Contains(encoded, "native-init") || strings.Contains(encoded, "command output") {
		t.Fatalf("native diagnostic leaked: %s", encoded)
	}
	result := safe["result"].(map[string]any)
	lastError := result["last_error"].(map[string]any)
	if lastError["code"] != "internal_error" || lastError["phase"] != "internal" || lastError["retryable"] != false {
		t.Fatalf("safe diagnostic = %#v", lastError)
	}
}

func TestUnifiedTokenRevokeConfirmationCrossesSlots(t *testing.T) {
	authorityBlue, storeBlue, cfg := testAuthority(t)
	storeGreen, err := state.OpenForTest(storeBlue.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer storeGreen.Close()
	authorityGreen, err := NewAuthority(AuthorityOptions{Config: cfg, Management: management.New(storeGreen, management.Options{}), Store: storeGreen, ProductVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	serverBlue, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authorityBlue, Store: storeBlue, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}})
	if err != nil {
		t.Fatal(err)
	}
	serverGreen, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authorityGreen, Store: storeGreen, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}})
	if err != nil {
		t.Fatal(err)
	}
	sessionRaw := "session-secret-for-cross-slot-test"
	now := serverBlue.now()
	session := state.BrowserSession{
		SessionDigest: digestString(sessionRaw), Issuer: cfg.AdminPrincipal.Issuer, Subject: cfg.AdminPrincipal.Subject,
		CSRFSecretDigest: digestString(serverBlue.OIDC.csrfToken(digestString(sessionRaw))), Capabilities: cfg.AdminPrincipal.Capabilities,
		CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(15 * time.Minute).Format(time.RFC3339Nano),
	}
	if err := storeBlue.InsertBrowserSession(context.Background(), session); err != nil {
		t.Fatal(err)
	}
	makeRequest := func(server *UnifiedServer, method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		request.AddCookie(&http.Cookie{Name: managementSessionCookie, Value: sessionRaw})
		request.Header.Set("Origin", cfg.ExternalOrigin)
		request.Header.Set("X-CSRF-Token", server.OIDC.csrfToken(session.SessionDigest))
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, request)
		return recorder
	}
	createBody := `{"operation_id":"70e8400e-ccf8-4c7b-8d1d-14f66c7fb126","scopes":["agent:read"],"expires_at":"` + now.Add(5*time.Minute).UTC().Format(time.RFC3339Nano) + `"}`
	created := makeRequest(serverBlue, http.MethodPost, "/api/v1/tokens", createBody)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var createdValue struct {
		Metadata struct {
			TokenID         string `json:"token_id"`
			ResourceVersion string `json:"resource_version"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &createdValue); err != nil {
		t.Fatal(err)
	}
	preview := makeRequest(serverBlue, http.MethodPost, "/api/v1/tokens/"+createdValue.Metadata.TokenID+"/revoke/preview", `{"target_version":"`+createdValue.Metadata.ResourceVersion+`"}`)
	if preview.Code != http.StatusOK {
		t.Fatalf("preview status=%d body=%s", preview.Code, preview.Body.String())
	}
	var previewValue struct {
		Confirmation string `json:"confirmation_token"`
	}
	if err := json.Unmarshal(preview.Body.Bytes(), &previewValue); err != nil || previewValue.Confirmation == "" {
		t.Fatalf("preview=%s err=%v", preview.Body.String(), err)
	}
	revokeBody := `{"operation_id":"70e8400e-ccf8-4c7b-8d1d-14f66c7fb127","target_version":"` + createdValue.Metadata.ResourceVersion + `","confirmation_token":"` + previewValue.Confirmation + `"}`
	revoked := makeRequest(serverGreen, http.MethodPost, "/api/v1/tokens/"+createdValue.Metadata.TokenID+"/revoke", revokeBody)
	if revoked.Code != http.StatusAccepted {
		t.Fatalf("cross-slot revoke status=%d body=%s", revoked.Code, revoked.Body.String())
	}
	replay := makeRequest(serverBlue, http.MethodPost, "/api/v1/tokens/"+createdValue.Metadata.TokenID+"/revoke", `{"operation_id":"70e8400e-ccf8-4c7b-8d1d-14f66c7fb128","target_version":"`+createdValue.Metadata.ResourceVersion+`","confirmation_token":"`+previewValue.Confirmation+`"}`)
	if replay.Code == http.StatusAccepted {
		t.Fatalf("cross-slot confirmation replay status/body=%d %s", replay.Code, replay.Body.String())
	}
}

func TestUnifiedCapabilityMappingMatchesManagementContract(t *testing.T) {
	cases := []struct {
		method   string
		path     string
		required []string
	}{
		{http.MethodGet, "/operations", []string{"read"}},
		{http.MethodGet, "/operations/op-1", []string{"read"}},
		{http.MethodGet, "/overview", []string{"observe_admin"}},
		{http.MethodGet, "/vms", []string{"observe_admin"}},
		{http.MethodGet, "/images", []string{"observe_admin", "manage_images"}},
		{http.MethodGet, "/system", []string{"observe_admin"}},
		{http.MethodGet, "/audit", []string{"observe_admin"}},
		{http.MethodPost, "/vms/task/actions", []string{"operate"}},
		{http.MethodPost, "/images/default", []string{"manage_images"}},
		{http.MethodPost, "/system/maintenance", []string{"maintain_system"}},
	}
	for _, test := range cases {
		actual := requiredCapabilities(test.method, test.path)
		if len(actual) != len(test.required) {
			t.Fatalf("%s %s capabilities=%#v, want %#v", test.method, test.path, actual, test.required)
		}
		for _, value := range test.required {
			if !containsString(actual, value) {
				t.Fatalf("%s %s capabilities=%#v, missing %q", test.method, test.path, actual, value)
			}
		}
	}
	if hasAnyCapability([]string{"read"}, []string{"observe_admin"}) {
		t.Fatal("read capability unexpectedly satisfied observe_admin")
	}
	if !hasAnyCapability([]string{"manage_images"}, []string{"observe_admin", "manage_images"}) {
		t.Fatal("manage_images did not satisfy image observation access")
	}
}

func testLifecycleConfig(cfg config.ManagementConfig) config.Config {
	value := config.Defaults()
	value.StateDB = "test"
	value.Management = cfg
	return value
}
