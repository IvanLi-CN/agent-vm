package webadmin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	store := newConfirmationStore()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	token, _, err := store.issue(now, "admin@example", "restart", "vm:task-1", "vm:task-1:updated", map[string]any{"live": false})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.consume(now, token, "admin@example", "restart", "vm:task-1", "vm:task-1:updated", map[string]any{"live": true}); err == nil {
		t.Fatal("parameter mismatch unexpectedly accepted")
	}
	if err := store.consume(now, token, "admin@example", "restart", "vm:task-1", "vm:task-1:updated", map[string]any{"live": false}); err != nil {
		t.Fatal(err)
	}
	if err := store.consume(now, token, "admin@example", "restart", "vm:task-1", "vm:task-1:updated", map[string]any{"live": false}); err == nil {
		t.Fatal("confirmation token was reusable")
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
