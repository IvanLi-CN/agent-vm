package webadmin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/management"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

func testManagementConfig(t *testing.T) config.ManagementConfig {
	t.Helper()
	root := t.TempDir()
	return config.ManagementConfig{
		Enabled: true, ExternalOrigin: "https://console.example",
		OIDC:                 config.OIDCConfig{Issuer: "https://issuer.example", ClientID: "client", ClientSecret: "secret", RedirectURI: "https://console.example/api/v1/auth/oidc/callback"},
		AdminPrincipal:       config.AdminPrincipalConfig{Issuer: "https://issuer.example", Subject: "admin@example", Capabilities: []string{"read", "operate", "manage_images", "maintain_system", "manage_tokens", "observe_admin"}},
		ForwardAuth:          config.ForwardAuthConfig{SocketPath: filepath.Join(root, "web.sock"), PeerUID: 1001, IdentityHeader: "X-Forwarded-User"},
		AllowedAdminSubjects: []string{"admin@example"}, WebUID: 1002,
		SessionTTLSeconds: 900,
		Authority:         config.ManagementAuthorityConfig{SocketPath: filepath.Join(root, "authority.sock"), ExpectedWebUID: 1002},
	}
}

func testAuthority(t *testing.T) (*Authority, *state.Store, config.ManagementConfig) {
	t.Helper()
	root := t.TempDir()
	store, err := state.OpenForTest(filepath.Join(root, "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := strings.Repeat("a", 64)
	if err := store.SeedLegacyImage(context.Background(), fingerprint); err != nil {
		store.Close()
		t.Fatal(err)
	}
	cfg := testManagementConfig(t)
	managementService := management.New(store, management.Options{})
	authority, err := NewAuthority(AuthorityOptions{Config: cfg, Management: managementService, Store: store, ProductVersion: "test"})
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return authority, store, cfg
}

func TestAuthorityRejectsUntrustedPeer(t *testing.T) {
	authority, _, _ := testAuthority(t)
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/management/images", nil)
	request = request.WithContext(WithPeerUID(request.Context(), 9999))
	request.Header.Set(forwardedActorHeader, "admin@example")
	recorder := httptest.NewRecorder()
	authority.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "9999") {
		t.Fatalf("peer UID leaked in response: %s", recorder.Body.String())
	}
}

func TestSafeOperationErrorPreservesImportRecoveryEvidence(t *testing.T) {
	fingerprint := strings.Repeat("b", 64)
	value := safeOperationError(map[string]any{
		"code": "internal_error", "phase": "reconciliation", "retryable": true,
		"imported_fingerprint": fingerprint, "expected_fingerprint": fingerprint, "cleanup": "unverified",
	}, "operation-1", "reconciliation")
	fields, ok := value["safe_fields"].(map[string]any)
	if !ok || fields["imported_fingerprint"] != fingerprint || fields["expected_fingerprint"] != fingerprint || fields["cleanup"] != "unverified" {
		t.Fatalf("recovery evidence = %#v", value)
	}
	if strings.Contains(jsonString(value), "catalog_error") {
		t.Fatalf("internal catalog diagnostic leaked: %#v", value)
	}
}

func jsonString(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func TestWebReasonValidationUsesUnicodeCodePoints(t *testing.T) {
	for _, value := range []string{"", strings.Repeat("a", 512), strings.Repeat("\U0001f431", 512)} {
		if err := validateReason(value); err != nil {
			t.Fatalf("reason rejected: %v", err)
		}
	}
	for _, value := range []string{" \t\n", strings.Repeat("a", 513), string([]byte{0xff})} {
		if err := validateReason(value); err == nil {
			t.Fatalf("invalid reason was accepted")
		}
	}
}

func TestAuthorityListsImagesOnlyForAuthorizedPeer(t *testing.T) {
	authority, _, cfg := testAuthority(t)
	request := httptest.NewRequest(http.MethodGet, "/internal/v1/management/images?limit=1", nil)
	request = request.WithContext(WithPeerUID(request.Context(), cfg.WebUID))
	request.Header.Set(forwardedActorHeader, "admin@example")
	recorder := httptest.NewRecorder()
	authority.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Items []map[string]any `json:"items"`
		Page  map[string]any   `json:"page"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 1 || payload.Items[0]["instance_type"] != "container" {
		t.Fatalf("unexpected image page: %#v", payload)
	}
	if payload.Page["next_cursor"] != nil {
		t.Fatalf("unexpected next cursor: %#v", payload.Page)
	}
}

func TestPageBoundsContinuesWhenCursorAnchorDisappears(t *testing.T) {
	query := url.Values{"limit": []string{"1"}}
	encoded, err := json.Marshal(map[string]string{"key": "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	query.Set("cursor", base64.RawURLEncoding.EncodeToString(encoded))
	start, limit, err := pageBounds(query, []string{"task-2", "task-3"}, pageAfterAscending)
	if err != nil {
		t.Fatal(err)
	}
	if start != 0 || limit != 1 {
		t.Fatalf("page bounds = start %d limit %d", start, limit)
	}

	encoded, err = json.Marshal(map[string]string{"key": "2026-10-27T00:00:00Z\x00aaaa"})
	if err != nil {
		t.Fatal(err)
	}
	query.Set("cursor", base64.RawURLEncoding.EncodeToString(encoded))
	start, _, err = pageBounds(query, []string{"2026-10-26T00:00:00Z\x00bbbb", "2026-10-25T00:00:00Z\x00cccc"}, pageAfterImage)
	if err != nil {
		t.Fatal(err)
	}
	if start != 0 {
		t.Fatalf("image page start = %d", start)
	}
}

func TestOverviewMarksStaleSourcesAndBlocksUnsafeAdmission(t *testing.T) {
	authority, store, _ := testAuthority(t)
	updatedAt := "2026-10-27T00:00:00Z"
	if err := store.InsertTask(context.Background(), state.Task{TaskID: "durable-active", SessionID: "session", Generation: 1, State: "active", InstanceName: "agent-vm-durable-active", CreatedAt: updatedAt, UpdatedAt: updatedAt, ImageFingerprint: strings.Repeat("a", 64), ResourcesJSON: "{}", ResourceReservationJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	metrics := map[string]any{
		"observed_at":       time.Now().UTC().Format(time.RFC3339Nano),
		"up":                false,
		"admission_ready":   true,
		"draining":          false,
		"task_states":       map[string]any{"active": float64(4)},
		"default_admission": map[string]any{"ready": true, "reasons": []any{}},
	}
	value, err := authority.overview(metrics, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	admission := value["admission"].(map[string]any)
	if admission["ready"] != false {
		t.Fatalf("admission = %#v", admission)
	}

	metrics["observed_at"] = time.Now().UTC().Add(-2 * time.Minute).Format(time.RFC3339Nano)
	value, err = authority.overview(metrics, context.Background())
	if err != nil {
		t.Fatal(err)
	}
	counts := value["task_counts"].(map[string]any)
	if counts["active"] != int64(1) {
		t.Fatalf("durable task counts = %#v", counts)
	}
	sources := value["sources"].([]any)
	durable := sources[0].(map[string]any)
	if durable["state"] != "available" || durable["safe_code"] != nil {
		t.Fatalf("durable source = %#v", durable)
	}
}

func TestWebSessionAndWriteBoundary(t *testing.T) {
	cfg := testManagementConfig(t)
	server, err := NewWebServer(cfg, NewAuthorityClient(cfg.Authority.SocketPath))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	request = request.WithContext(WithPeerUID(request.Context(), cfg.ForwardAuth.PeerUID))
	request.Header.Set(cfg.ForwardAuth.IdentityHeader, "admin@example")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("session status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if cookie := recorder.Result().Cookies(); len(cookie) != 1 || !cookie[0].Secure || !cookie[0].HttpOnly || cookie[0].SameSite != http.SameSiteStrictMode || cookie[0].MaxAge != 900 {
		t.Fatalf("session cookie does not meet the contract: %#v", cookie)
	}
	var session struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	writeRequest := httptest.NewRequest(http.MethodPost, "/api/v1/system/maintenance/preview", strings.NewReader(`{"action":"reconcile","target_version":"system:initial","parameters":{}}`))
	writeRequest = writeRequest.WithContext(WithPeerUID(writeRequest.Context(), cfg.ForwardAuth.PeerUID))
	writeRequest.Header.Set(cfg.ForwardAuth.IdentityHeader, "admin@example")
	writeRequest.AddCookie(recorder.Result().Cookies()[0])
	writeRequest.Header.Set("X-CSRF-Token", session.CSRFToken)
	writeRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(writeRecorder, writeRequest)
	if writeRecorder.Code != http.StatusForbidden {
		t.Fatalf("missing Origin status = %d, body = %s", writeRecorder.Code, writeRecorder.Body.String())
	}
	wrongPeer := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	wrongPeer = wrongPeer.WithContext(WithPeerUID(wrongPeer.Context(), cfg.ForwardAuth.PeerUID+1))
	wrongPeer.Header.Set(cfg.ForwardAuth.IdentityHeader, "admin@example")
	wrongRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(wrongRecorder, wrongPeer)
	if wrongRecorder.Code != http.StatusForbidden {
		t.Fatalf("wrong peer status = %d, body = %s", wrongRecorder.Code, wrongRecorder.Body.String())
	}
}

func TestWebServesAuthenticatedAssetsAndDeepLinks(t *testing.T) {
	cfg := testManagementConfig(t)
	assets := fstest.MapFS{
		"index.html":        &fstest.MapFile{Data: []byte("<!doctype html>")},
		"assets/app.js":     &fstest.MapFile{Data: []byte("console.log('ok')")},
		"assets/app.css":    &fstest.MapFile{Data: []byte("body{}")},
		"assets/font.woff2": &fstest.MapFile{Data: []byte("font")},
	}
	server, err := NewWebServer(cfg, NewAuthorityClient(cfg.Authority.SocketPath), assets)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/console/tasks/one", nil)
	request = request.WithContext(WithPeerUID(request.Context(), cfg.ForwardAuth.PeerUID))
	request.Header.Set(cfg.ForwardAuth.IdentityHeader, "admin@example")
	request.Header.Set("Accept", "text/html")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "<!doctype html>" || recorder.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("deep link response = %d %q headers=%v", recorder.Code, recorder.Body.String(), recorder.Header())
	}

	assetRequest := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	assetRequest = assetRequest.WithContext(WithPeerUID(assetRequest.Context(), cfg.ForwardAuth.PeerUID))
	assetRequest.Header.Set(cfg.ForwardAuth.IdentityHeader, "admin@example")
	assetRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(assetRecorder, assetRequest)
	if assetRecorder.Code != http.StatusOK || assetRecorder.Body.String() != "console.log('ok')" || assetRecorder.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("asset response = %d %q headers=%v", assetRecorder.Code, assetRecorder.Body.String(), assetRecorder.Header())
	}

	missing := httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil)
	missing = missing.WithContext(WithPeerUID(missing.Context(), cfg.ForwardAuth.PeerUID))
	missing.Header.Set(cfg.ForwardAuth.IdentityHeader, "admin@example")
	missingRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(missingRecorder, missing)
	if missingRecorder.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d, body = %s", missingRecorder.Code, missingRecorder.Body.String())
	}
}

func TestWebRejectsAPIPathsFromStaticFallback(t *testing.T) {
	cfg := testManagementConfig(t)
	server, err := NewWebServer(cfg, NewAuthorityClient(cfg.Authority.SocketPath), fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("index")},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/unknown", nil)
	request = request.WithContext(WithPeerUID(request.Context(), cfg.ForwardAuth.PeerUID))
	request.Header.Set(cfg.ForwardAuth.IdentityHeader, "admin@example")
	request.AddCookie(&http.Cookie{Name: managementSessionCookie, Value: "missing"})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code == http.StatusOK || strings.Contains(recorder.Body.String(), "index") {
		t.Fatalf("API path fell through to static assets: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestConfirmationTokenIsBoundAndSingleUse(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	params := map[string]any{"live": false}
	token, _, err := issueDurableConfirmation(ctx, store, now, "admin@example", "restart", "vm:task-1", "vm:task-1:updated", params)
	if err != nil {
		t.Fatal(err)
	}
	if err := consumeDurableConfirmation(ctx, store, now, token, "admin@example", "restart", "vm:task-1", "vm:task-1:updated", map[string]any{"live": true}); err == nil {
		t.Fatal("parameter mismatch unexpectedly accepted")
	}
	if err := consumeDurableConfirmation(ctx, store, now, token, "admin@example", "restart", "vm:task-1", "vm:task-1:updated", params); err != nil {
		t.Fatal(err)
	}
	if err := consumeDurableConfirmation(ctx, store, now, token, "admin@example", "restart", "vm:task-1", "vm:task-1:updated", params); err == nil {
		t.Fatal("confirmation token was reusable")
	}
}

func TestDurableConfirmationClassesCrossSlots(t *testing.T) {
	blue, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer blue.Close()
	green, err := state.OpenForTest(blue.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer green.Close()

	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		action string
		target string
		params map[string]any
	}{
		{name: "vm", action: "restart", target: "vm:task-1", params: map[string]any{"live": false}},
		{name: "default-image", action: "default_image_select", target: "image:" + strings.Repeat("a", 64), params: map[string]any{"fingerprint": strings.Repeat("a", 64)}},
		{name: "image-mutation", action: "image_retire", target: "image:" + strings.Repeat("b", 64), params: map[string]any{"fingerprint": strings.Repeat("b", 64)}},
		{name: "maintenance", action: "reconcile", target: "system", params: map[string]any{"scope": "all"}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			token, _, err := issueDurableConfirmation(context.Background(), blue, now, "admin@example", test.action, test.target, "version-1", test.params)
			if err != nil {
				t.Fatal(err)
			}
			if err := consumeDurableConfirmation(context.Background(), green, now, token, "admin@example", test.action, test.target, "version-1", test.params); err != nil {
				t.Fatalf("cross-slot consume: %v", err)
			}
			if err := consumeDurableConfirmation(context.Background(), blue, now, token, "admin@example", test.action, test.target, "version-1", test.params); err == nil {
				t.Fatal("confirmation replay was accepted")
			}
		})
	}
}

func TestPrivateStagingRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "staging")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := stageReader(link, strings.NewReader("data"), 4, 8); err == nil || !strings.HasPrefix(err.Error(), "authority_unavailable:") {
		t.Fatalf("symlink staging was accepted: %v", err)
	}
}

func TestStagingCapacityChecksOnlyUnreadBytes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "staging")
	reader := &chunkReader{value: []byte("123456"), chunk: 2}
	var remaining []int64
	value, err := stageReaderWithCapacity(root, reader, 6, 0, func(unread int64) error {
		remaining = append(remaining, unread)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	decreased := false
	for _, unread := range remaining[1:] {
		if unread < remaining[0] {
			decreased = true
			break
		}
	}
	if string(value) != "123456" || len(remaining) < 2 || remaining[0] != 6 || !decreased {
		t.Fatalf("staging value=%q capacity checks=%v", value, remaining)
	}
}

func TestCleanupUploadStagingRemovesOnlyOwnedStaleFiles(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(staging, ".agent-vm-upload-stale")
	keep := filepath.Join(staging, "keep-me")
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keep, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CleanupUploadStaging(staging); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale upload still exists: %v", err)
	}
	if value, err := os.ReadFile(keep); err != nil || string(value) != "keep" {
		t.Fatalf("unrelated file changed: %q err=%v", value, err)
	}
}

type chunkReader struct {
	value []byte
	chunk int
}

func (reader *chunkReader) Read(buffer []byte) (int, error) {
	if len(reader.value) == 0 {
		return 0, io.EOF
	}
	count := reader.chunk
	if count > len(reader.value) {
		count = len(reader.value)
	}
	if count > len(buffer) {
		count = len(buffer)
	}
	copy(buffer, reader.value[:count])
	reader.value = reader.value[count:]
	return count, nil
}

func TestImportSourceRequiresExactWireVariant(t *testing.T) {
	if _, err := decodeImportSource([]byte(`{"kind":"local_upload","format":"unified","expected_sha256":"` + strings.Repeat("a", 64) + `"}`)); err == nil {
		t.Fatal("unified import without an exact size was accepted")
	}
	if _, err := decodeImportSource([]byte(`{"kind":"trusted_remote","format":"unified","source_id":"approved","expected_fingerprint":"` + strings.Repeat("a", 64) + `"}`)); err == nil {
		t.Fatal("trusted import accepted a local-only format")
	}
	value, err := decodeImportSource([]byte(`{"kind":"local_upload","format":"split","metadata":{"expected_sha256":"` + strings.Repeat("a", 64) + `","expected_size_bytes":"4"},"rootfs":{"expected_sha256":"` + strings.Repeat("b", 64) + `","expected_size_bytes":"8"}}`))
	if err != nil || value.Metadata.ExpectedSize != 4 || value.Rootfs.ExpectedSize != 8 {
		t.Fatalf("split import = %#v, error = %v", value, err)
	}
}

func TestDecimalInputsRemainCanonical(t *testing.T) {
	if value, err := parseDecimalInt("42"); err != nil || value != 42 {
		t.Fatalf("decimal value = %d, error = %v", value, err)
	}
	for _, value := range []string{"", "01", "+1", "-1"} {
		if _, err := parseDecimalInt(value); err == nil {
			t.Fatalf("non-canonical decimal %q was accepted", value)
		}
	}
}
