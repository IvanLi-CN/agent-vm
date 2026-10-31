package webadmin

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

const (
	oidcTransactionTTL = 5 * time.Minute
	oidcClockSkew      = 30 * time.Second
)

var oidcErrorPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

type oidcDiscovery struct {
	Issuer              string
	Authorization       string
	TokenEndpoint       string
	JWKSURI             string
	CodeChallengeMethod []string
}

type oidcAuthenticator struct {
	server     *UnifiedServer
	client     *http.Client
	mu         sync.Mutex
	discovery  oidcDiscovery
	discovered time.Time
}

func (auth *oidcAuthenticator) oidcStart(c *gin.Context) {
	request := c.Request
	if request.Method != http.MethodGet || len(request.URL.Query()) != 0 {
		writeProblem(c.Writer, invalidRequest("OIDC start accepts only a query-free GET"))
		return
	}
	discovery, err := auth.getDiscovery(request.Context())
	if err != nil {
		writeProblem(c.Writer, newAPIError("oidc_unavailable", "the configured identity provider is unavailable", http.StatusServiceUnavailable, "authentication", true, ""))
		return
	}
	stateValue, err := randomToken(32)
	if err != nil {
		writeProblem(c.Writer, newAPIError("internal_error", "OIDC login could not be started", http.StatusInternalServerError, "internal", true, ""))
		return
	}
	nonce, err := randomToken(32)
	if err != nil {
		writeProblem(c.Writer, newAPIError("internal_error", "OIDC login could not be started", http.StatusInternalServerError, "internal", true, ""))
		return
	}
	verifier, err := randomToken(32)
	if err != nil {
		writeProblem(c.Writer, newAPIError("internal_error", "OIDC login could not be started", http.StatusInternalServerError, "internal", true, ""))
		return
	}
	binding, err := randomToken(32)
	if err != nil {
		writeProblem(c.Writer, newAPIError("internal_error", "OIDC login could not be started", http.StatusInternalServerError, "internal", true, ""))
		return
	}
	sealed, err := auth.seal(verifier)
	if err != nil {
		writeProblem(c.Writer, newAPIError("internal_error", "OIDC login could not be started", http.StatusInternalServerError, "internal", true, ""))
		return
	}
	now := auth.server.now()
	transaction := state.OIDCTransaction{
		TransactionID:       randomTransactionID(),
		StateDigest:         digestString(stateValue),
		NonceDigest:         digestString(nonce),
		CodeVerifierSealed:  sealed,
		BindingCookieDigest: digestString(binding),
		ClientID:            auth.server.Config.OIDC.ClientID,
		RedirectURI:         auth.server.Config.OIDC.RedirectURI,
		CreatedAt:           now.Format(time.RFC3339Nano),
		ExpiresAt:           now.Add(oidcTransactionTTL).Format(time.RFC3339Nano),
	}
	if err := auth.server.Store.InsertOIDCTransaction(request.Context(), transaction); err != nil {
		writeProblem(c.Writer, newAPIError("state_unavailable", "OIDC login state could not be stored", http.StatusServiceUnavailable, "authentication", true, ""))
		return
	}
	challenge := sha256.Sum256([]byte(verifier))
	values := url.Values{}
	values.Set("client_id", auth.server.Config.OIDC.ClientID)
	values.Set("redirect_uri", auth.server.Config.OIDC.RedirectURI)
	values.Set("response_type", "code")
	values.Set("scope", "openid")
	values.Set("state", stateValue)
	values.Set("nonce", nonce)
	values.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	values.Set("code_challenge_method", "S256")
	location := discovery.Authorization + "?" + values.Encode()
	http.SetCookie(c.Writer, &http.Cookie{
		Name: loginBindingCookie, Value: binding, Path: "/api/v1/auth/oidc", MaxAge: int(oidcTransactionTTL / time.Second),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
	})
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	http.Redirect(c.Writer, request, location, http.StatusFound)
}

