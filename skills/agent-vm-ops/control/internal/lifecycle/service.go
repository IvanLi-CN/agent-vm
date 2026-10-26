package lifecycle

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/native"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/observe"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/state"
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
	Release   *protocol.ReleaseMetadata

	mu          sync.RWMutex
	controlMu   sync.Mutex
	admissionMu sync.Mutex
	resident    bool
	ready       bool
	wake        func()
}

type Options struct {
	Guest     native.Guest
	Clock     func() time.Time
	Resident  bool
	Snapshots *observe.SnapshotStore
	Wake      func()
	CPU       CPUObserver
	Release   *protocol.ReleaseMetadata
}

type CPUObserver interface {
	Observe(context.Context) (observe.CPUObservation, error)
}

func New(store *state.Store, cfg config.Config, options Options) *Service {
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	return &Service{Store: store, Config: cfg, Guest: options.Guest, Clock: clock, Snapshots: options.Snapshots, CPU: options.CPU, Release: options.Release, resident: options.Resident, ready: false, wake: options.Wake}
}

func (s *Service) MarkReady()  { s.mu.Lock(); s.ready = true; s.mu.Unlock() }
func (s *Service) Ready() bool { s.mu.RLock(); defer s.mu.RUnlock(); return s.ready }
func (s *Service) SetWake(wake func()) {
	s.mu.Lock()
	s.wake = wake
	s.mu.Unlock()
}

