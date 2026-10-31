package webadmin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

const (
	rejectedActorPrefix       = "\x00rejected\x00"
	maxRejectedAuditBodyBytes = 64 << 10
)

func rejectedActor(kind string) string {
	return rejectedActorPrefix + kind
}

func rejectedActorKind(value string) string {
	if !strings.HasPrefix(value, rejectedActorPrefix) {
		return ""
	}
	kind := strings.TrimPrefix(value, rejectedActorPrefix)
	if kind == "unauthenticated" || kind == "unauthorized" {
		return kind
	}
	return ""
}

func (server *UnifiedServer) writeRejectedProblem(writer http.ResponseWriter, request *http.Request, pathValue string, apiErr *APIError) {
	recordRejectedManagementAudit(server.Store, server.Clock, request, pathValue, apiErr)
	writeProblem(writer, apiErr)
}

func recordRejectedManagementAudit(store *state.Store, clock func() time.Time, request *http.Request, pathValue string, apiErr *APIError) {
	if store == nil || request == nil || apiErr == nil {
		return
	}
	actorKind := rejectedActorKindForCode(apiErr.Code)
	if actorKind == "" {
		return
	}
	action, target := rejectedAuditIdentity(request, pathValue)
	var operationID *string
	if apiErr.OperationID != "" && ensureOperationID(apiErr.OperationID) == nil {
		operationID = &apiErr.OperationID
	}
	if clock == nil {
		clock = time.Now
	}
	audit := state.ManagementAudit{
		EventID:     uuid.NewString(),
		OperationID: operationID,
		Action:      action,
		Actor:       rejectedActor(actorKind),
		Target:      target,
		Outcome:     "rejected",
		Phase:       "reconciliation",
		Detail:      map[string]any{"code": safeCode(apiErr.Code)},
		OccurredAt:  clock().UTC().Format(time.RFC3339Nano),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = store.RecordManagementAudit(ctx, audit)
}

func rejectedActorKindForCode(code string) string {
	switch code {
	case "authentication_required":
		return "unauthenticated"
	case "administrator_required", "permission_denied", "origin_invalid", "csrf_invalid":
		return "unauthorized"
	default:
		return ""
	}
}

func rejectedAuditIdentity(request *http.Request, pathValue string) (string, map[string]any) {
	pathValue = normalizeRejectedAuditPath(pathValue)
	target := map[string]any{"kind": "system", "id": "management"}
	if request != nil && request.Method == http.MethodPost {
		if taskID, ok := rejectedVMTaskID(pathValue); ok {
			return rejectedVMAction(request), map[string]any{"kind": "vm", "id": taskID}
		}
	}
	switch {
	case request != nil && pathValue == "/tokens" && request.Method == http.MethodPost:
		return "token_create", map[string]any{"kind": "token", "id": "unknown"}
	case strings.HasPrefix(pathValue, "/tokens/") && (strings.HasSuffix(pathValue, "/revoke") || strings.HasSuffix(pathValue, "/revoke/preview")):
		id := strings.TrimPrefix(pathValue, "/tokens/")
		id = strings.TrimSuffix(id, "/revoke/preview")
		id = strings.TrimSuffix(id, "/revoke")
		if !validTokenID(id) {
			id = "unknown"
		}
		return "token_revoke", map[string]any{"kind": "token", "id": id}
	case strings.HasPrefix(pathValue, "/tokens/"):
		return "token_create", map[string]any{"kind": "token", "id": "unknown"}
	case pathValue == "/images/imports" || strings.HasPrefix(pathValue, "/images/imports/"):
		return "image_import", map[string]any{"kind": "image_catalog", "id": "image-catalog"}
	case pathValue == "/images/default" || pathValue == "/images/default/preview":
		return "default_image_select", map[string]any{"kind": "image_catalog", "id": "image-catalog"}
	case strings.HasPrefix(pathValue, "/images/") && strings.HasSuffix(pathValue, "/retire") || strings.HasPrefix(pathValue, "/images/") && strings.HasSuffix(pathValue, "/retire/preview"):
		return "image_retire", rejectedImageTarget(pathValue, "/retire/preview", "/retire")
	case strings.HasPrefix(pathValue, "/images/") && strings.HasSuffix(pathValue, "/delete") || strings.HasPrefix(pathValue, "/images/") && strings.HasSuffix(pathValue, "/delete/preview"):
		return "image_delete", rejectedImageTarget(pathValue, "/delete/preview", "/delete")
	default:
		return "reconcile", target
	}
}

func normalizeRejectedAuditPath(pathValue string) string {
	for _, prefix := range []string{"/api/v1", "/internal/v1/management"} {
		if strings.HasPrefix(pathValue, prefix) {
			pathValue = strings.TrimPrefix(pathValue, prefix)
			break
		}
	}
	if pathValue == "" {
		return "/"
	}
	return pathValue
}

func rejectedVMTaskID(pathValue string) (string, bool) {
	if !strings.HasPrefix(pathValue, "/vms/") {
		return "", false
	}
	for _, suffix := range []string{"/actions/preview", "/actions"} {
		if !strings.HasSuffix(pathValue, suffix) {
			continue
		}
		taskID := strings.TrimSuffix(strings.TrimPrefix(pathValue, "/vms/"), suffix)
		if protocol.TaskIDPattern.MatchString(taskID) {
			return taskID, true
		}
	}
	return "", false
}

func rejectedVMAction(request *http.Request) string {
	if request == nil || request.Body == nil {
		return "reconcile"
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxRejectedAuditBodyBytes))
	if err != nil {
		return "reconcile"
	}
	var payload struct {
		Action string `json:"action"`
	}
	if json.Unmarshal(body, &payload) != nil || !containsString([]string{"restart", "resize", "release", "destroy", "port_add", "port_remove"}, payload.Action) {
		return "reconcile"
	}
	return payload.Action
}

func rejectedImageTarget(pathValue string, suffixes ...string) map[string]any {
	id := strings.TrimPrefix(pathValue, "/images/")
	for _, suffix := range suffixes {
		id = strings.TrimSuffix(id, suffix)
	}
	if !managementFingerprintPattern.MatchString(id) {
		id = "unknown"
	}
	return map[string]any{"kind": "image", "id": id}
}