func (auth *oidcAuthenticator) oidcCallback(c *gin.Context) {
	request := c.Request
	query := request.URL.Query()
	if request.Method != http.MethodGet || !allowedQueryKeys(query, map[string]bool{"code": true, "state": true, "error": true, "error_description": true, "error_uri": true}) {
		writeProblem(c.Writer, invalidRequest("OIDC callback query is invalid"))
		return
	}
	stateValue, stateOK := singleQuery(query, "state")
	codeValue, codeOK := singleQuery(query, "code")
	errorValue, errorOK := singleQuery(query, "error")
	if !stateOK || len(stateValue) < 32 || (codeOK == errorOK) || (codeOK && strings.TrimSpace(codeValue) == "") || (errorOK && !oidcErrorPattern.MatchString(errorValue)) {
		writeProblem(c.Writer, newAPIError("invalid_state", "OIDC login response is invalid", http.StatusBadRequest, "authentication", false, ""))
		return
	}
	if values := query["error_description"]; len(values) > 1 || (len(values) == 1 && len(values[0]) > 512) {
		writeProblem(c.Writer, newAPIError("invalid_state", "OIDC login response is invalid", http.StatusBadRequest, "authentication", false, ""))
		return
	}
	if values := query["error_uri"]; len(values) > 1 || (len(values) == 1 && (len(values[0]) > 2048 || !validOIDCErrorURI(values[0]))) {
		writeProblem(c.Writer, newAPIError("invalid_state", "OIDC login response is invalid", http.StatusBadRequest, "authentication", false, ""))
		return
	}
	bindingCookie, cookieErr := request.Cookie(loginBindingCookie)
	if cookieErr != nil || bindingCookie.Value == "" {
		writeProblem(c.Writer, newAPIError("invalid_state", "OIDC login binding is invalid", http.StatusBadRequest, "authentication", false, ""))
		return
	}
	transaction, err := auth.server.Store.GetOIDCTransaction(request.Context(), digestString(stateValue))
	if err != nil {
		writeProblem(c.Writer, newAPIError("state_unavailable", "OIDC login state could not be read", http.StatusServiceUnavailable, "authentication", true, ""))
		return
	}
	if transaction == nil || subtle.ConstantTimeCompare([]byte(transaction.BindingCookieDigest), []byte(digestString(bindingCookie.Value))) != 1 {
		writeProblem(c.Writer, newAPIError("invalid_state", "OIDC login binding is invalid", http.StatusBadRequest, "authentication", false, ""))
		return
	}
	now := auth.server.now()
	transaction, consumed, err := auth.server.Store.ConsumeOIDCTransaction(request.Context(), digestString(stateValue), now.Format(time.RFC3339Nano))
	if err != nil {
		writeProblem(c.Writer, newAPIError("state_unavailable", "OIDC login state could not be consumed", http.StatusServiceUnavailable, "authentication", true, ""))
		return
	}
	if !consumed || transaction == nil {
		writeProblem(c.Writer, newAPIError("invalid_state", "OIDC login state has already been used or expired", http.StatusBadRequest, "authentication", false, ""))
		return
	}
	if errorOK {
		auth.redirectLoginFailure(c.Writer, request)
		return
	}
	if transaction.ClientID != auth.server.Config.OIDC.ClientID || transaction.RedirectURI != auth.server.Config.OIDC.RedirectURI {
		auth.redirectLoginFailure(c.Writer, request)
		return
	}
	verifier, err := auth.unseal(transaction.CodeVerifierSealed)
	if err != nil {
		auth.redirectLoginFailure(c.Writer, request)
		return
	}
	discovery, err := auth.getDiscovery(request.Context())
	if err != nil {
		auth.redirectLoginFailure(c.Writer, request)
		return
	}
	idToken, err := auth.exchangeCode(request.Context(), discovery, codeValue, verifier)
	if err != nil {
		auth.redirectLoginFailure(c.Writer, request)
		return
	}
	issuer, subject, err := auth.verifyIDToken(request.Context(), discovery, idToken, transaction, now)
	if err != nil || issuer != auth.server.Config.AdminPrincipal.Issuer || subject != auth.server.Config.AdminPrincipal.Subject {
		auth.redirectLoginFailure(c.Writer, request)
		return
	}
	sessionRaw, err := randomToken(32)
	if err != nil {
		auth.redirectLoginFailure(c.Writer, request)
		return
	}
	expires := now.Add(time.Duration(auth.server.Config.SessionTTLSeconds) * time.Second)
	session := state.BrowserSession{
		SessionDigest:    digestString(sessionRaw),
		Issuer:           issuer,
		Subject:          subject,
		CSRFSecretDigest: digestString(auth.csrfToken(digestString(sessionRaw))),
		Capabilities:     append([]string(nil), auth.server.Config.AdminPrincipal.Capabilities...),
		CreatedAt:        now.Format(time.RFC3339Nano),
		ExpiresAt:        expires.Format(time.RFC3339Nano),
	}
	if err := auth.server.Store.InsertBrowserSession(request.Context(), session); err != nil {
		auth.redirectLoginFailure(c.Writer, request)
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{Name: managementSessionCookie, Value: sessionRaw, Path: "/", MaxAge: int(auth.server.Config.SessionTTLSeconds), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	auth.clearBinding(c.Writer)
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	http.Redirect(c.Writer, request, strings.TrimRight(auth.server.Config.ExternalOrigin, "/")+"/", http.StatusFound)
}

func (auth *oidcAuthenticator) sessionFor(request *http.Request) (unifiedPrincipal, *state.BrowserSession, *APIError) {
	if len(request.Header.Values("Authorization")) != 0 {
		return unifiedPrincipal{}, nil, newAPIError("authentication_required", "browser session authentication is required", http.StatusUnauthorized, "authentication", false, "")
	}
	cookie, err := request.Cookie(managementSessionCookie)
	if err != nil || cookie.Value == "" {
		return unifiedPrincipal{}, nil, newAPIError("authentication_required", "management session is required", http.StatusUnauthorized, "authentication", false, "")
	}
	session, err := auth.server.Store.GetBrowserSession(request.Context(), digestString(cookie.Value))
	if err != nil {
		return unifiedPrincipal{}, nil, newAPIError("state_unavailable", "management session could not be read", http.StatusServiceUnavailable, "authentication", true, "")
	}
	if session == nil || session.RevokedAt != nil || session.Issuer != auth.server.Config.AdminPrincipal.Issuer || session.Subject != auth.server.Config.AdminPrincipal.Subject {
		return unifiedPrincipal{}, nil, newAPIError("authentication_required", "management session is not valid", http.StatusUnauthorized, "authentication", false, "")
	}
	expires, err := time.Parse(time.RFC3339Nano, session.ExpiresAt)
	if err != nil || !expires.After(auth.server.now()) {
		return unifiedPrincipal{}, nil, newAPIError("authentication_required", "management session has expired", http.StatusUnauthorized, "authentication", false, "")
	}
	return auth.server.actorFromSession(session), session, nil
}

func (auth *oidcAuthenticator) csrfToken(sessionDigest string) string {
	digest := sha256.Sum256([]byte("agent-vm-csrf\x00" + sessionDigest))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

func (auth *oidcAuthenticator) validCSRF(request *http.Request, session *state.BrowserSession, value string) bool {
	if session == nil || value == "" || subtle.ConstantTimeCompare([]byte(value), []byte(auth.csrfToken(session.SessionDigest))) != 1 {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(session.CSRFSecretDigest), []byte(digestString(value))) == 1
}

func (auth *oidcAuthenticator) clearBinding(writer http.ResponseWriter) {
	http.SetCookie(writer, &http.Cookie{Name: loginBindingCookie, Value: "", Path: "/api/v1/auth/oidc", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
}

func (auth *oidcAuthenticator) redirectLoginFailure(writer http.ResponseWriter, request *http.Request) {
	auth.clearBinding(writer)
	writer.Header().Set("Cache-Control", "no-store")
	http.Redirect(writer, request, strings.TrimRight(auth.server.Config.ExternalOrigin, "/")+"/?auth=failed", http.StatusFound)
}

func (auth *oidcAuthenticator) getDiscovery(ctx context.Context) (oidcDiscovery, error) {
	auth.mu.Lock()
	if auth.discovery.Issuer != "" && auth.server.now().Sub(auth.discovered) < 5*time.Minute {
		value := auth.discovery
		auth.mu.Unlock()
		return value, nil
	}
	auth.mu.Unlock()
	issuer := strings.TrimRight(auth.server.Config.OIDC.Issuer, "/")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return oidcDiscovery{}, err
	}
	response, err := auth.client.Do(request)
	if err != nil {
		return oidcDiscovery{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return oidcDiscovery{}, errors.New("OIDC discovery returned a non-success status")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 256*1024+1))
	if err != nil || len(body) > 256*1024 {
		return oidcDiscovery{}, errors.New("OIDC discovery response is invalid")
	}
	var value struct {
		Issuer              string   `json:"issuer"`
		Authorization       string   `json:"authorization_endpoint"`
		TokenEndpoint       string   `json:"token_endpoint"`
		JWKSURI             string   `json:"jwks_uri"`
		CodeChallengeMethod []string `json:"code_challenge_methods_supported"`
	}
	if err := json.Unmarshal(body, &value); err != nil || value.Issuer != auth.server.Config.OIDC.Issuer || value.Authorization == "" || value.TokenEndpoint == "" || value.JWKSURI == "" {
		return oidcDiscovery{}, errors.New("OIDC discovery metadata is invalid")
	}
	if !httpsURL(value.Authorization) || !httpsURL(value.TokenEndpoint) || !httpsURL(value.JWKSURI) || !containsString(value.CodeChallengeMethod, "S256") {
		return oidcDiscovery{}, errors.New("OIDC discovery metadata is not compatible")
	}
	discovery := oidcDiscovery{Issuer: value.Issuer, Authorization: value.Authorization, TokenEndpoint: value.TokenEndpoint, JWKSURI: value.JWKSURI, CodeChallengeMethod: value.CodeChallengeMethod}
	auth.mu.Lock()
	auth.discovery, auth.discovered = discovery, auth.server.now()
	auth.mu.Unlock()
	return discovery, nil
}

func (auth *oidcAuthenticator) exchangeCode(ctx context.Context, discovery oidcDiscovery, code, verifier string) (string, error) {
	values := url.Values{}
	values.Set("grant_type", "authorization_code")
	values.Set("code", code)
	values.Set("redirect_uri", auth.server.Config.OIDC.RedirectURI)
	values.Set("client_id", auth.server.Config.OIDC.ClientID)
	values.Set("client_secret", auth.server.Config.OIDC.ClientSecret)
	values.Set("code_verifier", verifier)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, discovery.TokenEndpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := auth.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 || response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", errors.New("OIDC token exchange failed")
	}
	var value struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &value); err != nil || value.IDToken == "" {
		return "", errors.New("OIDC token response is invalid")
	}
	return value.IDToken, nil
}

func (auth *oidcAuthenticator) verifyIDToken(ctx context.Context, discovery oidcDiscovery, raw string, transaction *state.OIDCTransaction, now time.Time) (string, string, error) {
	header, claims, signingInput, signature, err := parseJWT(raw)
	if err != nil {
		return "", "", err
	}
	alg, _ := header["alg"].(string)
	if alg != "RS256" && alg != "ES256" && alg != "EdDSA" {
		return "", "", errors.New("OIDC ID Token algorithm is not allowed")
	}
	kid, _ := header["kid"].(string)
	keys, err := auth.fetchJWKS(ctx, discovery.JWKSURI)
	if err != nil {
		return "", "", err
	}
	key, ok := keys[kid]
	if !ok {
		return "", "", errors.New("OIDC ID Token signing key is unknown")
	}
	if err := verifyJWTSignature(alg, key, signingInput, signature); err != nil {
		return "", "", err
	}
	issuer, ok := claims["iss"].(string)
	if !ok || issuer != discovery.Issuer {
		return "", "", errors.New("OIDC issuer claim is invalid")
	}
	subject, ok := claims["sub"].(string)
	if !ok || strings.TrimSpace(subject) == "" || len(subject) > 255 {
		return "", "", errors.New("OIDC subject claim is invalid")
	}
	if !audienceContains(claims["aud"], auth.server.Config.OIDC.ClientID) {
		return "", "", errors.New("OIDC audience claim is invalid")
	}
	exp, ok := numericDateClaim(claims["exp"])
	if !ok || now.After(exp.Add(oidcClockSkew)) {
		return "", "", errors.New("OIDC ID Token is expired")
	}
	iat, ok := numericDateClaim(claims["iat"])
	if !ok || iat.After(now.Add(oidcClockSkew)) {
		return "", "", errors.New("OIDC issued-at claim is invalid")
	}
	if audienceCount(claims["aud"]) > 1 {
		azp, ok := claims["azp"].(string)
		if !ok || azp != auth.server.Config.OIDC.ClientID {
			return "", "", errors.New("OIDC authorized-party claim is invalid")
		}
	} else if azp, present := claims["azp"]; present {
		if value, ok := azp.(string); !ok || value != auth.server.Config.OIDC.ClientID {
			return "", "", errors.New("OIDC authorized-party claim is invalid")
		}
	}
	nonce, ok := claims["nonce"].(string)
	if !ok || subtle.ConstantTimeCompare([]byte(transaction.NonceDigest), []byte(digestString(nonce))) != 1 {
		return "", "", errors.New("OIDC nonce claim is invalid")
	}
	return issuer, subject, nil
}

func (auth *oidcAuthenticator) fetchJWKS(ctx context.Context, endpoint string) (map[string]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	response, err := auth.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, errors.New("OIDC JWKS request failed")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		return nil, errors.New("OIDC JWKS response is invalid")
	}
	var value struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	keys := make(map[string]any, len(value.Keys))
	for _, item := range value.Keys {
		kid, _ := item["kid"].(string)
		if kid == "" {
			continue
		}
		key, err := parseJWK(item)
		if err == nil {
			keys[kid] = key
		}
	}
	return keys, nil
}

func (auth *oidcAuthenticator) seal(value string) (string, error) {
	block, err := aes.NewCipher(auth.key())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(value), nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (auth *oidcAuthenticator) unseal(value string) (string, error) {
	sealed, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(auth.key())
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(sealed) < gcm.NonceSize() {
		return "", errors.New("sealed verifier is invalid")
	}
	valueBytes, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(valueBytes), nil
}

func (auth *oidcAuthenticator) key() []byte {
	digest := sha256.Sum256([]byte("agent-vm-oidc-verifier\x00" + auth.server.Config.OIDC.ClientSecret))
	return digest[:]
}

func parseJWT(raw string) (map[string]any, map[string]any, []byte, []byte, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, nil, nil, nil, errors.New("OIDC ID Token is not a compact JWT")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, nil, nil, nil, err
	}
	header, err := decodeJSONObject(headerBytes)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	payload, err := decodeJSONObject(payloadBytes)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return header, payload, []byte(parts[0] + "." + parts[1]), signature, nil
}

func decodeJSONObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, errors.New("JWT object contains trailing JSON")
		}
		return nil, err
	}
	if value == nil {
		return nil, errors.New("JWT value is not an object")
	}
	return value, nil
}

