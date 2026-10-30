package webadmin

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

var agentTokenScopes = map[string]bool{
	"agent:read": true, "agent:operate": true, "agent:sync": true, "agent:observe_admin": true,
}

type tokenCreatePayload struct {
	OperationID string   `json:"operation_id"`
	Reason      string   `json:"reason"`
	Scopes      []string `json:"scopes"`
	ExpiresAt   string   `json:"expires_at"`
}

type tokenRevokePreviewPayload struct {
	TargetVersion string `json:"target_version"`
}

type tokenRevokePayload struct {
	OperationID       string `json:"operation_id"`
	Reason            string `json:"reason"`
	TargetVersion     string `json:"target_version"`
	ConfirmationToken string `json:"confirmation_token"`
}

func (server *UnifiedServer) tokenRoute(writer http.ResponseWriter, request *http.Request, pathValue string, principal unifiedPrincipal, session *state.BrowserSession) {
	if !containsString(session.Capabilities, "manage_tokens") {
		writeProblem(writer, newAPIError("permission_denied", "Token-management capability is required", http.StatusForbidden, "authorization", false, ""))
		return
	}
	switch {
	case pathValue == "/tokens" && request.Method == http.MethodGet:
		server.listTokens(writer, request)
	case pathValue == "/tokens" && request.Method == http.MethodPost:
		server.createToken(writer, request, principal)
	case strings.HasPrefix(pathValue, "/tokens/") && strings.HasSuffix(pathValue, "/revoke/preview") && request.Method == http.MethodPost:
		server.previewTokenRevoke(writer, request, strings.TrimSuffix(strings.TrimPrefix(pathValue, "/tokens/"), "/revoke/preview"), principal)
	case strings.HasPrefix(pathValue, "/tokens/") && strings.HasSuffix(pathValue, "/revoke") && request.Method == http.MethodPost:
		server.revokeToken(writer, request, strings.TrimSuffix(strings.TrimPrefix(pathValue, "/tokens/"), "/revoke"), principal)
	case strings.HasPrefix(pathValue, "/tokens/") && request.Method == http.MethodGet:
		server.getToken(writer, request, strings.TrimPrefix(pathValue, "/tokens/"))
	default:
		writeProblem(writer, newAPIError("not_found", "Token route was not found", http.StatusNotFound, "request_validation", false, ""))
	}
}

func (server *UnifiedServer) listTokens(writer http.ResponseWriter, request *http.Request) {
	if err := rejectUnknownQueryKeys(request.URL.Query(), map[string]bool{"limit": true, "cursor": true}); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	limit, cursor, err := pageRequest(request.URL.Query())
	if err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	createdAt, tokenID := pageKeyParts(cursor)
	values, more, err := server.Store.ListAgentTokensPage(request.Context(), createdAt, tokenID, limit)
	if err != nil {
		writeProblem(writer, newAPIError("state_unavailable", "Token state could not be read", http.StatusServiceUnavailable, "internal", true, ""))
		return
	}
	items := make([]any, 0, len(values))
	for _, value := range values {
		items = append(items, agentTokenValue(value))
	}
	next := ""
	if len(values) > 0 {
		last := values[len(values)-1]
		next = last.CreatedAt + "\x00" + last.TokenID
	}
	writeJSON(writer, http.StatusOK, map[string]any{"items": items, "page": pageValue(limit, more, next)})
}

func (server *UnifiedServer) getToken(writer http.ResponseWriter, request *http.Request, tokenID string) {
	if !validTokenID(tokenID) {
		writeProblem(writer, invalidRequest("token_id is invalid"))
		return
	}
	if len(request.URL.Query()) != 0 {
		writeProblem(writer, invalidRequest("query parameters are not supported"))
		return
	}
	value, err := server.Store.GetAgentToken(request.Context(), tokenID)
	if err != nil {
		writeProblem(writer, newAPIError("state_unavailable", "Token state could not be read", http.StatusServiceUnavailable, "internal", true, ""))
		return
	}
	if value == nil {
		writeProblem(writer, newAPIError("not_found", "the requested Token was not found", http.StatusNotFound, "request_validation", false, ""))
		return
	}
	writeJSON(writer, http.StatusOK, agentTokenValue(*value))
}

