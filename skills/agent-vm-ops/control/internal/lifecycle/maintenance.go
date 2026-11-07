package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

const (
	maintenanceReconcileLane   = "reconcile"
	maintenanceAgeGCLane       = "age-gc"
	maintenancePageSize        = 16
	maintenancePendingPageSize = 4
	maintenanceBudget          = 2 * time.Second
)

type maintenancePageResult struct {
	nextTaskID             string
	taskCursorUpdated      bool
	nextOperationAt        string
	nextOperationID        string
	operationCursorUpdated bool
}

// RunMaintenance is the scheduler-owned maintenance boundary. It is
// deliberately separate from Dispatch: scheduled ticks do not create a
// journal operation, while legacy timer operations remain recoverable through
// Dispatch and startup reconciliation.
func (s *Service) RunMaintenance(ctx context.Context, lane string) error {
	if lane != maintenanceReconcileLane && lane != maintenanceAgeGCLane {
		return errors.New("unknown maintenance lane")
	}
	unlockDrain, err := s.lockNamedActionWithContext(ctx, "drain")
	if err != nil {
		return resourceLockError(err)
	}
	defer unlockDrain()
	unlockMaintenance, err := s.lockNamedActionWithContext(ctx, "maintenance")
	if err != nil {
		return resourceLockError(err)
	}
	defer unlockMaintenance()
	s.maintenance.Lock()
	defer s.maintenance.Unlock()
	if !s.mutationAllowed() {
		return protocol.NewError("controller_lease_required", "controller lease is required for maintenance", map[string]any{"admission_ready": false})
	}
	workCtx, stopMutationWatch, mutationLost := s.mutationContext(ctx)
	defer stopMutationWatch()

	now := s.Clock().UTC()
	cursor, err := s.Store.MaintenanceCursor(workCtx)
	if err != nil {
		return s.sqliteError(err)
	}
	laneCursor := maintenanceLane(&cursor, lane)
	if next, parseErr := time.Parse(time.RFC3339Nano, laneCursor.NextAttemptAt); parseErr == nil && next.After(now) {
		return nil
	} else if parseErr != nil {
		return protocol.NewError("state_unverified", "maintenance cursor deadline is invalid", map[string]any{"lane": lane})
	}

	workCtx, cancel := context.WithTimeout(workCtx, maintenanceBudget)
	defer cancel()
	var page maintenancePageResult
	if lane == maintenanceReconcileLane {
		page, err = s.reconcileMaintenancePageWithCursor(workCtx, laneCursor.ScanAfterTaskID, laneCursor.ScanAfterOperationAt, laneCursor.ScanAfterOperationID)
	} else {
		page, err = s.ageGCMaintenancePage(workCtx, laneCursor.ScanAfterTaskID)
	}
	if err == nil && (mutationLost() || !s.mutationAllowed()) {
		err = controllerLeaseLostError(nil)
	}
	completedAt := s.Clock().UTC()
	if page.taskCursorUpdated {
		laneCursor.ScanAfterTaskID = page.nextTaskID
	}
	if page.operationCursorUpdated {
		laneCursor.ScanAfterOperationAt = page.nextOperationAt
		laneCursor.ScanAfterOperationID = page.nextOperationID
	}
	if err == nil {
		laneCursor.Attempt = 0
		laneCursor.NextAttemptAt = nowString(completedAt)
		if lane != maintenanceReconcileLane {
			laneCursor.LastErrorCode = ""
			laneCursor.LastErrorPhase = ""
		}
	} else {
		laneCursor.Attempt++
		if laneCursor.Attempt > 6 {
			laneCursor.Attempt = 6
		}
		laneCursor.NextAttemptAt = nowString(completedAt.Add(maintenanceBackoff(laneCursor.Attempt)))
		control := protocol.AsError(err)
		laneCursor.LastErrorCode = control.Code
		laneCursor.LastErrorPhase = lane
	}
	if lane == maintenanceReconcileLane {
		if err != nil {
			laneCursor.CycleFailed = true
		}
		if page.taskCursorUpdated && page.nextTaskID == "" {
			laneCursor.TaskCycleComplete = true
		}
		if page.operationCursorUpdated && page.nextOperationID == "" {
			laneCursor.OperationCycleComplete = true
		}
		if laneCursor.TaskCycleComplete && laneCursor.OperationCycleComplete {
			if !laneCursor.CycleFailed {
				laneCursor.LastErrorCode = ""
				laneCursor.LastErrorPhase = ""
			}
			laneCursor.CycleFailed = false
			laneCursor.TaskCycleComplete = false
			laneCursor.OperationCycleComplete = false
		}
	}
	laneCursor.UpdatedAt = nowString(completedAt)
	if mutationLost() || !s.mutationAllowed() {
		return controllerLeaseLostError(err)
	}
	if cursorErr := s.commitMutationContext(ctx, func() error {
		return s.Store.SetMaintenanceCursor(ctx, cursor)
	}); cursorErr != nil {
		return s.sqliteError(cursorErr)
	}
	return err
}

