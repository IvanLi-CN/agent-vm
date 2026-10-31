package webadmin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

type APIError struct {
	Code        string
	Message     string
	Status      int
	Phase       string
	Retryable   bool
	OperationID string
	SafeFields  map[string]any
}

func newAPIError(code, message string, status int, phase string, retryable bool, operationID string) *APIError {
	return &APIError{Code: code, Message: message, Status: status, Phase: normalizePhase(phase), Retryable: retryable, OperationID: operationID}
}

func (err *APIError) Error() string { return err.Code + ": " + err.Message }

func problemFromError(cause error, phase, operationID string) *APIError {
	if cause == nil {
		return newAPIError("internal_error", "management request failed", http.StatusInternalServerError, phase, true, operationID)
	}
	var apiErr *APIError
	if errors.As(cause, &apiErr) {
		if apiErr.OperationID == "" {
			apiErr.OperationID = operationID
		}
		return apiErr
	}
	code := safeErrorCode(cause)
	status := http.StatusInternalServerError
	retryable := false
	switch code {
	case "authentication_required":
		status = http.StatusUnauthorized
	case "administrator_required", "permission_denied", "csrf_invalid", "origin_invalid":
		status = http.StatusForbidden
	case "invalid_request", "invalid_state", "unsupported_action", "source_not_allowed":
		status = http.StatusBadRequest
	case "not_found":
		status = http.StatusNotFound
	case "stale_target", "operation_conflict", "precondition_required", "image_referenced", "default_image_required", "fingerprint_mismatch", "image_not_applicable", "lease_expired", "task_not_active", "draining", "mapping_unverified", "cleanup_incomplete", "admission_denied", "resource_insufficient":
		status = http.StatusConflict
	case "upload_too_large":
		status = http.StatusRequestEntityTooLarge
	case "authority_unavailable", "state_unavailable", "oidc_unavailable", "staging_capacity_low", "capacity_unverified", "resource_unverified", "startup_reconcile_required", "sqlite_unavailable", "watermarks_unreachable":
		status = http.StatusServiceUnavailable
		retryable = true
	case "rate_limited":
		status = http.StatusTooManyRequests
		retryable = true
	case "internal_error":
		status = http.StatusInternalServerError
		retryable = true
	}
	value := newAPIError(code, publicErrorMessage(code), status, phaseForCode(code, phase), retryable, operationID)
	var referenceErr *state.ImageReferenceError
	if errors.As(cause, &referenceErr) {
		value.SafeFields = map[string]any{"affected_references": referenceErr.Counts}
	}
	return value
}

func safeErrorCode(cause error) string {
	var control *protocol.Error
	if errors.As(cause, &control) {
		return safeCode(control.Code)
	}
	message := cause.Error()
	if index := strings.IndexByte(message, ':'); index > 0 {
		return safeCode(message[:index])
	}
	return "internal_error"
}

func safeCode(value string) string {
	switch value {
	case "authentication_required", "administrator_required", "csrf_invalid", "origin_invalid", "invalid_request", "invalid_state", "stale_target", "operation_conflict", "not_found", "unsupported_action", "permission_denied", "source_not_allowed", "upload_too_large", "staging_capacity_low", "fingerprint_mismatch", "image_not_applicable", "image_referenced", "default_image_required", "capacity_unverified", "admission_denied", "precondition_required", "authority_unavailable", "rate_limited", "sqlite_unavailable", "state_unavailable", "oidc_unavailable", "token_expired", "token_revoked", "internal_error", "lease_expired", "task_not_active", "draining", "mapping_unverified", "cleanup_incomplete", "resource_unverified", "startup_reconcile_required", "resource_insufficient", "watermarks_unreachable":
		return value
	default:
		return "internal_error"
	}
}