func verifyJWTSignature(algorithm string, key any, input, signature []byte) error {
	digest := sha256.Sum256(input)
	switch algorithm {
	case "RS256":
		publicKey, ok := key.(*rsa.PublicKey)
		if !ok || publicKey == nil || publicKey.N == nil || publicKey.N.Sign() <= 0 || publicKey.E < 3 || publicKey.E%2 == 0 {
			return errors.New("OIDC RSA key type is invalid")
		}
		return rsa.VerifyPKCS1v15(publicKey, crypto.SHA256, digest[:], signature)
	case "ES256":
		publicKey, ok := key.(*ecdsa.PublicKey)
		if !ok || len(signature) != 64 {
			return errors.New("OIDC EC key type is invalid")
		}
		return nilIf(ecdsa.Verify(publicKey, digest[:], new(big.Int).SetBytes(signature[:32]), new(big.Int).SetBytes(signature[32:])), "OIDC ECDSA signature is invalid")
	case "EdDSA":
		publicKey, ok := key.(ed25519.PublicKey)
		if !ok || !ed25519.Verify(publicKey, input, signature) {
			return errors.New("OIDC EdDSA signature is invalid")
		}
		return nil
	default:
		return errors.New("OIDC signature algorithm is not allowed")
	}
}

func parseJWK(value map[string]any) (any, error) {
	kty, _ := value["kty"].(string)
	decode := func(name string) ([]byte, error) {
		encoded, ok := value[name].(string)
		if !ok || encoded == "" {
			return nil, errors.New("JWK member is missing")
		}
		return base64.RawURLEncoding.DecodeString(encoded)
	}
	switch kty {
	case "RSA":
		n, err := decode("n")
		if err != nil {
			return nil, err
		}
		e, err := decode("e")
		if err != nil {
			return nil, err
		}
		modulus := new(big.Int).SetBytes(n)
		exponent := new(big.Int).SetBytes(e)
		if modulus.Sign() <= 0 || !exponent.IsInt64() || exponent.Int64() < 3 || exponent.Int64()%2 == 0 || exponent.Int64() > int64(^uint(0)>>1) {
			return nil, errors.New("JWK RSA exponent is invalid")
		}
		return &rsa.PublicKey{N: modulus, E: int(exponent.Int64())}, nil
	case "EC":
		curve, _ := value["crv"].(string)
		if curve != "P-256" {
			return nil, errors.New("JWK EC curve is not allowed")
		}
		x, err := decode("x")
		if err != nil {
			return nil, err
		}
		y, err := decode("y")
		if err != nil {
			return nil, err
		}
		publicX, publicY := new(big.Int).SetBytes(x), new(big.Int).SetBytes(y)
		if !elliptic.P256().IsOnCurve(publicX, publicY) {
			return nil, errors.New("JWK EC point is invalid")
		}
		return &ecdsa.PublicKey{Curve: elliptic.P256(), X: publicX, Y: publicY}, nil
	case "OKP":
		curve, _ := value["crv"].(string)
		if curve != "Ed25519" {
			return nil, errors.New("JWK OKP curve is not allowed")
		}
		x, err := decode("x")
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("JWK Ed25519 key is invalid")
		}
		return ed25519.PublicKey(x), nil
	default:
		return nil, errors.New("JWK key type is not allowed")
	}
}

