package lifecycle

import (
	"context"
	"reflect"
	"strconv"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
)

// reconcileSession deliberately does not call the global reconciler, recover a
// saved acquire, or advance a maintenance cursor. Only a proven pre-spawn
// acquire and its expired, exactly bound task may be closed.
func (s *Service) reconcileSession(ctx context.Context, request protocol.Request) (map[string]any, error) {
	return s.withSessionLock(ctx, stringValue(request["session_id"]), func() (map[string]any, error) {
		task, err := s.task(ctx, request)
		if err != nil {
			return nil, err
		}
		if task.TaskID == "" || task.TaskID != stringValue(request["task_id"]) {
			return nil, protocol.NewError("stale_target", "reconciliation task identity does not match", nil)
		}
		if task.State != stateActive && task.State != statePreparing && task.State != stateClosing && task.State != stateCached {
			return nil, protocol.NewError("task_transition", "task is outside bounded expiry reconciliation", nil)
		}
		if task.LeaseExpiresAt != nil {
			deadline, parseErr := time.Parse(time.RFC3339Nano, *task.LeaseExpiresAt)
			if parseErr != nil {
				return nil, protocol.NewError("state_unverified", "reconciliation lease deadline is invalid", nil)
			}
			if deadline.After(s.Clock()) {
				return nil, protocol.NewError("stale_target", "reconciliation refuses an unexpired lease", nil)
			}
		} else if task.State == stateActive || task.State == statePreparing {
			return nil, protocol.NewError("state_unverified", "active or preparing task has no verified lease deadline", nil)
		}
		pendingID := stringValue(request["pending_operation_id"])
		pending, err := s.Store.GetOperation(ctx, pendingID)
		if err != nil {
			return nil, s.sqliteError(err)
		}
		if pending == nil || pending.Action != "acquire" || pending.SessionID == nil || *pending.SessionID != task.SessionID || pending.Generation == nil || *pending.Generation != task.Generation || !acquireOperationMatchesTask(*pending, task) || stringValue(pending.Response["outcome"]) != "pending" || s.operationActive(pendingID) {
			return nil, protocol.NewError("operation_unverified", "target is not an inactive pending acquire of this exact task generation", nil)
		}
		saved, savedOK := pending.Response["_request"].(map[string]any)
		digest, digestErr := protocol.Digest(protocol.Request(saved))
		if !savedOK || protocol.ValidateRequest(protocol.Request(saved)) != nil || digestErr != nil || digest != stringValue(pending.Response["_request_digest"]) || !operationIdentityMatches(pending, protocol.Request(saved)) || stringValue(saved["operation_id"]) != pendingID || stringValue(saved["action"]) != "acquire" {
			return nil, protocol.NewError("operation_unverified", "pending acquire request identity or digest is unverified", nil)
		}
		worker, workerOK := pending.Response["worker"].(map[string]any)
		if !workerOK || !verifiedUnstartedWorker(worker) {
			return nil, protocol.NewError("operation_unverified", "scoped reconciliation requires explicit pre-spawn worker evidence", nil)
		}
		if raw, exists := pending.Response["_worker_recovery_evidence"]; exists {
			evidence, ok := raw.(map[string]any)
			prior, _ := evidence["worker"].(map[string]any)
			if !ok || evidence["kind"] != "scoped-pre-spawn-acquire" || evidence["operation_id"] != request["operation_id"] || evidence["request_digest"] != digest || !reflect.DeepEqual(evidence["request"], saved) || !verifiedUnstartedWorker(prior) {
				return nil, protocol.NewError("operation_unverified", "existing recovery evidence does not belong to this scoped operation", nil)
			}
		}
		others, more, err := s.Store.PendingOperationsForSessionPage(ctx, task.SessionID, 128)
		if err != nil {
			return nil, s.sqliteError(err)
		}
		if more {
			return nil, protocol.NewError("operation_unverified", "session recovery exceeds the bounded journal page", nil)
		}
		for _, other := range others {
			if other.OperationID != pendingID && other.OperationID != stringValue(request["operation_id"]) {
				return nil, protocol.NewError("operation_unverified", "another same-session pending operation requires separate reconciliation", map[string]any{"operation_id": other.OperationID})
			}
		}
		if s.Guest == nil {
			return nil, protocol.NewError("native_unavailable", "scoped reconciliation requires native ownership verification", nil)
		}
		info, err := s.verifyOwnership(ctx, task)
		if err != nil {
			return nil, err
		}
		if info["config"].(map[string]any)["user.agent-vm.generation"] != strconv.FormatInt(task.Generation, 10) {
			return nil, protocol.NewError("ownership_mismatch", "scoped reconciliation requires the exact native generation", nil)
		}
		if task.State == stateCached {
			if err := s.verifyCachedGuest(ctx, task); err != nil {
				return nil, err
			}
			ports, err := s.Store.ListPorts(ctx, task.TaskID)
			if err != nil {
				return nil, s.sqliteError(err)
			}
			if len(ports) != 0 {
				return nil, protocol.NewError("mapping_unverified", "cached target still has durable mappings", nil)
			}
		}
		result := map[string]any{"dry_run": !requestBool(request, "apply"), "task_id": task.TaskID, "session_id": task.SessionID, "generation": task.Generation, "pending_operation_id": pendingID, "previous_worker": cloneMap(worker), "action": "close-expired-and-retain-cache"}
		if !requestBool(request, "apply") {
			return result, nil
		}
		// Preserve the exact pre-spawn observation in a field that the state
		// journal carries immutably through terminal operation recording.
		if err := s.commitMutationContext(ctx, func() error {
			return s.Store.UpdateOperationResponse(ctx, pendingID, func(response map[string]any) error {
				if response["outcome"] != "pending" || !reflect.DeepEqual(response["worker"], worker) || response["_request_digest"] != digest {
					return protocol.NewError("operation_unverified", "pending acquire changed before reconciliation", nil)
				}
				if _, exists := response["_worker_recovery_evidence"]; !exists {
					response["_worker_recovery_evidence"] = map[string]any{"kind": "scoped-pre-spawn-acquire", "operation_id": stringValue(request["operation_id"]), "worker": cloneMap(worker), "request": cloneMap(saved), "request_digest": digest}
				}
				return nil
			})
		}); err != nil {
			return nil, s.sqliteError(err)
		}
		if err := s.reconcilePendingWorker(ctx, pendingID); err != nil {
			return nil, err
		}
		// Keep the caller's recorder: cleanup workers belong to this scoped
		// maintenance operation, never to a replay of the failed acquire.
		cleanup := protocol.Request{"protocol": protocol.Version, "action": "release", "session_id": task.SessionID, "generation": task.Generation, "operation_id": request["operation_id"]}
		closed, err := s.releaseUnlocked(withInternalCleanup(ctx), cleanup, false)
		if err != nil {
			return nil, err
		}
		terminal := map[string]any{"outcome": "error", "_request_digest": digest, "error": protocol.NewError("lease_released", "expired lease cleanup cancelled the unstarted acquire", map[string]any{"task_id": task.TaskID, "task_generation": task.Generation, "outcome_verified": true})}
		if err := s.recordOperation(ctx, pendingID, pending.Action, pending.SessionID, pending.Generation, terminal); err != nil {
			return nil, s.sqliteError(err)
		}
		result["current_state"] = closed
		return result, nil
	})
}

func verifiedUnstartedWorker(worker map[string]any) bool {
	state := stringValue(worker["state"])
	attempted, explicit := worker["start_attempted"].(bool)
	_, pid := worker["pid"]
	_, group := worker["process_group"]
	_, start := worker["start_time"]
	return (state == "starting" || state == "exited") && explicit && !attempted && !pid && !group && !start
}
