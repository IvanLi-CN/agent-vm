package webadmin

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

func TestOIDCLoginUsesDurableTransactionAndSession(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var nonce string
	var provider *httptest.Server
	provider = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"issuer": provider.URL, "authorization_endpoint": provider.URL + "/authorize", "token_endpoint": provider.URL + "/token", "jwks_uri": provider.URL + "/jwks", "code_challenge_methods_supported": []string{"S256"},
			})
		case "/jwks":
			n := base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes())
			e := base64.RawURLEncoding.EncodeToString(bigIntBytes(int64(privateKey.PublicKey.E)))
			_ = json.NewEncoder(writer).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "key-1", "alg": "RS256", "n": n, "e": e}}})
		case "/token":
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(map[string]string{"id_token": signedTestIDToken(t, privateKey, provider.URL, nonce)})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer provider.Close()
	authority, store, cfg := testAuthority(t)
	cfg.OIDC.Issuer = provider.URL
	cfg.OIDC.RedirectURI = "https://console.example/api/v1/auth/oidc/callback"
	cfg.AdminPrincipal.Issuer = provider.URL
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Store: store, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}})
	if err != nil {
		t.Fatal(err)
	}
	server.OIDC.client = provider.Client()

	start := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/start", nil)
	startRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(startRecorder, start)
	if startRecorder.Code != http.StatusFound {
		t.Fatalf("start status=%d body=%s", startRecorder.Code, startRecorder.Body.String())
	}
	location, err := url.Parse(startRecorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	stateValue := location.Query().Get("state")
	nonce = location.Query().Get("nonce")
	if stateValue == "" || nonce == "" || location.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("OIDC redirect = %s", location.String())
	}
	transaction, err := store.GetOIDCTransaction(t.Context(), digestString(stateValue))
	if err != nil || transaction == nil || transaction.ConsumedAt != nil || strings.Contains(transaction.CodeVerifierSealed, location.Query().Get("code_challenge")) {
		t.Fatalf("OIDC transaction=%#v err=%v", transaction, err)
	}
	cookies := startRecorder.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != loginBindingCookie || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("OIDC binding cookie = %#v", cookies)
	}

	callback := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code=one-time-code&state="+url.QueryEscape(stateValue), nil)
	callback.AddCookie(cookies[0])
	callbackRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(callbackRecorder, callback)
	if callbackRecorder.Code != http.StatusFound {
		t.Fatalf("callback status=%d body=%s", callbackRecorder.Code, callbackRecorder.Body.String())
	}
	callbackCookies := callbackRecorder.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, cookie := range callbackCookies {
		if cookie.Name == managementSessionCookie {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil || !sessionCookie.Secure || !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %#v", callbackCookies)
	}
	sessionRequest := httptest.NewRequest(http.MethodGet, "/api/v1/session", nil)
	sessionRequest.AddCookie(sessionCookie)
	sessionRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(sessionRecorder, sessionRequest)
	if sessionRecorder.Code != http.StatusOK || !strings.Contains(sessionRecorder.Body.String(), cfg.AdminPrincipal.Subject) {
		t.Fatalf("session status/body=%d %s", sessionRecorder.Code, sessionRecorder.Body.String())
	}

	replay := httptest.NewRequest(http.MethodGet, "/api/v1/auth/oidc/callback?code=one-time-code&state="+url.QueryEscape(stateValue), nil)
	replay.AddCookie(cookies[0])
	replayRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(replayRecorder, replay)
	if replayRecorder.Code != http.StatusBadRequest || !strings.Contains(replayRecorder.Body.String(), "invalid_state") {
		t.Fatalf("callback replay status/body=%d %s", replayRecorder.Code, replayRecorder.Body.String())
	}
}

func signedTestIDToken(t *testing.T, key *rsa.PrivateKey, issuer, nonce string) string {
	t.Helper()
	now := time.Now()
	return signedTestIDTokenClaims(t, key, map[string]any{"alg": "RS256", "kid": "key-1", "typ": "JWT"}, map[string]any{
		"iss": issuer, "sub": "admin@example", "aud": "client", "iat": now.Add(-time.Second).Unix(), "exp": now.Add(5 * time.Minute).Unix(), "nonce": nonce,
	})
}

