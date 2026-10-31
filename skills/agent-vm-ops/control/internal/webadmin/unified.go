package webadmin

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/observe"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/webassets"
)

const (
	loginBindingCookie = "agent_vm_oidc_binding"
	maxAgentBodyBytes  = 2 * 1024 * 1024
)

type UnifiedOptions struct {
	Config         config.ManagementConfig
	Authority      *Authority
	Lifecycle      *lifecycle.Service
	Store          *state.Store
	Snapshots      *observe.SnapshotStore
	ProductVersion string
	Assets         fs.FS
	Clock          func() time.Time
}

// UnifiedServer is the single browser, Agent API and legacy control authority
// for one blue-green slot. Authentication state is read from Store so a slot
// switch never depends on process-local sessions.
type UnifiedServer struct {
	Config         config.ManagementConfig
	Authority      *Authority
	Lifecycle      *lifecycle.Service
	Store          *state.Store
	Snapshots      *observe.SnapshotStore
	ProductVersion string
	Assets         fs.FS
	Clock          func() time.Time
	OIDC           *oidcAuthenticator
}

func NewUnifiedServer(options UnifiedOptions) (*UnifiedServer, error) {
	if !options.Config.Enabled {
		return nil, errors.New("management_disabled: management authority is disabled")
	}
	if err := options.Config.Validate(false); err != nil {
		return nil, err
	}
	if options.Authority == nil || options.Store == nil {
		return nil, errors.New("unified service dependencies are incomplete")
	}
	if options.Clock == nil {
		options.Clock = time.Now
	}
	if options.Assets == nil {
		options.Assets = webassets.Files
	}
	if options.Config.SessionTTLSeconds == 0 {
		options.Config.SessionTTLSeconds = 900
	}
	if options.Config.TokenTTLSeconds == 0 {
		options.Config.TokenTTLSeconds = 3600
	}
	server := &UnifiedServer{
		Config:         options.Config,
		Authority:      options.Authority,
		Lifecycle:      options.Lifecycle,
		Store:          options.Store,
		Snapshots:      options.Snapshots,
		ProductVersion: options.ProductVersion,
		Assets:         options.Assets,
		Clock:          options.Clock,
	}
	server.OIDC = &oidcAuthenticator{server: server, client: &http.Client{Timeout: 15 * time.Second}}
	return server, nil
}

func (server *UnifiedServer) Handler() http.Handler {
	gin.SetMode(gin.ReleaseMode)
	router := gin.New()
	router.Use(gin.CustomRecovery(func(c *gin.Context, _ any) {
		writeProblem(c.Writer, newAPIError("internal_error", "unified management server failed", http.StatusInternalServerError, "internal", true, ""))
		c.Abort()
	}))
	router.Any("/api/v1", server.browserAPI)
	router.Any("/api/v1/*path", server.browserAPI)
	router.POST("/agent/v1/control", func(c *gin.Context) { server.agentControl(c.Writer, c.Request) })
	router.NoRoute(func(c *gin.Context) {
		server.serveStatic(c.Writer, c.Request)
	})
	return router
}

func (server *UnifiedServer) browserAPI(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	request := c.Request
	if request.Method == http.MethodOptions {
		writeProblem(c.Writer, newAPIError("invalid_request", "method is not supported", http.StatusMethodNotAllowed, "request_validation", false, ""))
		return
	}
	pathValue := strings.TrimPrefix(request.URL.Path, "/api/v1")
	if pathValue == "" {
		pathValue = "/"
	}
	if pathValue == "/auth/oidc/start" {
		server.OIDC.oidcStart(c)
		return
	}
	if pathValue == "/auth/oidc/callback" {
		server.OIDC.oidcCallback(c)
		return
	}
	principal, session, authErr := server.sessionFor(request)
	if pathValue == "/session" && request.Method == http.MethodGet {
		if authErr != nil {
			server.writeRejectedProblem(c.Writer, request, pathValue, authErr)
			return
		}
		server.writeSession(c.Writer, principal, session)
		return
	}
	if authErr != nil {
		server.writeRejectedProblem(c.Writer, request, pathValue, authErr)
		return
	}
	if request.Method == http.MethodPost || request.Method == http.MethodPut || request.Method == http.MethodDelete || request.Method == http.MethodPatch {
		origins := request.Header.Values("Origin")
		if len(origins) != 1 || origins[0] != server.Config.ExternalOrigin {
			server.writeRejectedProblem(c.Writer, request, pathValue, newAPIError("origin_invalid", "request origin is not authorized", http.StatusForbidden, "authentication", false, ""))
			return
		}
		csrfValues := request.Header.Values("X-CSRF-Token")
		if len(csrfValues) != 1 {
			server.writeRejectedProblem(c.Writer, request, pathValue, newAPIError("csrf_invalid", "request CSRF token is not valid", http.StatusForbidden, "authentication", false, ""))
			return
		}
		csrf := csrfValues[0]
		if csrf == "" || !server.OIDC.validCSRF(request, session, csrf) {
			server.writeRejectedProblem(c.Writer, request, pathValue, newAPIError("csrf_invalid", "request CSRF token is not valid", http.StatusForbidden, "authentication", false, ""))
			return
		}
		if !isUploadContentPath(request.Method, pathValue) {
			mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
			if err != nil || mediaType != "application/json" {
				writeProblem(c.Writer, invalidRequest("management mutations require application/json"))
				return
			}
		}
	}
	if pathValue == "/session" {
		writeProblem(c.Writer, newAPIError("invalid_request", "session must be established with GET", http.StatusMethodNotAllowed, "request_validation", false, ""))
		return
	}
	if pathValue == "/tokens" || strings.HasPrefix(pathValue, "/tokens/") {
		server.tokenRoute(c.Writer, request, pathValue, principal, session)
		return
	}
	if required := requiredCapabilities(request.Method, pathValue); len(required) > 0 && !hasAnyCapability(session.Capabilities, required) {
		server.writeRejectedProblem(c.Writer, request, pathValue, newAPIError("permission_denied", "the administrator capability is required", http.StatusForbidden, "authorization", false, ""))
		return
	}
	if server.Authority == nil {
		writeProblem(c.Writer, newAPIError("authority_unavailable", "management authority is unavailable", http.StatusServiceUnavailable, "internal", true, ""))
		return
	}
	server.Authority.route(c.Writer, request, pathValue, principal.actor())
}