func maintenanceCleanupOperationID(action, sessionID string, generation int64) string {
	digest := sha256.Sum256([]byte(action + "\x00" + sessionID + "\x00" + strconv.FormatInt(generation, 10)))
	return "maintenance-" + action + "-" + hex.EncodeToString(digest[:12])
}

func maintenanceLane(cursor *state.MaintenanceCursor, lane string) *state.MaintenanceLane {
	if lane == maintenanceAgeGCLane {
		return &cursor.AgeGC
	}
	return &cursor.Reconcile
}

func maintenanceBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 2 * time.Second
	case 2:
		return 4 * time.Second
	case 3:
		return 8 * time.Second
	case 4:
		return 16 * time.Second
	case 5:
		return 32 * time.Second
	default:
		return 60 * time.Second
	}
}

func (s *Service) reconcileMaintenancePage(ctx context.Context, afterTaskID string) (maintenancePageResult, error) {
	return s.reconcileMaintenancePageWithCursor(ctx, afterTaskID, "", "")
}

func (s *Service) reconcileMaintenancePageWithCursor(ctx context.Context, afterTaskID, afterOperationAt, afterOperationID string) (maintenancePageResult, error) {
	pending, morePending, err := s.Store.PendingOperationsPageAfter(ctx, afterOperationAt, afterOperationID, maintenancePendingPageSize)
	if err != nil {
		return maintenancePageResult{}, s.sqliteError(err)
	}
	page := maintenancePageResult{}
	var firstErr error
	pendingCtx, cancelPending := context.WithTimeout(ctx, maintenanceBudget/2)
	defer cancelPending()
	for index := range pending {
		if err := pendingCtx.Err(); err != nil {
			if firstErr == nil {
				firstErr = protocol.NewError("maintenance_timeout", "reconcile maintenance budget expired", map[string]any{"phase": maintenanceReconcileLane})
			}
			break
		}
		one := pending[index : index+1]
		for _, reconcile := range []func(context.Context, []state.Operation) error{
			s.reconcilePendingWorkers,
			s.reconcilePendingAbsentDestroy,
			s.recoverPending,
			s.reconcileCachedReleases,
			s.reconcileOrphanedAcquireOperations,
		} {
			if reconcileErr := reconcile(pendingCtx, one); reconcileErr != nil {
				if firstErr == nil {
					firstErr = reconcileErr
				}
				break
			}
		}
		if err := s.ensureMaintenanceOperationResolved(pendingCtx, pending[index].OperationID); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
		page.nextOperationAt = pending[index].CreatedAt
		page.nextOperationID = pending[index].OperationID
		page.operationCursorUpdated = true
	}
	if !morePending && (len(pending) == 0 || page.nextOperationID == pending[len(pending)-1].OperationID) {
		page.nextOperationAt = ""
		page.nextOperationID = ""
		page.operationCursorUpdated = true
	}

	tasksPage, err := s.reconcileMaintenanceTaskPage(ctx, afterTaskID)
	if err != nil {
		if firstErr == nil {
			firstErr = err
		}
	}
	page.nextTaskID = tasksPage.nextTaskID
	page.taskCursorUpdated = tasksPage.taskCursorUpdated
	return page, firstErr
}

func (s *Service) reconcileMaintenanceTaskPage(ctx context.Context, afterTaskID string) (maintenancePageResult, error) {
	tasks, more, err := s.Store.ListTasksPage(ctx, state.TaskPageFilter{AfterTaskID: afterTaskID, Limit: maintenancePageSize})
	if err != nil {
		return maintenancePageResult{}, s.sqliteError(err)
	}
	page := maintenancePageResult{}
	var firstErr error
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			return page, protocol.NewError("maintenance_timeout", "reconcile maintenance budget expired", map[string]any{"phase": maintenanceReconcileLane})
		}
		if reconcileErr := s.reconcileMaintenanceTask(ctx, &task); reconcileErr != nil {
			if firstErr == nil {
				firstErr = reconcileErr
			}
		}
		page.nextTaskID = task.TaskID
		page.taskCursorUpdated = true
	}
	if !more {
		page.nextTaskID = ""
		page.taskCursorUpdated = true
	}
	return page, firstErr
}

