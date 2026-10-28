package lifecycle

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/observe"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

const (
	stateAbsent    = "absent"
	stateCached    = "cached"
	statePreparing = "preparing"
	stateActive    = "active"
	stateClosing   = "closing"
	stateDeleting  = "deleting"
	stateFailed    = "failed"
)

var resourceKeys = []string{"cpu", "memory_mib", "disk_gib"}

var (
	timerFallbackSequence  uint64
	timerOperationSequence uint64
	timerInstanceID        = newTimerInstanceID()
)

func newTimerInstanceID() string {
	value := make([]byte, 16)
	if _, err := crand.Read(value); err == nil {
		return hex.EncodeToString(value)
	}
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(), atomic.AddUint64(&timerFallbackSequence, 1))
}

func newTimerOperationID(action string) string {
	return fmt.Sprintf("timer-%s-%s-%d", timerInstanceID, strings.TrimPrefix(action, "timer-"), atomic.AddUint64(&timerOperationSequence, 1))
}

var (
	writeActions = map[string]bool{
		"acquire": true, "renew": true, "release": true, "destroy": true,
		"restart": true, "resize": true, "port-add": true, "port-remove": true,
		"drain-enter": true, "drain-exit": true, "admin-reconcile": true, "admin-gc": true,
	}
	sessionActions = map[string]bool{
		"status": true, "acquire": true, "renew": true, "release": true,
		"destroy": true, "restart": true, "resize": true, "ssh-info": true,
		"port-add": true, "port-list": true, "port-remove": true,
	}
)

type Service struct {
	Store     *state.Store
	Config    config.Config
	Guest     native.Guest
	Clock     func() time.Time
	Snapshots *observe.SnapshotStore
	CPU       CPUObserver
	Memory    MemoryObserver
	Release   *protocol.ReleaseMetadata

	mu                sync.RWMutex
	lockMu            sync.Mutex
	actionLocks       map[string]*sync.Mutex
	maintenance       sync.Mutex
	activeMu          sync.Mutex
	activeOps         map[string]int
	admissionMu       sync.Mutex
	storageMu         sync.Mutex
	storageReserved   int64
	auditSummaryMu    sync.RWMutex
	auditSummaryEpoch string
	auditSummaryCache state.AuditSummary
	resident          bool
	ready             bool
	wake              func()
}

type internalCleanupContextKey struct{}

func withInternalCleanup(ctx context.Context) context.Context {
	return context.WithValue(ctx, internalCleanupContextKey{}, true)
}

func isInternalCleanup(ctx context.Context) bool {
	value, _ := ctx.Value(internalCleanupContextKey{}).(bool)
	return value
}

type Options struct {
	Guest     native.Guest
	Clock     func() time.Time
	Resident  bool
	Snapshots *observe.SnapshotStore
	Wake      func()
	CPU       CPUObserver
	Memory    MemoryObserver
	Release   *protocol.ReleaseMetadata
}

type CPUObserver interface {
	Observe(context.Context) (observe.CPUObservation, error)
}

type MemoryObserver interface {
	Observe(context.Context) (observe.MemoryObservation, error)
}

func New(store *state.Store, cfg config.Config, options Options) *Service {
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Service{Store: store, Config: cfg, Guest: options.Guest, Clock: clock, Snapshots: options.Snapshots, CPU: options.CPU, Memory: options.Memory, Release: options.Release, resident: options.Resident, ready: false, wake: options.Wake, actionLocks: make(map[string]*sync.Mutex), activeOps: make(map[string]int)}
}

func (s *Service) auditSummary(ctx context.Context, epoch string) (state.AuditSummary, error) {
	s.auditSummaryMu.RLock()
	if epoch != "" && epoch == s.auditSummaryEpoch {
		cached := cloneAuditSummary(s.auditSummaryCache)
		s.auditSummaryMu.RUnlock()
		return cached, nil
	}
	s.auditSummaryMu.RUnlock()
	summary, err := s.Store.AuditSummary(ctx)
	if err != nil {
		return state.AuditSummary{}, err
	}
	s.auditSummaryMu.Lock()
	s.auditSummaryEpoch = epoch
	s.auditSummaryCache = cloneAuditSummary(summary)
	s.auditSummaryMu.Unlock()
	return summary, nil
}

func cloneAuditSummary(summary state.AuditSummary) state.AuditSummary {
	clone := state.AuditSummary{
		Operations:          make(map[string]int64, len(summary.Operations)),
		ImageUnpackFailures: make(map[string]int64, len(summary.ImageUnpackFailures)),
		Warnings:            make(map[string]int64, len(summary.Warnings)),
	}
	for key, value := range summary.Operations {
		clone.Operations[key] = value
	}
	for key, value := range summary.ImageUnpackFailures {
		clone.ImageUnpackFailures[key] = value
	}
	for key, value := range summary.Warnings {
		clone.Warnings[key] = value
	}
	return clone
}

func (s *Service) MarkReady()  { s.mu.Lock(); s.ready = true; s.mu.Unlock() }
func (s *Service) Ready() bool { s.mu.RLock(); defer s.mu.RUnlock(); return s.ready }
func (s *Service) SetWake(wake func()) {
	s.mu.Lock()
	s.wake = wake
	s.mu.Unlock()
}

// ReservePhysicalStorage serializes an external pool consumer, such as an
// image import, with lifecycle admission. The reservation remains in the
// physical admission snapshot until the returned release function is called.
func (s *Service) ReservePhysicalStorage(ctx context.Context, size int64) (func(), error) {
	if size < 0 {
		return nil, protocol.NewError("invalid_request", "physical storage reservation must not be negative", nil)
	}
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	admission, err := s.diskAdmission(ctx, "")
	if err != nil {
		return nil, err
	}
	minimumFreeBytes := s.Config.Management.Upload.MinimumFreeBytes
	if minimumFreeBytes < 0 {
		return nil, protocol.NewError("config_invalid", "management upload minimum free bytes must not be negative", nil)
	}
	if _, err := s.diskAdmissionSufficientWithFloor(ctx, admission, size, "", minimumFreeBytes); err != nil {
		control := protocol.AsError(err)
		if control.Code == "resource_insufficient" {
			if reclaimed, reclaimErr := s.reclaimCachedCapacity(ctx, size, "", minimumFreeBytes); reclaimErr != nil {
				return nil, reclaimErr
			} else if reclaimed {
				admission, err = s.diskAdmission(ctx, "")
				if err != nil {
					return nil, err
				}
				if _, sufficientErr := s.diskAdmissionSufficientWithFloor(ctx, admission, size, "", minimumFreeBytes); sufficientErr != nil {
					return nil, sufficientErr
				}
			} else {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	s.storageMu.Lock()
	if size > maxInt64Value-s.storageReserved {
		s.storageMu.Unlock()
		return nil, protocol.NewError("resource_unverified", "physical storage reservation exceeds supported range", map[string]any{"verified": false})
	}
	s.storageReserved += size
	s.storageMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.storageMu.Lock()
			if s.storageReserved >= size {
				s.storageReserved -= size
			} else {
				s.storageReserved = 0
			}
			s.storageMu.Unlock()
		})
	}, nil
}

func (s *Service) externalStorageReserved() int64 {
	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	return s.storageReserved
}

func (s *Service) wakeScheduler() {
	s.mu.RLock()
	wake := s.wake
	s.mu.RUnlock()
	if wake != nil {
		wake()
	}
}

func (s *Service) lockAction(action string, request protocol.Request) func() {
	var unlocks []func()
	if releaseSensitiveAction(action, request) || action == "drain-enter" || action == "drain-exit" || action == "admin-metrics" {
		// Drain ownership is a process-wide release boundary. It must not be
		// serialized only by the caller's session lock: a release
		// transaction and a native management action must observe one durable
		// owner in order.
		unlocks = append(unlocks, s.lockNamedAction("drain"))
	}
	if action == "admin-reconcile" || action == "timer-reconcile" || action == "admin-gc" || action == "timer-age-gc" {
		s.maintenance.Lock()
		unlocks = append(unlocks, s.maintenance.Unlock)
	}
	key := ""
	if action != "drain-enter" && action != "drain-exit" && !releaseSensitiveAction(action, request) {
		if session := stringValue(request["session_id"]); session != "" {
			key = "session:" + session
		} else if taskID := stringValue(request["task_id"]); taskID != "" {
			key = "task:" + taskID
		}
	}
	if key != "" {
		unlocks = append(unlocks, s.lockNamedAction(key))
	}
	return func() {
		for index := len(unlocks) - 1; index >= 0; index-- {
			unlocks[index]()
		}
	}
}

func (s *Service) lockNamedAction(key string) func() {
	if key == "" {
		return func() {}
	}
	s.lockMu.Lock()
	lock, exists := s.actionLocks[key]
	if !exists {
		lock = &sync.Mutex{}
		s.actionLocks[key] = lock
	}
	s.lockMu.Unlock()
	lock.Lock()
	return lock.Unlock
}

func (s *Service) trySessionLock(session string) (func(), bool) {
	s.lockMu.Lock()
	key := "session:" + session
	lock, exists := s.actionLocks[key]
	if !exists {
		lock = &sync.Mutex{}
		s.actionLocks[key] = lock
	}
	s.lockMu.Unlock()
	if !lock.TryLock() {
		return nil, false
	}
	return lock.Unlock, true
}

func (s *Service) withSessionLock(session string, fn func() (map[string]any, error)) (map[string]any, error) {
	if session == "" {
		return fn()
	}
	unlock := s.lockAction("session", protocol.Request{"session_id": session})
	defer unlock()
	return fn()
}

func (s *Service) markOperationActive(operationID string) func() {
	if operationID == "" {
		return func() {}
	}
	s.activeMu.Lock()
	s.activeOps[operationID]++
	s.activeMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.activeMu.Lock()
			if count := s.activeOps[operationID]; count <= 1 {
				delete(s.activeOps, operationID)
			} else {
				s.activeOps[operationID] = count - 1
			}
			s.activeMu.Unlock()
		})
	}
}

func (s *Service) operationActive(operationID string) bool {
	s.activeMu.Lock()
	_, active := s.activeOps[operationID]
	s.activeMu.Unlock()
	return active
}

func (s *Service) WorkerStarted(ctx context.Context, operationID string, identity native.WorkerIdentity) error {
	return s.updateWorker(ctx, operationID, map[string]any{
		"pid":           identity.PID,
		"start_time":    identity.StartTime,
		"process_group": identity.ProcessGroup,
		"state":         "running",
		"started_at":    nowString(s.Clock()),
	}, workerCallbackStarted)
}

func (s *Service) WorkerFinished(ctx context.Context, operationID string, identity native.WorkerIdentity, runErr error) error {
	workerState := "finished"
	if runErr != nil {
		// A native error can mean cleanup or ownership observation remained
		// uncertain. Keep the worker reconciliable instead of making a pending
		// operation look safely finished.
		workerState = "running"
	}
	worker := map[string]any{
		"pid":           identity.PID,
		"start_time":    identity.StartTime,
		"process_group": identity.ProcessGroup,
		"state":         workerState,
		"finished_at":   nowString(s.Clock()),
	}
	if runErr != nil {
		message := runErr.Error()
		if len(message) > 2048 {
			message = message[:2048]
		}
		worker["error"] = message
	}
	return s.updateWorker(ctx, operationID, worker, workerCallbackFinished)
}

// WorkerStartFailed records a start attempt that was proven not to create a
// native process. This clears the pre-spawn barrier without inventing a
// process identity, so a later reconciliation can retry the operation.
func (s *Service) WorkerStartFailed(ctx context.Context, operationID string, startErr error) error {
	worker := map[string]any{
		"state":           "exited",
		"start_attempted": true,
		"finished_at":     nowString(s.Clock()),
	}
	if startErr != nil {
		message := startErr.Error()
		if len(message) > 2048 {
			message = message[:2048]
		}
		worker["error"] = message
	}
	return s.updateWorker(ctx, operationID, worker, workerCallbackStartFailed)
}

var errUnresolvedWorker = errors.New("native worker ownership remains unresolved")

func (s *Service) WorkerStartAllowed(ctx context.Context, operationID string) error {
	if operationID == "" {
		return nil
	}
	operation, err := s.Store.GetOperation(ctx, operationID)
	if err != nil {
		return err
	}
	if operation == nil || stringValue(operation.Response["outcome"]) != "pending" {
		return errUnresolvedWorker
	}
	if unresolved, err := s.pendingWorkerUnresolved(ctx, operation.SessionID, operationID, native.WorkerRecoveryCoordinator(ctx)); err != nil {
		return err
	} else if unresolved {
		return errUnresolvedWorker
	}
	previous, _ := operation.Response["worker"].(map[string]any)
	if workerNeedsReconciliation(previous, true) {
		return errUnresolvedWorker
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return s.Store.UpdateOperationResponse(persistCtx, operationID, func(response map[string]any) error {
		worker, _ := response["worker"].(map[string]any)
		if workerNeedsReconciliation(worker, true) {
			return errUnresolvedWorker
		}
		response["worker"] = map[string]any{
			"state":           "starting",
			"start_attempted": true,
			"prepared_at":     nowString(s.Clock()),
		}
		return nil
	})
}

func (s *Service) recordWorkerState(ctx context.Context, operationID string, worker map[string]any) error {
	return s.updateWorker(ctx, operationID, worker, workerCallbackState)
}

type workerCallbackKind uint8

const (
	workerCallbackStarted workerCallbackKind = iota
	workerCallbackFinished
	workerCallbackStartFailed
	workerCallbackState
)

func (s *Service) updateWorker(ctx context.Context, operationID string, worker map[string]any, callback workerCallbackKind) error {
	if operationID == "" {
		return nil
	}
	// A worker must remain journaled even when its action context has timed out.
	// The bounded background context only covers this small durable update.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	return s.Store.UpdateOperationResponse(persistCtx, operationID, func(response map[string]any) error {
		if callback != workerCallbackState {
			if previous, ok := response["worker"].(map[string]any); ok && stringValue(previous["state"]) == "running" {
				if callback == workerCallbackStarted || callback == workerCallbackStartFailed || !workerIdentityMatches(previous, worker) {
					// A callback for a new worker must never overwrite an older
					// identity that has not been reconciled.
					return errUnresolvedWorker
				}
			}
		}
		response["worker"] = worker
		return nil
	})
}

func (s *Service) pendingWorkerUnresolved(ctx context.Context, sessionID *string, currentOperationID, recoveryCoordinatorID string) (bool, error) {
	pending, err := s.Store.PendingOperations(ctx)
	if err != nil {
		return false, err
	}
	for _, operation := range pending {
		if !sameWorkerScope(sessionID, operation.SessionID) {
			continue
		}
		current := operation.OperationID == currentOperationID
		if worker, ok := operation.Response["worker"].(map[string]any); ok && workerNeedsReconciliation(worker, current) {
			if operation.OperationID == recoveryCoordinatorID && workerStartPrepared(worker) {
				continue
			}
			return true, nil
		}
	}
	return false, nil
}

func workerStartPrepared(worker map[string]any) bool {
	return worker != nil && stringValue(worker["state"]) == "starting" && !boolValue(worker["start_attempted"])
}

