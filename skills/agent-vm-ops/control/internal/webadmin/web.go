package webadmin

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
)

const (
	managementSessionCookie = "agent_vm_session"
	forwardedActorHeader    = "X-Agent-VM-Actor"
)

type AuthorityClient struct {
	Socket string
}

func NewAuthorityClient(socket string) *AuthorityClient {
	return &AuthorityClient{Socket: socket}
}

func (client *AuthorityClient) Do(ctx context.Context, method, path, actor string, body io.Reader, contentType string, contentLength int64) (*clientResponse, error) {
	if client == nil || client.Socket == "" {
		return nil, errors.New("authority socket is not configured")
	}
	if actor == "" {
		return nil, errors.New("authority actor is empty")
	}
	if !strings.HasPrefix(path, "/internal/v1/management/") {
		return nil, errors.New("authority path is outside the management boundary")
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://agent-vm"+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set(forwardedActorHeader, actor)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if contentLength >= 0 {
		request.ContentLength = contentLength
	}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", client.Socket)
		},
	}
	response, err := (&http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}).Do(request)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	return &clientResponse{Response: response, transport: transport}, nil
}

type clientResponse struct {
	*http.Response
	transport *http.Transport
}

func (response *clientResponse) Close() error {
	err := response.Response.Body.Close()
	response.transport.CloseIdleConnections()
	return err
}

type WebServer struct {
	Config config.ManagementConfig
	Client *AuthorityClient
	Clock  func() time.Time

	sessionsMu sync.Mutex
	sessions   map[string]webSession
}

type webSession struct {
	Subject   string
	CSRFToken string
	ExpiresAt time.Time
}

func NewWebServer(cfg config.ManagementConfig, client *AuthorityClient) (*WebServer, error) {
	if !cfg.Enabled {
		return nil, errors.New("management_disabled: Web management is disabled")
	}
	if err := cfg.Validate(true); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("authority client is nil")
	}
	ttl := cfg.SessionTTLSeconds
	if ttl == 0 {
		ttl = 900
	}
	cfg.SessionTTLSeconds = ttl
	return &WebServer{Config: cfg, Client: client, Clock: time.Now, sessions: make(map[string]webSession)}, nil
}

func (server *WebServer) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.CustomRecovery(func(c *gin.Context, _ any) {
		writeProblem(c.Writer, newAPIError("internal_error", "internal management server failure", http.StatusInternalServerError, "internal", true, ""))
		c.Abort()
	}))
	router.Any("/api/v1/*path", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		server.handle(c.Writer, c.Request)
	})
	return router
}

func (server *WebServer) Serve(ctx context.Context) error {
	return serveUnix(ctx, server.Config.ForwardAuth.SocketPath, server.Handler())
}

func (server *WebServer) handle(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodOptions {
		writeProblem(writer, newAPIError("invalid_request", "method is not supported", http.StatusMethodNotAllowed, "request_validation", false, ""))
		return
	}
	subject, err := server.authenticatedSubject(request)
	if err != nil {
		writeProblem(writer, err)
		return
	}
	path := strings.TrimPrefix(request.URL.Path, "/api/v1")
	if path == "/session" && request.Method == http.MethodGet {
		server.establishSession(writer, subject)
		return
	}
	session, err := server.sessionFor(request, subject)
	if err != nil {
		writeProblem(writer, err)
		return
	}
	if request.Method == http.MethodPost || request.Method == http.MethodPut || request.Method == http.MethodDelete {
		if request.Header.Get("Origin") != server.Config.ExternalOrigin {
			writeProblem(writer, newAPIError("origin_invalid", "request origin is not authorized", http.StatusForbidden, "authentication", false, ""))
			return
		}
		if request.Header.Get("X-CSRF-Token") == "" || request.Header.Get("X-CSRF-Token") != session.CSRFToken {
			writeProblem(writer, newAPIError("csrf_invalid", "request CSRF token is not valid", http.StatusForbidden, "authentication", false, ""))
			return
		}
	}
	if path == "/session" {
		writeProblem(writer, newAPIError("invalid_request", "session must be established with GET", http.StatusMethodNotAllowed, "request_validation", false, ""))
		return
	}
	server.proxy(writer, request, path, subject)
}