func isUploadContentPath(method, pathValue string) bool {
	return method == http.MethodPut && strings.HasPrefix(pathValue, "/images/imports/") && strings.HasSuffix(pathValue, "/content")
}

func requiredCapabilities(method, pathValue string) []string {
	if method == http.MethodGet {
		switch {
		case pathValue == "/operations", strings.HasPrefix(pathValue, "/operations/"):
			return []string{"read"}
		case pathValue == "/images", strings.HasPrefix(pathValue, "/images/"):
			return []string{"observe_admin", "manage_images"}
		case pathValue == "/audit", pathValue == "/system", pathValue == "/overview", pathValue == "/vms", strings.HasPrefix(pathValue, "/vms/"):
			return []string{"observe_admin"}
		}
		return nil
	}
	switch {
	case strings.HasPrefix(pathValue, "/vms/"):
		return []string{"operate"}
	case strings.HasPrefix(pathValue, "/images/"):
		return []string{"manage_images"}
	case strings.HasPrefix(pathValue, "/system/maintenance"):
		return []string{"maintain_system"}
	default:
		return nil
	}
}

func hasAnyCapability(values, required []string) bool {
	for _, capability := range required {
		if containsString(values, capability) {
			return true
		}
	}
	return false
}

func (server *UnifiedServer) writeSession(writer http.ResponseWriter, principal unifiedPrincipal, session *state.BrowserSession) {
	csrf := server.OIDC.csrfToken(session.SessionDigest)
	writeJSON(writer, http.StatusOK, map[string]any{
		"issuer": principal.Issuer, "subject": principal.Subject, "role": "administrator",
		"capabilities": append([]string(nil), session.Capabilities...), "csrf_token": csrf,
		"expires_at": session.ExpiresAt,
	})
}

func (server *UnifiedServer) serveStatic(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writeProblem(writer, newAPIError("invalid_request", "method is not supported", http.StatusMethodNotAllowed, "request_validation", false, ""))
		return
	}
	if server.Assets == nil {
		writeProblem(writer, newAPIError("internal_error", "Web assets are unavailable", http.StatusInternalServerError, "internal", true, ""))
		return
	}
	name, valid := staticAssetName(request.URL.Path)
	if !valid || strings.HasPrefix(name, "api/") || strings.HasPrefix(name, "agent/") || strings.HasPrefix(name, "internal/") {
		writeProblem(writer, newAPIError("not_found", "Web asset was not found", http.StatusNotFound, "request_validation", false, ""))
		return
	}
	data, found := readStaticAsset(server.Assets, name)
	if !found && name != "index.html" && staticDeepLink(request, name) {
		name = "index.html"
		data, found = readStaticAsset(server.Assets, name)
	}
	if !found {
		writeProblem(writer, newAPIError("not_found", "Web asset was not found", http.StatusNotFound, "request_validation", false, ""))
		return
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	if strings.HasPrefix(contentType, "text/") || contentType == "application/javascript" || contentType == "application/json" {
		contentType += "; charset=utf-8"
	}
	writer.Header().Set("Content-Type", contentType)
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if name == "index.html" || !strings.HasPrefix(name, "assets/") {
		writer.Header().Set("Cache-Control", "no-cache")
	} else {
		writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	writer.WriteHeader(http.StatusOK)
	if request.Method != http.MethodHead {
		_, _ = writer.Write(data)
	}
}

func (server *UnifiedServer) actorFromSession(session *state.BrowserSession) unifiedPrincipal {
	return unifiedPrincipal{Issuer: session.Issuer, Subject: session.Subject, Capabilities: append([]string(nil), session.Capabilities...)}
}

func (server *UnifiedServer) sessionFor(request *http.Request) (unifiedPrincipal, *state.BrowserSession, *APIError) {
	return server.OIDC.sessionFor(request)
}

type unifiedPrincipal struct {
	Issuer       string
	Subject      string
	Capabilities []string
}

func (principal unifiedPrincipal) actor() string {
	return principal.Issuer + "\x00" + principal.Subject
}

func (principal unifiedPrincipal) value() map[string]any {
	return map[string]any{"issuer": principal.Issuer, "subject": principal.Subject, "role": "administrator"}
}

func digestString(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func (server *UnifiedServer) now() time.Time {
	if server.Clock == nil {
		return time.Now().UTC()
	}
	return server.Clock().UTC()
}