func sameWorkerScope(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func workerNeedsReconciliation(worker map[string]any, current bool) bool {
	if worker == nil {
		return false
	}
	if workerOwnershipUnresolved(worker) {
		return true
	}
	return stringValue(worker["state"]) == "starting" && !current
}

func workerOwnershipUnresolved(worker map[string]any) bool {
	if worker == nil {
		return false
	}
	stateValue := stringValue(worker["state"])
	return stateValue == "running" || stateValue == "starting" && boolValue(worker["start_attempted"]) || stateValue == "finished" && stringValue(worker["error"]) != ""
}

func workerIdentityMatches(previous, current map[string]any) bool {
	previousPID, previousPIDOK := protocol.Integer(previous["pid"])
	currentPID, currentPIDOK := protocol.Integer(current["pid"])
	previousGroup, previousGroupOK := protocol.Integer(previous["process_group"])
	currentGroup, currentGroupOK := protocol.Integer(current["process_group"])
	return previousPIDOK && currentPIDOK && previousGroupOK && currentGroupOK && previousPID == currentPID && previousGroup == currentGroup && stringValue(previous["start_time"]) == stringValue(current["start_time"])
}

func (s *Service) reconcilePendingWorkers(ctx context.Context, pending []state.Operation) error {
	for _, operation := range pending {
		unlock := func() {}
		if operation.SessionID != nil {
			var acquired bool
			unlock, acquired = s.trySessionLock(*operation.SessionID)
			if !acquired {
				continue
			}
		}
		err := s.reconcilePendingWorker(ctx, operation.OperationID)
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) reconcilePendingWorker(ctx context.Context, operationID string) error {
	operation, err := s.Store.GetOperation(ctx, operationID)
	if err != nil {
		return s.sqliteError(err)
	}
	if operation == nil || stringValue(operation.Response["outcome"]) != "pending" {
		return nil
	}
	if s.operationActive(operation.OperationID) {
		return nil
	}
	worker, ok := operation.Response["worker"].(map[string]any)
	if !ok {
		return nil
	}
	if stringValue(worker["state"]) == "starting" {
		attempted, explicit := worker["start_attempted"].(bool)
		_, hasPID := worker["pid"]
		_, hasStart := worker["start_time"]
		_, hasGroup := worker["process_group"]
		if explicit && !attempted && !hasPID && !hasStart && !hasGroup {
			worker["state"] = "exited"
			worker["observed_at"] = nowString(s.Clock())
			if err := s.recordWorkerState(ctx, operation.OperationID, worker); err != nil {
				return s.sqliteError(err)
			}
			return nil
		}
		if attempted {
			return protocol.NewError("state_unverified", "pending native worker start has no durable identity", map[string]any{"operation_id": operation.OperationID})
		}
	}
	if stringValue(worker["state"]) != "running" && !(stringValue(worker["state"]) == "finished" && stringValue(worker["error"]) != "") {
		return nil
	}
	pid, valid := protocol.Integer(worker["pid"])
	startTime, hasStartTime := worker["start_time"].(string)
	if !valid || pid <= 0 || !hasStartTime || startTime == "" {
		return protocol.NewError("state_unverified", "pending operation has an invalid native worker identity", map[string]any{"operation_id": operation.OperationID})
	}
	processGroup, validProcessGroup := protocol.Integer(worker["process_group"])
	if !validProcessGroup || processGroup <= 0 {
		return protocol.NewError("state_unverified", "pending operation has an invalid native worker identity", map[string]any{"operation_id": operation.OperationID})
	}
	identity := native.WorkerIdentity{PID: int(pid), StartTime: startTime, ProcessGroup: int(processGroup)}
	alive, err := native.WorkerAlive(identity)
	if err != nil {
		return protocol.NewError("state_unverified", "pending native worker identity could not be verified", map[string]any{"operation_id": operation.OperationID, "cause": err.Error()})
	}
	if alive {
		if err := native.KillWorker(identity); err != nil {
			return protocol.NewError("state_unverified", "pending native worker could not be stopped safely", map[string]any{"operation_id": operation.OperationID, "cause": err.Error()})
		}
		// KillWorker returns only after the root process group has been
		// confirmed absent. Keep the operation pending for recovery, but do
		// not leave a dead worker in a state reconciliation will skip forever.
		worker["state"] = "exited"
		worker["observed_at"] = nowString(s.Clock())
		if err := s.recordWorkerState(ctx, operation.OperationID, worker); err != nil {
			return s.sqliteError(err)
		}
		return protocol.NewError("startup_reconcile_required", "a pending native worker was stopped; retry reconciliation before admission", map[string]any{"operation_id": operation.OperationID})
	}
	worker["state"] = "exited"
	worker["observed_at"] = nowString(s.Clock())
	if err := s.recordWorkerState(ctx, operation.OperationID, worker); err != nil {
		return s.sqliteError(err)
	}
	return nil
}

func (s *Service) recoverPending(ctx context.Context, pending []state.Operation) error {
	var firstErr error
	for _, snapshot := range pending {
		operationPtr, err := s.Store.GetOperation(ctx, snapshot.OperationID)
		if err != nil {
			return s.sqliteError(err)
		}
		if operationPtr == nil || stringValue(operationPtr.Response["outcome"]) != "pending" {
			continue
		}
		operation := *operationPtr
		if s.operationActive(operation.OperationID) {
			continue
		}
		preStartTimer := false
		if workerValue, hasWorker := operation.Response["worker"]; hasWorker {
			worker, validWorker := workerValue.(map[string]any)
			stateValue := ""
			if validWorker {
				stateValue = stringValue(worker["state"])
				preStartTimer = recoverableBeforeNativeStart(operation.Action, worker)
			}
			if !validWorker || !recoverableOperation(operation.Action) || (stateValue != "finished" && stateValue != "exited" && !preStartTimer) {
				continue
			}
		}
		if !recoverableOperation(operation.Action) {
			continue
		}
		if recoveryAttempts(operation.Response) > 0 && !preStartTimer {
			eligible, retryErr := s.mayRetryClearedMaintenance(ctx, operation)
			if retryErr != nil {
				return retryErr
			}
			if !eligible {
				continue
			}
			if err := s.Store.UpdateOperationResponse(ctx, operation.OperationID, func(response map[string]any) error {
				if stringValue(response["outcome"]) != "pending" || boolValue(response["_recovery_after_clear_attempted"]) {
					return errUnresolvedWorker
				}
				response["_recovery_after_clear_attempted"] = true
				return nil
			}); err != nil {
				return s.sqliteError(err)
			}
		}
		rawRequest, ok := operation.Response["_request"].(map[string]any)
		if !ok {
			continue
		}
		if err := s.Store.MarkOperationRecoveryAttempt(ctx, operation.OperationID); err != nil {
			if firstErr == nil {
				firstErr = s.sqliteError(err)
			}
			continue
		}
		request := protocol.Request(rawRequest)
		if err := protocol.ValidateRequest(request); err != nil {
			if recordErr := s.recordError(ctx, request, protocol.NewError("operation_unverified", "saved pending request is invalid", map[string]any{"cause": err.Error()})); recordErr != nil && firstErr == nil {
				firstErr = s.sqliteError(recordErr)
			}
			continue
		}
		if _, compatibilityErr := protocol.ValidateCompatibility(request, s.Release); compatibilityErr != nil {
			if recordErr := s.recordError(ctx, request, compatibilityErr); recordErr != nil && firstErr == nil {
				firstErr = s.sqliteError(recordErr)
			}
			continue
		}
		finishActive := s.markOperationActive(operation.OperationID)
		recoveryCtx := native.WithWorkerRecorder(ctx, operation.OperationID, s)
		if coordinatorID := native.WorkerOperationID(ctx); coordinatorID != "" && coordinatorID != operation.OperationID {
			recoveryCtx = native.WithWorkerRecovery(recoveryCtx, coordinatorID)
		}
		result, dispatchErr := s.dispatchAction(recoveryCtx, operation.Action, request)
		finishActive()
		if dispatchErr != nil {
			if uncertain(dispatchErr) {
				if firstErr == nil {
					firstErr = protocol.NewError("startup_reconcile_required", "pending operation recovery remains uncertain", map[string]any{"operation_id": operation.OperationID, "cause": dispatchErr.Error()})
				}
				continue
			}
			if recordErr := s.recordError(ctx, request, dispatchErr); recordErr != nil && firstErr == nil {
				firstErr = s.sqliteError(recordErr)
			}
			continue
		}
		response := protocol.SuccessWithRelease(request, result, s.Release, nil)
		responseDigest, digestErr := protocol.Digest(request)
		if digestErr != nil {
			if firstErr == nil {
				firstErr = protocol.NewError("state_unverified", "recovered request cannot be canonicalized", map[string]any{"operation_id": operation.OperationID})
			}
			continue
		}
		response["_request_digest"] = responseDigest
		if err := s.Store.RecordOperation(ctx, operation.OperationID, operation.Action, operation.SessionID, operation.Generation, response); err != nil && firstErr == nil {
			firstErr = s.sqliteError(err)
		}
	}
	return firstErr
}

// A blocked maintenance operation may receive one more attempt after all task
// transitions have cleared and its last native worker is confirmed absent.
func (s *Service) mayRetryClearedMaintenance(ctx context.Context, operation state.Operation) (bool, error) {
	if operation.Action != "admin-reconcile" || boolValue(operation.Response["_recovery_after_clear_attempted"]) {
		return false, nil
	}
	raw, ok := operation.Response["_request"].(map[string]any)
	if !ok {
		return false, nil
	}
	req := protocol.Request(raw)
	digest, err := protocol.Digest(req)
	if err != nil || protocol.ValidateRequest(req) != nil || digest != stringValue(operation.Response["_request_digest"]) || stringValue(req["action"]) != operation.Action || stringValue(req["operation_id"]) != operation.OperationID || operation.Generation == nil || intValue(req["generation"]) != *operation.Generation || optionalString(req["session_id"]) != nil && (operation.SessionID == nil || stringValue(req["session_id"]) != *operation.SessionID) {
		return false, nil
	}
	if operation.SessionID != nil && stringValue(req["session_id"]) != *operation.SessionID {
		return false, nil
	}
	worker, ok := operation.Response["worker"].(map[string]any)
	if !ok || (stringValue(worker["state"]) != "finished" && stringValue(worker["state"]) != "exited") || workerOwnershipUnresolved(worker) {
		return false, nil
	}
	pid, pidOK := protocol.Integer(worker["pid"])
	group, groupOK := protocol.Integer(worker["process_group"])
	start := stringValue(worker["start_time"])
	if !pidOK || !groupOK || pid <= 0 || group <= 0 || start == "" {
		return false, nil
	}
	alive, err := native.WorkerAlive(native.WorkerIdentity{PID: int(pid), ProcessGroup: int(group), StartTime: start})
	if err != nil {
		return false, protocol.NewError("state_unverified", "pending maintenance worker could not be verified", map[string]any{"cause": err.Error()})
	}
	if alive {
		return false, nil
	}
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return false, s.sqliteError(err)
	}
	for _, task := range tasks {
		if task.State == statePreparing || task.State == stateClosing || task.State == stateDeleting || task.State == stateFailed {
			return false, nil
		}
	}
	return true, nil
}

func recoverableOperation(action string) bool {
	switch action {
	case "drain-enter", "drain-exit", "admin-reconcile", "admin-gc", "timer-reconcile", "timer-age-gc":
		return true
	default:
		return false
	}
}

func recoverableBeforeNativeStart(action string, worker map[string]any) bool {
	if action != "timer-reconcile" && action != "timer-age-gc" {
		return false
	}
	return stringValue(worker["state"]) == "starting" && !boolValue(worker["start_attempted"])
}

func operationHasNativeEffect(action string) bool {
	switch action {
	case "acquire", "release", "destroy", "restart", "resize", "port-add", "port-remove", "admin-reconcile", "admin-gc", "timer-reconcile", "timer-age-gc":
		return true
	default:
		return false
	}
}

func releaseSensitiveAction(action string, request protocol.Request) bool {
	switch action {
	case "acquire", "renew", "release", "destroy", "restart", "resize", "port-add", "port-remove", "timer-expire":
		return true
	case "admin-reconcile", "timer-reconcile":
		return requestBool(request, "apply")
	case "admin-gc", "timer-age-gc":
		return true
	default:
		return false
	}
}

func isReleaseCleanupAction(action string) bool {
	switch action {
	case "timer-reconcile", "timer-age-gc":
		return true
	default:
		return false
	}
}

func isJournaledTimerAction(action string, request protocol.Request) bool {
	return (action == "timer-reconcile" || action == "timer-age-gc") && requestBool(request, "apply")
}

func recoveryAttempts(response map[string]any) int64 {
	value, ok := response["_recovery_attempts"]
	if !ok {
		return 0
	}
	parsed, valid := protocol.Integer(value)
	if !valid || parsed < 0 {
		return 1
	}
	return parsed
}

func (s *Service) Dispatch(ctx context.Context, request protocol.Request) (map[string]any, error) {
	if err := protocol.ValidateRequest(request); err != nil {
		return nil, err
	}
	warnings, compatibilityErr := protocol.ValidateCompatibility(request, s.Release)
	if compatibilityErr != nil {
		return nil, compatibilityErr
	}
	action, _ := request["action"].(string)
	if isJournaledTimerAction(action, request) {
		request = cloneMap(request)
		if request["operation_id"] == nil {
			request["operation_id"] = newTimerOperationID(action)
		}
		if request["generation"] == nil {
			request["generation"] = int64(0)
		}
	}
	if sessionActions[action] && request["session_id"] == nil {
		return nil, protocol.NewError("missing_session", "session_id is required for this action", nil)
	}
	if action == "admin-metrics" && (request["session_id"] != nil || request["operation_id"] != nil) {
		return nil, protocol.NewError("invalid_request", "admin-metrics is sessionless and read-only", nil)
	}
	write := (writeActions[action] || isJournaledTimerAction(action, request)) && !(action == "admin-reconcile" && requestBool(request, "apply") == false && request["operation_id"] == nil)
	if write && request["operation_id"] == nil {
		return nil, protocol.NewError("invalid_request", "write request requires operation_id", nil)
	}
	if write && request["generation"] == nil {
		return nil, protocol.NewError("invalid_request", "write request requires generation", nil)
	}
	unlock := s.lockAction(action, request)
	defer unlock()
	if releaseSensitiveAction(action, request) && !isInternalCleanup(ctx) {
		releaseDrain, drainErr := s.releaseDrainOwned(ctx)
		if drainErr != nil {
			return nil, s.sqliteError(drainErr)
		}
		sealed, sealedErr := s.releaseDrainSealed(ctx)
		if sealedErr != nil {
			return nil, s.sqliteError(sealedErr)
		}
		if releaseDrain && (sealed || !isReleaseCleanupAction(action)) {
			return nil, protocol.NewError("draining", "native session actions are refused while a release activation fence is owned", nil)
		}
	}
	if write {
		operationID := stringValue(request["operation_id"])
		digest, err := protocol.Digest(request)
		if err != nil {
			return nil, protocol.NewError("invalid_request", "request cannot be canonicalized", nil)
		}
		pending := map[string]any{"outcome": "pending", "_request_digest": digest, "_request": map[string]any(request)}
		// Persist the native-effect barrier before invoking Incus. A crash in the
		// window before WorkerStarted must remain fail-closed and must not become a
		// replayable operation.
		if operationHasNativeEffect(action) {
			pending["worker"] = map[string]any{"state": "starting", "start_attempted": false, "prepared_at": nowString(s.Clock())}
		}
		generation := intValue(request["generation"])
		session := optionalString(request["session_id"])
		finishActive := s.markOperationActive(operationID)
		existing, err := s.Store.ClaimOperation(ctx, operationID, action, session, generationPointer(generation, request["generation"] != nil), pending)
		if err != nil {
			finishActive()
			return nil, s.sqliteError(err)
		}
		if existing != nil {
			finishActive()
			return s.replayRecord(request, existing, warnings)
		}
		defer finishActive()
		ctx = native.WithWorkerRecorder(ctx, operationID, s)
	}
	dispatchCtx := ctx
	if isReleaseCleanupAction(action) {
		dispatchCtx = withInternalCleanup(ctx)
	}
	result, err := s.dispatchAction(dispatchCtx, action, request)
	if err != nil {
		if s.Snapshots != nil {
			_ = s.publishSnapshotFailure(action, err)
		}
		if write && !uncertain(err) {
			_ = s.recordError(ctx, request, err)
		}
		if write {
			outcome := "error"
			if uncertain(err) {
				outcome = "unknown"
			}
			_ = s.recordAudit(ctx, request, outcome, err, nil)
		}
		return nil, err
	}
	response := protocol.SuccessWithRelease(request, result, s.Release, warnings)
	if write {
		operationID := stringValue(request["operation_id"])
		digest, _ := protocol.Digest(request)
		response["_request_digest"] = digest
		if err := s.Store.RecordOperation(ctx, operationID, stringValue(request["action"]), optionalString(request["session_id"]), generationPointer(intValue(request["generation"]), request["generation"] != nil), response); err != nil {
			return nil, s.sqliteError(err)
		}
		_ = s.recordAudit(ctx, request, "success", nil, response)
	}
	if s.Snapshots != nil {
		_ = s.publishSnapshot(action, response)
	}
	if write && !isJournaledTimerAction(action, request) {
		s.wakeScheduler()
	}
	return response, nil
}

func (s *Service) publishSnapshot(action string, response map[string]any) error {
	source := map[string]string{"admin-metrics": "capacity", "admin-status": "inventory", "doctor": "health"}[action]
	if source == "" {
		return nil
	}
	return s.Snapshots.Publish(source, response)
}

func (s *Service) publishSnapshotFailure(action string, err error) error {
	source := map[string]string{"admin-metrics": "capacity", "admin-status": "inventory", "doctor": "health"}[action]
	if source == "" || s.Snapshots == nil {
		return nil
	}
	control := protocol.AsError(err)
	value := map[string]any{
		"up":          false,
		"observed_at": nowString(s.Clock()),
		"error": map[string]any{
			"code":    control.Code,
			"message": control.Message,
		},
	}
	return s.Snapshots.Publish(source, value)
}

func (s *Service) replay(ctx context.Context, request protocol.Request, write bool) (map[string]any, error) {
	if !write {
		return nil, nil
	}
	op := stringValue(request["operation_id"])
	if op == "" {
		return nil, nil
	}
	record, err := s.Store.GetOperation(ctx, op)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	if record == nil {
		return nil, nil
	}
	return s.replayRecord(request, record)
}

func (s *Service) replayRecord(request protocol.Request, record *state.Operation, warningGroups ...[]any) (map[string]any, error) {
	op := stringValue(request["operation_id"])
	if !operationIdentityMatches(record, request) {
		return nil, protocol.NewError("operation_conflict", "operation ID belongs to a different lifecycle identity", map[string]any{"operation_id": op})
	}
	digest, err := protocol.Digest(request)
	if err != nil {
		return nil, protocol.NewError("invalid_request", "request cannot be canonicalized", nil)
	}
	if stringValue(record.Response["_request_digest"]) != digest {
		return nil, protocol.NewError("operation_conflict", "operation ID belongs to a different request", nil)
	}
	outcome := stringValue(record.Response["outcome"])
	if outcome == "pending" {
		return nil, protocol.NewError("operation_incomplete", "previous execution has an uncertain outcome; inspect status and reconcile before another mutation", map[string]any{"operation_id": op})
	}
	if outcome == "error" {
		if raw, ok := record.Response["error"].(map[string]any); ok {
			return nil, protocol.NewError(stringValue(raw["code"]), stringValue(raw["message"]), raw["detail"])
		}
		return nil, protocol.NewError("operation_unverified", "operation journal error is malformed", map[string]any{"operation_id": op})
	}
	if outcome != "success" {
		return nil, protocol.NewError("operation_unverified", "operation journal outcome is invalid", map[string]any{"operation_id": op})
	}
	replayed := cloneMap(record.Response)
	delete(replayed, "_request_digest")
	replayed["outcome"] = "replayed"
	replayed["server_time"] = nowString(s.Clock())
	if s.Release != nil {
		replayed["release"] = protocol.ReleaseEnvelope(s.Release)
		if len(warningGroups) > 0 {
			storedWarnings, _ := replayed["warnings"].([]any)
			for _, incoming := range warningGroups[0] {
				if !hasWarning(storedWarnings, incoming) {
					storedWarnings = append(storedWarnings, incoming)
				}
			}
			replayed["warnings"] = storedWarnings
		}
	}
	return replayed, nil
}

func operationIdentityMatches(record *state.Operation, request protocol.Request) bool {
	if record == nil || record.Action != stringValue(request["action"]) {
		return false
	}
	expectedSession := optionalString(request["session_id"])
	if (record.SessionID == nil) != (expectedSession == nil) || record.SessionID != nil && *record.SessionID != *expectedSession {
		return false
	}
	_, generationPresent := request["generation"]
	expectedGeneration := generationPointer(intValue(request["generation"]), generationPresent)
	if (record.Generation == nil) != (expectedGeneration == nil) || record.Generation != nil && *record.Generation != *expectedGeneration {
		return false
	}
	return true
}

func hasWarning(existing []any, candidate any) bool {
	want, ok := candidate.(map[string]any)
	if !ok {
		return false
	}
	for _, value := range existing {
		item, ok := value.(map[string]any)
		if ok && stringValue(item["code"]) == stringValue(want["code"]) && stringValue(item["message"]) == stringValue(want["message"]) && stringValue(item["action"]) == stringValue(want["action"]) {
			return true
		}
	}
	return false
}

// StartupReconcile performs the fail-closed recovery pass before admission is
// marked ready. Pending journal intents are evidence of an uncertain native
// outcome and must remain visible for explicit inspection or cleanup.
func (s *Service) StartupReconcile(ctx context.Context) error {
	s.maintenance.Lock()
	defer s.maintenance.Unlock()
	if _, err := s.Store.StateEpoch(ctx); err != nil {
		return s.sqliteError(err)
	}
	_, reconcileErr := s.reconcile(ctx, true)
	if reconcileErr != nil && protocol.AsError(reconcileErr).Code != "cleanup_incomplete" {
		return reconcileErr
	}
	pending, err := s.Store.PendingOperations(ctx)
	if err != nil {
		return s.sqliteError(err)
	}
	if reconcileErr != nil {
		unresolved, workerErr := s.pendingWorkerUnresolved(ctx, nil, "", native.WorkerRecoveryCoordinator(ctx))
		if workerErr != nil {
			return s.sqliteError(workerErr)
		}
		if unresolved {
			return reconcileErr
		}
	}
	if len(pending) > 0 {
		if err := s.reconcilePendingWorkers(ctx, pending); err != nil {
			return err
		}
		if err := s.recoverPending(ctx, pending); err != nil {
			return err
		}
		pending, err = s.Store.PendingOperations(ctx)
		if err != nil {
			return s.sqliteError(err)
		}
		if err := s.reconcileSupersededOperations(ctx, pending); err != nil {
			return err
		}
		pending, err = s.Store.PendingOperations(ctx)
		if err != nil {
			return s.sqliteError(err)
		}
		if err := s.reconcileOrphanedAcquireOperations(ctx, pending); err != nil {
			if protocol.AsError(err).Code != "startup_reconcile_required" {
				return err
			}
			if reconcileErr == nil {
				reconcileErr = err
			}
		}
		pending, err = s.Store.PendingOperations(ctx)
		if err != nil {
			return s.sqliteError(err)
		}
	}
	if reconcileErr != nil {
		return reconcileErr
	}
	if len(pending) > 0 {
		return protocol.NewError("startup_reconcile_required", "pending operations require explicit recovery before admission", map[string]any{"count": len(pending), "admission_ready": false})
	}
	return s.verifyResidentNativeState(ctx)
}

// recoveryQuiescence is implemented by the native control authority only. It
// proves that no old control child or Incus background operation remains.
type recoveryQuiescence interface {
	VerifyRecoveryQuiescence(context.Context) error
}

// reconcileSupersededOperations preserves the original request and worker
// evidence while reporting an unknown historical outcome. A later serialized
// successful operation and a verified, quiescent cached task fence the old
// effect; the original native request is never replayed.
func (s *Service) reconcileSupersededOperations(ctx context.Context, pending []state.Operation) error {
	probe, ok := s.Guest.(recoveryQuiescence)
	if !ok || native.WorkerOperationID(ctx) != "" {
		return nil
	}
	seen := make(map[string]bool)
	for _, candidate := range pending {
		if candidate.SessionID == nil || seen[*candidate.SessionID] {
			continue
		}
		session := *candidate.SessionID
		seen[session] = true
		unlock, acquired := s.trySessionLock(session)
		if !acquired {
			continue
		}
		err := s.reconcileSupersededSession(ctx, session, probe)
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) reconcileSupersededSession(ctx context.Context, session string, probe recoveryQuiescence) error {
	task, err := s.Store.GetTask(ctx, map[string]any{"session_id": session})
	if err != nil {
		return s.sqliteError(err)
	}
	if task == nil || task.State != stateCached || task.LeaseExpiresAt != nil || task.ReservationBytes != 0 || task.ResourceReservationJSON != "{}" {
		return nil
	}
	pending, err := s.Store.PendingOperations(ctx)
	if err != nil {
		return s.sqliteError(err)
	}
	type superseded struct {
		operation state.Operation
		witness   string
	}
	var group []superseded
	for _, operation := range pending {
		if operation.SessionID == nil || *operation.SessionID != session {
			continue
		}
		if operation.Generation == nil || *operation.Generation > task.Generation || s.operationActive(operation.OperationID) || (operation.Action != "acquire" && operation.Action != "resize" && operation.Action != "restart") {
			return nil
		}
		if taskID, exists := operation.Response["_task_id"]; exists && stringValue(taskID) != task.TaskID {
			return nil
		}
		if boundGeneration, exists := operation.Response["_task_generation"]; exists {
			value, valid := protocol.Integer(boundGeneration)
			if !valid || value < *operation.Generation || value > task.Generation {
				return nil
			}
		}
		worker, valid := operation.Response["worker"].(map[string]any)
		if !valid || (stringValue(worker["state"]) != "finished" && stringValue(worker["state"]) != "exited") || workerOwnershipUnresolved(worker) {
			return nil
		}
		pid, pidOK := protocol.Integer(worker["pid"])
		pgid, groupOK := protocol.Integer(worker["process_group"])
		start := stringValue(worker["start_time"])
		if !pidOK || !groupOK || pid <= 0 || pgid <= 0 || start == "" {
			return nil
		}
		alive, workerErr := native.WorkerAlive(native.WorkerIdentity{PID: int(pid), ProcessGroup: int(pgid), StartTime: start})
		if workerErr != nil || alive {
			return nil
		}
		raw, valid := operation.Response["_request"].(map[string]any)
		if !valid {
			return nil
		}
		req := protocol.Request(raw)
		digest, digestErr := protocol.Digest(req)
		if protocol.ValidateRequest(req) != nil || digestErr != nil || digest != stringValue(operation.Response["_request_digest"]) || stringValue(req["operation_id"]) != operation.OperationID || stringValue(req["action"]) != operation.Action || stringValue(req["session_id"]) != session || intValue(req["generation"]) != *operation.Generation {
			return nil
		}
		later, err := s.Store.LaterSuccessfulSessionOperations(ctx, operation.OperationID)
		if err != nil {
			return s.sqliteError(err)
		}
		witness := ""
		for _, completed := range later {
			if supersedingOperationMatches(operation, completed, task) {
				witness = completed.OperationID
				break
			}
		}
		if witness == "" {
			return nil
		}
		group = append(group, superseded{operation: operation, witness: witness})
	}
	if len(group) == 0 {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
	defer cancel()
	if err := s.verifyCachedGuest(checkCtx, task); err != nil {
		return nil
	}
	if err := probe.VerifyRecoveryQuiescence(checkCtx); err != nil {
		return protocol.NewError("startup_reconcile_required", "historical operation quiescence could not be verified", map[string]any{"admission_ready": false, "cause": err.Error()})
	}
	for _, item := range group {
		response := cloneMap(item.operation.Response)
		response["outcome"] = "error"
		response["error"] = protocol.NewError("operation_outcome_unknown", "the original outcome is unknown; a later operation superseded its lease", map[string]any{"outcome_verified": false, "lease_state_verified": true, "superseded_by_operation_id": item.witness})
		if err := s.Store.RecordOperation(ctx, item.operation.OperationID, item.operation.Action, item.operation.SessionID, item.operation.Generation, response); err != nil {
			return s.sqliteError(err)
		}
	}
	return nil
}

func supersedingOperationMatches(original, completed state.Operation, task *state.Task) bool {
	if original.Generation == nil || completed.Generation == nil || completed.SessionID == nil || *completed.SessionID != task.SessionID || *completed.Generation < *original.Generation || *completed.Generation > task.Generation {
		return false
	}
	worker, workerOK := original.Response["worker"].(map[string]any)
	created, createdErr := time.Parse(time.RFC3339Nano, original.CreatedAt)
	finished, finishedErr := time.Parse(time.RFC3339Nano, stringValue(worker["finished_at"]))
	laterCreated, laterErr := time.Parse(time.RFC3339Nano, completed.CreatedAt)
	if !workerOK || createdErr != nil || finishedErr != nil || laterErr != nil || !laterCreated.After(created) || !laterCreated.After(finished) {
		return false
	}
	response := completed.Response
	digest := stringValue(response["_request_digest"])
	digestBytes, digestErr := hex.DecodeString(digest)
	if digestErr != nil || len(digestBytes) != sha256.Size || stringValue(response["outcome"]) != "success" || !boolValue(response["ok"]) || intValue(response["protocol"]) != protocol.Version || stringValue(response["operation_id"]) != completed.OperationID || stringValue(response["action"]) != completed.Action || stringValue(response["session_id"]) != task.SessionID || intValue(response["generation"]) != *completed.Generation {
		return false
	}
	result, ok := response["result"].(map[string]any)
	if !ok || stringValue(result["task_id"]) != task.TaskID || stringValue(result["session_id"]) != task.SessionID {
		return false
	}
	resultGeneration, validGeneration := protocol.Integer(result["generation"])
	effectiveLease, validLease := result["effective_lease"].(bool)
	if !validGeneration || !validLease || resultGeneration < *completed.Generation || resultGeneration > task.Generation {
		return false
	}
	if completed.Action == "release" && stringValue(result["state"]) == stateCached && !effectiveLease {
		return original.Action != "acquire" || resultGeneration > *original.Generation
	}
	return original.Action == "acquire" && completed.Action == "acquire" && *completed.Generation > *original.Generation && resultGeneration > *original.Generation && stringValue(result["state"]) == stateActive && effectiveLease
}

// reconcileOrphanedAcquireOperations closes the narrow crash window where a
// native acquire was journaled but the durable task was never committed. The
// native identity must be explicitly verified missing before the operation can
// be finalized; every other result remains fail-closed for operator recovery.
func (s *Service) reconcileOrphanedAcquireOperations(ctx context.Context, pending []state.Operation) error {
	var firstErr error
	for _, operation := range pending {
		if operation.Action != "acquire" || operation.SessionID == nil || operation.Generation == nil {
			continue
		}
		if s.Guest == nil {
			return protocol.NewError("startup_reconcile_required", "pending acquire operations cannot be reconciled without native access", map[string]any{"admission_ready": false})
		}
		unlock, acquired := s.trySessionLock(*operation.SessionID)
		if !acquired {
			continue
		}
		err := s.reconcileOrphanedAcquire(ctx, operation.OperationID)
		unlock()
		if err != nil {
			if protocol.AsError(err).Code != "startup_reconcile_required" {
				return err
			}
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	return firstErr
}

// The session lock prevents an acquire from starting between the journal and native checks.
func (s *Service) reconcileOrphanedAcquire(ctx context.Context, operationID string) error {
	operation, err := s.Store.GetOperation(ctx, operationID)
	if err != nil {
		return s.sqliteError(err)
	}
	if operation == nil || operation.Action != "acquire" || operation.SessionID == nil || operation.Generation == nil || stringValue(operation.Response["outcome"]) != "pending" {
		return nil
	}
	if s.operationActive(operation.OperationID) {
		return nil
	}
	worker, ok := operation.Response["worker"].(map[string]any)
	if ok && stringValue(worker["state"]) == "starting" {
		return s.reconcileLegacyCachedStarts(ctx, operation)
	}
	if !ok || (stringValue(worker["state"]) != "finished" && stringValue(worker["state"]) != "exited") || workerOwnershipUnresolved(worker) {
		return nil
	}
	unresolved, workerErr := s.pendingWorkerUnresolved(ctx, operation.SessionID, "", native.WorkerRecoveryCoordinator(ctx))
	if workerErr != nil {
		return s.sqliteError(workerErr)
	}
	if unresolved {
		return nil
	}
	task, err := s.Store.GetTask(ctx, map[string]any{"session_id": *operation.SessionID})
	if err != nil {
		return s.sqliteError(err)
	}
	if task != nil && task.State != stateAbsent && task.State != stateCached {
		return nil
	}
	digest := stringValue(operation.Response["_request_digest"])
	if digest == "" {
		return protocol.NewError("startup_reconcile_required", "orphaned acquire operation is missing its request digest", map[string]any{"operation_id": operation.OperationID, "admission_ready": false})
	}
	saved, valid := operation.Response["_request"].(map[string]any)
	if !valid {
		return protocol.NewError("startup_reconcile_required", "orphaned acquire request is missing", map[string]any{"operation_id": operation.OperationID})
	}
	request := protocol.Request(saved)
	computed, digestErr := protocol.Digest(request)
	if protocol.ValidateRequest(request) != nil || digestErr != nil || digest != computed || stringValue(request["action"]) != "acquire" || stringValue(request["operation_id"]) != operation.OperationID || stringValue(request["session_id"]) != *operation.SessionID || intValue(request["generation"]) != *operation.Generation {
		return protocol.NewError("startup_reconcile_required", "orphaned acquire request identity is inconsistent", map[string]any{"operation_id": operation.OperationID})
	}
	if task != nil && task.State == stateCached {
		if !acquireOperationMatchesTask(*operation, task) {
			return protocol.NewError("startup_reconcile_required", "pending acquire generation does not match cached task", map[string]any{"operation_id": operation.OperationID, "admission_ready": false, "outcome_verified": false})
		}
		if _, ownershipErr := s.verifyOwnership(ctx, task); ownershipErr != nil {
			return protocol.NewError("startup_reconcile_required", "cached task ownership could not be verified while reconciling acquire operations", map[string]any{"operation_id": operation.OperationID, "admission_ready": false, "outcome_verified": false, "cause": ownershipErr.Error()})
		}
		statusCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
		status, statusErr := s.Guest.Status(statusCtx, task.InstanceName)
		cancel()
		if statusErr != nil {
			return protocol.NewError("startup_reconcile_required", "cached task status could not be verified while reconciling acquire operations", map[string]any{"operation_id": operation.OperationID, "admission_ready": false, "outcome_verified": false, "cause": statusErr.Error()})
		}
		stateValue, parseErr := native.ParseStatus(status)
		if parseErr != nil || !strings.EqualFold(stateValue, "stopped") {
			if parseErr != nil {
				return protocol.NewError("startup_reconcile_required", "cached task status is malformed while reconciling acquire operations", map[string]any{"operation_id": operation.OperationID, "admission_ready": false, "outcome_verified": false, "cause": parseErr.Error()})
			}
			return protocol.NewError("startup_reconcile_required", "cached task is not stopped while reconciling acquire operations", map[string]any{"operation_id": operation.OperationID, "admission_ready": false, "outcome_verified": false, "status": stateValue})
		}
		response := map[string]any{
			"outcome": "error",
			"error": protocol.NewError("lease_released", "the lease was released before the acquire operation was acknowledged", map[string]any{
				"task_id": task.TaskID, "task_generation": task.Generation, "outcome_verified": true,
			}),
			"_request_digest": digest,
		}
		if err := s.Store.RecordOperation(ctx, operation.OperationID, operation.Action, operation.SessionID, operation.Generation, response); err != nil {
			return s.sqliteError(err)
		}
		return nil
	}
	instanceName := "agent-vm-" + taskID(*operation.SessionID)
	if task != nil && task.InstanceName != "" {
		instanceName = task.InstanceName
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
	_, configErr := s.Guest.Config(checkCtx, instanceName)
	cancel()
	if configErr == nil {
		return protocol.NewError("startup_reconcile_required", "orphaned acquire still has a native instance without durable task state", map[string]any{"operation_id": operation.OperationID, "admission_ready": false, "outcome_verified": false})
	}
	if !native.IsNotFound(configErr, instanceName, s.Config.Project) {
		return protocol.NewError("startup_reconcile_required", "orphaned acquire native outcome could not be verified", map[string]any{"operation_id": operation.OperationID, "admission_ready": false, "outcome_verified": false, "cause": configErr.Error()})
	}
	response := map[string]any{
		"outcome": "error",
		"error": protocol.NewError("acquire_partial", "native initialization did not complete", map[string]any{
			"phase": "native-init", "operation_phase": "native-init", "native_init_recovered_absent": true, "outcome_verified": true,
		}),
		"_request_digest": digest,
	}
	if err := s.Store.RecordOperation(ctx, operation.OperationID, operation.Action, operation.SessionID, operation.Generation, response); err != nil {
		return s.sqliteError(err)
	}
	return nil
}

// Legacy acquire performs no native mutation before allocating a task generation.
// An unchanged cached generation or an absent generation tombstone fences that phase.
func (s *Service) reconcileLegacyCachedStarts(ctx context.Context, operation *state.Operation) error {
	task, err := s.Store.GetTask(ctx, map[string]any{"session_id": *operation.SessionID})
	if err != nil {
		return s.sqliteError(err)
	}
	session, generation := *operation.SessionID, *operation.Generation
	if task == nil {
		if generation != 0 {
			return nil
		}
		allocated, err := s.Store.HasGeneration(ctx, session)
		if err != nil {
			return s.sqliteError(err)
		}
		if allocated {
			return nil
		}
	} else if task.State != stateCached || task.Generation != generation || task.LeaseExpiresAt != nil || task.ReservationBytes != 0 || task.ResourceReservationJSON != "{}" {
		return nil
	}
	pending, err := s.Store.PendingOperations(ctx)
	if err != nil {
		return s.sqliteError(err)
	}
	group := make([]state.Operation, 0)
	for _, candidate := range pending {
		if candidate.SessionID == nil || *candidate.SessionID != session {
			continue
		}
		// An older acquire stays pending, but cannot change whether this
		// generation's legacy acquire advanced the durable task generation.
		if candidate.Action == "acquire" && candidate.Generation != nil && *candidate.Generation < generation && !s.operationActive(candidate.OperationID) {
			worker, ok := candidate.Response["worker"].(map[string]any)
			if ok && stringValue(worker["state"]) == "finished" && !workerOwnershipUnresolved(worker) {
				continue
			}
		}
		if candidate.Action != "acquire" || candidate.Generation == nil || *candidate.Generation != generation || s.operationActive(candidate.OperationID) {
			return nil
		}
		worker, ok := candidate.Response["worker"].(map[string]any)
		if !ok || stringValue(worker["state"]) != "starting" {
			return nil
		}
		if _, ok := worker["start_attempted"]; ok {
			return nil
		}
		if _, ok := worker["pid"]; ok {
			return nil
		}
		if _, ok := worker["start_time"]; ok {
			return nil
		}
		if _, ok := worker["process_group"]; ok {
			return nil
		}
		raw, ok := candidate.Response["_request"].(map[string]any)
		if !ok {
			return nil
		}
		req := protocol.Request(raw)
		digest, digestErr := protocol.Digest(req)
		if protocol.ValidateRequest(req) != nil || digestErr != nil || digest != stringValue(candidate.Response["_request_digest"]) || stringValue(req["action"]) != "acquire" || stringValue(req["operation_id"]) != candidate.OperationID || stringValue(req["session_id"]) != session || intValue(req["generation"]) != generation {
			return nil
		}
		group = append(group, candidate)
	}
	if len(group) == 0 {
		return nil
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
	defer cancel()
	if task != nil {
		if err := s.verifyCachedGuest(checkCtx, task); err != nil {
			return protocol.NewError("startup_reconcile_required", "legacy cached acquisition could not be verified", map[string]any{"outcome_verified": false, "cause": err.Error()})
		}
	} else {
		name := "agent-vm-" + taskID(session)
		_, err := s.Guest.Config(checkCtx, name)
		if err == nil || !native.IsNotFound(err, name, s.Config.Project) {
			return protocol.NewError("startup_reconcile_required", "never-allocated instance absence could not be verified", map[string]any{"outcome_verified": false})
		}
	}
	for _, candidate := range group {
		response := map[string]any{"outcome": "error", "error": protocol.NewError("startup_reconcile_required", "acquire ended before allocating or advancing a task generation", map[string]any{"outcome_verified": true}), "_request_digest": candidate.Response["_request_digest"]}
		if err := s.Store.RecordOperation(ctx, candidate.OperationID, candidate.Action, candidate.SessionID, candidate.Generation, response); err != nil {
			return s.sqliteError(err)
		}
	}
	return nil
}

// verifyResidentNativeState prevents a durable active or cached task from
// becoming admission-visible when its Incus identity or state cannot be
// verified after a process restart. Transitional states are handled by the
// reconciliation pass above and are intentionally not probed twice here.
func (s *Service) verifyResidentNativeState(ctx context.Context) error {
	if s.Guest == nil {
		return nil
	}
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return s.sqliteError(err)
	}
	for index := range tasks {
		task := &tasks[index]
		if task.State != stateActive && task.State != stateCached {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
		var verifyErr error
		if task.State == stateActive {
			verifyErr = s.verifyActiveGuest(checkCtx, task)
		} else {
			verifyErr = s.verifyCachedGuest(checkCtx, task)
		}
		cancel()
		if verifyErr != nil {
			control := protocol.AsError(verifyErr)
			return protocol.NewError("startup_reconcile_required", "durable task/native state requires explicit reconciliation", map[string]any{
				"task_id":          task.TaskID,
				"session_id":       task.SessionID,
				"generation":       task.Generation,
				"state":            task.State,
				"native_code":      control.Code,
				"native_message":   control.Message,
				"outcome_verified": false,
			})
		}
	}
	return nil
}

func (s *Service) verifyActiveGuest(ctx context.Context, task *state.Task) error {
	if _, err := s.verifyOwnership(ctx, task); err != nil {
		return err
	}
	status, err := s.Guest.Status(ctx, task.InstanceName)
	if err != nil {
		return protocol.NewError("native_unavailable", "active guest status could not be verified", map[string]any{"cause": err.Error(), "outcome_verified": false})
	}
	stateValue, parseErr := native.ParseStatus(status)
	if parseErr != nil {
		return protocol.NewError("state_unverified", "active guest status is malformed", map[string]any{"cause": parseErr.Error(), "outcome_verified": false})
	}
	if !strings.EqualFold(stateValue, "running") {
		return protocol.NewError("state_unverified", "durable active task has a non-running guest", map[string]any{"status": stateValue, "outcome_verified": false})
	}
	return nil
}

// ValidateManagementVMAction re-checks native ownership immediately before a
// browser-authorized action is delegated. Management remains responsible for
// the API target and lease checks; lifecycle remains responsible for ownership.
func (s *Service) ValidateManagementVMAction(ctx context.Context, task state.Task, _ string, _ map[string]any) error {
	if task.TaskID == "" {
		return protocol.NewError("not_found", "VM does not exist", nil)
	}
	if s.Guest == nil {
		return nil
	}
	_, err := s.verifyOwnership(ctx, &task)
	return err
}

func (s *Service) recordError(ctx context.Context, request protocol.Request, cause error) error {
	op := stringValue(request["operation_id"])
	if op == "" {
		return nil
	}
	control := protocol.AsError(cause)
	digest, _ := protocol.Digest(request)
	response := map[string]any{"outcome": "error", "error": control, "_request_digest": digest}
	return s.Store.RecordOperation(ctx, op, stringValue(request["action"]), optionalString(request["session_id"]), generationPointer(intValue(request["generation"]), request["generation"] != nil), response)
}

func (s *Service) recordAudit(ctx context.Context, request protocol.Request, outcome string, cause error, response map[string]any) error {
	detail := map[string]any{"phase": "dispatch"}
	if cause != nil {
		control := protocol.AsError(cause)
		detail["code"] = control.Code
		detail["message"] = control.Message
		if control.Detail != nil {
			detail["detail"] = control.Detail
		}
	}
	if response != nil {
		if warnings, ok := response["warnings"]; ok {
			detail["warnings"] = warnings
		}
	}
	return s.Store.Audit(ctx, stringValue(request["action"]), stringValue(request["task_id"]), stringValue(request["session_id"]), outcome, generationPointer(intValue(request["generation"]), request["generation"] != nil), detail)
}

func (s *Service) dispatchAction(ctx context.Context, action string, request protocol.Request) (any, error) {
	switch action {
	case "status":
		return s.status(ctx, stringValue(request["session_id"]))
	case "acquire":
		return s.acquire(ctx, request)
	case "renew":
		return s.renew(ctx, request)
	case "release":
		return s.releaseUnlocked(ctx, request, false)
	case "destroy":
		return s.releaseUnlocked(ctx, request, true)
	case "restart":
		return s.restart(ctx, request)
	case "resize":
		return s.resize(ctx, request)
	case "port-add":
		return s.portAdd(ctx, request)
	case "port-list":
		return s.portList(ctx, request)
	case "port-remove":
		return s.portRemove(ctx, request)
	case "drain-enter":
		return s.drain(ctx, true, request)
	case "drain-exit":
		return s.drain(ctx, false, request)
	case "drain-status":
		return s.drainStatus(ctx)
	case "admin-status":
		return s.adminStatus(ctx)
	case "admin-session-list":
		return s.adminSessionList(ctx)
	case "admin-metrics":
		return s.adminMetrics(ctx)
	case "admin-reconcile", "timer-reconcile":
		return s.reconcile(ctx, requestBool(request, "apply"))
	case "admin-gc", "timer-age-gc":
		return s.gc(ctx, requestBool(request, "apply"))
	case "timer-expire":
		return s.expire(ctx, request)
	case "doctor":
		return s.doctor(ctx)
	case "ssh-info":
		return s.sshInfo(ctx, request)
	default:
		return nil, protocol.NewError("unsupported_action", "action is not implemented by the Go controller", map[string]any{"action": action})
	}
}

func (s *Service) status(ctx context.Context, session string) (map[string]any, error) {
	task, err := s.Store.GetTask(ctx, map[string]any{"session_id": session})
	if err != nil {
		return nil, s.sqliteError(err)
	}
	if task == nil {
		return map[string]any{
			"state":           stateAbsent,
			"session_id":      session,
			"generation":      int64(0),
			"effective_lease": false,
			"native_status":   stateAbsent,
			"guest_available": false,
		}, nil
	}
	return s.summary(ctx, task)
}

func (s *Service) acquire(ctx context.Context, request protocol.Request) (map[string]any, error) {
	if s.resident && !s.Ready() {
		return nil, protocol.NewError("startup_reconcile_required", "control service is not ready for admission", map[string]any{"outcome_verified": true})
	}
	if draining, err := s.isDraining(ctx); err != nil {
		return nil, s.sqliteError(err)
	} else if draining {
		return nil, protocol.NewError("draining", "new acquisition is refused while draining", nil)
	}
	session := stringValue(request["session_id"])
	generation := intValue(request["generation"])
	unlockImageSelection := s.Store.LockImageSelection()
	defer unlockImageSelection()
	selectedFingerprint := s.Config.ImageFingerprint
	if managedFingerprint, found, selectionErr := s.Store.CurrentDefaultImage(ctx); selectionErr != nil {
		return nil, s.sqliteError(selectionErr)
	} else if found {
		selectedFingerprint = managedFingerprint
	}
	resources, err := requestedResources(request)
	if err != nil {
		return nil, err
	}
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	task, err := s.Store.GetTask(ctx, map[string]any{"session_id": session})
	if err != nil {
		return nil, s.sqliteError(err)
	}
	now := s.Clock()
	deadline := now.Add(time.Duration(s.Config.InitialLeaseSeconds) * time.Second)
	created := false
	if task == nil {
		created = true
		lease := nowString(deadline)
		task = &state.Task{TaskID: taskID(session), SessionID: session, Generation: 1, State: statePreparing, InstanceName: "agent-vm-" + taskID(session), CreatedAt: nowString(now), UpdatedAt: nowString(now), ImageFingerprint: selectedFingerprint, ResourcesJSON: mustJSON(resources), ResourceReservationJSON: mustJSON(resources), LeaseExpiresAt: &lease}
		if generation != 0 {
			return nil, protocol.NewError("generation_mismatch", "new session must start at generation 0", nil)
		}
	} else {
		if generation != task.Generation && !(task.State == stateCached && generation == task.Generation) {
			return nil, protocol.NewError("generation_mismatch", "task generation does not match request", map[string]any{"expected": task.Generation, "actual": generation})
		}
		if task.State == stateActive && effective(task, now) {
			if requestHasResourceFields(request) {
				current := map[string]int64{}
				_ = json.Unmarshal([]byte(task.ResourcesJSON), &current)
				if !sameResources(current, resources) {
					return nil, protocol.NewError("resource_config_mismatch", "active lease configuration differs; use resize explicitly", map[string]any{"current": current, "requested": resources})
				}
			}
			return s.summary(ctx, task)
		}
		if task.State != stateCached {
			return nil, protocol.NewError("task_transition", "session slot is not available until reconciliation completes", map[string]any{"state": task.State})
		}
		if usable, imageErr := s.Store.CachedImageUsable(ctx, task.ImageFingerprint); imageErr != nil {
			return nil, s.sqliteError(imageErr)
		} else if !usable {
			return nil, protocol.NewError("cache_stale", "cached image is no longer verified", map[string]any{
				"cached_image_fingerprint": task.ImageFingerprint,
				"outcome_verified":         true,
			})
		}
		if s.Guest != nil {
			if err := s.verifyCachedGuest(ctx, task); err != nil {
				return nil, err
			}
		}
		previousGeneration := task.Generation
		task.PreviousGeneration = &previousGeneration
		task.Generation++
		task.State = statePreparing
		task.ResourcesJSON = mustJSON(resources)
		task.ResourceReservationJSON = mustJSON(resources)
		task.ReservationBytes = 0
		task.LeaseExpiresAt = stringPointer(nowString(deadline))
	}
	if err := s.checkAdmission(ctx, resources, task.TaskID, true); err != nil {
		return nil, err
	}
	reservationBytes, err := s.admissionStorageCost(ctx, task.TaskID, resources, true)
	if err != nil {
		return nil, err
	}
	task.ReservationBytes = reservationBytes
	if created {
		nextGeneration, generationErr := s.Store.NextGeneration(ctx, session)
		if generationErr != nil {
			return nil, s.sqliteError(generationErr)
		}
		task.Generation = nextGeneration
		if err := s.Store.InsertTask(ctx, *task); err != nil {
			return nil, s.sqliteError(err)
		}
	} else if err := s.Store.UpdateTask(ctx, task.TaskID, map[string]any{"generation": task.Generation, "state": task.State, "resources_json": task.ResourcesJSON, "resource_reservation_json": task.ResourceReservationJSON, "reservation_bytes": reservationBytes, "lease_expires_at": task.LeaseExpiresAt, "updated_at": nowString(now), "last_error": nil, "image_fingerprint": task.ImageFingerprint, "previous_generation": task.PreviousGeneration}); err != nil {
		return nil, s.sqliteError(err)
	}
	if operationID := native.WorkerOperationID(ctx); operationID != "" {
		if err := s.Store.UpdateOperationResponse(ctx, operationID, func(response map[string]any) error {
			response["_task_id"] = task.TaskID
			response["_task_generation"] = task.Generation
			return nil
		}); err != nil {
			return nil, s.sqliteError(err)
		}
	}
	if s.Guest != nil {
		nativeCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
		defer cancel()
		if task.VMIP == nil {
			ip, allocationErr := s.allocateGuestIP(nativeCtx, task.InstanceName)
			if allocationErr != nil {
				return s.failTask(ctx, task, "native-network", allocationErr)
			}
			task.VMIP = &ip
			if err := s.Store.UpdateTask(ctx, task.TaskID, map[string]any{"vm_ip": ip, "updated_at": nowString(s.Clock())}); err != nil {
				return nil, s.sqliteError(err)
			}
		}
		if created {
			if err := s.Guest.Init(nativeCtx, task.ImageFingerprint, task.InstanceName, ownershipConfig(task)); err != nil {
				return s.failTaskForOperation(ctx, task, "native-init", err, stringValue(request["operation_id"]))
			}
		}
		if !created {
			if err := s.setOwnership(nativeCtx, task); err != nil {
				return s.failTask(ctx, task, "native-ownership", err)
			}
		}
		if err := s.configureNetwork(nativeCtx, task); err != nil {
			return s.failTask(ctx, task, "native-network", err)
		}
		if err := s.Guest.ConfigSet(nativeCtx, task.InstanceName, "limits.cpu", strconv.FormatInt(resources["cpu"], 10)); err != nil {
			return s.failTask(ctx, task, "native-config", err)
		}
		if err := s.Guest.ConfigSet(nativeCtx, task.InstanceName, "limits.memory", strconv.FormatInt(resources["memory_mib"], 10)+"MiB"); err != nil {
			return s.failTask(ctx, task, "native-config", err)
		}
		if err := s.Guest.DeviceSet(nativeCtx, task.InstanceName, "root", "size", strconv.FormatInt(resources["disk_gib"], 10)+"GiB"); err != nil {
			return s.failTask(ctx, task, "native-config", err)
		}
		if err := s.Guest.Start(nativeCtx, task.InstanceName); err != nil {
			return s.failTask(ctx, task, "native-start", err)
		}
		ip, readinessErr := waitForGuestReady(nativeCtx, s.Guest, task.InstanceName)
		if readinessErr != nil {
			return s.failTask(ctx, task, "guest-readiness", readinessErr)
		}
		if ip != *task.VMIP {
			return s.failTask(ctx, task, "guest-readiness", protocol.NewError("guest_ip_mismatch", "guest IPv4 address does not match the allocated fixed address", map[string]any{"expected": *task.VMIP, "actual": ip}))
		}
		if err := s.Store.UpdateTask(ctx, task.TaskID, map[string]any{"vm_ip": ip, "updated_at": nowString(s.Clock())}); err != nil {
			return nil, s.sqliteError(err)
		}
		if err := s.exposeSSH(ctx, task, request, nativeCtx); err != nil {
			return s.failTask(ctx, task, "access-exposure", err)
		}
	}
	if !deadline.After(s.Clock()) {
		return s.failTask(ctx, task, "lease-expiry", errors.New("lease expired during preparation"))
	}
	commitNow := s.Clock()
	updated, err := s.Store.UpdateTaskIf(ctx, task.TaskID, state.TaskCondition{Generation: &task.Generation, States: []string{statePreparing}, LeaseActiveAfter: &commitNow}, map[string]any{"state": stateActive, "lease_expires_at": nowString(deadline), "inactive_since": nil, "cleanup_after": nil, "updated_at": nowString(commitNow), "resource_reservation_json": "{}", "reservation_bytes": int64(0)})
	if err != nil {
		return nil, s.sqliteError(err)
	}
	if !updated {
		return nil, protocol.NewError("lease_expired", "lease changed before acquire could commit", map[string]any{"outcome_verified": false})
	}
	task.State, task.LeaseExpiresAt, task.ResourceReservationJSON = stateActive, stringPointer(nowString(deadline)), "{}"
	return s.summary(ctx, task)
}

func (s *Service) renew(ctx context.Context, request protocol.Request) (map[string]any, error) {
	task, err := s.task(ctx, request)
	if err != nil {
		return nil, err
	}
	if draining, err := s.isDraining(ctx); err != nil {
		return nil, s.sqliteError(err)
	} else if draining {
		return nil, protocol.NewError("draining", "renew is refused while draining", nil)
	}
	deadline := s.Clock().Add(time.Duration(s.Config.RenewSeconds) * time.Second)
	max := s.Clock().Add(time.Duration(s.Config.MaxLeaseSeconds) * time.Second)
	if deadline.After(max) {
		deadline = max
	}
	now := s.Clock()
	updated, err := s.Store.UpdateTaskIf(ctx, task.TaskID, state.TaskCondition{Generation: &task.Generation, States: []string{stateActive}, LeaseActiveAfter: &now}, map[string]any{"lease_expires_at": nowString(deadline), "updated_at": nowString(now)})
	if err != nil {
		return nil, s.sqliteError(err)
	}
	if !updated {
		return nil, protocol.NewError("lease_expired", "session lease is not effective", map[string]any{"outcome_verified": true})
	}
	task.LeaseExpiresAt = stringPointer(nowString(deadline))
	return s.summary(ctx, task)
}

func (s *Service) release(ctx context.Context, request protocol.Request, destroy bool) (map[string]any, error) {
	return s.withSessionLock(stringValue(request["session_id"]), func() (map[string]any, error) {
		return s.releaseUnlocked(ctx, request, destroy)
	})
}

func (s *Service) releaseUnlocked(ctx context.Context, request protocol.Request, destroy bool) (map[string]any, error) {
	if !isInternalCleanup(ctx) {
		if releaseDrain, err := s.releaseDrainOwned(ctx); err != nil {
			return nil, s.sqliteError(err)
		} else if releaseDrain {
			return nil, protocol.NewError("draining", "release and destroy are refused while a release activation fence is owned", nil)
		}
	}
	task, err := s.task(ctx, request)
	if err != nil {
		return nil, err
	}
	if destroy {
		if err := s.cancelPendingAcquireOperations(ctx, task); err != nil {
			return nil, err
		}
	}
	if task.State == stateAbsent {
		if err := s.cancelPendingAcquireOperations(ctx, task); err != nil {
			return nil, err
		}
		return s.summary(ctx, task)
	}
	if task.State == stateCached && !destroy {
		if err := s.cancelPendingAcquireOperations(ctx, task); err != nil {
			return nil, err
		}
		return s.summary(ctx, task)
	}
	if task.State == stateActive || task.State == statePreparing || task.State == stateFailed || (destroy && (task.State == stateCached || task.State == stateClosing)) {
		targetState := stateClosing
		if destroy {
			targetState = stateDeleting
		}
		updated, updateErr := s.Store.UpdateTaskIf(ctx, task.TaskID, state.TaskCondition{Generation: &task.Generation, States: []string{task.State}}, map[string]any{"state": targetState, "lease_expires_at": nil, "updated_at": nowString(s.Clock())})
		if updateErr != nil {
			return nil, s.sqliteError(updateErr)
		}
		if !updated {
			return nil, protocol.NewError("task_transition", "session changed before cleanup started", map[string]any{"outcome_verified": false})
		}
		task.State = targetState
	} else if task.State != stateClosing && task.State != stateDeleting {
		return nil, protocol.NewError("task_transition", "session is already in a non-retryable lifecycle state", map[string]any{"state": task.State})
	}
	unresolved, workerErr := s.pendingWorkerUnresolved(ctx, optionalString(request["session_id"]), stringValue(request["operation_id"]), native.WorkerRecoveryCoordinator(ctx))
	if workerErr != nil {
		return nil, s.sqliteError(workerErr)
	}
	if unresolved {
		return nil, protocol.NewError("cleanup_incomplete", "native worker ownership remains unresolved before cleanup", map[string]any{"outcome_verified": false})
	}
	cleanupErrors := make([]string, 0)
	nativePresent := s.Guest != nil
	if s.Guest != nil {
		if _, ownershipErr := s.verifyOwnership(ctx, task); ownershipErr != nil {
			control := protocol.AsError(ownershipErr)
			detail, _ := control.Detail.(map[string]any)
			missing, _ := detail["native_missing"].(bool)
			initMissing := !destroy && task.State == stateClosing && missing && failedNativeInit(task)
			if !(missing && (destroy && task.State == stateDeleting || initMissing)) {
				return nil, protocol.NewError("cleanup_incomplete", "native ownership could not be verified before cleanup", map[string]any{"cause": ownershipErr.Error(), "outcome_verified": false})
			}
			unresolved, workerErr := s.pendingWorkerUnresolved(ctx, optionalString(request["session_id"]), stringValue(request["operation_id"]), native.WorkerRecoveryCoordinator(ctx))
			if workerErr != nil {
				return nil, protocol.NewError("cleanup_incomplete", "native worker ownership could not be checked before finalizing a missing instance", map[string]any{"cause": workerErr.Error(), "outcome_verified": false})
			}
			if unresolved {
				return nil, protocol.NewError("cleanup_incomplete", "native worker ownership remains unresolved while the instance is missing", map[string]any{"outcome_verified": false})
			}
			if initMissing {
				if err := s.cancelPendingAcquireOperations(ctx, task); err != nil {
					return nil, err
				}
				updated, updateErr := s.Store.UpdateTaskIf(ctx, task.TaskID, state.TaskCondition{Generation: &task.Generation, States: []string{stateClosing}}, map[string]any{"state": stateDeleting, "updated_at": nowString(s.Clock())})
				if updateErr != nil {
					return nil, s.sqliteError(updateErr)
				}
				if !updated {
					return nil, protocol.NewError("cleanup_incomplete", "session changed before missing init cleanup", map[string]any{"outcome_verified": false})
				}
				task.State = stateDeleting
				destroy = true
			}
			nativePresent = false
		}
	}
	ports, listErr := s.Store.ListPorts(ctx, task.TaskID)
	if listErr != nil {
		return nil, s.sqliteError(listErr)
	}
	for _, port := range ports {
		if nativePresent {
			workerCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
			removeErr := s.removePortDevice(workerCtx, task, port)
			cancel()
			if removeErr != nil {
				cleanupErrors = append(cleanupErrors, port.DeviceName+": "+removeErr.Error())
				break
			}
		}
		if removeErr := s.Store.DeletePort(ctx, task.TaskID, port.PortName, port.Protocol); removeErr != nil {
			cleanupErrors = append(cleanupErrors, port.DeviceName+": "+removeErr.Error())
			break
		}
	}
	if len(cleanupErrors) == 0 && nativePresent {
		workerCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
		defer cancel()
		nativeState, statusErr := s.Guest.Status(workerCtx, task.InstanceName)
		if statusErr != nil {
			return nil, protocol.NewError("cleanup_incomplete", "guest status could not be verified before cleanup", map[string]any{"cause": statusErr.Error(), "outcome_verified": false})
		}
		stateValue, parseErr := native.ParseStatus(nativeState)
		if parseErr != nil {
			return nil, protocol.NewError("cleanup_incomplete", "guest status is malformed before cleanup", map[string]any{"cause": parseErr.Error(), "outcome_verified": false})
		}
		if !strings.EqualFold(stateValue, "stopped") {
			if err := s.Guest.Stop(workerCtx, task.InstanceName); err != nil {
				return nil, protocol.NewError("cleanup_incomplete", "guest stop did not complete", map[string]any{"cause": err.Error(), "outcome_verified": false})
			}
		}
		if !destroy && task.PreviousGeneration != nil {
			info, verifyErr := s.verifyOwnership(workerCtx, task)
			if verifyErr != nil {
				return nil, protocol.NewError("cleanup_incomplete", "native ownership could not be reverified before caching", map[string]any{"cause": verifyErr.Error(), "outcome_verified": false})
			}
			if info["config"].(map[string]any)["user.agent-vm.generation"] != strconv.FormatInt(task.Generation, 10) {
				return nil, protocol.NewError("cleanup_incomplete", "native generation differs from the task before caching", map[string]any{"outcome_verified": false})
			}
		}
		if destroy {
			if err := s.Guest.Delete(workerCtx, task.InstanceName); err != nil {
				return nil, protocol.NewError("cleanup_incomplete", "guest delete did not complete", map[string]any{"cause": err.Error(), "outcome_verified": false})
			}
		}
	}
	if len(cleanupErrors) > 0 {
		return nil, protocol.NewError("cleanup_incomplete", "one or more native mappings remain", map[string]any{"errors": cleanupErrors, "outcome_verified": false})
	}
	stateValue := stateCached
	if destroy {
		stateValue = stateAbsent
	}
	resourcesJSON := mustJSON(zeroResources())
	if !destroy {
		// Cached tasks retain their last requested resources so a same-size
		// resume is not misclassified as a disk growth operation. Cached tasks
		// are excluded from active logical and physical allocation totals.
		resourcesJSON = task.ResourcesJSON
	}
	updates := map[string]any{"state": stateValue, "updated_at": nowString(s.Clock()), "resources_json": resourcesJSON, "resource_reservation_json": "{}", "reservation_bytes": int64(0)}
	if destroy {
		updates["inactive_since"] = nil
		updates["cleanup_after"] = nil
	} else {
		inactive := nowString(s.Clock())
		updates["inactive_since"] = inactive
		updates["cleanup_after"] = nowString(s.Clock().Add(time.Duration(s.Config.CacheSeconds) * time.Second))
	}
	updated, err := s.Store.UpdateTaskIf(ctx, task.TaskID, state.TaskCondition{Generation: &task.Generation, States: []string{task.State}}, updates)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	if !updated {
		return nil, protocol.NewError("cleanup_incomplete", "session changed before cleanup could be committed", map[string]any{"outcome_verified": false})
	}
	if destroy {
		deleted, err := s.Store.DeleteTaskIf(ctx, task.TaskID, state.TaskCondition{Generation: &task.Generation, States: []string{stateAbsent}})
		if err != nil {
			return nil, s.sqliteError(err)
		}
		if !deleted {
			return nil, protocol.NewError("cleanup_incomplete", "session changed before task deletion could be committed", map[string]any{"outcome_verified": false})
		}
	}
	return s.status(ctx, task.SessionID)
}

func failedNativeInit(task *state.Task) bool {
	if task.LastError == nil {
		return false
	}
	var detail map[string]any
	return json.Unmarshal([]byte(*task.LastError), &detail) == nil && detail["phase"] == "native-init"
}

// A recorded binding is authoritative. Legacy unbound operations are only
// matched where the generation relationship is unambiguous.
func acquireOperationMatchesTask(operation state.Operation, task *state.Task) bool {
	if operation.Generation == nil {
		return false
	}
	rawGeneration, hasGeneration := operation.Response["_task_generation"]
	rawTask, hasTask := operation.Response["_task_id"]
	if hasGeneration || hasTask {
		generation, valid := protocol.Integer(rawGeneration)
		return valid && generation == task.Generation && rawTask == task.TaskID
	}
	return *operation.Generation == task.Generation || task.Generation > 0 && *operation.Generation == task.Generation-1
}

func (s *Service) pendingAcquireOperations(ctx context.Context, task *state.Task) ([]state.Operation, error) {
	pending, err := s.Store.PendingOperations(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	result := make([]state.Operation, 0)
	for _, operation := range pending {
		if operation.Action != "acquire" || operation.SessionID == nil || *operation.SessionID != task.SessionID || operation.Generation == nil {
			continue
		}
		if acquireOperationMatchesTask(operation, task) {
			result = append(result, operation)
		}
	}
	return result, nil
}

func (s *Service) cancelPendingAcquireOperations(ctx context.Context, task *state.Task) error {
	pending, err := s.pendingAcquireOperations(ctx, task)
	if err != nil || len(pending) == 0 {
		return err
	}
	for _, operation := range pending {
		if worker, ok := operation.Response["worker"].(map[string]any); ok && workerOwnershipUnresolved(worker) {
			return protocol.NewError("cleanup_incomplete", "pending acquire operation has unresolved native worker ownership", map[string]any{
				"operation_id": operation.OperationID, "outcome_verified": false,
			})
		}
	}
	for _, operation := range pending {
		digest := stringValue(operation.Response["_request_digest"])
		if digest == "" {
			return protocol.NewError("operation_unverified", "pending acquire operation cannot be finalized", map[string]any{"operation_id": operation.OperationID})
		}
		response := map[string]any{
			"outcome": "error",
			"error": protocol.NewError("lease_released", "the lease was released before the acquire operation was acknowledged", map[string]any{
				"task_id": task.TaskID, "task_generation": task.Generation, "outcome_verified": true,
			}),
			"_request_digest": digest,
		}
		if err := s.Store.RecordOperation(ctx, operation.OperationID, operation.Action, operation.SessionID, operation.Generation, response); err != nil {
			return s.sqliteError(err)
		}
	}
	marker := map[string]any{
		"pending_acquire_cancellation":  true,
		"pending_acquire_operation_ids": operationIDs(pending),
		"task_id":                       task.TaskID,
		"task_generation":               task.Generation,
	}
	if task.LastError != nil {
		var previous map[string]any
		if json.Unmarshal([]byte(*task.LastError), &previous) == nil {
			for key, value := range previous {
				if _, exists := marker[key]; !exists {
					marker[key] = value
				}
			}
		}
	}
	if err := s.Store.UpdateTask(ctx, task.TaskID, map[string]any{"last_error": mustJSON(marker), "updated_at": nowString(s.Clock())}); err != nil {
		return s.sqliteError(err)
	}
	return nil
}

func operationIDs(operations []state.Operation) []string {
	ids := make([]string, 0, len(operations))
	for _, operation := range operations {
		ids = append(ids, operation.OperationID)
	}
	return ids
}

func (s *Service) restart(ctx context.Context, request protocol.Request) (map[string]any, error) {
	if draining, err := s.isDraining(ctx); err != nil {
		return nil, s.sqliteError(err)
	} else if draining {
		return nil, protocol.NewError("draining", "restart is refused while draining", nil)
	}
	task, err := s.task(ctx, request)
	if err != nil {
		return nil, err
	}
	if !effective(task, s.Clock()) {
		return nil, protocol.NewError("lease_expired", "session lease is not effective", nil)
	}
	resources := map[string]int64{}
	if err := json.Unmarshal([]byte(task.ResourcesJSON), &resources); err != nil {
		return nil, protocol.NewError("resource_unverified", "task resources cannot be verified before restart", nil)
	}
	if err := s.checkAdmission(ctx, resources, task.TaskID, true); err != nil {
		return nil, err
	}
	if s.Guest != nil {
		workerCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
		defer cancel()
		if err := s.Guest.Stop(workerCtx, task.InstanceName); err != nil {
			return nil, protocol.NewError("restart_partial", "guest stop failed", map[string]any{"phase": "native-stop", "cause": err.Error()})
		}
		if err := s.Guest.Start(workerCtx, task.InstanceName); err != nil {
			return nil, protocol.NewError("restart_partial", "guest start failed", map[string]any{"phase": "native-start", "cause": err.Error()})
		}
	}
	return s.summary(ctx, task)
}

func (s *Service) resize(ctx context.Context, request protocol.Request) (map[string]any, error) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	task, err := s.task(ctx, request)
	if err != nil {
		return nil, err
	}
	draining, err := s.isDraining(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	if draining && (!requestBool(request, "live_memory") || request["memory_mib"] == nil || request["cpu"] != nil || request["disk_gib"] != nil) {
		return nil, protocol.NewError("draining", "only live memory-only resize is allowed while draining", nil)
	}
	if !effective(task, s.Clock()) {
		return nil, protocol.NewError("lease_expired", "session lease is not effective", nil)
	}
	current := map[string]int64{}
	_ = json.Unmarshal([]byte(task.ResourcesJSON), &current)
	target, err := requestedResourcesWithBase(request, current)
	if err != nil {
		return nil, err
	}
	if err := s.checkAdmission(ctx, target, task.TaskID, false); err != nil {
		return nil, err
	}
	reservationBytes, err := s.admissionStorageCost(ctx, task.TaskID, target, false)
	if err != nil {
		return nil, err
	}
	if requestBool(request, "live_memory") && (request["cpu"] != nil || request["disk_gib"] != nil) {
		return nil, protocol.NewError("invalid_request", "live resize requires memory only", nil)
	}
	if requestBool(request, "live_memory") && request["memory_mib"] == nil {
		return nil, protocol.NewError("invalid_request", "live resize requires memory_mib", nil)
	}
	reservationJSON := mustJSON(target)
	reservationNow := s.Clock()
	reserved, err := s.Store.UpdateTaskIf(ctx, task.TaskID, state.TaskCondition{Generation: &task.Generation, States: []string{stateActive}, LeaseActiveAfter: &reservationNow}, map[string]any{"resource_reservation_json": reservationJSON, "reservation_bytes": reservationBytes, "updated_at": nowString(reservationNow)})
	if err != nil {
		return nil, s.sqliteError(err)
	}
	if !reserved {
		return nil, protocol.NewError("lease_expired", "lease or generation changed before resize reservation", map[string]any{"outcome_verified": true})
	}
	task.ResourceReservationJSON = reservationJSON
	if requestBool(request, "allow_restart") && s.Guest != nil {
		if result, restartErr := s.restart(ctx, request); restartErr != nil {
			return nil, restartErr
		} else {
			_ = result
		}
	}
	if s.Guest != nil {
		workerCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
		defer cancel()
		if request["cpu"] != nil {
			if err := s.Guest.ConfigSet(workerCtx, task.InstanceName, "limits.cpu", strconv.FormatInt(target["cpu"], 10)); err != nil {
				return nil, protocol.NewError("resize_partial", "native CPU resize failed", nativePhaseDetail("native-config", err))
			}
		}
		if request["memory_mib"] != nil {
			if err := s.Guest.ConfigSet(workerCtx, task.InstanceName, "limits.memory", strconv.FormatInt(target["memory_mib"], 10)+"MiB"); err != nil {
				return nil, protocol.NewError("resize_partial", "native memory resize failed", nativePhaseDetail("native-config", err))
			}
		}
		if request["disk_gib"] != nil {
			if err := s.Guest.DeviceSet(workerCtx, task.InstanceName, "root", "size", strconv.FormatInt(target["disk_gib"], 10)+"GiB"); err != nil {
				return nil, protocol.NewError("resize_partial", "native disk resize failed", nativePhaseDetail("native-config", err))
			}
		}
	}
	commitNow := s.Clock()
	updated, err := s.Store.UpdateTaskIf(ctx, task.TaskID, state.TaskCondition{Generation: &task.Generation, States: []string{stateActive}, LeaseActiveAfter: &commitNow}, map[string]any{"resources_json": mustJSON(target), "resource_reservation_json": "{}", "reservation_bytes": int64(0), "updated_at": nowString(commitNow)})
	if err != nil {
		return nil, s.sqliteError(err)
	}
	if !updated {
		return nil, protocol.NewError("resize_partial", "lease or generation changed after native resize", map[string]any{"phase": "durable-fence", "outcome_verified": false})
	}
	task.ResourcesJSON = mustJSON(target)
	return map[string]any{"applied": true, "completed": []any{"durable_resources"}, "resources": target, "state": task.State, "generation": task.Generation}, nil
}

func (s *Service) portAdd(ctx context.Context, request protocol.Request) (map[string]any, error) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	task, err := s.task(ctx, request)
	if err != nil {
		return nil, err
	}
	if !effective(task, s.Clock()) {
		return nil, protocol.NewError("task_not_active", "port mappings require an effective lease", nil)
	}
	name := stringValue(request["name"])
	if !protocol.PortNamePattern.MatchString(name) || name == "ssh" {
		return nil, protocol.NewError("invalid_request", "invalid port name", nil)
	}
	protocolName := stringValue(request["port_protocol"])
	if protocolName == "" {
		protocolName = "tcp"
	}
	if protocolName != "tcp" && protocolName != "udp" {
		return nil, protocol.NewError("invalid_request", "unsupported port protocol", nil)
	}
	vmPort := intValue(request["vm_port"])
	if vmPort < 1 || vmPort > 65535 {
		return nil, protocol.NewError("invalid_request", "vm_port must be between 1 and 65535", nil)
	}
	ports, err := s.Store.ListPorts(ctx, task.TaskID)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	requestedHostPort := intValue(request["host_port"])
	for _, existing := range ports {
		if existing.PortName != name || existing.Protocol != protocolName {
			continue
		}
		if existing.State == "active" && existing.Generation == task.Generation && existing.VMPort == vmPort && (requestedHostPort == 0 || requestedHostPort == existing.HostPort) {
			return map[string]any{"task_id": existing.TaskID, "generation": existing.Generation, "port_name": existing.PortName, "protocol": existing.Protocol, "host_port": existing.HostPort, "vm_port": existing.VMPort, "device_name": existing.DeviceName, "state": existing.State, "created_at": existing.CreatedAt}, nil
		}
		return nil, protocol.NewError("operation_conflict", "a port with this name and protocol already exists", map[string]any{"port_name": name, "protocol": protocolName, "state": existing.State, "generation": existing.Generation})
	}
	hostPort := intValue(request["host_port"])
	if hostPort == 0 {
		var allocationErr error
		hostPort, allocationErr = s.nextPort(ctx, protocolName)
		if allocationErr != nil {
			return nil, allocationErr
		}
	} else if hostPort < 1024 || hostPort > 65535 {
		return nil, protocol.NewError("invalid_request", "host_port must be between 1024 and 65535", nil)
	}
	value := state.Port{TaskID: task.TaskID, Generation: task.Generation, PortName: name, Protocol: protocolName, HostPort: hostPort, VMPort: vmPort, DeviceName: "proxy-" + name + "-" + protocolName, State: "preparing", CreatedAt: nowString(s.Clock())}
	if err := s.Store.UpsertPort(ctx, value); err != nil {
		return nil, s.sqliteError(err)
	}
	if s.Guest != nil {
		if task.VMIP == nil {
			return nil, protocol.NewError("mapping_unverified", "guest IPv4 address is unavailable", map[string]any{"outcome_verified": false})
		}
		hostAddress := s.hostAddress()
		workerCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
		err := s.Guest.DeviceAdd(workerCtx, task.InstanceName, value.DeviceName, "proxy", map[string]string{
			"listen":  protocolName + ":" + hostAddress + ":" + strconv.FormatInt(hostPort, 10),
			"connect": protocolName + ":" + *task.VMIP + ":" + strconv.FormatInt(vmPort, 10),
			"nat":     "true",
		})
		cancel()
		if err != nil {
			return nil, protocol.NewError("mapping_unverified", "native proxy creation failed", nativePhaseDetail("port-add", err))
		}
	}
	commitNow := s.Clock()
	active, err := s.Store.UpsertPortIf(ctx, state.Port{TaskID: value.TaskID, Generation: value.Generation, PortName: value.PortName, Protocol: value.Protocol, HostPort: value.HostPort, VMPort: value.VMPort, DeviceName: value.DeviceName, State: "active", CreatedAt: value.CreatedAt}, state.TaskCondition{Generation: &task.Generation, States: []string{stateActive}, LeaseActiveAfter: &commitNow})
	if err != nil {
		return nil, s.sqliteError(err)
	}
	if !active {
		if s.Guest != nil {
			cleanupCtx, cleanupCancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
			cleanupErr := s.Guest.DeviceRemove(cleanupCtx, task.InstanceName, value.DeviceName)
			cleanupCancel()
			if cleanupErr != nil {
				return nil, protocol.NewError("cleanup_incomplete", "native proxy cleanup after lease fence failed", nativePhaseDetail("port-add-cleanup", cleanupErr))
			}
		}
		if deleteErr := s.Store.DeletePort(ctx, value.TaskID, value.PortName, value.Protocol); deleteErr != nil {
			return nil, s.sqliteError(deleteErr)
		}
		return nil, protocol.NewError("lease_expired", "lease or generation changed after native proxy creation", map[string]any{"phase": "durable-fence", "outcome_verified": true})
	}
	value.State = "active"
	return map[string]any{"task_id": value.TaskID, "generation": value.Generation, "port_name": value.PortName, "protocol": value.Protocol, "host_port": value.HostPort, "vm_port": value.VMPort, "device_name": value.DeviceName, "state": value.State, "created_at": value.CreatedAt}, nil
}

func (s *Service) portList(ctx context.Context, request protocol.Request) (map[string]any, error) {
	task, err := s.task(ctx, request)
	if err != nil {
		return nil, err
	}
	ports, err := s.Store.ListPorts(ctx, task.TaskID)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	return map[string]any{"task_id": task.TaskID, "ports": ports}, nil
}

func (s *Service) portRemove(ctx context.Context, request protocol.Request) (map[string]any, error) {
	s.admissionMu.Lock()
	defer s.admissionMu.Unlock()
	task, err := s.task(ctx, request)
	if err != nil {
		return nil, err
	}
	if !effective(task, s.Clock()) {
		return nil, protocol.NewError("task_not_active", "port mappings require an effective lease", nil)
	}
	name := stringValue(request["name"])
	protocolName := stringValue(request["port_protocol"])
	if protocolName == "" {
		protocolName = "tcp"
	}
	if portList, listErr := s.Store.ListPorts(ctx, task.TaskID); listErr != nil {
		return nil, s.sqliteError(listErr)
	} else {
		for _, port := range portList {
			if port.PortName == name && port.Protocol == protocolName && s.Guest != nil {
				workerCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
				err := s.removePortDevice(workerCtx, task, port)
				cancel()
				if err != nil {
					return nil, protocol.NewError("cleanup_incomplete", "native proxy removal failed", map[string]any{"cause": err.Error(), "outcome_verified": false})
				}
			}
		}
	}
	if err := s.Store.DeletePort(ctx, task.TaskID, name, protocolName); err != nil {
		return nil, s.sqliteError(err)
	}
	return map[string]any{"task_id": task.TaskID, "name": name, "protocol": protocolName, "removed": true}, nil
}

func (s *Service) drain(ctx context.Context, value bool, request protocol.Request) (map[string]any, error) {
	draining, _, err := s.Store.Meta(ctx, "draining")
	if err != nil {
		return nil, s.sqliteError(err)
	}
	if value {
		incoming := stringValue(request["session_id"])
		if incoming == "" {
			incoming = "operation:" + stringValue(request["operation_id"])
		}
		owner, found, ownerErr := s.Store.Meta(ctx, "drain_owner")
		if ownerErr != nil {
			return nil, s.sqliteError(ownerErr)
		}
		if draining == "1" {
			switch {
			case found && owner == incoming:
				// Idempotent retry by the current owner.
			case !found || owner == "":
				// A pre-management controller persisted only draining=1. A
				// release transaction may claim that legacy, ownerless fence.
				if !strings.HasPrefix(incoming, "release-") {
					return map[string]any{"draining": true}, nil
				}
				if err := s.Store.SetMeta(ctx, "drain_owner", incoming); err != nil {
					return nil, s.sqliteError(err)
				}
			case strings.HasPrefix(incoming, "release-") && strings.HasPrefix(owner, "operation:"):
				// A release may explicitly take over a browser-entered drain;
				// a browser or a different release may never replace an active
				// release owner.
				if err := s.Store.SetMeta(ctx, "drain_owner", incoming); err != nil {
					return nil, s.sqliteError(err)
				}
			default:
				return nil, protocol.NewError("operation_conflict", "admission drain is owned by another transaction", nil)
			}
		} else if err := s.Store.SetMeta(ctx, "drain_owner", incoming); err != nil {
			return nil, s.sqliteError(err)
		}
	} else {
		owner, found, ownerErr := s.Store.Meta(ctx, "drain_owner")
		if ownerErr != nil {
			return nil, s.sqliteError(ownerErr)
		}
		session := stringValue(request["session_id"])
		// The first management-aware release may inherit a drain entered by
		// the previous protocol-4 controller, which had no owner metadata.
		if session == "" || !strings.HasPrefix(session, "release-") || (found && owner != "" && session != owner) {
			return nil, protocol.NewError("permission_denied", "only the active release transaction may exit admission drain", nil)
		}
	}
	if err := s.Store.SetMeta(ctx, "draining", boolString(value)); err != nil {
		return nil, s.sqliteError(err)
	}
	if !value {
		_ = s.Store.SetMeta(ctx, "drain_owner", "")
	}
	if err := s.Store.SetMeta(ctx, "drain_sealed", "0"); err != nil {
		return nil, s.sqliteError(err)
	}
	return map[string]any{"draining": value}, nil
}

func (s *Service) drainStatus(ctx context.Context) (map[string]any, error) {
	draining, err := s.isDraining(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	capabilities, err := s.Store.Capabilities(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	var stateCapabilities any
	if capabilities != nil {
		values := make([]string, 0, len(capabilities))
		for _, capability := range capabilities {
			values = append(values, capability.Name+"@"+strconv.FormatInt(capability.Version, 10))
		}
		stateCapabilities = values
	}
	return map[string]any{"draining": draining, "ready": s.Ready(), "state_capabilities": stateCapabilities}, nil
}

func (s *Service) adminStatus(ctx context.Context) (map[string]any, error) {
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	items := make([]any, 0, len(tasks))
	for index := range tasks {
		item, summaryErr := s.summary(ctx, &tasks[index])
		if summaryErr != nil {
			return nil, summaryErr
		}
		items = append(items, item)
	}
	return map[string]any{"up": true, "ready": s.Ready(), "tasks": items}, nil
}

func (s *Service) adminSessionList(ctx context.Context) (map[string]any, error) {
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	items := make([]any, 0, len(tasks))
	for _, task := range tasks {
		items = append(items, map[string]any{"session_id": task.SessionID, "task_id": task.TaskID, "generation": task.Generation, "state": task.State})
	}
	return map[string]any{"sessions": items}, nil
}

func (s *Service) adminMetrics(ctx context.Context) (map[string]any, error) {
	for attempt := 1; attempt <= 3; attempt++ {
		start := nowString(s.Clock())
		epoch, err := s.Store.StateEpoch(ctx)
		if err != nil {
			return nil, s.sqliteError(err)
		}
		tasks, err := s.Store.ListTasks(ctx)
		if err != nil {
			return nil, s.sqliteError(err)
		}
		states := map[string]int64{}
		for _, stateName := range []string{stateActive, statePreparing, stateCached, stateClosing, stateDeleting, stateFailed, stateAbsent} {
			states[stateName] = 0
		}
		allocated := map[string]int64{"cpu": 0, "memory_mib": 0, "disk_gib": 0}
		reserved := map[string]int64{"cpu": 0, "memory_mib": 0, "disk_gib": 0}
		allocatedAndReserved := map[string]int64{"cpu": 0, "memory_mib": 0, "disk_gib": 0}
		admissionInstances := int64(0)
		unknownTaskStates := []string{}
		for _, task := range tasks {
			states[task.State]++
			if task.State != stateCached && task.State != stateAbsent {
				admissionInstances++
			}
			if task.State != stateActive && task.State != statePreparing && task.State != stateCached && task.State != stateClosing && task.State != stateDeleting && task.State != stateFailed && task.State != stateAbsent {
				unknownTaskStates = append(unknownTaskStates, task.State)
				continue
			}
			if task.State == stateCached || task.State == stateAbsent {
				continue
			}
			resources, resourceErr := decodeResourceMap(task.ResourcesJSON, "task resources", false)
			if resourceErr != nil {
				return nil, protocol.NewError("resource_unverified", "durable task resources are malformed", map[string]any{"task_id": task.TaskID, "verified": false})
			}
			if task.State == stateActive {
				for _, key := range resourceKeys {
					var sumOK bool
					allocated[key], sumOK = addInt64(allocated[key], resources[key])
					if !sumOK {
						return nil, protocol.NewError("resource_unverified", "durable allocated resources exceed supported range", map[string]any{"task_id": task.TaskID, "resource": key, "verified": false})
					}
				}
			}
			reservation, reservationErr := decodeResourceMap(task.ResourceReservationJSON, "task reservation", true)
			if reservationErr != nil {
				return nil, protocol.NewError("resource_unverified", "durable task reservation is malformed", map[string]any{"task_id": task.TaskID, "verified": false})
			}
			for _, key := range resourceKeys {
				var sumOK bool
				reserved[key], sumOK = addInt64(reserved[key], reservation[key])
				if !sumOK {
					return nil, protocol.NewError("resource_unverified", "durable reserved resources exceed supported range", map[string]any{"task_id": task.TaskID, "resource": key, "verified": false})
				}
				allocatedAndReserved[key], sumOK = addInt64(allocatedAndReserved[key], maxInt64(resources[key], reservation[key]))
				if !sumOK {
					return nil, protocol.NewError("resource_unverified", "durable admission resources exceed supported range", map[string]any{"task_id": task.TaskID, "resource": key, "verified": false})
				}
			}
		}
		endEpoch, err := s.Store.StateEpoch(ctx)
		if err != nil {
			return nil, s.sqliteError(err)
		}
		initialConsistent := epoch == endEpoch
		consistent := initialConsistent
		if initialConsistent || attempt == 3 {
			warnings := []any{}
			pending, pendingErr := s.Store.PendingOperations(ctx)
			if pendingErr != nil {
				return nil, s.sqliteError(pendingErr)
			}
			managementPending, managementPendingErr := s.Store.PendingManagementOperations(ctx)
			if managementPendingErr != nil {
				return nil, s.sqliteError(managementPendingErr)
			}
			auditSummary, auditSummaryErr := s.auditSummary(ctx, epoch)
			if auditSummaryErr != nil {
				return nil, s.sqliteError(auditSummaryErr)
			}
			pendingOperations := make([]any, 0, len(pending))
			for _, operation := range pending {
				item := map[string]any{
					"operation_id": operation.OperationID,
					"action":       operation.Action,
					"session_id":   nil,
					"generation":   nil,
					"created_at":   operation.CreatedAt,
				}
				if operation.SessionID != nil {
					item["session_id"] = *operation.SessionID
				}
				if operation.Generation != nil {
					item["generation"] = *operation.Generation
				}
				for _, key := range []string{"task_id", "task_generation", "acquire_resources"} {
					if value, ok := operation.Response[key]; ok {
						item[key] = value
					}
				}
				pendingOperations = append(pendingOperations, item)
			}
			for _, operation := range managementPending {
				pendingOperations = append(pendingOperations, map[string]any{
					"operation_id": operation.OperationID,
					"action":       operation.Action,
					"target_id":    operation.TargetID,
					"created_at":   operation.CreatedAt,
					"management":   true,
				})
			}
			unknown, unknownErr := s.Store.ReconciledUnknownOperations(ctx)
			if unknownErr != nil {
				return nil, s.sqliteError(unknownErr)
			}
			unknownOperations := make([]any, 0, len(unknown))
			for _, operation := range unknown {
				item := map[string]any{"operation_id": operation.OperationID, "action": operation.Action, "session_id": nil, "generation": nil, "created_at": operation.CreatedAt}
				if operation.SessionID != nil {
					item["session_id"] = *operation.SessionID
				}
				if operation.Generation != nil {
					item["generation"] = *operation.Generation
				}
				unknownOperations = append(unknownOperations, item)
			}
			var cpuObservation *observe.CPUObservation
			var cpuObservationErr error
			if s.CPU != nil {
				value, observeErr := s.CPU.Observe(ctx)
				if observeErr != nil {
					cpuObservationErr = observeErr
				} else {
					cpuObservation = &value
				}
			} else if s.resident {
				cpuObservationErr = errors.New("CPU observer is not configured")
			}
			if cpuObservationErr != nil {
				warnings = append(warnings, map[string]any{"code": "resource_unverified", "message": "CT CPU pressure observation is not verified", "detail": map[string]any{"cause": cpuObservationErr.Error()}})
			}
			nativeUp := false
			var nativeProbeErr error
			if s.Guest == nil {
				nativeProbeErr = errors.New("native guest client is unavailable")
			} else {
				probe, probeOK := s.Guest.(native.HealthProbe)
				if !probeOK {
					nativeProbeErr = errors.New("native guest does not expose a health probe")
				} else {
					probeCtx, probeCancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
					nativeProbeErr = probe.Probe(probeCtx)
					probeCancel()
					if nativeProbeErr == nil {
						nativeUp = true
					}
				}
			}
			if nativeProbeErr != nil {
				nativeUp = false
				warnings = append(warnings, map[string]any{"code": "native_unavailable", "message": "Incus native availability could not be verified", "detail": map[string]any{"cause": nativeProbeErr.Error()}})
			}
			var memoryObservationValue *observe.MemoryObservation
			var memoryObservationErr error
			if s.Memory != nil {
				value, observeErr := s.Memory.Observe(ctx)
				if observeErr != nil {
					memoryObservationErr = observeErr
				} else {
					memoryObservationValue = &value
				}
			} else if s.resident {
				memoryObservationErr = errors.New("memory observer is not configured")
			}
			if memoryObservationErr != nil {
				warnings = append(warnings, map[string]any{"code": "resource_unverified", "message": "CT memory observation is not verified", "detail": map[string]any{"cause": memoryObservationErr.Error()}})
			}
			draining, drainErr := s.isDraining(ctx)
			if drainErr != nil {
				return nil, s.sqliteError(drainErr)
			}
			if initialConsistent && len(pendingOperations) == 0 && states[stateClosing] == 0 && states[stateDeleting] == 0 && states[stateFailed] == 0 {
				if releaseDrain, drainErr := s.releaseDrainOwned(ctx); drainErr != nil {
					return nil, s.sqliteError(drainErr)
				} else if releaseDrain {
					if err := s.Store.SetMeta(ctx, "drain_sealed", "1"); err != nil {
						return nil, s.sqliteError(err)
					}
				}
			}
			limits := map[string]any{}
			remaining := map[string]any{}
			if s.Config.CPUBudget > 0 {
				limit, limitErr := config.LogicalLimit(s.Config.CPUBudget, s.Config.CPUOvercommitRatio)
				if limitErr != nil {
					return nil, protocol.NewError("config_invalid", "CPU admission ratio is invalid", map[string]any{"cause": limitErr.Error()})
				}
				limits["cpu"] = limit
				remaining["cpu"] = limit - allocatedAndReserved["cpu"]
			}
			if s.Config.MemoryBudgetMiB > 0 {
				limit, limitErr := config.LogicalLimit(s.Config.MemoryBudgetMiB, s.Config.MemoryOvercommitRatio)
				if limitErr != nil {
					return nil, protocol.NewError("config_invalid", "memory admission ratio is invalid", map[string]any{"cause": limitErr.Error()})
				}
				limits["memory_mib"] = limit
				remaining["memory_mib"] = limit - allocatedAndReserved["memory_mib"]
			}
			if s.Config.DiskBudgetGiB > 0 {
				limits["disk_gib"] = s.Config.DiskBudgetGiB
				remaining["disk_gib"] = s.Config.DiskBudgetGiB - allocatedAndReserved["disk_gib"]
			}
			if len(unknownTaskStates) > 0 {
				// The known-state totals are incomplete. Keep authoritative limits,
				// but omit derived headroom so bridges cannot turn a partial view into
				// a positive slot estimate.
				remaining = map[string]any{}
			}
			memoryObservation := map[string]any{"limit_bytes": nil, "current_bytes": nil, "available_bytes": nil, "threshold_bytes": nil, "pressure": nil, "oom_events": nil}
			if memoryObservationValue != nil {
				memoryObservation["limit_bytes"] = memoryObservationValue.LimitBytes
				memoryObservation["current_bytes"] = memoryObservationValue.CurrentBytes
				memoryObservation["available_bytes"] = memoryObservationValue.AvailableBytes
				memoryObservation["threshold_bytes"] = memoryObservationValue.ThresholdBytes
				memoryObservation["pressure"] = memoryObservationValue.AvailableBytes < memoryObservationValue.ThresholdBytes
				memoryObservation["oom_events"] = memoryObservationValue.OOMEvents
				if memoryObservationValue.AvailableBytes < memoryObservationValue.ThresholdBytes {
					warnings = append(warnings, map[string]any{"code": "memory_pressure", "message": "CT memory available capacity is below the pressure threshold", "detail": memoryObservationValue.Map()})
				}
			}
			resourcesValue := map[string]any{"allocated": allocated, "reserved": reserved, "allocated_and_reserved": allocatedAndReserved, "limits": limits, "remaining": remaining, "policy": map[string]any{"cpu_overcommit_ratio": s.Config.CPUOvercommitRatio, "memory_overcommit_ratio": s.Config.MemoryOvercommitRatio, "cpu_startup_pressure_full_avg10_percent": s.Config.CPUStartupPressureFullAvg10Percent}, "memory": memoryObservation, "memory_pressure": memoryObservation["pressure"]}
			ctCPU := map[string]any{"capacity_vcpu": nil, "pressure": nil, "startup_pressure": nil}
			defaultReasons := []any{}
			if !s.Ready() {
				defaultReasons = append(defaultReasons, map[string]any{"code": "startup_reconcile_required", "message": "control service is not ready for admission"})
			}
			if draining {
				defaultReasons = append(defaultReasons, map[string]any{"code": "draining", "message": "new leases are refused while draining"})
			}
			if !nativeUp {
				defaultReasons = append(defaultReasons, map[string]any{"code": "native_unavailable", "message": "Incus native availability is not verified"})
			}
			if s.resident && memoryObservationErr != nil {
				defaultReasons = append(defaultReasons, map[string]any{"code": "resource_unverified", "message": "CT memory observation is not verified"})
			}
			defaultRequested := map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}
			logicalExceeded := []string{}
			logicalUnverified := []string{}
			for _, key := range resourceKeys {
				limit, hasLimit := limits[key]
				if !hasLimit {
					continue
				}
				limitValue, ok := limit.(int64)
				if !ok {
					continue
				}
				projected, sumOK := addInt64(allocatedAndReserved[key], defaultRequested[key])
				if !sumOK {
					logicalUnverified = append(logicalUnverified, key)
				} else if projected > limitValue {
					logicalExceeded = append(logicalExceeded, key)
				}
			}
			if len(logicalUnverified) > 0 {
				defaultReasons = append(defaultReasons, map[string]any{"code": "resource_unverified", "message": "default admission resource projection exceeds supported range", "detail": map[string]any{"dimensions": logicalUnverified, "verified": false}})
			}
			if len(logicalExceeded) > 0 {
				defaultReasons = append(defaultReasons, map[string]any{"code": "resource_insufficient", "dimensions": logicalExceeded, "requested": defaultRequested, "remaining": remaining})
			}
			if len(unknownTaskStates) > 0 {
				defaultReasons = append(defaultReasons, map[string]any{"code": "resource_unverified", "message": "task state cannot be verified", "detail": map[string]any{"states": unknownTaskStates}})
			}
			if cpuObservation != nil {
				pressure := cpuObservation.Pressure()
				startupPressure := s.startupPressureDetail(*cpuObservation, allocatedAndReserved["cpu"], 2)
				resourcesValue["ct_capacity"] = map[string]any{"cpu": cpuObservation.Capacity}
				resourcesValue["cpu_pressure"] = pressure
				ctCPU["capacity_vcpu"] = cpuObservation.Capacity
				ctCPU["pressure"] = pressure
				ctCPU["startup_pressure"] = startupPressure
				if startupPressure["verified"] != true {
					defaultReasons = append(defaultReasons, map[string]any{"code": "resource_unverified", "message": "CPU startup resource projection is not verified", "detail": startupPressure})
				} else if startupPressure["startup_blocked"] == true {
					defaultReasons = append(defaultReasons, map[string]any{"code": "resource_pressure", "message": "CPU scheduler pressure blocks VM startup", "detail": startupPressure})
				}
			} else {
				resourcesValue["ct_capacity"] = map[string]any{"cpu": nil}
				resourcesValue["cpu_pressure"] = nil
				if cpuObservationErr != nil {
					defaultReasons = append(defaultReasons, map[string]any{"code": "resource_unverified", "message": "CT CPU pressure observation is not verified"})
				}
			}
			diskAdmission, diskErr := s.diskAdmission(ctx, "")
			var reclaimRequired any
			if diskErr != nil {
				control := protocol.AsError(diskErr)
				diskAdmission = map[string]any{"verified": false, "error": map[string]any{"code": control.Code, "message": control.Message, "detail": control.Detail}}
				if s.requiresPhysicalDiskAdmission() {
					defaultReasons = append(defaultReasons, map[string]any{"code": control.Code, "message": control.Message, "detail": control.Detail})
				}
			} else {
				diskReady, diskReadyErr := s.diskAdmissionSufficient(ctx, diskAdmission, s.Config.CreateCostBytes, "")
				if diskReadyErr != nil {
					control := protocol.AsError(diskReadyErr)
					if control.Code == "resource_insufficient" {
						reclaimRequired = true
						if s.requiresPhysicalDiskAdmission() {
							defaultReasons = append(defaultReasons, map[string]any{"code": "disk_reclaim_required", "message": "default creation needs capacity reclamation", "detail": control.Detail})
						}
					} else if s.requiresPhysicalDiskAdmission() {
						defaultReasons = append(defaultReasons, map[string]any{"code": control.Code, "message": control.Message, "detail": control.Detail})
					}
				} else {
					reclaimRequired = !diskReady
					if !diskReady {
						defaultReasons = append(defaultReasons, map[string]any{"code": "disk_reclaim_required", "message": "default creation needs capacity reclamation"})
					}
				}
			}
			endEpoch, err = s.Store.StateEpoch(ctx)
			if err != nil {
				return nil, s.sqliteError(err)
			}
			consistent = epoch == endEpoch
			if !consistent && attempt < 3 {
				continue
			}
			if !consistent {
				warnings = append(warnings, map[string]any{"code": "observation_inconsistent", "message": "durable state changed while collecting metrics"})
				defaultReasons = append(defaultReasons, map[string]any{"code": "observation_inconsistent", "message": "durable state changed while collecting metrics"})
			}
			instanceLimit := any(nil)
			instanceRemaining := any(nil)
			if s.Config.InstanceLimit > 0 {
				instanceLimit = s.Config.InstanceLimit
				instanceRemaining = maxInt64(0, s.Config.InstanceLimit-admissionInstances)
				if admissionInstances >= s.Config.InstanceLimit {
					defaultReasons = append(defaultReasons, map[string]any{"code": "resource_insufficient", "dimensions": []string{"instances"}, "requested": 1, "remaining": instanceRemaining})
				}
			}
			instances := map[string]any{"verified": len(unknownTaskStates) == 0, "source": "durable_state", "native_verified": false, "total": len(tasks), "limits": map[string]any{}, "admission": map[string]any{"allocated": admissionInstances - states[statePreparing], "reserved": states[statePreparing], "allocated_and_reserved": admissionInstances, "remaining": instanceRemaining, "limit": instanceLimit, "capacity": nil}}
			defaultReady := consistent && len(defaultReasons) == 0
			admissionReady := s.Ready() && nativeUp && defaultReady
			doctorIssues := []any{}
			if !s.Ready() {
				doctorIssues = append(doctorIssues, map[string]any{"code": "startup_reconcile_required", "message": "control service is not ready"})
			}
			if !consistent {
				doctorIssues = append(doctorIssues, map[string]any{"code": "observation_inconsistent", "message": "durable state changed while collecting metrics"})
			}
			if !nativeUp {
				doctorIssues = append(doctorIssues, map[string]any{"code": "native_unavailable", "message": "Incus native availability is not verified"})
			}
			if s.resident && memoryObservationErr != nil {
				doctorIssues = append(doctorIssues, map[string]any{"code": "resource_unverified", "message": "CT memory observation is not verified"})
			}
			if diskErr != nil && s.requiresPhysicalDiskAdmission() {
				control := protocol.AsError(diskErr)
				doctorIssues = append(doctorIssues, map[string]any{"code": control.Code, "message": control.Message, "detail": control.Detail})
			}
			capacityObservation := any(nil)
			if diskErr == nil {
				physical := diskAdmission["physical_admission"].(map[string]any)
				capacityObservation = map[string]any{"verified": true, "total_bytes": physical["capacity_bytes"], "used_bytes": physical["allocated_bytes"], "free_bytes": physical["free_bytes"], "low_watermark_bytes": physical["low_watermark_bytes"], "target_watermark_bytes": physical["target_watermark_bytes"]}
			}
			warningCounts := make(map[string]int64, len(auditSummary.Warnings))
			for code, count := range auditSummary.Warnings {
				warningCounts[code] = count
			}
			// warning_counts is the durable counter source for the *_total
			// Prometheus family. Instantaneous observation warnings remain in
			// warnings and must not change a counter on every scrape.
			operationMetrics := map[string]any{"pending": len(pendingOperations), "historical_unknown": len(unknownOperations)}
			for key, count := range auditSummary.Operations {
				operationMetrics[key] = count
			}
			return map[string]any{
				"observation_epoch": endEpoch, "observation_consistent": consistent, "observation_attempts": attempt,
				"observed_at": start, "up": nativeUp, "draining": draining, "admission_ready": admissionReady,
				"resources": resourcesValue, "capacity": capacityObservation, "ct_cpu": ctCPU,
				"reclaim_required": reclaimRequired, "disk_admission": diskAdmission,
				"default_admission": map[string]any{"requested": defaultRequested, "request_cost_bytes": s.Config.CreateCostBytes, "ready": defaultReady, "reasons": defaultReasons, "reclaim_required": reclaimRequired},
				"ct_memory":         map[string]any{"capacity_bytes": memoryObservation["limit_bytes"], "current_bytes": memoryObservation["current_bytes"], "available_bytes": memoryObservation["available_bytes"], "pressure_threshold_bytes": memoryObservation["threshold_bytes"], "pressure": memoryObservation["pressure"], "oom_events": memoryObservation["oom_events"]},
				"task_states":       states, "pending_operations": pendingOperations, "historical_unknown_operations": unknownOperations, "instances": instances, "operations": operationMetrics, "image_unpack_failures": auditSummary.ImageUnpackFailures, "doctor": map[string]any{"ready": len(doctorIssues) == 0, "issues": doctorIssues}, "warnings": warnings,
				"warning_counts": warningCounts,
			}, nil
		}
	}
	return nil, protocol.NewError("observation_unavailable", "metrics observation did not converge", nil)
}

func (s *Service) reconcile(ctx context.Context, apply bool, _ ...string) (map[string]any, error) {
	if apply {
		pending, pendingErr := s.Store.PendingOperations(ctx)
		if pendingErr != nil {
			return nil, s.sqliteError(pendingErr)
		}
		if err := s.reconcilePendingWorkers(ctx, pending); err != nil {
			return nil, err
		}
		if err := s.reconcilePendingAbsentDestroy(ctx, pending); err != nil {
			return nil, err
		}
		if err := s.recoverPending(ctx, pending); err != nil {
			return nil, err
		}
	}
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	actions := make([]any, 0)
	var firstErr error
	for _, task := range tasks {
		if (task.State == stateActive || task.State == statePreparing) && !effective(&task, s.Clock()) {
			action := map[string]any{"task_id": task.TaskID, "generation": task.Generation, "action": "close-incomplete-or-expired"}
			actions = append(actions, action)
			if apply {
				request := protocol.Request{"protocol": protocol.Version, "action": "timer-expire", "task_id": task.TaskID, "generation": task.Generation}
				if _, expireErr := s.expire(ctx, request); expireErr != nil && firstErr == nil {
					firstErr = expireErr
				}
			}
		}
		if task.State == stateClosing && apply {
			if cleanupErr := s.reconcileClosing(ctx, &task); cleanupErr != nil && firstErr == nil {
				firstErr = cleanupErr
			}
		}
		if task.State == stateFailed && apply {
			cleanupRequest, cleanupOperationID, requestErr := s.pendingCleanupRequest(ctx, task.SessionID, task.Generation, "release")
			if requestErr != nil {
				if firstErr == nil {
					firstErr = requestErr
				}
				continue
			}
			cleanupCtx := ctx
			if cleanupOperationID != "" {
				cleanupCtx = native.WithWorkerRecorder(cleanupCtx, cleanupOperationID, s)
			}
			if result, releaseErr := s.release(withInternalCleanup(cleanupCtx), cleanupRequest, false); releaseErr != nil {
				if firstErr == nil {
					firstErr = releaseErr
				}
			} else if finalizeErr := s.finalizePendingCleanup(ctx, task.SessionID, task.Generation, "release", result); finalizeErr != nil && firstErr == nil {
				firstErr = finalizeErr
			}
		}
		if task.State == stateDeleting && apply {
			cleanupRequest, cleanupOperationID, requestErr := s.pendingCleanupRequest(ctx, task.SessionID, task.Generation, "destroy")
			if requestErr != nil {
				if firstErr == nil {
					firstErr = requestErr
				}
				continue
			}
			cleanupCtx := ctx
			if cleanupOperationID != "" {
				cleanupCtx = native.WithWorkerRecorder(cleanupCtx, cleanupOperationID, s)
			}
			if result, releaseErr := s.release(withInternalCleanup(cleanupCtx), cleanupRequest, true); releaseErr != nil {
				if firstErr == nil {
					firstErr = releaseErr
				}
			} else if finalizeErr := s.finalizePendingCleanup(ctx, task.SessionID, task.Generation, "destroy", result); finalizeErr != nil && firstErr == nil {
				firstErr = finalizeErr
			}
		}
	}
	result := map[string]any{"dry_run": !apply, "actions": actions}
	return result, firstErr
}

// A pending destroy remains authoritative even if earlier recovery left the task closing.
func (s *Service) reconcileClosing(ctx context.Context, task *state.Task) error {
	unlock := s.lockAction("session", protocol.Request{"session_id": task.SessionID})
	defer unlock()
	pending, err := s.Store.PendingOperations(ctx)
	if err != nil {
		return s.sqliteError(err)
	}
	request, operationID, err := s.pendingCleanupRequest(ctx, task.SessionID, task.Generation, "release")
	if err != nil {
		return err
	}
	if operationID != "" && s.operationActive(operationID) {
		return protocol.NewError("operation_unverified", "pending release is active", nil)
	}
	action := "release"
	for _, op := range pending {
		if op.Action != "destroy" || op.SessionID == nil || *op.SessionID != task.SessionID || op.Generation == nil || *op.Generation != task.Generation {
			continue
		}
		if action == "destroy" || s.operationActive(op.OperationID) {
			return protocol.NewError("operation_unverified", "pending destroy is active or ambiguous", nil)
		}
		worker, ok := op.Response["worker"].(map[string]any)
		if !ok || (stringValue(worker["state"]) != "finished" && stringValue(worker["state"]) != "exited") || workerOwnershipUnresolved(worker) {
			return protocol.NewError("operation_unverified", "pending destroy worker is unresolved", nil)
		}
		saved, ok := op.Response["_request"].(map[string]any)
		if !ok {
			return protocol.NewError("operation_unverified", "pending destroy request is missing", nil)
		}
		request = protocol.Request(saved)
		digest, digestErr := protocol.Digest(request)
		if protocol.ValidateRequest(request) != nil || digestErr != nil || digest != stringValue(op.Response["_request_digest"]) || stringValue(request["action"]) != "destroy" || stringValue(request["session_id"]) != task.SessionID || intValue(request["generation"]) != task.Generation || stringValue(request["operation_id"]) != op.OperationID {
			return protocol.NewError("operation_unverified", "pending destroy identity is inconsistent", nil)
		}
		if _, compatibilityErr := protocol.ValidateCompatibility(request, s.Release); compatibilityErr != nil {
			return protocol.NewError("operation_unverified", "pending destroy request is incompatible", map[string]any{"cause": compatibilityErr.Error()})
		}
		action = "destroy"
		operationID = op.OperationID
	}
	if operationID != "" {
		ctx = native.WithWorkerRecorder(ctx, operationID, s)
	}
	result, err := s.releaseUnlocked(withInternalCleanup(ctx), request, action == "destroy")
	if err != nil {
		return err
	}
	return s.finalizePendingCleanup(ctx, task.SessionID, task.Generation, action, result)
}

// reconcilePendingAbsentDestroy closes the crash window after native destroy
// and durable task transition but before the terminal operation journal write.
// The task row, request identity, digest, worker ownership and native absence
// must all be verified before the pending destroy is finalized.
func (s *Service) reconcilePendingAbsentDestroy(ctx context.Context, pending []state.Operation) error {
	for _, operation := range pending {
		if operation.Action != "destroy" || operation.SessionID == nil || operation.Generation == nil {
			continue
		}
		unlock, acquired := s.trySessionLock(*operation.SessionID)
		if !acquired {
			continue
		}
		err := s.reconcilePendingAbsentDestroyOne(ctx, operation)
		unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) reconcilePendingAbsentDestroyOne(ctx context.Context, operation state.Operation) error {
	current, err := s.Store.GetOperation(ctx, operation.OperationID)
	if err != nil {
		return s.sqliteError(err)
	}
	if current == nil || stringValue(current.Response["outcome"]) != "pending" {
		return nil
	}
	if current.Action != "destroy" || current.SessionID == nil || current.Generation == nil || *current.SessionID != *operation.SessionID || *current.Generation != *operation.Generation {
		return protocol.NewError("startup_reconcile_required", "pending absent destroy identity changed during reconciliation", map[string]any{"operation_id": operation.OperationID, "admission_ready": false})
	}
	operation = *current
	task, err := s.Store.GetTask(ctx, map[string]any{"session_id": *operation.SessionID})
	if err != nil {
		return s.sqliteError(err)
	}
	rowExists := task != nil
	if task != nil && task.State != stateAbsent {
		return nil
	}
	if task != nil && task.Generation != *operation.Generation {
		return protocol.NewError("startup_reconcile_required", "pending absent destroy generation differs from task", map[string]any{"operation_id": operation.OperationID, "admission_ready": false})
	}
	if task == nil {
		generation, exists, generationErr := s.Store.LastGeneration(ctx, *operation.SessionID)
		if generationErr != nil {
			return s.sqliteError(generationErr)
		}
		if !exists || generation != *operation.Generation {
			return protocol.NewError("startup_reconcile_required", "pending absent destroy generation is unverified", map[string]any{"operation_id": operation.OperationID, "admission_ready": false})
		}
	}
	if s.operationActive(operation.OperationID) {
		return protocol.NewError("startup_reconcile_required", "pending absent destroy is still active", map[string]any{"operation_id": operation.OperationID, "admission_ready": false})
	}
	worker, ok := operation.Response["worker"].(map[string]any)
	if !ok || (stringValue(worker["state"]) != "finished" && stringValue(worker["state"]) != "exited") || workerOwnershipUnresolved(worker) {
		return protocol.NewError("startup_reconcile_required", "pending absent destroy worker is unverified", map[string]any{"operation_id": operation.OperationID, "admission_ready": false})
	}
	if verified, verifyErr := terminalWorkerVerified(worker); verifyErr != nil || !verified {
		return protocol.NewError("startup_reconcile_required", "pending absent destroy worker identity is unverified", map[string]any{"operation_id": operation.OperationID, "admission_ready": false})
	}
	rawRequest, ok := operation.Response["_request"].(map[string]any)
	if !ok {
		return protocol.NewError("startup_reconcile_required", "pending absent destroy request is missing", map[string]any{"operation_id": operation.OperationID, "admission_ready": false})
	}
	request := protocol.Request(rawRequest)
	if validationErr := protocol.ValidateRequest(request); validationErr != nil {
		return protocol.NewError("startup_reconcile_required", "pending absent destroy request is invalid", map[string]any{"operation_id": operation.OperationID, "admission_ready": false, "cause": validationErr.Error()})
	}
	if _, compatibilityErr := protocol.ValidateCompatibility(request, s.Release); compatibilityErr != nil {
		return protocol.NewError("startup_reconcile_required", "pending absent destroy request is incompatible", map[string]any{"operation_id": operation.OperationID, "admission_ready": false, "cause": compatibilityErr.Error()})
	}
	if !operationIdentityMatches(&operation, request) || stringValue(request["operation_id"]) != operation.OperationID || intValue(request["generation"]) != *operation.Generation {
		return protocol.NewError("startup_reconcile_required", "pending absent destroy identity does not match its journal or task", map[string]any{"operation_id": operation.OperationID, "admission_ready": false})
	}
	digest := stringValue(operation.Response["_request_digest"])
	calculated, digestErr := protocol.Digest(request)
	if digest == "" || digestErr != nil || digest != calculated {
		return protocol.NewError("startup_reconcile_required", "pending absent destroy request digest does not match", map[string]any{"operation_id": operation.OperationID, "admission_ready": false})
	}
	if unresolved, workerErr := s.pendingWorkerUnresolved(ctx, operation.SessionID, "", ""); workerErr != nil {
		return protocol.NewError("startup_reconcile_required", "pending absent destroy worker ownership could not be checked", map[string]any{"operation_id": operation.OperationID, "admission_ready": false, "cause": workerErr.Error()})
	} else if unresolved {
		return protocol.NewError("startup_reconcile_required", "pending absent destroy worker ownership remains unresolved", map[string]any{"operation_id": operation.OperationID, "admission_ready": false})
	}
	otherPending, err := s.Store.PendingOperations(ctx)
	if err != nil {
		return s.sqliteError(err)
	}
	for _, other := range otherPending {
		if other.OperationID == operation.OperationID || other.SessionID == nil || *other.SessionID != *operation.SessionID || !operationHasNativeEffect(other.Action) {
			continue
		}
		otherWorker, ok := other.Response["worker"].(map[string]any)
		if !ok || s.operationActive(other.OperationID) || workerOwnershipUnresolved(otherWorker) || (stringValue(otherWorker["state"]) != "finished" && stringValue(otherWorker["state"]) != "exited") {
			return protocol.NewError("startup_reconcile_required", "same-session worker is unverified", map[string]any{"operation_id": other.OperationID, "admission_ready": false})
		}
		if verified, verifyErr := terminalWorkerVerified(otherWorker); verifyErr != nil || !verified {
			return protocol.NewError("startup_reconcile_required", "same-session worker identity is unverified", map[string]any{"operation_id": other.OperationID, "admission_ready": false})
		}
	}
	if !rowExists {
		task = &state.Task{TaskID: taskID(*operation.SessionID), SessionID: *operation.SessionID, Generation: *operation.Generation, State: stateAbsent, InstanceName: "agent-vm-" + taskID(*operation.SessionID), ResourcesJSON: mustJSON(zeroResources()), ResourceReservationJSON: "{}"}
	}
	if s.Guest == nil {
		return protocol.NewError("startup_reconcile_required", "pending absent destroy requires native verification", map[string]any{"operation_id": operation.OperationID, "admission_ready": false, "outcome_verified": false})
	}
	if _, ownershipErr := s.verifyOwnership(ctx, task); ownershipErr == nil {
		return protocol.NewError("startup_reconcile_required", "pending absent destroy still has a native instance", map[string]any{"operation_id": operation.OperationID, "admission_ready": false, "outcome_verified": true})
	} else {
		control := protocol.AsError(ownershipErr)
		detail, _ := control.Detail.(map[string]any)
		if detail["native_missing"] != true {
			return protocol.NewError("startup_reconcile_required", "pending absent destroy native absence could not be verified", map[string]any{"operation_id": operation.OperationID, "admission_ready": false, "outcome_verified": false, "cause": ownershipErr.Error()})
		}
	}
	response := map[string]any{"state": stateAbsent, "session_id": *operation.SessionID, "effective_lease": false}
	if rowExists {
		var summaryErr error
		response, summaryErr = s.summary(ctx, task)
		if summaryErr != nil {
			return summaryErr
		}
	}
	terminal := protocol.SuccessWithRelease(request, response, s.Release, nil)
	terminal["_request_digest"] = digest
	if rowExists {
		if err := s.deleteDestroyedTask(ctx, request, operation.Generation); err != nil {
			return err
		}
	}
	if err := s.Store.RecordOperation(ctx, operation.OperationID, operation.Action, operation.SessionID, operation.Generation, terminal); err != nil {
		return s.sqliteError(err)
	}
	return nil
}

// terminalWorkerVerified accepts a durable pre-spawn result or a process
// identity whose root and process group are both confirmed absent.
func terminalWorkerVerified(worker map[string]any) (bool, error) {
	pid, pidOK := protocol.Integer(worker["pid"])
	group, groupOK := protocol.Integer(worker["process_group"])
	start, startOK := worker["start_time"].(string)
	_, attempted := worker["start_attempted"].(bool)
	_, hasPID := worker["pid"]
	_, hasGroup := worker["process_group"]
	_, hasStart := worker["start_time"]
	if !hasPID && !hasGroup && !hasStart && stringValue(worker["state"]) == "exited" && attempted {
		return true, nil
	}
	if !pidOK || !groupOK || !startOK || pid <= 0 || group <= 0 || start == "" {
		return false, nil
	}
	alive, err := native.WorkerAlive(native.WorkerIdentity{PID: int(pid), ProcessGroup: int(group), StartTime: start})
	return !alive && err == nil, err
}

func (s *Service) pendingCleanupRequest(ctx context.Context, session string, generation int64, action string) (protocol.Request, string, error) {
	pending, err := s.Store.PendingOperations(ctx)
	if err != nil {
		return nil, "", s.sqliteError(err)
	}
	var matched *state.Operation
	for index := range pending {
		operation := &pending[index]
		if operation.Action != action || operation.SessionID == nil || *operation.SessionID != session || operation.Generation == nil || *operation.Generation != generation {
			continue
		}
		rawRequest, ok := operation.Response["_request"].(map[string]any)
		if !ok {
			return nil, "", protocol.NewError("operation_unverified", "pending cleanup request is missing", map[string]any{"operation_id": operation.OperationID})
		}
		request := protocol.Request(rawRequest)
		if validationErr := protocol.ValidateRequest(request); validationErr != nil {
			return nil, "", protocol.NewError("operation_unverified", "pending cleanup request is invalid", map[string]any{"operation_id": operation.OperationID, "cause": validationErr.Error()})
		}
		if _, compatibilityErr := protocol.ValidateCompatibility(request, s.Release); compatibilityErr != nil {
			return nil, "", protocol.NewError("operation_unverified", "pending cleanup request is incompatible", map[string]any{"operation_id": operation.OperationID, "cause": compatibilityErr.Error()})
		}
		generationValue, generationOK := protocol.Integer(request["generation"])
		if !generationOK || stringValue(request["operation_id"]) != operation.OperationID || stringValue(request["action"]) != action || stringValue(request["session_id"]) != session || generationValue != generation || !operationIdentityMatches(operation, request) {
			return nil, "", protocol.NewError("operation_unverified", "pending cleanup request identity does not match its journal", map[string]any{"operation_id": operation.OperationID})
		}
		digest := stringValue(operation.Response["_request_digest"])
		calculated, digestErr := protocol.Digest(request)
		if digest == "" || digestErr != nil || digest != calculated {
			return nil, "", protocol.NewError("operation_unverified", "pending cleanup request digest does not match", map[string]any{"operation_id": operation.OperationID})
		}
		if workerValue, exists := operation.Response["worker"]; exists {
			worker, workerOK := workerValue.(map[string]any)
			if !workerOK || workerOwnershipUnresolved(worker) {
				return nil, "", protocol.NewError("operation_unverified", "pending cleanup worker evidence is not terminal or verifiable", map[string]any{"operation_id": operation.OperationID})
			}
		}
		if matched != nil {
			return nil, "", protocol.NewError("operation_unverified", "multiple pending cleanup journals match the same lifecycle generation", map[string]any{"session_id": session, "generation": generation, "action": action})
		}
		matched = operation
	}
	if matched == nil {
		return protocol.Request{"protocol": protocol.Version, "action": action, "session_id": session, "generation": generation}, "", nil
	}
	return protocol.Request(matched.Response["_request"].(map[string]any)), matched.OperationID, nil
}

func (s *Service) deleteDestroyedTask(ctx context.Context, request protocol.Request, expectedGeneration *int64) error {
	task, err := s.Store.GetTask(ctx, map[string]any{"session_id": stringValue(request["session_id"])})
	if err != nil {
		return s.sqliteError(err)
	}
	if task == nil || task.State != stateAbsent {
		return nil
	}
	if expectedGeneration != nil && task.Generation != *expectedGeneration {
		return nil
	}
	ports, err := s.Store.ListPorts(ctx, task.TaskID)
	if err != nil {
		return s.sqliteError(err)
	}
	if len(ports) != 0 {
		return protocol.NewError("cleanup_incomplete", "session retains durable port mappings", map[string]any{"outcome_verified": false})
	}
	conditionGeneration := task.Generation
	if expectedGeneration != nil {
		conditionGeneration = *expectedGeneration
	}
	deleted, err := s.Store.DeleteTaskIf(ctx, task.TaskID, state.TaskCondition{Generation: &conditionGeneration, States: []string{stateAbsent}})
	if err != nil {
		return s.sqliteError(err)
	}
	if !deleted {
		return protocol.NewError("cleanup_incomplete", "session changed before terminal destroy deletion could be committed", map[string]any{"outcome_verified": false})
	}
	return nil
}

func (s *Service) finalizePendingCleanup(ctx context.Context, session string, generation int64, action string, result map[string]any) error {
	pending, err := s.Store.PendingOperations(ctx)
	if err != nil {
		return s.sqliteError(err)
	}
	for _, operation := range pending {
		if operation.Action != action || operation.SessionID == nil || *operation.SessionID != session || operation.Generation == nil || *operation.Generation != generation {
			continue
		}
		rawRequest, ok := operation.Response["_request"].(map[string]any)
		if !ok {
			return protocol.NewError("operation_unverified", "pending cleanup request is missing", map[string]any{"operation_id": operation.OperationID})
		}
		request := protocol.Request(rawRequest)
		response := protocol.SuccessWithRelease(request, result, s.Release, nil)
		if digest, ok := operation.Response["_request_digest"]; ok {
			response["_request_digest"] = digest
		}
		if err := s.Store.RecordOperation(ctx, operation.OperationID, operation.Action, operation.SessionID, operation.Generation, response); err != nil {
			return s.sqliteError(err)
		}
	}
	return nil
}

func (s *Service) gc(ctx context.Context, apply bool) (map[string]any, error) {
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	candidates := make([]any, 0)
	var firstErr error
	for _, task := range tasks {
		if task.State != stateCached || task.CleanupAfter == nil {
			continue
		}
		deadline, parseErr := time.Parse(time.RFC3339Nano, *task.CleanupAfter)
		if parseErr != nil || deadline.After(s.Clock()) {
			continue
		}
		candidates = append(candidates, map[string]any{"task_id": task.TaskID, "instance_name": task.InstanceName, "generation": task.Generation, "inactive_since": task.InactiveSince})
		if apply {
			gcNow := s.Clock()
			updated, updateErr := s.Store.UpdateTaskIf(ctx, task.TaskID, state.TaskCondition{Generation: &task.Generation, States: []string{stateCached}, LeaseExpiredAt: &gcNow}, map[string]any{"state": stateDeleting, "updated_at": nowString(gcNow)})
			if updateErr != nil {
				if firstErr == nil {
					firstErr = s.sqliteError(updateErr)
				}
			} else if updated {
				cleanupRequest := protocol.Request{"session_id": task.SessionID, "generation": task.Generation}
				if _, releaseErr := s.release(withInternalCleanup(ctx), cleanupRequest, true); releaseErr != nil {
					if firstErr == nil {
						firstErr = releaseErr
					}
				} else if deleteErr := s.deleteDestroyedTask(ctx, cleanupRequest, &task.Generation); deleteErr != nil && firstErr == nil {
					firstErr = deleteErr
				}
			}
		}
	}
	return map[string]any{"dry_run": !apply, "selection": "age", "minimum_idle_seconds": s.Config.CacheSeconds, "candidates": candidates}, firstErr
}

func (s *Service) expire(ctx context.Context, request protocol.Request) (map[string]any, error) {
	taskID := stringValue(request["task_id"])
	task, err := s.Store.GetTask(ctx, map[string]any{"task_id": taskID})
	if err != nil {
		return nil, s.sqliteError(err)
	}
	if task == nil {
		return map[string]any{"expired": false}, nil
	}
	generation := intValue(request["generation"])
	if generation != task.Generation {
		return map[string]any{"expired": false, "stale": true}, nil
	}
	now := s.Clock()
	marked, err := s.Store.UpdateTaskIf(ctx, task.TaskID, state.TaskCondition{Generation: &generation, States: []string{stateActive, statePreparing}, LeaseExpiredAt: &now}, map[string]any{"state": stateClosing, "lease_expires_at": nil, "updated_at": nowString(now)})
	if err != nil {
		return nil, s.sqliteError(err)
	}
	if !marked {
		return map[string]any{"expired": false}, nil
	}
	return s.release(withInternalCleanup(ctx), protocol.Request{"session_id": task.SessionID, "generation": task.Generation}, false)
}

func (s *Service) doctor(ctx context.Context) (map[string]any, error) {
	epoch, err := s.Store.StateEpoch(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	draining, err := s.isDraining(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	return map[string]any{"up": true, "ready": s.Ready(), "issues": []any{}, "draining": draining, "state_epoch": epoch, "transport": "http-over-uds"}, nil
}

func (s *Service) sshInfo(ctx context.Context, request protocol.Request) (map[string]any, error) {
	task, err := s.task(ctx, request)
	if err != nil {
		return nil, err
	}
	if task.SSHHostPort == nil {
		return nil, protocol.NewError("mapping_unverified", "managed SSH mapping is not active", nil)
	}
	result := map[string]any{"task_id": task.TaskID, "generation": task.Generation, "host": s.hostAddress(), "port": *task.SSHHostPort, "user": "root", "host_key_alias": "agent-vm-" + task.TaskID + "-g" + strconv.FormatInt(task.Generation, 10)}
	if s.Guest == nil {
		return nil, protocol.NewError("guest_host_key_missing", "guest host key cannot be verified without a native guest", nil)
	}
	workerCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
	defer cancel()
	hostKey, err := s.Guest.Exec(workerCtx, task.InstanceName, []string{"sh", "-lc", "cat /etc/ssh/ssh_host_ed25519_key.pub"}, nil)
	if err != nil {
		return nil, protocol.NewError("guest_host_key_missing", "guest did not provide an ed25519 host key", map[string]any{"cause": err.Error()})
	}
	key, err := parseSSHKey(hostKey.Stdout)
	if err != nil {
		return nil, protocol.NewError("guest_host_key_missing", "guest host key is not a valid ed25519 public key", nil)
	}
	result["host_key"] = key
	return result, nil
}

func (s *Service) task(ctx context.Context, request protocol.Request) (*state.Task, error) {
	session := stringValue(request["session_id"])
	task, err := s.Store.GetTask(ctx, map[string]any{"session_id": session})
	if err != nil {
		return nil, s.sqliteError(err)
	}
	if task == nil {
		return &state.Task{SessionID: session, State: stateAbsent}, nil
	}
	if request["generation"] != nil && intValue(request["generation"]) != task.Generation {
		return nil, protocol.NewError("generation_mismatch", "task generation does not match request", map[string]any{"expected": task.Generation, "actual": intValue(request["generation"])})
	}
	if expectedVersion := stringValue(request["management_target_version"]); expectedVersion != "" {
		currentVersion := "vm:" + task.TaskID + ":" + task.UpdatedAt
		if expectedVersion != currentVersion {
			return nil, protocol.NewError("stale_target", "VM resource version changed", map[string]any{"expected": currentVersion, "actual": expectedVersion})
		}
	}
	return task, nil
}

func (s *Service) summary(ctx context.Context, task *state.Task) (map[string]any, error) {
	if task == nil || task.TaskID == "" {
		session := ""
		if task != nil {
			session = task.SessionID
		}
		return map[string]any{"state": stateAbsent, "session_id": session, "effective_lease": false, "native_status": stateAbsent, "guest_available": false}, nil
	}
	var resources any
	var reservation any
	resourceState := any(nil)
	if _, resourceErr := decodeResourceMap(task.ResourcesJSON, "task resources", false); resourceErr != nil {
		resourceState = map[string]any{"verified": false, "error": map[string]any{"code": "resource_unverified", "message": "task resource state cannot be verified"}}
		resources = nil
		reservation = nil
	} else if _, reservationErr := decodeResourceMap(task.ResourceReservationJSON, "task reservation", true); reservationErr != nil {
		resourceState = map[string]any{"verified": false, "error": map[string]any{"code": "resource_unverified", "message": "task resource reservation cannot be verified"}}
		resources = nil
		reservation = nil
	} else {
		resourceMap := map[string]any{}
		reservationMap := map[string]any{}
		if err := json.Unmarshal([]byte(task.ResourcesJSON), &resourceMap); err != nil {
			resources = nil
			resourceState = map[string]any{"verified": false, "error": map[string]any{"code": "resource_unverified", "message": "task resource state cannot be decoded"}}
		} else {
			resources = resourceMap
		}
		if err := json.Unmarshal([]byte(task.ResourceReservationJSON), &reservationMap); err != nil {
			reservation = nil
			resourceState = map[string]any{"verified": false, "error": map[string]any{"code": "resource_unverified", "message": "task resource reservation cannot be decoded"}}
		} else {
			reservation = reservationMap
		}
	}
	ports, err := s.Store.ListPorts(ctx, task.TaskID)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	result := map[string]any{"task_id": task.TaskID, "session_id": task.SessionID, "generation": task.Generation, "state": task.State, "instance_name": task.InstanceName, "vm_ip": task.VMIP, "ssh_host_port": task.SSHHostPort, "lease_expires_at": task.LeaseExpiresAt, "inactive_since": task.InactiveSince, "cleanup_after": task.CleanupAfter, "resources": resources, "resource_reservation": reservation, "reservation_bytes": task.ReservationBytes, "ports": ports, "last_error": task.LastError, "effective_lease": effective(task, s.Clock()), "native_status": "unverified", "guest_available": nil}
	if resourceState != nil {
		result["resource_state"] = resourceState
	}
	if s.Guest == nil {
		return result, nil
	}
	if _, ownershipErr := s.verifyOwnership(ctx, task); ownershipErr != nil {
		result["guest_error"] = protocol.AsError(ownershipErr)
		return result, nil
	}
	status, statusErr := s.Guest.Status(ctx, task.InstanceName)
	if statusErr != nil {
		result["guest_error"] = map[string]any{"code": "native_unavailable", "message": "native guest status could not be verified"}
		return result, nil
	}
	nativeStatus, parseErr := native.ParseStatus(status)
	if parseErr != nil {
		result["guest_error"] = map[string]any{"code": "state_unverified", "message": "native guest status is malformed"}
		return result, nil
	}
	result["native_status"] = nativeStatus
	guestAvailable := result["effective_lease"] == true && strings.EqualFold(nativeStatus, "running")
	result["guest_available"] = guestAvailable
	if result["effective_lease"] == true && !guestAvailable {
		result["guest_error"] = map[string]any{"code": "guest_unavailable", "message": "effective lease has no running guest; inspect before explicit restart"}
	}
	return result, nil
}

func (s *Service) isDraining(ctx context.Context) (bool, error) {
	value, exists, err := s.Store.Meta(ctx, "draining")
	return exists && value == "1", err
}

func (s *Service) releaseDrainOwned(ctx context.Context) (bool, error) {
	draining, err := s.isDraining(ctx)
	if err != nil || !draining {
		return false, err
	}
	owner, found, err := s.Store.Meta(ctx, "drain_owner")
	return found && strings.HasPrefix(owner, "release-"), err
}

func (s *Service) releaseDrainSealed(ctx context.Context) (bool, error) {
	value, found, err := s.Store.Meta(ctx, "drain_sealed")
	return found && value == "1", err
}

// IsDraining exposes the durable admission fence to the browser-independent
// management authority so new high-risk operations cannot enter during a
// control-plane publication drain.
func (s *Service) IsDraining(ctx context.Context) (bool, error) {
	return s.isDraining(ctx)
}

func ownershipConfig(task *state.Task) map[string]string {
	return map[string]string{
		"user.agent-vm.managed":    "native-v1",
		"user.agent-vm.task":       task.TaskID,
		"user.agent-vm.session":    task.SessionID,
		"user.agent-vm.generation": strconv.FormatInt(task.Generation, 10),
	}
}

func (s *Service) setOwnership(ctx context.Context, task *state.Task) error {
	for key, value := range ownershipConfig(task) {
		if err := s.Guest.ConfigSet(ctx, task.InstanceName, key, value); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) allocateGuestIP(ctx context.Context, ownName string) (string, error) {
	start := net.ParseIP(s.Config.VMIPStart).To4()
	end := net.ParseIP(s.Config.VMIPEnd).To4()
	if start == nil || end == nil {
		return "", protocol.NewError("config_invalid", "managed guest IPv4 range is unavailable", nil)
	}
	startValue := uint32(start[0])<<24 | uint32(start[1])<<16 | uint32(start[2])<<8 | uint32(start[3])
	endValue := uint32(end[0])<<24 | uint32(end[1])<<16 | uint32(end[2])<<8 | uint32(end[3])
	if startValue > endValue {
		return "", protocol.NewError("config_invalid", "managed guest IPv4 range is invalid", nil)
	}
	used := make(map[string]struct{})
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return "", s.sqliteError(err)
	}
	for _, task := range tasks {
		if task.InstanceName == ownName || task.State == stateAbsent || task.VMIP == nil {
			continue
		}
		used[*task.VMIP] = struct{}{}
	}
	if inventory, ok := s.Guest.(native.NetworkInventory); ok {
		addresses, inventoryErr := inventory.NetworkIPs(ctx)
		if inventoryErr != nil {
			return "", protocol.NewError("vm_ip_unverified", "native network address inventory is unavailable", map[string]any{"cause": inventoryErr.Error(), "outcome_verified": false})
		}
		for address := range addresses {
			used[address] = struct{}{}
		}
	}
	for value := startValue; ; value++ {
		candidate := net.IPv4(byte(value>>24), byte(value>>16), byte(value>>8), byte(value)).String()
		if _, occupied := used[candidate]; !occupied {
			return candidate, nil
		}
		if value == endValue {
			break
		}
	}
	return "", protocol.NewError("vm_ip_exhausted", "no managed-container private IP is available", nil)
}

func (s *Service) configureNetwork(ctx context.Context, task *state.Task) error {
	if task == nil || task.VMIP == nil {
		return protocol.NewError("mapping_unverified", "managed guest IPv4 address is unavailable", map[string]any{"outcome_verified": false})
	}
	properties := []struct{ key, value string }{
		{"ipv4.address", *task.VMIP},
		{"security.mac_filtering", "true"},
		{"security.ipv4_filtering", "true"},
		{"security.ipv6_filtering", "true"},
		{"security.port_isolation", "true"},
		{"security.acls", s.Config.NetworkACL},
		{"security.acls.default.ingress.action", "allow"},
		{"security.acls.default.egress.action", "allow"},
	}
	for _, property := range properties {
		if err := s.Guest.DeviceSet(ctx, task.InstanceName, "eth0", property.key, property.value); err != nil {
			return err
		}
	}
	info, err := s.Guest.Config(ctx, task.InstanceName)
	if err != nil {
		return protocol.NewError("network_unverified", "managed guest network configuration could not be verified", map[string]any{"cause": err.Error(), "outcome_verified": false})
	}
	expanded, ok := info["expanded_devices"].(map[string]any)
	if !ok {
		return protocol.NewError("network_unverified", "managed guest network devices are unavailable", map[string]any{"outcome_verified": false})
	}
	eth0, ok := expanded["eth0"].(map[string]any)
	if !ok {
		return protocol.NewError("network_unverified", "managed guest eth0 device is unavailable", map[string]any{"outcome_verified": false})
	}
	expected := map[string]string{
		"type": "nic", "network": s.Config.Network, "ipv4.address": *task.VMIP,
		"security.mac_filtering": "true", "security.ipv4_filtering": "true",
		"security.ipv6_filtering": "true", "security.port_isolation": "true",
		"security.acls":                        s.Config.NetworkACL,
		"security.acls.default.ingress.action": "allow", "security.acls.default.egress.action": "allow",
	}
	for key, expectedValue := range expected {
		if eth0[key] != expectedValue {
			return protocol.NewError("network_unverified", "managed guest network isolation differs from configuration", map[string]any{"field": key, "expected": expectedValue, "actual": eth0[key], "outcome_verified": true})
		}
	}
	return nil
}

func (s *Service) verifyOwnership(ctx context.Context, task *state.Task) (map[string]any, error) {
	info, err := s.Guest.Config(ctx, task.InstanceName)
	if err != nil {
		detail := map[string]any{"cause": err.Error(), "outcome_verified": false}
		if native.IsNotFound(err, task.InstanceName, s.Config.Project) {
			detail["native_missing"] = true
		}
		return nil, protocol.NewError("ownership_mismatch", "native instance configuration could not be read", detail)
	}
	config, configOK := info["config"].(map[string]any)
	devices, devicesOK := info["devices"].(map[string]any)
	expanded, expandedOK := info["expanded_devices"].(map[string]any)
	if !configOK || !devicesOK || !expandedOK {
		return nil, protocol.NewError("ownership_mismatch", "native instance ownership fields are incomplete", map[string]any{"outcome_verified": false})
	}
	for key, expected := range map[string]string{
		"user.agent-vm.managed": "native-v1",
		"user.agent-vm.task":    task.TaskID,
		"user.agent-vm.session": task.SessionID,
	} {
		if config[key] != expected {
			return nil, protocol.NewError("ownership_mismatch", "native instance identity differs from durable ownership", map[string]any{"field": key, "outcome_verified": true})
		}
	}
	generations := map[string]bool{strconv.FormatInt(task.Generation, 10): true}
	if task.PreviousGeneration != nil && (task.State == statePreparing || task.State == stateClosing) {
		generations[strconv.FormatInt(*task.PreviousGeneration, 10)] = true
	}
	if generation, ok := config["user.agent-vm.generation"].(string); !ok || !generations[generation] {
		return nil, protocol.NewError("ownership_mismatch", "native instance generation differs from durable ownership", map[string]any{"outcome_verified": true})
	}
	return map[string]any{"config": config, "devices": devices, "expanded_devices": expanded}, nil
}

func (s *Service) verifyCachedGuest(ctx context.Context, task *state.Task) error {
	info, err := s.verifyOwnership(ctx, task)
	if err != nil {
		return err
	}
	status, err := s.Guest.Status(ctx, task.InstanceName)
	if err != nil {
		return protocol.NewError("cache_ineligible", "cached guest status could not be verified", map[string]any{"cause": err.Error()})
	}
	stateValue, parseErr := native.ParseStatus(status)
	if parseErr != nil || !strings.EqualFold(stateValue, "stopped") {
		return protocol.NewError("cache_ineligible", "cached guest is not stopped", map[string]any{"status": stateValue})
	}
	ports, err := s.Store.ListPorts(ctx, task.TaskID)
	if err != nil {
		return s.sqliteError(err)
	}
	if len(ports) > 0 {
		return protocol.NewError("cache_ineligible", "cached task still has managed port mappings", nil)
	}
	if expanded, ok := info["expanded_devices"].(map[string]any); ok {
		for _, value := range expanded {
			if device, ok := value.(map[string]any); ok && device["type"] == "proxy" {
				return protocol.NewError("cache_ineligible", "cached guest still has proxy mappings", nil)
			}
		}
	}
	return nil
}

func (s *Service) removePortDevice(ctx context.Context, task *state.Task, port state.Port) error {
	info, err := s.verifyOwnership(ctx, task)
	if err != nil {
		return err
	}
	devices, _ := info["devices"].(map[string]any)
	expanded, _ := info["expanded_devices"].(map[string]any)
	nativeDevice, exists := devices[port.DeviceName]
	if !exists {
		if _, expandedExists := expanded[port.DeviceName]; expandedExists {
			return protocol.NewError("ownership_mismatch", "native mapping is inherited and is not owned by the task", nil)
		}
		return nil
	}
	device, ok := nativeDevice.(map[string]any)
	if !ok || !s.proxyMatches(task, port, device) {
		return protocol.NewError("ownership_mismatch", "native mapping differs from its reservation", nil)
	}
	if err := s.Guest.DeviceRemove(ctx, task.InstanceName, port.DeviceName); err != nil {
		return err
	}
	after, err := s.verifyOwnership(ctx, task)
	if err != nil {
		return err
	}
	if expanded, ok := after["expanded_devices"].(map[string]any); ok {
		if _, stillPresent := expanded[port.DeviceName]; stillPresent {
			return protocol.NewError("mapping_unverified", "native mapping is still present", nil)
		}
	}
	return nil
}

func (s *Service) proxyMatches(task *state.Task, port state.Port, device map[string]any) bool {
	if task.VMIP == nil || device["type"] != "proxy" {
		return false
	}
	prefix := port.Protocol + ":"
	return device["listen"] == prefix+s.hostAddress()+":"+strconv.FormatInt(port.HostPort, 10) && device["connect"] == prefix+*task.VMIP+":"+strconv.FormatInt(port.VMPort, 10) && device["nat"] == "true"
}

func (s *Service) hostAddress() string {
	if s.Config.HostAddress == "" {
		return "0.0.0.0"
	}
	return s.Config.HostAddress
}

func parseSSHKey(raw []byte) (string, error) {
	text := string(raw)
	text = strings.TrimSuffix(text, "\n")
	text = strings.TrimSuffix(text, "\r")
	if strings.ContainsAny(text, "\r\n") {
		return "", errors.New("invalid ssh key")
	}
	line := strings.TrimSpace(text)
	fields := strings.Fields(line)
	if len(fields) < 2 || fields[0] != "ssh-ed25519" {
		return "", errors.New("invalid ssh key")
	}
	decoded, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(decoded) == 0 {
		return "", errors.New("invalid ssh key")
	}
	return fields[0] + " " + fields[1], nil
}

func (s *Service) nextPort(ctx context.Context, protocolName string) (int64, error) {
	if s.Config.PortStart == 0 || s.Config.PortEnd == 0 {
		return 0, protocol.NewError("resource_unverified", "managed port range is not configured", nil)
	}
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return 0, s.sqliteError(err)
	}
	occupied := make(map[int64]struct{})
	for _, task := range tasks {
		ports, listErr := s.Store.ListPorts(ctx, task.TaskID)
		if listErr != nil {
			return 0, s.sqliteError(listErr)
		}
		for _, port := range ports {
			if port.Protocol == protocolName && port.State != "deleted" {
				occupied[port.HostPort] = struct{}{}
			}
		}
	}
	for candidate := s.Config.PortStart; candidate <= s.Config.PortEnd; candidate++ {
		if _, exists := occupied[candidate]; !exists {
			return candidate, nil
		}
	}
	return 0, protocol.NewError("resource_insufficient", "managed port range is exhausted", map[string]any{"protocol": protocolName})
}

func (s *Service) exposeSSH(ctx context.Context, task *state.Task, request protocol.Request, workerCtx context.Context) error {
	if task.VMIP == nil {
		return errors.New("guest IPv4 address is unavailable")
	}
	hostPort := intValue(request["ssh_host_port"])
	if hostPort == 0 {
		var err error
		hostPort, err = s.nextPort(ctx, "tcp")
		if err != nil {
			return err
		}
	}
	if hostPort < 1024 || hostPort > 65535 {
		return errors.New("ssh_host_port must be between 1024 and 65535")
	}
	port := state.Port{TaskID: task.TaskID, Generation: task.Generation, PortName: "ssh", Protocol: "tcp", HostPort: hostPort, VMPort: 22, DeviceName: "proxy-ssh-tcp", State: "preparing", CreatedAt: nowString(s.Clock())}
	if err := s.Store.UpsertPort(ctx, port); err != nil {
		return err
	}
	host := s.Config.HostAddress
	host = s.hostAddress()
	if err := s.Guest.DeviceAdd(workerCtx, task.InstanceName, port.DeviceName, "proxy", map[string]string{
		"listen":  "tcp:" + host + ":" + strconv.FormatInt(hostPort, 10),
		"connect": "tcp:" + *task.VMIP + ":22",
		"nat":     "true",
	}); err != nil {
		return err
	}
	publicKey, ok := request["public_key"].(string)
	if !ok || publicKey == "" {
		return protocol.NewError("invalid_request", "public_key is required", nil)
	}
	if _, err := parseSSHKey([]byte(publicKey)); err != nil {
		return protocol.NewError("invalid_request", "public_key is not a supported SSH public key", nil)
	}
	if _, err := s.Guest.Exec(workerCtx, task.InstanceName, []string{"sh", "-c", "install -d -m 700 /root/.ssh && cat > /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys"}, []byte(publicKey+"\n")); err != nil {
		return err
	}
	if info, err := s.verifyOwnership(ctx, task); err != nil {
		return err
	} else if devices, ok := info["devices"].(map[string]any); !ok {
		return protocol.NewError("mapping_unverified", "native SSH mapping was not confirmed", nil)
	} else if device, ok := devices[port.DeviceName].(map[string]any); !ok || !s.proxyMatches(task, port, device) {
		return protocol.NewError("mapping_unverified", "native SSH mapping was not confirmed", nil)
	}
	if err := s.Store.UpsertPort(ctx, state.Port{TaskID: port.TaskID, Generation: port.Generation, PortName: port.PortName, Protocol: port.Protocol, HostPort: port.HostPort, VMPort: port.VMPort, DeviceName: port.DeviceName, State: "active", CreatedAt: port.CreatedAt}); err != nil {
		return err
	}
	task.SSHHostPort = &hostPort
	return s.Store.UpdateTask(ctx, task.TaskID, map[string]any{"ssh_host_port": hostPort, "updated_at": nowString(s.Clock())})
}

func guestIPv4(value map[string]any) string {
	network, ok := value["network"].(map[string]any)
	if !ok {
		return ""
	}
	for _, rawInterface := range network {
		iface, ok := rawInterface.(map[string]any)
		if !ok {
			continue
		}
		addresses, ok := iface["addresses"].([]any)
		if !ok {
			continue
		}
		for _, rawAddress := range addresses {
			address, ok := rawAddress.(map[string]any)
			if !ok || address["family"] != "inet" {
				continue
			}
			if value, ok := address["address"].(string); ok && value != "" && value != "127.0.0.1" {
				return value
			}
		}
	}
	return ""
}

const guestReadinessPollInterval = 250 * time.Millisecond

func waitForGuestReady(ctx context.Context, guest native.Guest, name string) (string, error) {
	if guest == nil {
		return "", errors.New("guest client is unavailable")
	}
	ticker := time.NewTicker(guestReadinessPollInterval)
	defer ticker.Stop()
	for {
		status, err := guest.Status(ctx, name)
		if err != nil {
			return "", err
		}
		value, parseErr := native.ParseStatus(status)
		if parseErr != nil {
			return "", parseErr
		}
		if !strings.EqualFold(value, "running") {
			return "", fmt.Errorf("guest status is %s", value)
		}
		if ip := guestIPv4(status); ip != "" {
			return ip, nil
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("guest did not report an IPv4 address: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

const bytesPerGiB int64 = 1024 * 1024 * 1024

func maxInt64(left, right int64) int64 {
	if left >= right {
		return left
	}
	return right
}

const maxInt64Value int64 = 1<<63 - 1

func addInt64(left, right int64) (int64, bool) {
	if left < 0 || right < 0 || left > maxInt64Value-right {
		return 0, false
	}
	return left + right, true
}

func multiplyGiB(value int64) (int64, bool) {
	if value < 0 || value > maxInt64Value/bytesPerGiB {
		return 0, false
	}
	return value * bytesPerGiB, true
}

func percentageOf(value, percentage int64) int64 {
	whole, remainder := value/100, value%100
	return whole*percentage + remainder*percentage/100
}

func (s *Service) requiresPhysicalDiskAdmission() bool {
	return s.resident || s.Config.StoragePath != ""
}

func (s *Service) diskAdmission(ctx context.Context, excludeTaskID string) (map[string]any, error) {
	return s.diskAdmissionWithExternalReservation(ctx, excludeTaskID, s.externalStorageReserved())
}

func (s *Service) diskAdmissionWithExternalReservation(ctx context.Context, excludeTaskID string, externalReserved int64) (map[string]any, error) {
	if s.Guest == nil {
		return nil, protocol.NewError("resource_unverified", "Incus storage pool capacity is unavailable", map[string]any{"verified": false})
	}
	probe, ok := s.Guest.(native.StoragePoolProbe)
	if !ok {
		return nil, protocol.NewError("resource_unverified", "Incus storage pool capacity is unavailable", map[string]any{"verified": false})
	}
	capacity, err := probe.StoragePoolCapacity(ctx)
	if err != nil {
		return nil, protocol.NewError("resource_unverified", "Incus storage pool capacity could not be verified", map[string]any{"verified": false, "cause": err.Error()})
	}
	if capacity.TotalBytes <= 0 || capacity.FreeBytes < 0 || capacity.FreeBytes > capacity.TotalBytes {
		return nil, protocol.NewError("resource_unverified", "Incus storage pool capacity is invalid", map[string]any{"verified": false})
	}
	if externalReserved < 0 {
		return nil, protocol.NewError("resource_unverified", "external physical storage reservation is malformed", map[string]any{"verified": false})
	}
	low := maxInt64(s.Config.DiskLowBytes, percentageOf(capacity.TotalBytes, 15))
	target := maxInt64(s.Config.DiskTargetBytes, percentageOf(capacity.TotalBytes, 25))
	if target >= capacity.TotalBytes || low >= target {
		return nil, protocol.NewError("watermarks_unreachable", "configure reachable CT storage watermarks", map[string]any{"total_bytes": capacity.TotalBytes, "low_bytes": low, "target_bytes": target})
	}
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	var durableReserved int64
	var guestAllocated, guestReserved, guestAllocatedAndReserved int64
	for _, task := range tasks {
		if task.TaskID == excludeTaskID {
			continue
		}
		if task.ReservationBytes < 0 {
			return nil, protocol.NewError("resource_unverified", "durable disk reservation is malformed", map[string]any{"task_id": task.TaskID, "verified": false})
		}
		if task.ReservationBytes > 0 && durableReserved > int64(^uint64(0)>>1)-task.ReservationBytes {
			return nil, protocol.NewError("resource_unverified", "durable disk reservation exceeds supported range", map[string]any{"verified": false})
		}
		durableReserved, _ = addInt64(durableReserved, task.ReservationBytes)
		if task.State == stateAbsent || task.State == stateCached {
			continue
		}
		resources := map[string]int64{}
		if err := json.Unmarshal([]byte(task.ResourcesJSON), &resources); err != nil {
			return nil, protocol.NewError("resource_unverified", "task disk allocation is malformed", map[string]any{"task_id": task.TaskID, "verified": false})
		}
		diskGiB, diskOK := resources["disk_gib"]
		if !diskOK {
			return nil, protocol.NewError("resource_unverified", "task disk allocation is incomplete", map[string]any{"task_id": task.TaskID, "verified": false})
		}
		allocation, allocationOK := multiplyGiB(diskGiB)
		if !allocationOK {
			return nil, protocol.NewError("resource_unverified", "task disk allocation exceeds supported range", map[string]any{"task_id": task.TaskID, "verified": false})
		}
		reservation := map[string]int64{}
		if task.ResourceReservationJSON != "" && task.ResourceReservationJSON != "{}" {
			if err := json.Unmarshal([]byte(task.ResourceReservationJSON), &reservation); err != nil {
				return nil, protocol.NewError("resource_unverified", "task disk reservation is malformed", map[string]any{"task_id": task.TaskID, "verified": false})
			}
		}
		reservationGiB, reservationFieldOK := reservation["disk_gib"]
		if task.ResourceReservationJSON != "" && task.ResourceReservationJSON != "{}" && !reservationFieldOK {
			return nil, protocol.NewError("resource_unverified", "task disk reservation is incomplete", map[string]any{"task_id": task.TaskID, "verified": false})
		}
		reserved, reservationOK := multiplyGiB(reservationGiB)
		if !reservationOK {
			return nil, protocol.NewError("resource_unverified", "task disk reservation exceeds supported range", map[string]any{"task_id": task.TaskID, "verified": false})
		}
		var sumOK bool
		if guestAllocated, sumOK = addInt64(guestAllocated, allocation); !sumOK {
			return nil, protocol.NewError("resource_unverified", "guest disk allocation exceeds supported range", map[string]any{"verified": false})
		}
		if guestReserved, sumOK = addInt64(guestReserved, reserved); !sumOK {
			return nil, protocol.NewError("resource_unverified", "guest disk reservation exceeds supported range", map[string]any{"verified": false})
		}
		if guestAllocatedAndReserved, sumOK = addInt64(guestAllocatedAndReserved, maxInt64(allocation, reserved)); !sumOK {
			return nil, protocol.NewError("resource_unverified", "guest disk admission exceeds supported range", map[string]any{"verified": false})
		}
	}
	used := capacity.TotalBytes - capacity.FreeBytes
	reserved, sumOK := addInt64(durableReserved, externalReserved)
	if !sumOK {
		return nil, protocol.NewError("resource_unverified", "physical disk reservations exceed supported range", map[string]any{"verified": false})
	}
	freeAfterLow := int64(0)
	if capacity.FreeBytes > low {
		freeAfterLow = capacity.FreeBytes - low
		if reserved < freeAfterLow {
			freeAfterLow -= reserved
		} else {
			freeAfterLow = 0
		}
	}
	allocatedAndReserved, sumOK := addInt64(used, reserved)
	if !sumOK {
		return nil, protocol.NewError("resource_unverified", "physical disk admission exceeds supported range", map[string]any{"verified": false})
	}
	return map[string]any{
		"verified":         true,
		"guest_allocation": map[string]any{"allocated_bytes": guestAllocated, "reserved_bytes": guestReserved, "allocated_and_reserved_bytes": guestAllocatedAndReserved},
		"physical_admission": map[string]any{
			"allocated_bytes": used, "reserved_bytes": reserved, "allocated_and_reserved_bytes": allocatedAndReserved,
			"remaining_bytes": freeAfterLow, "limit_bytes": capacity.TotalBytes - low, "capacity_bytes": capacity.TotalBytes,
			"free_bytes": capacity.FreeBytes, "low_watermark_bytes": low, "target_watermark_bytes": target,
			"durable_reserved_bytes": durableReserved, "external_reserved_bytes": externalReserved,
		},
	}, nil
}

func (s *Service) diskAdmissionSufficient(ctx context.Context, admission map[string]any, requestCost int64, excludeTaskID string) (bool, error) {
	return s.diskAdmissionSufficientWithFloor(ctx, admission, requestCost, excludeTaskID, 0)
}

func (s *Service) diskAdmissionSufficientWithFloor(_ context.Context, admission map[string]any, requestCost int64, excludeTaskID string, minimumFreeBytes int64) (bool, error) {
	if admission == nil {
		return true, nil
	}
	physical, ok := admission["physical_admission"].(map[string]any)
	if !ok {
		return false, protocol.NewError("resource_unverified", "physical disk admission is unavailable", map[string]any{"verified": false})
	}
	free, freeOK := physical["free_bytes"].(int64)
	low, lowOK := physical["low_watermark_bytes"].(int64)
	reserved, reservedOK := physical["reserved_bytes"].(int64)
	if !reservedOK {
		reserved, reservedOK = physical["durable_reserved_bytes"].(int64)
	}
	if !freeOK || !lowOK || !reservedOK || requestCost < 0 || minimumFreeBytes < 0 {
		return false, protocol.NewError("resource_unverified", "physical disk admission observation is malformed", map[string]any{"verified": false})
	}
	threshold := low
	if minimumFreeBytes > threshold {
		threshold = minimumFreeBytes
	}
	if excludeTaskID != "" {
		// diskAdmission already excluded this task when its reservation was
		// calculated; retain the argument to make the exclusion explicit at the
		// call boundary and prevent future callers from silently dropping it.
		_ = excludeTaskID
	}
	if reserved > free || requestCost > free-reserved {
		return false, protocol.NewError("resource_insufficient", "physical disk admission is below the configured free-space floor", map[string]any{"request_cost_bytes": requestCost, "free_bytes": free, "reserved_bytes": reserved, "low_watermark_bytes": low, "required_free_bytes": threshold})
	}
	sufficient := free-reserved-requestCost >= threshold
	if !sufficient {
		return false, protocol.NewError("resource_insufficient", "physical disk admission is below the configured free-space floor", map[string]any{"request_cost_bytes": requestCost, "free_bytes": free, "reserved_bytes": reserved, "low_watermark_bytes": low, "required_free_bytes": threshold})
	}
	return true, nil
}

func (s *Service) reclaimCachedCapacity(ctx context.Context, requestCost int64, excludeTaskID string, minimumFreeBytes int64) (bool, error) {
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return false, s.sqliteError(err)
	}
	for _, task := range tasks {
		if task.TaskID == excludeTaskID || task.State != stateCached {
			continue
		}
		// The caller holds admissionMu, while acquire holds a session lock before
		// waiting for admissionMu. Never wait for another session lock here: a
		// concurrent acquire for this cached session could otherwise deadlock the
		// admission mutex. A busy cached session is skipped and can be reclaimed by
		// a later admission attempt after its owner releases the lock.
		unlockSession, locked := s.trySessionLock(task.SessionID)
		if !locked {
			continue
		}
		current, currentErr := s.Store.GetTask(ctx, map[string]any{"session_id": task.SessionID})
		if currentErr != nil {
			unlockSession()
			return false, s.sqliteError(currentErr)
		}
		if current == nil || current.TaskID != task.TaskID || current.Generation != task.Generation || current.State != stateCached {
			unlockSession()
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
		cacheErr := s.verifyCachedGuest(checkCtx, current)
		cancel()
		if cacheErr != nil {
			unlockSession()
			control := protocol.AsError(cacheErr)
			if control.Code == "cache_ineligible" {
				continue
			}
			return false, cacheErr
		}
		cleanupRequest := protocol.Request{"session_id": current.SessionID, "generation": current.Generation}
		_, releaseErr := s.releaseUnlocked(withInternalCleanup(ctx), cleanupRequest, true)
		unlockSession()
		if releaseErr != nil {
			return false, releaseErr
		}
		admission, admissionErr := s.diskAdmission(ctx, excludeTaskID)
		if admissionErr != nil {
			return false, admissionErr
		}
		sufficient, sufficientErr := s.diskAdmissionSufficientWithFloor(ctx, admission, requestCost, excludeTaskID, minimumFreeBytes)
		if sufficientErr != nil {
			if protocol.AsError(sufficientErr).Code != "resource_insufficient" {
				return false, sufficientErr
			}
			continue
		}
		if sufficient {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) admissionStorageCost(ctx context.Context, excludeTaskID string, requested map[string]int64, startup bool) (int64, error) {
	if !startup {
		current, err := s.Store.GetTask(ctx, map[string]any{"task_id": excludeTaskID})
		if err != nil {
			return 0, s.sqliteError(err)
		}
		if current != nil {
			if current.ReservationBytes < 0 {
				return 0, protocol.NewError("resource_unverified", "current physical reservation cannot be verified", map[string]any{"task_id": current.TaskID, "verified": false})
			}
			resources, resourceErr := decodeResourceMap(current.ResourcesJSON, "current task resources", false)
			if resourceErr != nil {
				return 0, protocol.NewError("resource_unverified", "current task resources cannot be verified", map[string]any{"task_id": current.TaskID, "verified": false})
			}
			cost := int64(0)
			if requested["disk_gib"] > resources["disk_gib"] {
				cost = s.Config.ResizeCostBytes
			}
			return maxInt64(current.ReservationBytes, cost), nil
		}
		return 0, nil
	}
	if excludeTaskID == "" {
		return s.Config.CreateCostBytes, nil
	}
	task, err := s.Store.GetTask(ctx, map[string]any{"task_id": excludeTaskID})
	if err != nil {
		return 0, s.sqliteError(err)
	}
	if task != nil && task.State == stateCached {
		if task.ReservationBytes < 0 {
			return 0, protocol.NewError("resource_unverified", "cached physical reservation cannot be verified", map[string]any{"task_id": task.TaskID, "verified": false})
		}
		cost := s.Config.ResumeCostBytes
		resources, resourceErr := decodeResourceMap(task.ResourcesJSON, "cached task resources", false)
		if resourceErr != nil {
			return 0, protocol.NewError("resource_unverified", "cached task resources are malformed", map[string]any{"verified": false})
		}
		if requested["disk_gib"] > resources["disk_gib"] {
			if cost > maxInt64Value-s.Config.ResizeCostBytes {
				return 0, protocol.NewError("config_invalid", "resume and resize physical costs exceed supported range", nil)
			}
			cost += s.Config.ResizeCostBytes
		}
		return maxInt64(task.ReservationBytes, cost), nil
	}
	if task != nil && (task.State == stateActive || task.State == statePreparing) {
		return 0, nil
	}
	return s.Config.CreateCostBytes, nil
}

func (s *Service) checkAdmission(ctx context.Context, requested map[string]int64, excludeTaskID string, startup bool) error {
	if s.Guest != nil {
		probe, ok := s.Guest.(native.HealthProbe)
		if !ok {
			return protocol.NewError("resource_unverified", "Incus native availability is not verified", map[string]any{"verified": false})
		}
		if err := probe.Probe(ctx); err != nil {
			return protocol.NewError("resource_unverified", "Incus native availability could not be verified", map[string]any{"verified": false, "cause": err.Error()})
		}
	}
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return s.sqliteError(err)
	}
	used := map[string]int64{"cpu": 0, "memory_mib": 0, "disk_gib": 0}
	instances := int64(0)
	for _, task := range tasks {
		if task.TaskID == excludeTaskID || task.State == stateCached || task.State == stateAbsent {
			continue
		}
		if !admissionStateKnown(task.State) {
			return protocol.NewError("resource_unverified", "task state cannot be verified", map[string]any{"task_id": task.TaskID, "state": task.State, "verified": false})
		}
		instances++
		resources, resourceErr := decodeResourceMap(task.ResourcesJSON, "task admission resources", false)
		if resourceErr != nil {
			return protocol.NewError("resource_unverified", "durable task resources cannot be verified", map[string]any{"task_id": task.TaskID, "verified": false})
		}
		reservation, reservationErr := decodeResourceMap(task.ResourceReservationJSON, "task admission reservation", true)
		if reservationErr != nil {
			return protocol.NewError("resource_unverified", "durable task reservation cannot be verified", map[string]any{"task_id": task.TaskID, "verified": false})
		}
		for _, key := range resourceKeys {
			accounted := maxInt64(resources[key], reservation[key])
			if used[key] > maxInt64Value-accounted {
				return protocol.NewError("resource_unverified", "durable task resources exceed supported range", map[string]any{"task_id": task.TaskID, "resource": key, "verified": false})
			}
			used[key] += accounted
		}
	}
	if s.Config.InstanceLimit > 0 && instances >= s.Config.InstanceLimit {
		return protocol.NewError("resource_insufficient", "configured managed-container slot capacity is exhausted", map[string]any{"instances": instances, "limit": s.Config.InstanceLimit})
	}
	cpuLimit, cpuErr := config.LogicalLimit(s.Config.CPUBudget, s.Config.CPUOvercommitRatio)
	if cpuErr != nil {
		return protocol.NewError("config_invalid", "CPU admission ratio is invalid", map[string]any{"cause": cpuErr.Error()})
	}
	memoryLimit, memoryErr := config.LogicalLimit(s.Config.MemoryBudgetMiB, s.Config.MemoryOvercommitRatio)
	if memoryErr != nil {
		return protocol.NewError("config_invalid", "memory admission ratio is invalid", map[string]any{"cause": memoryErr.Error()})
	}
	limits := map[string]int64{"cpu": cpuLimit, "memory_mib": memoryLimit, "disk_gib": s.Config.DiskBudgetGiB}
	for _, key := range resourceKeys {
		if used[key] > maxInt64Value-requested[key] {
			return protocol.NewError("resource_unverified", "requested resources exceed supported range", map[string]any{"resource": key, "verified": false})
		}
		if limits[key] > 0 && used[key]+requested[key] > limits[key] {
			return protocol.NewError("resource_insufficient", "requested resources exceed the configured admission budget", map[string]any{"resource": key, "used": used[key], "requested": requested[key], "limit": limits[key]})
		}
	}
	diskAdmission, diskErr := s.diskAdmission(ctx, excludeTaskID)
	if diskErr != nil {
		if s.requiresPhysicalDiskAdmission() {
			return diskErr
		}
	} else {
		requestCost, costErr := s.admissionStorageCost(ctx, excludeTaskID, requested, startup)
		if costErr != nil {
			return costErr
		}
		if _, sufficientErr := s.diskAdmissionSufficient(ctx, diskAdmission, requestCost, excludeTaskID); sufficientErr != nil {
			control := protocol.AsError(sufficientErr)
			if control.Code == "resource_insufficient" {
				if startup {
					hasCached, cachedErr := s.hasCachedCapacityCandidate(ctx, excludeTaskID)
					if cachedErr != nil {
						return cachedErr
					}
					if hasCached {
						if err := s.checkStartupPressure(ctx, used["cpu"], requested["cpu"]); err != nil {
							return err
						}
					}
				}
				if reclaimed, reclaimErr := s.reclaimCachedCapacity(ctx, requestCost, excludeTaskID, 0); reclaimErr != nil {
					return reclaimErr
				} else if reclaimed {
					return nil
				}
			}
			return sufficientErr
		}
	}
	if startup {
		if err := s.checkStartupPressure(ctx, used["cpu"], requested["cpu"]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) hasCachedCapacityCandidate(ctx context.Context, excludeTaskID string) (bool, error) {
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return false, s.sqliteError(err)
	}
	for _, task := range tasks {
		if task.TaskID != excludeTaskID && task.State == stateCached {
			return true, nil
		}
	}
	return false, nil
}

func (s *Service) checkStartupPressure(ctx context.Context, allocatedCPU, requestedCPU int64) error {
	if s.CPU == nil {
		if !s.resident {
			return nil
		}
		return protocol.NewError("resource_unverified", "CT CPU pressure observation is unavailable", map[string]any{"verified": false})
	}
	observation, err := s.CPU.Observe(ctx)
	if err != nil {
		return protocol.NewError("resource_unverified", "CT CPU pressure observation is not verified", map[string]any{"verified": false, "cause": err.Error()})
	}
	detail := s.startupPressureDetail(observation, allocatedCPU, requestedCPU)
	if detail["verified"] != true {
		return protocol.NewError("resource_unverified", "CPU startup resource projection is not verified", detail)
	}
	if detail["startup_blocked"] == true {
		return protocol.NewError("resource_pressure", "CPU scheduler pressure blocks VM startup", detail)
	}
	return nil
}

func (s *Service) startupPressureDetail(observation observe.CPUObservation, allocatedCPU, requestedCPU int64) map[string]any {
	projectedCPU, projectedOK := addInt64(allocatedCPU, requestedCPU)
	detail := map[string]any{
		"verified":                     projectedOK,
		"threshold_full_avg10_percent": s.Config.CPUStartupPressureFullAvg10Percent,
		"allocated_cpu":                allocatedCPU,
		"requested_cpu":                requestedCPU,
		"ct_capacity_cpu":              observation.Capacity,
		"full_avg10_percent":           observation.Full.Avg10,
		"some_avg10_percent":           observation.Some.Avg10,
		"cpu_pressure":                 observation.Map(),
	}
	if !projectedOK {
		detail["projected_cpu"] = nil
		detail["physically_oversubscribed"] = nil
		detail["startup_blocked"] = nil
		return detail
	}
	detail["projected_cpu"] = projectedCPU
	detail["physically_oversubscribed"] = projectedCPU > observation.Capacity
	detail["startup_blocked"] = projectedCPU > observation.Capacity && observation.Full.Avg10 >= s.Config.CPUStartupPressureFullAvg10Percent
	return detail
}

func decodeResourceMap(raw, label string, allowEmpty bool) (map[string]int64, error) {
	if allowEmpty && (strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "{}") {
		return map[string]int64{}, nil
	}
	var rawResult map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &rawResult); err != nil || rawResult == nil {
		return nil, fmt.Errorf("%s is not a JSON object", label)
	}
	if len(rawResult) != len(resourceKeys) {
		return nil, fmt.Errorf("%s is incomplete", label)
	}
	result := make(map[string]int64, len(resourceKeys))
	for _, key := range resourceKeys {
		rawValue, ok := rawResult[key]
		if !ok || bytes.Equal(bytes.TrimSpace(rawValue), []byte("null")) {
			return nil, fmt.Errorf("%s has an invalid %s value", label, key)
		}
		var value int64
		if err := json.Unmarshal(rawValue, &value); err != nil || value < 0 {
			return nil, fmt.Errorf("%s has an invalid %s value", label, key)
		}
		result[key] = value
	}
	return result, nil
}

func sameResources(left, right map[string]int64) bool {
	for _, key := range resourceKeys {
		if left[key] != right[key] {
			return false
		}
	}
	return true
}

func admissionStateKnown(stateName string) bool {
	switch stateName {
	case stateActive, statePreparing, stateCached, stateClosing, stateDeleting, stateFailed, stateAbsent:
		return true
	default:
		return false
	}
}

func requestHasResourceFields(request protocol.Request) bool {
	for _, key := range resourceKeys {
		if _, exists := request[key]; exists {
			return true
		}
	}
	return false
}

func (s *Service) failTask(ctx context.Context, task *state.Task, phase string, err error) (map[string]any, error) {
	detail := nativePhaseDetail(phase, err)
	if stateErr := s.Store.UpdateTask(ctx, task.TaskID, map[string]any{"state": stateFailed, "lease_expires_at": nil, "last_error": mustJSON(detail), "updated_at": nowString(s.Clock())}); stateErr != nil {
		detail["state_persisted"] = false
		detail["state_error"] = stateErr.Error()
		return nil, protocol.NewError("state_unverified", "native failure could not be durably recorded", detail)
	}
	detail["state_persisted"] = true
	return nil, protocol.NewError("native_failed", "native lifecycle action failed", detail)
}

func (s *Service) failTaskForOperation(ctx context.Context, task *state.Task, phase string, err error, operationID string) (map[string]any, error) {
	detail := nativePhaseDetail(phase, err)
	if operationID != "" {
		detail["operation_id"] = operationID
	}
	if stateErr := s.Store.UpdateTask(ctx, task.TaskID, map[string]any{"state": stateFailed, "lease_expires_at": nil, "last_error": mustJSON(detail), "updated_at": nowString(s.Clock())}); stateErr != nil {
		detail["state_persisted"] = false
		detail["state_error"] = stateErr.Error()
		return nil, protocol.NewError("state_unverified", "native failure could not be durably recorded", detail)
	}
	detail["state_persisted"] = true
	return nil, protocol.NewError("native_failed", "native lifecycle action failed", detail)
}

func nativePhaseDetail(phase string, err error) map[string]any {
	detail := map[string]any{"phase": phase, "cause": err.Error(), "outcome_verified": false}
	var commandErr *native.CommandError
	if errors.As(err, &commandErr) {
		for key, value := range commandErr.Diagnostic() {
			detail[key] = value
		}
	}
	return detail
}

func (s *Service) sqliteError(err error) error {
	var epochErr *state.StateEpochError
	if errors.As(err, &epochErr) {
		return protocol.NewError("state_unverified", "state database epoch is not trustworthy", map[string]any{"key": "state_epoch", "cause": err.Error()})
	}
	detail := map[string]any{"cause": err.Error(), "phase": "unknown", "sql_operation": "unknown", "state_committed": nil}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) {
		detail["sqlite_code"] = sqliteErr.Code()
		if name, ok := sqlite.ErrorCodeString[sqliteErr.Code()]; ok {
			detail["sqlite_name"] = name
		} else {
			detail["sqlite_name"] = fmt.Sprintf("SQLITE_%d", sqliteErr.Code())
		}
	}
	var transactionErr *state.TransactionError
	if errors.As(err, &transactionErr) {
		detail["phase"] = transactionErr.Phase
		detail["sql_operation"] = transactionErr.SQLOperation
		detail["state_committed"] = transactionErr.StateCommitted
		if transactionErr.RollbackError != nil {
			detail["rollback_error"] = transactionErr.RollbackError.Error()
		}
	}
	return protocol.NewError("sqlite_unavailable", "state database operation failed", detail)
}

func requestedResources(request protocol.Request) (map[string]int64, error) {
	return requestedResourcesWithBase(request, map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20})
}
func requestedResourcesWithBase(request protocol.Request, base map[string]int64) (map[string]int64, error) {
	result := map[string]int64{"cpu": base["cpu"], "memory_mib": base["memory_mib"], "disk_gib": base["disk_gib"]}
	for _, key := range []string{"cpu", "memory_mib", "disk_gib"} {
		if value, ok := request[key]; ok {
			parsed, valid := protocol.Integer(value)
			if !valid || parsed <= 0 {
				return nil, protocol.NewError("invalid_request", key+" must be a positive integer", nil)
			}
			result[key] = parsed
		}
	}
	return result, nil
}
func zeroResources() map[string]int64 {
	return map[string]int64{"cpu": 0, "memory_mib": 0, "disk_gib": 0}
}
func taskID(session string) string {
	sum := sha256.Sum256([]byte(session))
	return "avm-" + hex.EncodeToString(sum[:])[:16]
}
func effective(task *state.Task, now time.Time) bool {
	return task != nil && task.State == stateActive && task.LeaseExpiresAt != nil && func() bool {
		parsed, err := time.Parse(time.RFC3339Nano, *task.LeaseExpiresAt)
		return err == nil && parsed.After(now)
	}()
}
func nowString(value time.Time) string {
	text := value.UTC().Format(time.RFC3339Nano)
	if strings.HasSuffix(text, "Z") {
		text = strings.TrimSuffix(text, "Z") + "+00:00"
	}
	return text
}
func mustJSON(value any) string    { raw, _ := json.Marshal(value); return string(raw) }
func stringValue(value any) string { text, _ := value.(string); return text }
func optionalString(value any) *string {
	if text, ok := value.(string); ok {
		return &text
	}
	return nil
}
func stringPointer(value string) *string { return &value }
func intValue(value any) int64           { parsed, _ := protocol.Integer(value); return parsed }
func generationPointer(value int64, present bool) *int64 {
	if !present {
		return nil
	}
	return &value
}
func requestBool(request protocol.Request, key string) bool {
	value, ok := request[key].(bool)
	return ok && value
}
func boolValue(value any) bool {
	parsed, _ := value.(bool)
	return parsed
}
func boolString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
func cloneMap(value map[string]any) map[string]any {
	result := make(map[string]any, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}
func uncertain(err error) bool {
	control := protocol.AsError(err)
	if detail, ok := control.Detail.(map[string]any); ok {
		if verified, present := detail["outcome_verified"].(bool); present {
			return !verified
		}
		if phase, present := detail["phase"].(string); present {
			for _, uncertainPhase := range []string{"native-generation", "native-init", "native-config", "native-stop", "native-start", "guest-readiness", "guest-post-check", "access-exposure", "port-add", "port-remove", "cache-delete"} {
				if phase == uncertainPhase {
					return true
				}
			}
		}
	}
	switch control.Code {
	case "acquire_partial", "resize_partial", "restart_partial", "cleanup_incomplete", "operation_unverified", "native_failed":
		return true
	case "invalid_request", "missing_session", "generation_mismatch", "lease_expired", "draining", "cache_stale", "task_missing", "task_not_active", "resource_pressure", "resource_insufficient", "resource_unverified":
		return false
	default:
		return true
	}
}
func (s *Service) String() string { return strconv.FormatBool(s.Ready()) }