func (server *WebServer) authenticatedSubject(request *http.Request) (string, *APIError) {
	if err := requirePeerUID(request.Context(), server.Config.ForwardAuth.PeerUID); err != nil {
		return "", newAPIError("permission_denied", "trusted proxy authentication is required", http.StatusForbidden, "authorization", false, "")
	}
	header := server.Config.ForwardAuth.IdentityHeader
	subject := singleHeader(request, header)
	if subject == "" {
		return "", newAPIError("authentication_required", "administrator authentication is required", http.StatusUnauthorized, "authentication", false, "")
	}
	if !containsString(server.Config.AllowedAdminSubjects, subject) {
		return "", newAPIError("administrator_required", "administrator access is required", http.StatusForbidden, "authorization", false, "")
	}
	return subject, nil
}

func (server *WebServer) establishSession(writer http.ResponseWriter, subject string) {
	sessionID, err := randomToken(32)
	if err != nil {
		writeProblem(writer, newAPIError("internal_error", "management session could not be established", http.StatusInternalServerError, "internal", true, ""))
		return
	}
	csrfToken, err := randomToken(32)
	if err != nil {
		writeProblem(writer, newAPIError("internal_error", "management session could not be established", http.StatusInternalServerError, "internal", true, ""))
		return
	}
	now := server.Clock().UTC()
	expires := now.Add(time.Duration(server.Config.SessionTTLSeconds) * time.Second)
	server.sessionsMu.Lock()
	for existingID, existing := range server.sessions {
		if !existing.ExpiresAt.After(now) {
			delete(server.sessions, existingID)
		}
	}
	server.sessions[sessionID] = webSession{Subject: subject, CSRFToken: csrfToken, ExpiresAt: expires}
	server.sessionsMu.Unlock()
	http.SetCookie(writer, &http.Cookie{Name: managementSessionCookie, Value: sessionID, Path: "/", Expires: expires, MaxAge: int(server.Config.SessionTTLSeconds), HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode})
	writeJSON(writer, http.StatusOK, map[string]any{"subject": subject, "capabilities": []string{"read", "operate", "manage_images", "maintain_system"}, "csrf_token": csrfToken, "expires_at": expires.Format(time.RFC3339Nano)})
}

func (server *WebServer) sessionFor(request *http.Request, subject string) (webSession, *APIError) {
	cookie, err := request.Cookie(managementSessionCookie)
	if err != nil || cookie.Value == "" {
		return webSession{}, newAPIError("authentication_required", "management session is required", http.StatusUnauthorized, "authentication", false, "")
	}
	server.sessionsMu.Lock()
	session, exists := server.sessions[cookie.Value]
	if exists && !session.ExpiresAt.After(server.Clock().UTC()) {
		delete(server.sessions, cookie.Value)
		exists = false
	}
	server.sessionsMu.Unlock()
	if !exists || session.Subject != subject {
		return webSession{}, newAPIError("authentication_required", "management session is not valid", http.StatusUnauthorized, "authentication", false, "")
	}
	return session, nil
}

func (server *WebServer) proxy(writer http.ResponseWriter, request *http.Request, path, subject string) {
	if path == "" || !strings.HasPrefix(path, "/") {
		writeProblem(writer, newAPIError("not_found", "management route was not found", http.StatusNotFound, "request_validation", false, ""))
		return
	}
	contentType := request.Header.Get("Content-Type")
	contentLength := request.ContentLength
	if contentLength < 0 {
		contentLength = -1
	}
	authorityPath := "/internal/v1/management" + path
	if request.URL.RawQuery != "" {
		authorityPath += "?" + request.URL.RawQuery
	}
	response, err := server.Client.Do(request.Context(), request.Method, authorityPath, subject, request.Body, contentType, contentLength)
	if err != nil {
		writeProblem(writer, newAPIError("authority_unavailable", "management authority is unavailable", http.StatusServiceUnavailable, "internal", true, ""))
		return
	}
	defer response.Close()
	if response.Header.Get("Content-Type") != "" {
		writer.Header().Set("Content-Type", response.Header.Get("Content-Type"))
	}
	writer.WriteHeader(response.StatusCode)
	if _, err := io.CopyN(writer, response.Body, maxAPIResponseBytes); err != nil && !errors.Is(err, io.EOF) {
		return
	}
}

func randomToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func containsString(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func singleHeader(request *http.Request, name string) string {
	values := request.Header.Values(name)
	if len(values) != 1 {
		return ""
	}
	return strings.TrimSpace(values[0])
}
