package webadmin

import (
	"context"
	"errors"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/management"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

// LifecycleActions is the only adapter from management actions into the
// resident lifecycle authority. The Web process never receives this object.
type LifecycleActions struct {
	Service *lifecycle.Service
	Store   *state.Store
}

var _ management.ActionBackend = (*LifecycleActions)(nil)
var _ management.VersionedVMActionBackend = (*LifecycleActions)(nil)
var _ management.VMActionGuard = (*LifecycleActions)(nil)

func (actions *LifecycleActions) ValidateVMAction(ctx context.Context, task state.Task, action string, parameters map[string]any) error {
	if actions == nil || actions.Service == nil {
		return errors.New("authority_unavailable: lifecycle service is not configured")
	}
	return actions.Service.ValidateManagementVMAction(ctx, task, action, parameters)
}

func (actions *LifecycleActions) ExecuteVMAction(ctx context.Context, operationID, action, taskID string, generation int64, parameters map[string]any) (map[string]any, error) {
	return actions.ExecuteVMActionWithVersion(ctx, operationID, action, taskID, generation, "", parameters)
}

func (actions *LifecycleActions) ExecuteVMActionWithVersion(ctx context.Context, operationID, action, taskID string, generation int64, targetVersion string, parameters map[string]any) (map[string]any, error) {
	if actions == nil || actions.Service == nil || actions.Store == nil {
		return nil, errors.New("authority_unavailable: lifecycle service is not configured")
	}
	task, err := actions.Store.GetTask(ctx, map[string]any{"task_id": taskID})
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, errors.New("not_found: VM does not exist")
	}
	protocolAction := map[string]string{"port_add": "port-add", "port_remove": "port-remove"}[action]
	if protocolAction == "" {
		protocolAction = action
	}
	request := protocol.Request{
		"protocol":     protocol.Version,
		"action":       protocolAction,
		"operation_id": operationID,
		"session_id":   task.SessionID,
		"generation":   generation,
	}
	if targetVersion != "" {
		request["management_target_version"] = targetVersion
	}
	for key, value := range parameters {
		request[key] = value
	}
	response, err := actions.Service.Dispatch(ctx, request)
	if err != nil {
		return nil, err
	}
	if result, ok := response["result"].(map[string]any); ok {
		return result, nil
	}
	return response, nil
}

func (actions *LifecycleActions) ExecuteMaintenance(ctx context.Context, operationID, action string, parameters map[string]any) (map[string]any, error) {
	if actions == nil || actions.Service == nil {
		return nil, errors.New("authority_unavailable: lifecycle service is not configured")
	}
	protocolAction := map[string]string{"drain_enter": "drain-enter", "reconcile": "admin-reconcile", "gc": "admin-gc"}[action]
	if protocolAction == "" {
		return nil, errors.New("unsupported_action: maintenance action is not allowed")
	}
	request := protocol.Request{"protocol": protocol.Version, "action": protocolAction, "operation_id": operationID, "generation": int64(0)}
	for key, value := range parameters {
		request[key] = value
	}
	request["apply"] = true
	response, err := actions.Service.Dispatch(ctx, request)
	if err != nil {
		return nil, err
	}
	if result, ok := response["result"].(map[string]any); ok {
		return result, nil
	}
	return response, nil
}