func (s *Service) ensureMaintenanceOperationResolved(ctx context.Context, operationID string) error {
	operation, err := s.Store.GetOperation(ctx, operationID)
	if err != nil {
		return s.sqliteError(err)
	}
	if operation != nil && stringValue(operation.Response["outcome"]) == "pending" {
		return protocol.NewError("operation_unverified", "maintenance operation remains pending and will be revisited", map[string]any{"operation_id": operationID})
	}
	return nil
}

func maintenanceLaneForAction(action string) (string, bool) {
	switch action {
	case "admin-reconcile", "timer-reconcile":
		return maintenanceReconcileLane, true
	case "admin-gc", "timer-age-gc":
		return maintenanceAgeGCLane, true
	default:
		return "", false
	}
}

func (s *Service) recoverLegacyMaintenancePage(ctx context.Context, action string) (map[string]any, error) {
	lane, ok := maintenanceLaneForAction(action)
	if !ok {
		return nil, protocol.NewError("operation_unverified", "legacy operation is not a maintenance action", map[string]any{"action": action})
	}
	cursor, err := s.Store.MaintenanceCursor(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	laneCursor := maintenanceLane(&cursor, lane)
	workCtx, cancel := context.WithTimeout(ctx, maintenanceBudget)
	defer cancel()
	var page maintenancePageResult
	if lane == maintenanceReconcileLane {
		page, err = s.reconcileMaintenanceTaskPage(workCtx, laneCursor.ScanAfterTaskID)
	} else {
		page, err = s.ageGCMaintenancePage(workCtx, laneCursor.ScanAfterTaskID)
	}
	if err != nil {
		return nil, err
	}
	if page.taskCursorUpdated {
		laneCursor.ScanAfterTaskID = page.nextTaskID
	}
	completedAt := s.Clock().UTC()
	laneCursor.Attempt = 0
	laneCursor.NextAttemptAt = nowString(completedAt)
	// A legacy reconcile only scans one task page. It cannot prove that
	// both pending and task scans completed a clean cycle.
	if lane != maintenanceReconcileLane {
		laneCursor.LastErrorCode = ""
		laneCursor.LastErrorPhase = ""
	}
	laneCursor.UpdatedAt = nowString(completedAt)
	if err := s.commitMutationContext(ctx, func() error {
		return s.Store.SetMaintenanceCursor(ctx, cursor)
	}); err != nil {
		return nil, s.sqliteError(err)
	}
	return map[string]any{"legacy_recovered": true, "maintenance_lane": lane, "task_cursor": laneCursor.ScanAfterTaskID}, nil
}

func (s *Service) reconcileMaintenanceTask(ctx context.Context, task *state.Task) error {
	if maintenanceExpiryCandidate(task, s.Clock()) {
		if err := s.maintenanceExpire(ctx, task); err != nil {
			return err
		}
	}
	if task.State == stateClosing {
		if err := s.reconcileClosing(ctx, task); err != nil {
			return err
		}
	}
	if task.State == stateFailed {
		return s.reconcileFailedTask(ctx, task)
	}
	if task.State == stateDeleting {
		return s.reconcileDeletingTask(ctx, task)
	}
	return nil
}

func (s *Service) reconcileFailedTask(ctx context.Context, task *state.Task) error {
	result, err := s.maintenanceCleanup(ctx, task, "release")
	if err != nil {
		return err
	}
	return s.finalizePendingCleanup(ctx, task.SessionID, task.Generation, "release", result)
}

func (s *Service) reconcileDeletingTask(ctx context.Context, task *state.Task) error {
	result, err := s.maintenanceCleanup(ctx, task, "destroy")
	if err != nil {
		return err
	}
	return s.finalizePendingCleanup(ctx, task.SessionID, task.Generation, "destroy", result)
}

func (s *Service) maintenanceCleanup(ctx context.Context, task *state.Task, action string) (map[string]any, error) {
	request, operationID, err := s.pendingCleanupRequest(ctx, task.SessionID, task.Generation, action)
	if err != nil {
		return nil, err
	}
	if operationID == "" {
		request = cloneMap(request)
		request["protocol"] = protocol.Version
		request["action"] = action
		operationID = maintenanceCleanupOperationID(action, task.SessionID, task.Generation)
		request["operation_id"] = operationID
		digest, digestErr := protocol.Digest(request)
		if digestErr != nil {
			return nil, protocol.NewError("invalid_request", "maintenance cleanup request cannot be canonicalized", nil)
		}
		pending := map[string]any{
			"outcome":         "pending",
			"_request_digest": digest,
			"_request":        map[string]any(request),
			"worker":          map[string]any{"state": "starting", "start_attempted": false, "prepared_at": nowString(s.Clock())},
		}
		finishActive := s.markOperationActive(operationID)
		var existing *state.Operation
		claimErr := s.commitMutationContext(ctx, func() error {
			var err error
			existing, err = s.Store.ClaimOperation(ctx, operationID, action, &task.SessionID, &task.Generation, pending)
			return err
		})
		if claimErr != nil {
			finishActive()
			return nil, s.sqliteError(claimErr)
		}
		if existing != nil {
			finishActive()
			return s.replayRecord(request, existing, nil)
		}
		defer finishActive()
	}
	cleanupCtx := native.WithWorkerRecorder(withInternalCleanup(ctx), operationID, s)
	cleanup := s.release
	if isSessionLockHeld(ctx) {
		cleanup = func(cleanupCtx context.Context, request protocol.Request, destroy bool) (map[string]any, error) {
			return s.releaseUnlocked(cleanupCtx, request, destroy)
		}
	}
	result, cleanupErr := cleanup(cleanupCtx, request, action == "destroy")
	if cleanupErr != nil {
		if !uncertain(cleanupErr) {
			_ = s.recordError(ctx, request, cleanupErr)
		}
		return nil, cleanupErr
	}
	response := protocol.SuccessWithRelease(request, result, s.Release, nil)
	responseDigest, digestErr := protocol.Digest(request)
	if digestErr != nil {
		return nil, protocol.NewError("state_unverified", "maintenance cleanup request digest could not be recomputed", map[string]any{"operation_id": operationID})
	}
	response["_request_digest"] = responseDigest
	if err := s.recordOperation(ctx, operationID, action, &task.SessionID, &task.Generation, response); err != nil {
		return nil, s.sqliteError(err)
	}
	return result, nil
}

func maintenanceExpiryCandidate(task *state.Task, now time.Time) bool {
	if task.State != stateActive && task.State != statePreparing {
		return false
	}
	if task.LeaseExpiresAt == nil {
		return true
	}
	deadline, err := time.Parse(time.RFC3339Nano, *task.LeaseExpiresAt)
	// Invalid evidence must reach the fail-closed validation, not be skipped.
	return err != nil || !deadline.After(now)
}

func (s *Service) maintenanceExpire(ctx context.Context, task *state.Task) error {
	// Preparing is not an effective lease, but its deadline still protects startup.
	if task.LeaseExpiresAt != nil {
		deadline, err := time.Parse(time.RFC3339Nano, *task.LeaseExpiresAt)
		if err != nil {
			return protocol.NewError("state_unverified", "maintenance lease deadline is invalid", map[string]any{"task_id": task.TaskID})
		}
		if deadline.After(s.Clock()) {
			return nil
		}
	}
	operationID := timerExpireOperationID(task.TaskID, task.Generation)
	request := protocol.Request{"protocol": protocol.Version, "action": "timer-expire", "task_id": task.TaskID, "session_id": task.SessionID, "generation": task.Generation, "operation_id": operationID}
	previous, err := s.Store.GetOperation(ctx, operationID)
	if err != nil {
		return s.sqliteError(err)
	}
	if previous != nil {
		// Preserve the old journal. Only an explicitly verified no-effect result
		// permits a new intent bound to the deadline that is now due.
		if _, err := s.replayRecord(request, previous); err != nil {
			return err
		}
		result, _ := previous.Response["result"].(map[string]any)
		worker, _ := previous.Response["worker"].(map[string]any)
		if previous.Response["ok"] == true && result["expired"] == false && worker["start_attempted"] == false && worker["state"] == "starting" && worker["pid"] == nil && worker["process_group"] == nil && worker["start_time"] == nil {
			deadline := "absent"
			if task.LeaseExpiresAt != nil {
				parsed, _ := time.Parse(time.RFC3339Nano, *task.LeaseExpiresAt)
				deadline = nowString(parsed)
			}
			digest := sha256.Sum256([]byte(task.TaskID + "\x00" + strconv.FormatInt(task.Generation, 10) + "\x00" + deadline))
			operationID = "timer-expire-deadline-" + hex.EncodeToString(digest[:12])
			request["operation_id"] = operationID
		}
	}
	digest, err := protocol.Digest(request)
	if err != nil {
		return protocol.NewError("invalid_request", "maintenance expiry request cannot be canonicalized", nil)
	}
	pending := map[string]any{
		"outcome":         "pending",
		"_request_digest": digest,
		"_request":        map[string]any(request),
		"worker":          map[string]any{"state": "starting", "start_attempted": false, "prepared_at": nowString(s.Clock())},
	}
	finishActive := s.markOperationActive(operationID)
	defer finishActive()
	var existing *state.Operation
	if err := s.commitMutationContext(ctx, func() error {
		var claimErr error
		existing, claimErr = s.Store.ClaimOperation(ctx, operationID, "timer-expire", &task.SessionID, &task.Generation, pending)
		return claimErr
	}); err != nil {
		return s.sqliteError(err)
	}
	if existing != nil {
		_, err := s.replayRecord(request, existing)
		return err
	}
	cleanupCtx := native.WithWorkerRecorder(withInternalCleanup(ctx), operationID, s)
	result, expireErr := s.expire(cleanupCtx, request)
	if expireErr != nil {
		if !uncertain(expireErr) {
			_ = s.recordError(ctx, request, expireErr)
		}
		return expireErr
	}
	response := protocol.SuccessWithRelease(request, result, s.Release, nil)
	response["_request_digest"] = digest
	return s.recordOperation(ctx, operationID, "timer-expire", &task.SessionID, &task.Generation, response)
}

func (s *Service) ageGCMaintenancePage(ctx context.Context, afterTaskID string) (maintenancePageResult, error) {
	tasks, more, err := s.Store.ListTasksPage(ctx, state.TaskPageFilter{AfterTaskID: afterTaskID, Limit: maintenancePageSize})
	if err != nil {
		return maintenancePageResult{}, s.sqliteError(err)
	}
	page := maintenancePageResult{}
	var firstErr error
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			if firstErr == nil {
				firstErr = protocol.NewError("maintenance_timeout", "age-gc maintenance budget expired", map[string]any{"phase": maintenanceAgeGCLane})
			}
			return page, firstErr
		}
		if task.State != stateCached || task.CleanupAfter == nil {
			page.nextTaskID = task.TaskID
			page.taskCursorUpdated = true
			continue
		}
		deadline, parseErr := time.Parse(time.RFC3339Nano, *task.CleanupAfter)
		if parseErr != nil || deadline.After(s.Clock()) {
			page.nextTaskID = task.TaskID
			page.taskCursorUpdated = true
			continue
		}
		gcNow := s.Clock()
		updated, updateErr := s.updateTaskIf(ctx, task.TaskID, state.TaskCondition{Generation: &task.Generation, States: []string{stateCached}, LeaseExpiredAt: &gcNow}, map[string]any{"state": stateDeleting, "updated_at": nowString(gcNow)})
		if updateErr != nil {
			return page, s.sqliteError(updateErr)
		}
		if !updated {
			page.nextTaskID = task.TaskID
			page.taskCursorUpdated = true
			continue
		}
		cleanupRequest := protocol.Request{"session_id": task.SessionID, "generation": task.Generation}
		if _, releaseErr := s.maintenanceCleanup(ctx, &task, "destroy"); releaseErr != nil {
			return page, releaseErr
		}
		if err := s.deleteDestroyedTask(ctx, cleanupRequest, &task.Generation); err != nil {
			return page, err
		}
		page.nextTaskID = task.TaskID
		page.taskCursorUpdated = true
	}
	if !more {
		page.nextTaskID = ""
		page.taskCursorUpdated = true
	}
	return page, firstErr
}

// Keep scheduler startup readiness separate: a failed maintenance round must
// close new admission while still allowing later bounded repair rounds.
func (s *Service) maintenanceAdmission(ctx context.Context) error {
	cursor, err := s.Store.MaintenanceCursor(ctx)
	if err != nil {
		return s.sqliteError(err)
	}
	if cursor.Reconcile.LastErrorCode != "" {
		return protocol.NewError("maintenance_unhealthy", "lifecycle maintenance requires recovery before admission", map[string]any{"phase": cursor.Reconcile.LastErrorPhase, "cause_code": cursor.Reconcile.LastErrorCode, "admission_ready": false})
	}
	return nil
}