func (s *Service) wakeScheduler() {
	s.mu.RLock()
	wake := s.wake
	s.mu.RUnlock()
	if wake != nil {
		wake()
	}
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
	if sessionActions[action] && request["session_id"] == nil {
		return nil, protocol.NewError("missing_session", "session_id is required for this action", nil)
	}
	if action == "admin-metrics" && (request["session_id"] != nil || request["operation_id"] != nil) {
		return nil, protocol.NewError("invalid_request", "admin-metrics is sessionless and read-only", nil)
	}
	write := writeActions[action] && !(action == "admin-reconcile" && requestBool(request, "apply") == false && request["operation_id"] == nil)
	if write && request["operation_id"] == nil {
		return nil, protocol.NewError("invalid_request", "write request requires operation_id", nil)
	}
	if write && request["generation"] == nil {
		return nil, protocol.NewError("invalid_request", "write request requires generation", nil)
	}
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	if write {
		operationID := stringValue(request["operation_id"])
		digest, err := protocol.Digest(request)
		if err != nil {
			return nil, protocol.NewError("invalid_request", "request cannot be canonicalized", nil)
		}
		pending := map[string]any{"outcome": "pending", "_request_digest": digest}
		generation := intValue(request["generation"])
		session := optionalString(request["session_id"])
		existing, err := s.Store.ClaimOperation(ctx, operationID, action, session, generationPointer(generation, request["generation"] != nil), pending)
		if err != nil {
			return nil, s.sqliteError(err)
		}
		if existing != nil {
			return s.replayRecord(request, existing, warnings)
		}
	}
	result, err := s.dispatchAction(ctx, action, request)
	if err != nil {
		if write && !uncertain(err) {
			_ = s.recordError(ctx, request, err)
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
	}
	if s.Snapshots != nil {
		_ = s.publishSnapshot(action, response)
	}
	if write {
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
	s.controlMu.Lock()
	defer s.controlMu.Unlock()
	if _, err := s.Store.StateEpoch(ctx); err != nil {
		return s.sqliteError(err)
	}
	if _, err := s.reconcile(ctx, true); err != nil {
		return err
	}
	pending, err := s.Store.PendingOperations(ctx)
	if err != nil {
		return s.sqliteError(err)
	}
	if len(pending) > 0 {
		return protocol.NewError("startup_reconcile_required", "pending operations require explicit recovery before admission", map[string]any{"count": len(pending)})
	}
	return nil
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

func (s *Service) dispatchAction(ctx context.Context, action string, request protocol.Request) (any, error) {
	switch action {
	case "status":
		return s.status(ctx, stringValue(request["session_id"]))
	case "acquire":
		return s.acquire(ctx, request)
	case "renew":
		return s.renew(ctx, request)
	case "release":
		return s.release(ctx, request, false)
	case "destroy":
		return s.release(ctx, request, true)
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
		return s.drain(ctx, true)
	case "drain-exit":
		return s.drain(ctx, false)
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
		return map[string]any{"state": stateAbsent, "session_id": session, "effective_lease": false}, nil
	}
	return s.summary(ctx, task)
}

func (s *Service) acquire(ctx context.Context, request protocol.Request) (map[string]any, error) {
	if s.resident && !s.Ready() {
		return nil, protocol.NewError("startup_reconcile_required", "control service is not ready for admission", nil)
	}
	if draining, err := s.isDraining(ctx); err != nil {
		return nil, s.sqliteError(err)
	} else if draining {
		return nil, protocol.NewError("draining", "new acquisition is refused while draining", nil)
	}
	session := stringValue(request["session_id"])
	generation := intValue(request["generation"])
	resources, err := requestedResources(request)
	if err != nil {
		return nil, err
	}
	s.admissionMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.admissionMu.Unlock()
		}
	}()
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
		task = &state.Task{TaskID: taskID(session), SessionID: session, Generation: 1, State: statePreparing, InstanceName: "agent-vm-" + taskID(session), CreatedAt: nowString(now), UpdatedAt: nowString(now), ImageFingerprint: s.Config.ImageFingerprint, ResourcesJSON: mustJSON(resources), ResourceReservationJSON: mustJSON(resources), LeaseExpiresAt: &lease}
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
		if task.State == stateCached && s.Config.ImageFingerprint != "" && task.ImageFingerprint != s.Config.ImageFingerprint {
			return nil, protocol.NewError("cache_stale", "cached image fingerprint differs from the configured image", map[string]any{"recorded": task.ImageFingerprint, "configured": s.Config.ImageFingerprint})
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
	if created {
		if err := s.Store.InsertTask(ctx, *task); err != nil {
			return nil, s.sqliteError(err)
		}
	} else if err := s.Store.UpdateTask(ctx, task.TaskID, map[string]any{"generation": task.Generation, "state": task.State, "resources_json": task.ResourcesJSON, "resource_reservation_json": task.ResourceReservationJSON, "reservation_bytes": int64(0), "lease_expires_at": task.LeaseExpiresAt, "updated_at": nowString(now), "last_error": nil, "image_fingerprint": s.Config.ImageFingerprint, "previous_generation": task.PreviousGeneration}); err != nil {
		return nil, s.sqliteError(err)
	}
	locked = false
	s.admissionMu.Unlock()
	if s.Guest != nil {
		nativeCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
		defer cancel()
		if created {
			if err := s.Guest.Init(nativeCtx, s.Config.ImageAlias, task.InstanceName); err != nil {
				return s.failTask(ctx, task, "native-init", err)
			}
		}
		if err := s.setOwnership(nativeCtx, task); err != nil {
			return s.failTask(ctx, task, "native-ownership", err)
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
		status, statusErr := s.Guest.Status(nativeCtx, task.InstanceName)
		if statusErr != nil {
			return s.failTask(ctx, task, "guest-readiness", statusErr)
		}
		if value, parseErr := native.ParseStatus(status); parseErr != nil || !strings.EqualFold(value, "running") {
			if parseErr != nil {
				return s.failTask(ctx, task, "guest-readiness", parseErr)
			}
			return s.failTask(ctx, task, "guest-readiness", fmt.Errorf("guest status is %s", value))
		}
		if ip := guestIPv4(status); ip != "" {
			task.VMIP = &ip
			if err := s.Store.UpdateTask(ctx, task.TaskID, map[string]any{"vm_ip": ip, "updated_at": nowString(s.Clock())}); err != nil {
				return nil, s.sqliteError(err)
			}
		} else {
			return s.failTask(ctx, task, "guest-readiness", errors.New("guest did not report an IPv4 address"))
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
	task, err := s.task(ctx, request)
	if err != nil {
		return nil, err
	}
	if task.State == stateAbsent {
		return s.summary(ctx, task)
	}
	if task.State == stateCached && !destroy {
		return s.summary(ctx, task)
	}
	if task.State == stateActive || task.State == statePreparing || task.State == stateFailed || (destroy && task.State == stateCached) {
		targetState := stateClosing
		if destroy && task.State == stateCached {
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
	cleanupErrors := make([]string, 0)
	if s.Guest != nil {
		if _, ownershipErr := s.verifyOwnership(ctx, task); ownershipErr != nil {
			return nil, protocol.NewError("cleanup_incomplete", "native ownership could not be verified before cleanup", map[string]any{"cause": ownershipErr.Error(), "outcome_verified": false})
		}
	}
	ports, listErr := s.Store.ListPorts(ctx, task.TaskID)
	if listErr != nil {
		return nil, s.sqliteError(listErr)
	}
	for _, port := range ports {
		if s.Guest != nil {
			workerCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
			removeErr := s.removePortDevice(workerCtx, task, port)
			cancel()
			if removeErr != nil {
				cleanupErrors = append(cleanupErrors, port.DeviceName+": "+removeErr.Error())
				continue
			}
		}
		if removeErr := s.Store.DeletePort(ctx, task.TaskID, port.PortName, port.Protocol); removeErr != nil {
			cleanupErrors = append(cleanupErrors, port.DeviceName+": "+removeErr.Error())
		}
	}
	if s.Guest != nil {
		workerCtx, cancel := context.WithTimeout(ctx, time.Duration(s.Config.WorkerTimeoutSeconds)*time.Second)
		defer cancel()
		if err := s.Guest.Stop(workerCtx, task.InstanceName); err != nil {
			return nil, protocol.NewError("cleanup_incomplete", "guest stop did not complete", map[string]any{"cause": err.Error(), "outcome_verified": false})
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
	updates := map[string]any{"state": stateValue, "updated_at": nowString(s.Clock()), "resources_json": mustJSON(zeroResources()), "resource_reservation_json": "{}", "reservation_bytes": int64(0)}
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
	if requestBool(request, "live_memory") && (request["cpu"] != nil || request["disk_gib"] != nil) {
		return nil, protocol.NewError("invalid_request", "live resize requires memory only", nil)
	}
	if requestBool(request, "live_memory") && request["memory_mib"] == nil {
		return nil, protocol.NewError("invalid_request", "live resize requires memory_mib", nil)
	}
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
	updated, err := s.Store.UpdateTaskIf(ctx, task.TaskID, state.TaskCondition{Generation: &task.Generation, States: []string{stateActive}, LeaseActiveAfter: &commitNow}, map[string]any{"resources_json": mustJSON(target), "updated_at": nowString(commitNow)})
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

func (s *Service) drain(ctx context.Context, value bool) (map[string]any, error) {
	if err := s.Store.SetMeta(ctx, "draining", boolString(value)); err != nil {
		return nil, s.sqliteError(err)
	}
	return map[string]any{"draining": value}, nil
}

func (s *Service) drainStatus(ctx context.Context) (map[string]any, error) {
	draining, err := s.isDraining(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	return map[string]any{"draining": draining, "ready": s.Ready()}, nil
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
		allocated := map[string]int64{"cpu": 0, "memory_mib": 0, "disk_gib": 0}
		reserved := map[string]int64{"cpu": 0, "memory_mib": 0, "disk_gib": 0}
		for _, task := range tasks {
			states[task.State]++
			resources := map[string]int64{}
			_ = json.Unmarshal([]byte(task.ResourcesJSON), &resources)
			if task.State == stateActive {
				for _, key := range resourceKeys {
					allocated[key] += resources[key]
				}
			}
			reservation := map[string]int64{}
			_ = json.Unmarshal([]byte(task.ResourceReservationJSON), &reservation)
			for _, key := range resourceKeys {
				reserved[key] += reservation[key]
			}
		}
		endEpoch, err := s.Store.StateEpoch(ctx)
		if err != nil {
			return nil, s.sqliteError(err)
		}
		consistent := epoch == endEpoch
		if consistent || attempt == 3 {
			warnings := []any{}
			if !consistent {
				warnings = append(warnings, map[string]any{"code": "observation_inconsistent", "message": "durable state changed while collecting metrics"})
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
			draining, _ := s.isDraining(ctx)
			limits := map[string]any{}
			remaining := map[string]any{}
			if s.Config.CPUBudget > 0 {
				limit, limitErr := config.LogicalLimit(s.Config.CPUBudget, s.Config.CPUOvercommitRatio)
				if limitErr != nil {
					return nil, protocol.NewError("config_invalid", "CPU admission ratio is invalid", map[string]any{"cause": limitErr.Error()})
				}
				limits["cpu"] = limit
				remaining["cpu"] = limit - allocated["cpu"] - reserved["cpu"]
			}
			if s.Config.MemoryBudgetMiB > 0 {
				limit, limitErr := config.LogicalLimit(s.Config.MemoryBudgetMiB, s.Config.MemoryOvercommitRatio)
				if limitErr != nil {
					return nil, protocol.NewError("config_invalid", "memory admission ratio is invalid", map[string]any{"cause": limitErr.Error()})
				}
				limits["memory_mib"] = limit
				remaining["memory_mib"] = limit - allocated["memory_mib"] - reserved["memory_mib"]
			}
			if s.Config.DiskBudgetGiB > 0 {
				limits["disk_gib"] = s.Config.DiskBudgetGiB
				remaining["disk_gib"] = s.Config.DiskBudgetGiB - allocated["disk_gib"] - reserved["disk_gib"]
			}
			resourcesValue := map[string]any{"allocated": allocated, "reserved": reserved, "allocated_and_reserved": addResourceMaps(allocated, reserved), "limits": limits, "remaining": remaining, "policy": map[string]any{"cpu_overcommit_ratio": s.Config.CPUOvercommitRatio, "memory_overcommit_ratio": s.Config.MemoryOvercommitRatio, "cpu_startup_pressure_full_avg10_percent": s.Config.CPUStartupPressureFullAvg10Percent}}
			ctCPU := map[string]any{"capacity_vcpu": nil, "pressure": nil, "startup_pressure": nil}
			defaultReasons := []any{}
			if !s.Ready() {
				defaultReasons = append(defaultReasons, map[string]any{"code": "startup_reconcile_required", "message": "control service is not ready for admission"})
			}
			if cpuObservation != nil {
				pressure := cpuObservation.Pressure()
				startupPressure := s.startupPressureDetail(*cpuObservation, allocated["cpu"], 2)
				resourcesValue["ct_capacity"] = map[string]any{"cpu": cpuObservation.Capacity}
				resourcesValue["cpu_pressure"] = pressure
				ctCPU["capacity_vcpu"] = cpuObservation.Capacity
				ctCPU["pressure"] = pressure
				ctCPU["startup_pressure"] = startupPressure
				if startupPressure["startup_blocked"] == true {
					defaultReasons = append(defaultReasons, map[string]any{"code": "resource_pressure", "message": "CPU scheduler pressure blocks VM startup", "detail": startupPressure})
				}
			} else {
				resourcesValue["ct_capacity"] = map[string]any{"cpu": nil}
				resourcesValue["cpu_pressure"] = nil
				if cpuObservationErr != nil {
					defaultReasons = append(defaultReasons, map[string]any{"code": "resource_unverified", "message": "CT CPU pressure observation is not verified"})
				}
			}
			return map[string]any{
				"observation_epoch": epoch, "observation_consistent": consistent, "observation_attempts": attempt,
				"observed_at": start, "up": true, "draining": draining, "admission_ready": s.Ready(),
				"resources": resourcesValue, "ct_cpu": ctCPU,
				"default_admission": map[string]any{"requested": map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}, "ready": s.Ready(), "reasons": defaultReasons},
				"task_states":       states, "instances": len(tasks), "warnings": warnings, "warning_counts": map[string]int{"observation_inconsistent": boolInt(!consistent), "resource_unverified": boolInt(cpuObservationErr != nil)},
			}, nil
		}
	}
	return nil, protocol.NewError("observation_unavailable", "metrics observation did not converge", nil)
}

func (s *Service) reconcile(ctx context.Context, apply bool) (map[string]any, error) {
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
			if _, releaseErr := s.release(ctx, protocol.Request{"session_id": task.SessionID, "generation": task.Generation}, false); releaseErr != nil && firstErr == nil {
				firstErr = releaseErr
			}
		}
		if task.State == stateFailed && apply {
			if _, releaseErr := s.release(ctx, protocol.Request{"session_id": task.SessionID, "generation": task.Generation}, false); releaseErr != nil && firstErr == nil {
				firstErr = releaseErr
			}
		}
		if task.State == stateDeleting && apply {
			if _, releaseErr := s.release(ctx, protocol.Request{"session_id": task.SessionID, "generation": task.Generation}, true); releaseErr != nil && firstErr == nil {
				firstErr = releaseErr
			}
		}
	}
	result := map[string]any{"dry_run": !apply, "actions": actions}
	return result, firstErr
}

func (s *Service) gc(ctx context.Context, apply bool) (map[string]any, error) {
	tasks, err := s.Store.ListTasks(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	candidates := make([]any, 0)
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
			if updateErr == nil && updated {
				_, _ = s.release(ctx, protocol.Request{"session_id": task.SessionID, "generation": task.Generation}, true)
			}
		}
	}
	return map[string]any{"dry_run": !apply, "selection": "age", "minimum_idle_seconds": s.Config.CacheSeconds, "candidates": candidates}, nil
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
	return s.release(ctx, protocol.Request{"session_id": task.SessionID, "generation": task.Generation}, false)
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
	return map[string]any{"up": true, "ready": s.Ready(), "draining": draining, "state_epoch": epoch, "transport": "http-over-uds"}, nil
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
	return task, nil
}

func (s *Service) summary(ctx context.Context, task *state.Task) (map[string]any, error) {
	if task == nil || task.TaskID == "" {
		return map[string]any{"state": stateAbsent, "session_id": task.SessionID, "effective_lease": false}, nil
	}
	resources := map[string]any{}
	_ = json.Unmarshal([]byte(task.ResourcesJSON), &resources)
	ports, err := s.Store.ListPorts(ctx, task.TaskID)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	return map[string]any{"task_id": task.TaskID, "session_id": task.SessionID, "generation": task.Generation, "state": task.State, "instance_name": task.InstanceName, "vm_ip": task.VMIP, "ssh_host_port": task.SSHHostPort, "lease_expires_at": task.LeaseExpiresAt, "inactive_since": task.InactiveSince, "cleanup_after": task.CleanupAfter, "resources": resources, "resource_reservation": map[string]any{}, "reservation_bytes": task.ReservationBytes, "ports": ports, "last_error": task.LastError, "effective_lease": effective(task, s.Clock())}, nil
}

func (s *Service) isDraining(ctx context.Context) (bool, error) {
	value, exists, err := s.Store.Meta(ctx, "draining")
	return exists && value == "1", err
}

func (s *Service) setOwnership(ctx context.Context, task *state.Task) error {
	for key, value := range map[string]string{
		"user.agent-vm.managed":    "native-v1",
		"user.agent-vm.task":       task.TaskID,
		"user.agent-vm.session":    task.SessionID,
		"user.agent-vm.generation": strconv.FormatInt(task.Generation, 10),
	} {
		if err := s.Guest.ConfigSet(ctx, task.InstanceName, key, value); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) verifyOwnership(ctx context.Context, task *state.Task) (map[string]any, error) {
	info, err := s.Guest.Config(ctx, task.InstanceName)
	if err != nil {
		return nil, protocol.NewError("ownership_mismatch", "native instance configuration could not be read", map[string]any{"cause": err.Error(), "outcome_verified": false})
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
			if port.Protocol == protocolName && port.State == "active" {
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

func (s *Service) checkAdmission(ctx context.Context, requested map[string]int64, excludeTaskID string, startup bool) error {
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
		instances++
		resources := map[string]int64{}
		if task.ResourceReservationJSON != "" && task.ResourceReservationJSON != "{}" {
			_ = json.Unmarshal([]byte(task.ResourceReservationJSON), &resources)
		} else {
			_ = json.Unmarshal([]byte(task.ResourcesJSON), &resources)
		}
		for _, key := range resourceKeys {
			used[key] += resources[key]
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
		if limits[key] > 0 && used[key]+requested[key] > limits[key] {
			return protocol.NewError("resource_insufficient", "requested resources exceed the configured admission budget", map[string]any{"resource": key, "used": used[key], "requested": requested[key], "limit": limits[key]})
		}
	}
	if startup {
		if err := s.checkStartupPressure(ctx, used["cpu"], requested["cpu"]); err != nil {
			return err
		}
	}
	return nil
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
	if detail["startup_blocked"] == true {
		return protocol.NewError("resource_pressure", "CPU scheduler pressure blocks VM startup", detail)
	}
	return nil
}

func (s *Service) startupPressureDetail(observation observe.CPUObservation, allocatedCPU, requestedCPU int64) map[string]any {
	projectedCPU := allocatedCPU + requestedCPU
	return map[string]any{
		"verified":                     true,
		"threshold_full_avg10_percent": s.Config.CPUStartupPressureFullAvg10Percent,
		"allocated_cpu":                allocatedCPU,
		"requested_cpu":                requestedCPU,
		"projected_cpu":                projectedCPU,
		"ct_capacity_cpu":              observation.Capacity,
		"physically_oversubscribed":    projectedCPU > observation.Capacity,
		"full_avg10_percent":           observation.Full.Avg10,
		"some_avg10_percent":           observation.Some.Avg10,
		"cpu_pressure":                 observation.Map(),
		"startup_blocked":              projectedCPU > observation.Capacity && observation.Full.Avg10 >= s.Config.CPUStartupPressureFullAvg10Percent,
	}
}

func addResourceMaps(left, right map[string]int64) map[string]int64 {
	result := map[string]int64{}
	for _, key := range resourceKeys {
		result[key] = left[key] + right[key]
	}
	return result
}

func sameResources(left, right map[string]int64) bool {
	for _, key := range resourceKeys {
		if left[key] != right[key] {
			return false
		}
	}
	return true
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
	_ = s.Store.UpdateTask(ctx, task.TaskID, map[string]any{"state": stateFailed, "lease_expires_at": nil, "last_error": mustJSON(detail), "updated_at": nowString(s.Clock())})
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
