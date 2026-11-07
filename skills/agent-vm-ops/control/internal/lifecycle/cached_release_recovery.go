package lifecycle

import (
	"context"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

// A cached task can outlive the final journal write of its release. Verify the
// completed cleanup rather than replaying the native release or inventing a
// later successful operation as a witness.
func (s *Service) reconcileCachedReleases(ctx context.Context, pending []state.Operation) error {
	var firstErr error
	for _, snapshot := range pending {
		if snapshot.Action != "release" || snapshot.SessionID == nil || snapshot.Generation == nil || s.operationActive(snapshot.OperationID) {
			continue
		}
		unlock, locked := s.trySessionLock(*snapshot.SessionID)
		if !locked {
			continue
		}
		err := s.reconcileCachedRelease(ctx, snapshot.OperationID)
		unlock()
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Service) reconcileCachedRelease(ctx context.Context, operationID string) error {
	op, err := s.Store.GetOperation(ctx, operationID)
	if err != nil {
		return s.sqliteError(err)
	}
	if op == nil || op.Response["outcome"] != "pending" || op.Action != "release" || op.SessionID == nil || op.Generation == nil {
		return nil
	}
	task, err := s.Store.GetTask(ctx, map[string]any{"session_id": *op.SessionID})
	if err != nil {
		return s.sqliteError(err)
	}
	if task == nil || task.State != stateCached || task.Generation != *op.Generation {
		return nil
	}
	reject := func() error {
		return protocol.NewError("operation_unverified", "cached release cleanup could not be verified", map[string]any{"operation_id": operationID, "outcome_verified": false})
	}
	if s.Guest == nil || task.LeaseExpiresAt != nil || task.ReservationBytes != 0 || task.ResourceReservationJSON != "{}" {
		return reject()
	}
	later, err := s.Store.HasLaterSuccessfulSessionOperation(ctx, operationID)
	if err != nil {
		return s.sqliteError(err)
	}
	// Later effects require the existing superseded-outcome proof; current
	// cache state alone must not turn an uncertain old release into success.
	if later {
		return nil
	}
	worker, ok := op.Response["worker"].(map[string]any)
	if !ok || (worker["state"] != "finished" && worker["state"] != "exited") || workerOwnershipUnresolved(worker) {
		return reject()
	}
	verified, err := terminalWorkerVerified(worker)
	if err != nil || !verified {
		return reject()
	}
	unresolved, err := s.pendingWorkerUnresolved(ctx, op.SessionID, operationID, "")
	if err != nil {
		return s.sqliteError(err)
	}
	if unresolved {
		return reject()
	}
	request, id, err := s.pendingCleanupRequest(ctx, task.SessionID, task.Generation, "release")
	if err != nil {
		return err
	}
	if id != operationID {
		return reject()
	}
	if raw, exists := op.Response["_task_id"]; exists && raw != task.TaskID {
		return reject()
	}
	if raw, exists := op.Response["_task_generation"]; exists {
		generation, ok := protocol.Integer(raw)
		if !ok || generation != task.Generation {
			return reject()
		}
	}
	if err := s.verifyCachedGuest(ctx, task); err != nil {
		return err
	}
	result, err := s.summary(ctx, task)
	if err != nil {
		return err
	}
	response := cloneMap(op.Response)
	for k, v := range protocol.SuccessWithRelease(request, result, s.Release, nil) {
		response[k] = v
	}
	response["cleanup_recovered"] = true
	return s.recordOperation(ctx, operationID, op.Action, op.SessionID, op.Generation, response)
}