func (server *UnifiedServer) createToken(writer http.ResponseWriter, request *http.Request, principal unifiedPrincipal) {
	var payload tokenCreatePayload
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := ensureOperationID(payload.OperationID); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := validateReason(payload.Reason); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	scopes, err := normalizeTokenScopes(payload.Scopes)
	if err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	expiresAt, expiresValue, err := server.validateTokenExpiry(payload.ExpiresAt)
	if err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	requestValue := map[string]any{"scopes": scopeValues(scopes), "expires_at": expiresValue}
	if payload.Reason != "" {
		requestValue["reason"] = payload.Reason
	}
	digest, err := protocol.Digest(protocol.Request(requestValue))
	if err != nil {
		writeProblem(writer, invalidRequest("Token request cannot be canonicalized"))
		return
	}
	actor := principal.actor()
	existing, err := server.Authority.Management.GetOperation(request.Context(), payload.OperationID)
	if err != nil {
		writeProblem(writer, newAPIError("state_unavailable", "Token operation state could not be read", http.StatusServiceUnavailable, "internal", true, payload.OperationID))
		return
	}
	if existing != nil {
		if existing.Action != "token_create" || existing.Actor != actor || stringValue(existing.Request["_request_digest"]) != digest {
			writeProblem(writer, newAPIError("operation_conflict", "operation ID belongs to a different request", http.StatusConflict, "precondition", false, payload.OperationID))
			return
		}
		value, stateErr := server.Store.GetAgentTokenByOperation(request.Context(), payload.OperationID)
		if stateErr != nil || value == nil {
			writeProblem(writer, newAPIError("state_unavailable", "Token operation result is unavailable", http.StatusServiceUnavailable, "internal", true, payload.OperationID))
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"operation_id": payload.OperationID, "metadata": agentTokenValue(*value), "plaintext_disclosure": "consumed"})
		return
	}
	rawToken, err := randomToken(32)
	if err != nil {
		writeProblem(writer, newAPIError("internal_error", "Token could not be generated", http.StatusInternalServerError, "internal", true, payload.OperationID))
		return
	}
	tokenID, err := randomToken(16)
	if err != nil {
		writeProblem(writer, newAPIError("internal_error", "Token could not be generated", http.StatusInternalServerError, "internal", true, payload.OperationID))
		return
	}
	resourceVersion, err := randomToken(24)
	if err != nil {
		writeProblem(writer, newAPIError("internal_error", "Token could not be generated", http.StatusInternalServerError, "internal", true, payload.OperationID))
		return
	}
	now := server.now()
	value := state.AgentToken{
		TokenID: tokenID, TokenDigest: digestString(rawToken), OperationID: payload.OperationID, RequestDigest: digest,
		Issuer: principal.Issuer, Subject: principal.Subject, Scopes: scopes, CreatedAt: now.Format(time.RFC3339Nano),
		ExpiresAt: expiresAt.Format(time.RFC3339Nano), ResourceVersion: "token:" + resourceVersion, PlaintextDisclosure: "returned",
	}
	operation := state.ManagementOperation{
		OperationID: payload.OperationID, Action: "token_create", TargetKind: "token", TargetID: tokenID,
		Actor: actor, Reason: payload.Reason, Phase: "durable_state", Status: "succeeded",
		Request:   map[string]any{"_request_digest": digest, "request": requestValue},
		Result:    map[string]any{"token_metadata": agentTokenValue(value), "plaintext_disclosure": "returned"},
		CreatedAt: value.CreatedAt, UpdatedAt: value.CreatedAt,
	}
	audit := state.ManagementAudit{EventID: uuid.NewString(), OperationID: stringPointer(payload.OperationID), Action: "token_create", Actor: actor, Reason: payload.Reason, Target: map[string]any{"kind": "token", "id": tokenID}, Outcome: "succeeded", Phase: "durable_state", OccurredAt: value.CreatedAt}
	committedOperation, committedToken, created, err := server.Store.ClaimAgentTokenWithOperation(request.Context(), value, operation, audit)
	if err != nil {
		writeProblem(writer, newAPIError("state_unavailable", "Token could not be committed", http.StatusServiceUnavailable, "internal", true, payload.OperationID))
		return
	}
	if committedOperation == nil || committedToken == nil {
		writeProblem(writer, newAPIError("state_unavailable", "Token operation result is unavailable", http.StatusServiceUnavailable, "internal", true, payload.OperationID))
		return
	}
	if !created {
		if committedOperation.Action != "token_create" || committedOperation.Actor != actor || stringValue(committedOperation.Request["_request_digest"]) != digest {
			writeProblem(writer, newAPIError("operation_conflict", "operation ID belongs to a different request", http.StatusConflict, "precondition", false, payload.OperationID))
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"operation_id": committedOperation.OperationID, "metadata": agentTokenValue(*committedToken), "plaintext_disclosure": "consumed"})
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"operation_id": committedOperation.OperationID, "token": rawToken, "metadata": agentTokenValue(*committedToken), "plaintext_disclosure": "returned"})
}