func audienceContains(value any, expected string) bool {
	switch item := value.(type) {
	case string:
		return item == expected
	case []any:
		for _, candidate := range item {
			if value, ok := candidate.(string); ok && value == expected {
				return true
			}
		}
	}
	return false
}

func audienceCount(value any) int {
	switch item := value.(type) {
	case string:
		if item == "" {
			return 0
		}
		return 1
	case []any:
		return len(item)
	default:
		return 0
	}
}

func numberClaim(value any) (float64, bool) {
	switch item := value.(type) {
	case float64:
		return item, true
	case json.Number:
		parsed, err := item.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func numericDateClaim(value any) (time.Time, bool) {
	seconds, ok := numberClaim(value)
	if !ok || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 || seconds >= float64(1<<63) || math.Trunc(seconds) != seconds {
		return time.Time{}, false
	}
	return time.Unix(int64(seconds), 0), true
}

func httpsURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func validOIDCErrorURI(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme != "" && parsed.Host != ""
}

func allowedQueryKeys(query url.Values, allowed map[string]bool) bool {
	for key := range query {
		if !allowed[key] {
			return false
		}
	}
	return true
}

func singleQuery(query url.Values, key string) (string, bool) {
	values, ok := query[key]
	if !ok || len(values) != 1 {
		return "", false
	}
	return values[0], true
}

func randomTransactionID() string {
	value, err := randomToken(24)
	if err != nil {
		return fmt.Sprintf("transaction-%d", time.Now().UnixNano())
	}
	return value
}

func nilIf(ok bool, message string) error {
	if ok {
		return nil
	}
	return errors.New(message)
}