func signedTestIDTokenClaims(t *testing.T, key *rsa.PrivateKey, headerValue, claimsValue map[string]any) string {
	t.Helper()
	headerJSON, err := json.Marshal(headerValue)
	if err != nil {
		t.Fatal(err)
	}
	claimsJSON, err := json.Marshal(claimsValue)
	if err != nil {
		t.Fatal(err)
	}
	header := base64.RawURLEncoding.EncodeToString(headerJSON)
	payload := base64.RawURLEncoding.EncodeToString(claimsJSON)
	input := []byte(header + "." + payload)
	digest := sha256.Sum256(input)
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestOIDCRejectsInvalidIDTokens(t *testing.T) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var provider *httptest.Server
	provider = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/jwks" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		n := base64.RawURLEncoding.EncodeToString(privateKey.PublicKey.N.Bytes())
		e := base64.RawURLEncoding.EncodeToString(bigIntBytes(int64(privateKey.PublicKey.E)))
		_ = json.NewEncoder(writer).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "key-1", "alg": "RS256", "n": n, "e": e}}})
	}))
	defer provider.Close()
	authority, store, cfg := testAuthority(t)
	cfg.OIDC.Issuer = provider.URL
	cfg.AdminPrincipal.Issuer = provider.URL
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Store: store, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}})
	if err != nil {
		t.Fatal(err)
	}
	server.OIDC.client = provider.Client()
	now := time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
	discovery := oidcDiscovery{Issuer: provider.URL, JWKSURI: provider.URL + "/jwks"}
	transaction := &state.OIDCTransaction{NonceDigest: digestString("nonce")}
	baseHeader := map[string]any{"alg": "RS256", "kid": "key-1", "typ": "JWT"}
	baseClaims := map[string]any{
		"iss": provider.URL, "sub": "admin@example", "aud": "client",
		"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Minute).Unix(), "nonce": "nonce",
	}
	cases := []struct {
		name       string
		mutate     func(map[string]any, map[string]any)
		signingKey *rsa.PrivateKey
	}{
		{name: "issuer", mutate: func(_ map[string]any, claims map[string]any) { claims["iss"] = "https://issuer.invalid" }, signingKey: privateKey},
		{name: "audience", mutate: func(_ map[string]any, claims map[string]any) { claims["aud"] = "other-client" }, signingKey: privateKey},
		{name: "authorized-party", mutate: func(_ map[string]any, claims map[string]any) {
			claims["aud"] = []string{"client", "other-client"}
			claims["azp"] = "other-client"
		}, signingKey: privateKey},
		{name: "nonce", mutate: func(_ map[string]any, claims map[string]any) { claims["nonce"] = "other-nonce" }, signingKey: privateKey},
		{name: "expired", mutate: func(_ map[string]any, claims map[string]any) { claims["exp"] = now.Add(-time.Minute).Unix() }, signingKey: privateKey},
		{name: "future-issued-at", mutate: func(_ map[string]any, claims map[string]any) { claims["iat"] = now.Add(time.Minute).Unix() }, signingKey: privateKey},
		{name: "bad-signature", mutate: func(_ map[string]any, _ map[string]any) {}, signingKey: otherKey},
		{name: "algorithm", mutate: func(header map[string]any, _ map[string]any) { header["alg"] = "HS256" }, signingKey: privateKey},
		{name: "unknown-key", mutate: func(header map[string]any, _ map[string]any) { header["kid"] = "missing" }, signingKey: privateKey},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			header := cloneStringAnyMap(baseHeader)
			claims := cloneStringAnyMap(baseClaims)
			testCase.mutate(header, claims)
			raw := signedTestIDTokenClaims(t, testCase.signingKey, header, claims)
			if _, _, err := server.OIDC.verifyIDToken(context.Background(), discovery, raw, transaction, now); err == nil {
				t.Fatal("invalid ID token was accepted")
			}
		})
	}
}

func TestOIDCDiscoveryDenialFailsClosed(t *testing.T) {
	provider := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer provider.Close()
	authority, store, cfg := testAuthority(t)
	cfg.OIDC.Issuer = provider.URL
	cfg.AdminPrincipal.Issuer = provider.URL
	server, err := NewUnifiedServer(UnifiedOptions{Config: cfg, Authority: authority, Store: store, Assets: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("index")}}})
	if err != nil {
		t.Fatal(err)
	}
	server.OIDC.client = provider.Client()
	if _, err := server.OIDC.getDiscovery(context.Background()); err == nil {
		t.Fatal("OIDC discovery denial was accepted")
	}
}

func cloneStringAnyMap(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func bigIntBytes(value int64) []byte {
	if value == 0 {
		return []byte{0}
	}
	result := make([]byte, 0, 8)
	for value > 0 {
		result = append([]byte{byte(value)}, result...)
		value >>= 8
	}
	return result
}