func (server *UnifiedServer) previewTokenRevoke(writer http.ResponseWriter, request *http.Request, tokenID string, principal unifiedPrincipal) {
	if !validTokenID(tokenID) {
		writeProblem(writer, invalidRequest("token_id is invalid"))
		return
	}
	var payload tokenRevokePreviewPayload
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if payload.TargetVersion == "" || len(payload.TargetVersion) > 256 {
		writeProblem(writer, invalidRequest("target_version is invalid"))
		return
	}
	value, err := server.Store.GetAgentToken(request.Context(), tokenID)
	if err != nil {
		writeProblem(writer, newAPIError("state_unavailable", "Token state could not be read", http.StatusServiceUnavailable, "internal", true, ""))
		return
	}
	if value == nil {
		writeProblem(writer, newAPIError("not_found", "the requested Token was not found", http.StatusNotFound, "request_validation", false, ""))
		return
	}
	if value.ResourceVersion != payload.TargetVersion {
		writeProblem(writer, newAPIError("stale_target", "the Token has changed; reload before retrying", http.StatusConflict, "precondition", false, ""))
		return
	}
	if value.RevokedAt != nil {
		writeProblem(writer, newAPIError("operation_conflict", "the Token is already revoked", http.StatusConflict, "precondition", false, ""))
		return
	}
	params := map[string]any{"token_id": tokenID, "target_version": payload.TargetVersion, "metadata": agentTokenValue(*value)}
	token, expiresAt, err := issueDurableConfirmation(request.Context(), server.Store, server.now(), principal.actor(), "token_revoke", "token:"+tokenID, payload.TargetVersion, params)
	if err != nil {
		writeProblem(writer, problemFromError(err, "internal", ""))
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"token_id": tokenID, "target_version": payload.TargetVersion, "confirmation_token": token, "expires_at": expiresAt.UTC().Format(time.RFC3339Nano), "metadata": agentTokenValue(*value)})
}

