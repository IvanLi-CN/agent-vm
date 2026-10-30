package webadmin

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

func (server *UnifiedServer) agentControl(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		writeAgentError(writer, nil, protocol.NewError("invalid_request", "Agent API requires POST", nil))
		return
	}
	if _, err := request.Cookie(managementSessionCookie); err == nil {
		writeAgentError(writer, nil, protocol.NewError("permission_denied", "browser sessions cannot call the Agent API", nil))
		return
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAgentError(writer, nil, protocol.NewError("invalid_request", "Agent API requires application/json", nil))
		return
	}
	token, authErr := server.agentToken(request)
	if authErr != nil {
		writeAgentError(writer, nil, authErr)
		return
	}
	reader := http.MaxBytesReader(writer, request.Body, maxAgentBodyBytes)
	defer request.Body.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		writeAgentError(writer, nil, protocol.NewError("request_too_large", "Agent request body exceeds the limit", nil))
		return
	}
	controlRequest, err := protocol.Decode(body)
	if err != nil {
		writeAgentError(writer, nil, err)
		return
	}
	if err := protocol.ValidateRequest(controlRequest); err != nil {
		writeAgentError(writer, controlRequest, err)
		return
	}
	action, _ := controlRequest["action"].(string)
	if !agentTokenAllows(token.Scopes, action) {
		writeAgentError(writer, controlRequest, protocol.NewError("permission_denied", "Agent Token scope does not authorize this action", nil))
		return
	}
	if server.Lifecycle == nil {
		writeAgentError(writer, controlRequest, protocol.NewError("state_unavailable", "control lifecycle is unavailable", nil))
		return
	}
	response, dispatchErr := server.Lifecycle.Dispatch(request.Context(), controlRequest)
	if dispatchErr != nil {
		writeAgentError(writer, controlRequest, dispatchErr)
		return
	}
	writeAgentJSON(writer, http.StatusOK, response, 0)
}

func (server *UnifiedServer) agentToken(request *http.Request) (*state.AgentToken, *protocol.Error) {
	values := request.Header.Values("Authorization")
	if len(values) != 1 {
		return nil, protocol.NewError("authentication_required", "Agent Token authentication is required", nil)
	}
	value := strings.TrimSpace(values[0])
	if !strings.HasPrefix(value, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(value, "Bearer ")) == "" {
		return nil, protocol.NewError("authentication_required", "Agent Token authentication is required", nil)
	}
	rawToken := strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	if strings.ContainsAny(rawToken, " \t\r\n") || len(rawToken) > 512 {
		return nil, protocol.NewError("authentication_required", "Agent Token authentication is required", nil)
	}
	token, err := server.Store.GetAgentTokenByDigest(request.Context(), digestString(rawToken))
	if err != nil {
		return nil, protocol.NewError("state_unavailable", "Agent Token state is unavailable", nil)
	}
	if token == nil {
		return nil, protocol.NewError("authentication_required", "Agent Token is not valid", nil)
	}
	expires, err := time.Parse(time.RFC3339Nano, token.ExpiresAt)
	if err != nil || !expires.After(server.now()) {
		return nil, protocol.NewError("token_expired", "Agent Token has expired", nil)
	}
	if token.RevokedAt != nil {
		return nil, protocol.NewError("token_revoked", "Agent Token has been revoked", nil)
	}
	return token, nil
}

func agentTokenAllows(scopes []string, action string) bool {
	readOnly := map[string]bool{"doctor": true, "status": true, "port-list": true}
	operate := map[string]bool{"acquire": true, "renew": true, "release": true, "destroy": true, "restart": true, "resize": true, "port-add": true, "port-remove": true}
	adminObserve := map[string]bool{"admin-metrics": true, "admin-status": true, "admin-session-list": true, "drain-status": true}
	for _, scope := range scopes {
		switch {
		case scope == "agent:read" && readOnly[action]:
			return true
		case scope == "agent:operate" && operate[action]:
			return true
		case scope == "agent:sync" && action == "ssh-info":
			return true
		case scope == "agent:observe_admin" && adminObserve[action]:
			return true
		}
	}
	return false
}

func writeAgentError(writer http.ResponseWriter, request protocol.Request, err error) {
	control := safeAgentError(err)
	status := agentErrorStatus(control.Code)
	if request == nil {
		writeAgentJSON(writer, status, map[string]any{"ok": false, "protocol": protocol.Version, "error": control}, 2)
		return
	}
	writeAgentJSON(writer, status, protocol.ErrorEnvelope(request, control), 2)
}

func writeAgentJSON(writer http.ResponseWriter, status int, value any, exitCode int) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("X-Agent-VM-Exit-Code", strconv.Itoa(exitCode))
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func safeAgentError(err error) *protocol.Error {
	if err == nil {
		return protocol.NewError("internal_error", "Agent request failed", nil)
	}
	control := protocol.AsError(err)
	code := safeAgentCode(control.Code)
	messages := map[string]string{
		"authentication_required": "Agent Token authentication is required",
		"permission_denied":       "Agent Token scope does not authorize this action",
		"token_expired":           "Agent Token has expired",
		"token_revoked":           "Agent Token has been revoked",
		"state_unavailable":       "Agent state is unavailable",
		"invalid_request":         "Agent request is invalid",
		"protocol_mismatch":       "Agent protocol is not supported",
		"unknown_action":          "Agent action is not supported",
		"operation_conflict":      "Agent operation conflicts with durable state",
		"generation_mismatch":     "Agent generation is stale",
		"lease_expired":           "Agent lease has expired",
		"task_missing":            "Agent session was not found",
		"task_not_active":         "Agent session is not active",
		"draining":                "Agent service is draining",
	}
	message, ok := messages[code]
	if !ok {
		message = "Agent request failed"
	}
	return protocol.NewError(code, message, nil)
}

func safeAgentCode(code string) string {
	switch code {
	case "authentication_required", "permission_denied", "token_expired", "token_revoked", "state_unavailable", "invalid_request", "protocol_mismatch", "unknown_action", "operation_conflict", "generation_mismatch", "lease_expired", "task_missing", "task_not_active", "draining", "request_too_large", "resource_insufficient", "startup_reconcile_required":
		return code
	default:
		return "internal_error"
	}
}

func agentErrorStatus(code string) int {
	switch code {
	case "authentication_required", "token_expired", "token_revoked":
		return http.StatusUnauthorized
	case "permission_denied":
		return http.StatusForbidden
	case "invalid_request", "protocol_mismatch", "unknown_action", "request_too_large":
		if code == "request_too_large" {
			return http.StatusRequestEntityTooLarge
		}
		return http.StatusBadRequest
	case "operation_conflict", "generation_mismatch", "lease_expired", "task_missing", "task_not_active", "draining", "resource_insufficient":
		return http.StatusConflict
	case "state_unavailable", "startup_reconcile_required":
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