func phaseForCode(code, fallback string) string {
	if fallback != "" {
		return normalizePhase(fallback)
	}
	switch code {
	case "authentication_required", "administrator_required", "csrf_invalid", "origin_invalid":
		return "authentication"
	case "stale_target", "precondition_required":
		return "precondition"
	case "upload_too_large", "fingerprint_mismatch", "staging_capacity_low":
		return "upload"
	case "authority_unavailable", "internal_error":
		return "internal"
	default:
		return "request_validation"
	}
}

func normalizePhase(phase string) string {
	if phase == "native_mutation" {
		return "native_action"
	}
	if phase == "" {
		return "internal"
	}
	return phase
}

func publicErrorMessage(code string) string {
	messages := map[string]string{
		"authentication_required":    "administrator authentication is required",
		"oidc_unavailable":           "the configured identity provider is unavailable",
		"administrator_required":     "administrator access is required",
		"csrf_invalid":               "request CSRF token is not valid",
		"origin_invalid":             "request origin is not authorized",
		"invalid_request":            "request is invalid",
		"invalid_state":              "stored management state is invalid",
		"stale_target":               "the target has changed; reload before retrying",
		"operation_conflict":         "operation ID belongs to a different request",
		"not_found":                  "the requested resource was not found",
		"unsupported_action":         "the requested action is not supported",
		"permission_denied":          "the requested action is not permitted",
		"source_not_allowed":         "the requested image source is not allowed",
		"upload_too_large":           "the upload exceeds the configured limit",
		"staging_capacity_low":       "upload staging capacity is insufficient",
		"fingerprint_mismatch":       "image integrity verification failed",
		"image_not_applicable":       "the image is not applicable to Agent VM",
		"image_referenced":           "the image still has references",
		"default_image_required":     "the default image cannot be removed",
		"capacity_unverified":        "capacity could not be verified",
		"admission_denied":           "the requested operation is not admitted",
		"resource_insufficient":      "the requested resources are insufficient",
		"watermarks_unreachable":     "configured storage watermarks could not be reached",
		"precondition_required":      "the operation requires a fresh confirmation",
		"token_expired":              "the Agent Token has expired",
		"token_revoked":              "the Agent Token has been revoked",
		"state_unavailable":          "durable state is unavailable",
		"authority_unavailable":      "management authority is unavailable",
		"lease_expired":              "the VM lease is no longer effective",
		"task_not_active":            "the VM is not active",
		"draining":                   "the control service is draining",
		"mapping_unverified":         "the network mapping could not be verified",
		"cleanup_incomplete":         "cleanup is incomplete and requires recovery",
		"resource_unverified":        "the resource state could not be verified",
		"startup_reconcile_required": "startup recovery is still required",
		"internal_error":             "management request failed",
	}
	if message, ok := messages[code]; ok {
		return message
	}
	return messages["internal_error"]
}

func writeProblem(writer http.ResponseWriter, err *APIError) {
	if err == nil {
		err = newAPIError("internal_error", "management request failed", http.StatusInternalServerError, "internal", true, "")
	}
	fields := map[string]any{
		"code":      err.Code,
		"phase":     err.Phase,
		"retryable": err.Retryable,
	}
	if err.OperationID != "" {
		fields["operation_id"] = err.OperationID
	}
	if err.SafeFields != nil {
		fields["safe_fields"] = err.SafeFields
	}
	writeJSON(writer, err.Status, map[string]any{"error": fields})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func invalidRequest(message string) *APIError {
	return newAPIError("invalid_request", message, http.StatusBadRequest, "request_validation", false, "")
}

func ensureOperationID(operationID string) error {
	if len(operationID) != 36 {
		return fmt.Errorf("operation ID is not a UUID")
	}
	for index, value := range operationID {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if value != '-' {
				return fmt.Errorf("operation ID is not a UUID")
			}
			continue
		}
		if !strings.ContainsRune("0123456789abcdefABCDEF", value) {
			return fmt.Errorf("operation ID is not a UUID")
		}
	}
	return nil
}