func (server *UnifiedServer) revokeToken(writer http.ResponseWriter, request *http.Request, tokenID string, principal unifiedPrincipal) {
	if !validTokenID(tokenID) {
		writeProblem(writer, invalidRequest("token_id is invalid"))
		return
	}
	var payload tokenRevokePayload
	if err := decodeJSON(request, &payload); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if err := ensureOperationID(payload.OperationID); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	if payload.TargetVersion == "" || len(payload.TargetVersion) > 256 || payload.ConfirmationToken == "" {
		writeProblem(writer, invalidRequest("target_version and confirmation_token are required"))
		return
	}
	if err := validateReason(payload.Reason); err != nil {
		writeProblem(writer, invalidRequest(err.Error()))
		return
	}
	requestValue := map[string]any{"token_id": tokenID, "target_version": payload.TargetVersion, "confirmation_digest": digestString(payload.ConfirmationToken)}
	if payload.Reason != "" {
		requestValue["reason"] = payload.Reason
	}
	digest, err := protocol.Digest(protocol.Request(requestValue))
	if err != nil {
		writeProblem(writer, invalidRequest("Token revoke request cannot be canonicalized"))
		return
	}
	actor := principal.actor()
	existing, err := server.Authority.Management.GetOperation(request.Context(), payload.OperationID)
	if err != nil {
		writeProblem(writer, newAPIError("state_unavailable", "Token operation state could not be read", http.StatusServiceUnavailable, "internal", true, payload.OperationID))
		return
	}
	if existing != nil {
		if existing.Action != "token_revoke" || existing.Actor != actor || stringValue(existing.Request["_request_digest"]) != digest {
			writeProblem(writer, newAPIError("operation_conflict", "operation ID belongs to a different request", http.StatusConflict, "precondition", false, payload.OperationID))
			return
		}
		writeJSON(writer, http.StatusAccepted, operationValue(*existing))
		return
	}
	value, err := server.Store.GetAgentToken(request.Context(), tokenID)
	if err != nil {
		writeProblem(writer, newAPIError("state_unavailable", "Token state could not be read", http.StatusServiceUnavailable, "internal", true, payload.OperationID))
		return
	}
	if value == nil {
		writeProblem(writer, newAPIError("not_found", "the requested Token was not found", http.StatusNotFound, "request_validation", false, payload.OperationID))
		return
	}
	if value.ResourceVersion != payload.TargetVersion {
		writeProblem(writer, newAPIError("stale_target", "the Token has changed; reload before retrying", http.StatusConflict, "precondition", false, payload.OperationID))
		return
	}
	params := map[string]any{"token_id": tokenID, "target_version": payload.TargetVersion, "metadata": agentTokenValue(*value)}
	confirmation, err := durableConfirmationClaim(payload.ConfirmationToken, actor, "token_revoke", "token:"+tokenID, payload.TargetVersion, params)
	if err != nil {
		writeProblem(writer, problemFromError(err, "precondition", payload.OperationID))
		return
	}
	nextVersion, err := randomToken(24)
	if err != nil {
		writeProblem(writer, newAPIError("internal_error", "Token revocation could not be prepared", http.StatusInternalServerError, "internal", true, payload.OperationID))
		return
	}
	now := server.now().Format(time.RFC3339Nano)
	operation := state.ManagementOperation{
		OperationID: payload.OperationID, Action: "token_revoke", TargetKind: "token", TargetID: tokenID,
		Actor: actor, Reason: payload.Reason, Phase: "durable_state", Status: "succeeded",
		Request: map[string]any{"_request_digest": digest, "request": requestValue},
		Result:  map[string]any{"token_id": tokenID, "effect": "revoked"}, CreatedAt: now, UpdatedAt: now,
	}
	audit := state.ManagementAudit{EventID: uuid.NewString(), OperationID: stringPointer(payload.OperationID), Action: "token_revoke", Actor: actor, Reason: payload.Reason, Target: map[string]any{"kind": "token", "id": tokenID}, Outcome: "succeeded", Phase: "durable_state", OccurredAt: now}
	committedOperation, _, _, err := server.Store.RevokeAgentTokenWithOperationAndConfirmation(request.Context(), tokenID, payload.TargetVersion, "token:"+nextVersion, now, confirmation, operation, audit)
	if err != nil {
		writeProblem(writer, problemFromError(err, "precondition", payload.OperationID))
		return
	}
	if committedOperation == nil {
		writeProblem(writer, newAPIError("state_unavailable", "Token revocation result is unavailable", http.StatusServiceUnavailable, "internal", true, payload.OperationID))
		return
	}
	if committedOperation.Action != "token_revoke" || committedOperation.Actor != actor || stringValue(committedOperation.Request["_request_digest"]) != digest {
		writeProblem(writer, newAPIError("operation_conflict", "operation ID belongs to a different request", http.StatusConflict, "precondition", false, payload.OperationID))
		return
	}
	writeJSON(writer, http.StatusAccepted, operationValue(*committedOperation))
}

func normalizeTokenScopes(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > len(agentTokenScopes) {
		return nil, errors.New("scopes must contain at least one supported value")
	}
	seen := make(map[string]bool, len(values))
	result := append([]string(nil), values...)
	for _, value := range result {
		if !agentTokenScopes[value] || seen[value] {
			return nil, errors.New("scopes contains an unsupported or duplicate value")
		}
		seen[value] = true
	}
	if seen["agent:operate"] && !seen["agent:read"] {
		return nil, errors.New("agent:operate requires explicit agent:read")
	}
	sort.Strings(result)
	return result, nil
}

func (server *UnifiedServer) validateTokenExpiry(raw string) (time.Time, string, error) {
	if raw == "" {
		return time.Time{}, "", errors.New("expires_at is required")
	}
	value, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, "", errors.New("expires_at must be an RFC3339 timestamp")
	}
	now := server.now()
	maximum := now.Add(time.Duration(server.Config.TokenTTLSeconds) * time.Second)
	if !value.After(now) || value.After(maximum) {
		return time.Time{}, "", fmt.Errorf("expires_at must be within %d seconds", server.Config.TokenTTLSeconds)
	}
	value = value.UTC()
	return value, value.Format(time.RFC3339Nano), nil
}

func agentTokenValue(value state.AgentToken) map[string]any {
	var revoked any
	if value.RevokedAt != nil {
		revoked = *value.RevokedAt
	}
	return map[string]any{"token_id": value.TokenID, "resource_version": value.ResourceVersion, "scopes": scopeValues(value.Scopes), "created_at": value.CreatedAt, "expires_at": value.ExpiresAt, "revoked_at": revoked}
}

func scopeValues(values []string) []any {
	result := make([]any, 0, len(values))
	for _, value := range values {
		result = append(result, value)
	}
	return result
}

func validTokenID(value string) bool {
	if len(value) < 16 || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '-' && char != '_' {
			return false
		}
	}
	return true
}

func stringPointer(value string) *string { return &value }
