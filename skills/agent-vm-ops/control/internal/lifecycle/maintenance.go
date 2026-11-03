package lifecycle

import (
	"context"
	"errors"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

const (
	maintenanceReconcileLane = "reconcile"
	maintenanceAgeGCLane     = "age-gc"
	maintenancePageSize      = 16
	maintenanceBudget        = 2 * time.Second
)

type maintenancePageResult struct {
	nextTaskID string
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

	now := s.Clock().UTC()
	cursor, err := s.Store.MaintenanceCursor(ctx)
	if err != nil {
		return s.sqliteError(err)
	}
	laneCursor := maintenanceLane(&cursor, lane)
	if next, parseErr := time.Parse(time.RFC3339Nano, laneCursor.NextAttemptAt); parseErr == nil && next.After(now) {
		return nil
	} else if parseErr != nil {
		return protocol.NewError("state_unverified", "maintenance cursor deadline is invalid", map[string]any{"lane": lane})
	}

	workCtx, cancel := context.WithTimeout(ctx, maintenanceBudget)
	defer cancel()
	var page maintenancePageResult
	if lane == maintenanceReconcileLane {
		page, err = s.reconcileMaintenancePage(workCtx, laneCursor.ScanAfterTaskID)
	} else {
		page, err = s.ageGCMaintenancePage(workCtx, laneCursor.ScanAfterTaskID)
	}
	if err == nil {
		laneCursor.ScanAfterTaskID = page.nextTaskID
		laneCursor.Attempt = 0
		laneCursor.NextAttemptAt = nowString(now)
		laneCursor.LastErrorCode = ""
		laneCursor.LastErrorPhase = ""
	} else {
		laneCursor.Attempt++
		if laneCursor.Attempt > 6 {
			laneCursor.Attempt = 6
		}
		laneCursor.NextAttemptAt = nowString(now.Add(maintenanceBackoff(laneCursor.Attempt)))
		control := protocol.AsError(err)
		laneCursor.LastErrorCode = control.Code
		laneCursor.LastErrorPhase = lane
	}
	laneCursor.UpdatedAt = nowString(now)
	if cursorErr := s.Store.SetMaintenanceCursor(ctx, cursor); cursorErr != nil {
		return s.sqliteError(cursorErr)
	}
	return err
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
	pending, err := s.Store.PendingOperations(ctx)
	if err != nil {
		return maintenancePageResult{}, s.sqliteError(err)
	}
	if err := s.reconcilePendingWorkers(ctx, pending); err != nil {
		return maintenancePageResult{}, err
	}
	if err := s.reconcilePendingAbsentDestroy(ctx, pending); err != nil {
		return maintenancePageResult{}, err
	}
	if err := s.recoverPending(ctx, pending); err != nil {
		return maintenancePageResult{}, err
	}
	if err := s.reconcileOrphanedAcquireOperations(ctx, pending); err != nil {
		return maintenancePageResult{}, err
	}

	tasks, more, err := s.Store.ListTasksPage(ctx, state.TaskPageFilter{AfterTaskID: afterTaskID, Limit: maintenancePageSize})
	if err != nil {
		return maintenancePageResult{}, s.sqliteError(err)
	}
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			return maintenancePageResult{}, protocol.NewError("maintenance_timeout", "reconcile maintenance budget expired", map[string]any{"phase": maintenanceReconcileLane})
		}
		if err := s.reconcileMaintenanceTask(ctx, &task); err != nil {
			return maintenancePageResult{}, err
		}
	}
	result := maintenancePageResult{}
	if more && len(tasks) > 0 {
		result.nextTaskID = tasks[len(tasks)-1].TaskID
	}
	return result, nil
}

func (s *Service) reconcileMaintenanceTask(ctx context.Context, task *state.Task) error {
	if (task.State == stateActive || task.State == statePreparing) && !effective(task, s.Clock()) {
		request := protocol.Request{"protocol": protocol.Version, "action": "timer-expire", "task_id": task.TaskID, "generation": task.Generation}
		if _, err := s.expire(ctx, request); err != nil {
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
	cleanupRequest, cleanupOperationID, err := s.pendingCleanupRequest(ctx, task.SessionID, task.Generation, "release")
	if err != nil {
		return err
	}
	cleanupCtx := ctx
	if cleanupOperationID != "" {
		cleanupCtx = native.WithWorkerRecorder(cleanupCtx, cleanupOperationID, s)
	}
	result, releaseErr := s.release(withInternalCleanup(cleanupCtx), cleanupRequest, false)
	if releaseErr != nil {
		return releaseErr
	}
	return s.finalizePendingCleanup(ctx, task.SessionID, task.Generation, "release", result)
}

func (s *Service) reconcileDeletingTask(ctx context.Context, task *state.Task) error {
	cleanupRequest, cleanupOperationID, err := s.pendingCleanupRequest(ctx, task.SessionID, task.Generation, "destroy")
	if err != nil {
		return err
	}
	cleanupCtx := ctx
	if cleanupOperationID != "" {
		cleanupCtx = native.WithWorkerRecorder(cleanupCtx, cleanupOperationID, s)
	}
	result, releaseErr := s.release(withInternalCleanup(cleanupCtx), cleanupRequest, true)
	if releaseErr != nil {
		return releaseErr
	}
	return s.finalizePendingCleanup(ctx, task.SessionID, task.Generation, "destroy", result)
}

func (s *Service) ageGCMaintenancePage(ctx context.Context, afterTaskID string) (maintenancePageResult, error) {
	tasks, more, err := s.Store.ListTasksPage(ctx, state.TaskPageFilter{AfterTaskID: afterTaskID, Limit: maintenancePageSize})
	if err != nil {
		return maintenancePageResult{}, s.sqliteError(err)
	}
	for _, task := range tasks {
		if err := ctx.Err(); err != nil {
			return maintenancePageResult{}, protocol.NewError("maintenance_timeout", "age-gc maintenance budget expired", map[string]any{"phase": maintenanceAgeGCLane})
		}
		if task.State != stateCached || task.CleanupAfter == nil {
			continue
		}
		deadline, parseErr := time.Parse(time.RFC3339Nano, *task.CleanupAfter)
		if parseErr != nil || deadline.After(s.Clock()) {
			continue
		}
		gcNow := s.Clock()
		updated, updateErr := s.Store.UpdateTaskIf(ctx, task.TaskID, state.TaskCondition{Generation: &task.Generation, States: []string{stateCached}, LeaseExpiredAt: &gcNow}, map[string]any{"state": stateDeleting, "updated_at": nowString(gcNow)})
		if updateErr != nil {
			return maintenancePageResult{}, s.sqliteError(updateErr)
		}
		if !updated {
			continue
		}
		cleanupRequest := protocol.Request{"session_id": task.SessionID, "generation": task.Generation}
		if _, releaseErr := s.release(withInternalCleanup(ctx), cleanupRequest, true); releaseErr != nil {
			return maintenancePageResult{}, releaseErr
		}
		if err := s.deleteDestroyedTask(ctx, cleanupRequest, &task.Generation); err != nil {
			return maintenancePageResult{}, err
		}
	}
	result := maintenancePageResult{}
	if more && len(tasks) > 0 {
		result.nextTaskID = tasks[len(tasks)-1].TaskID
	}
	return result, nil
}
