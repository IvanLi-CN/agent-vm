package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

// reconcileVerifiedNativeInitAbsence handles only the durable residue of an
// acquire that failed before an owned native instance existed. The caller
// must keep ordinary failed/closing cleanup available for every other phase.
func (s *Service) reconcileVerifiedNativeInitAbsence(ctx context.Context, candidate state.Task, excludedOperationID string) (bool, error) {
	if candidate.State != stateFailed && candidate.State != stateClosing {
		return false, nil
	}
	if _, matched := nativeInitFailureRecord(candidate.LastError); !matched {
		return false, nil
	}

	_, err := s.withSessionLock(candidate.SessionID, func() (map[string]any, error) {
		current, err := s.Store.GetTask(ctx, map[string]any{"session_id": candidate.SessionID})
		if err != nil {
			return nil, s.sqliteError(err)
		}
		if current == nil || current.State == stateAbsent {
			return nil, nil
		}
		if current.Generation != candidate.Generation || current.State != stateFailed && current.State != stateClosing {
			return nil, nativeInitRecoveryBlocked("native-init recovery task identity changed", map[string]any{
				"task_id": candidate.TaskID, "expected_generation": candidate.Generation, "actual_generation": current.Generation,
			})
		}
		failure, matched := nativeInitFailureRecord(current.LastError)
		if !matched {
			return nil, nativeInitRecoveryBlocked("native-init recovery evidence changed before verification", map[string]any{"task_id": current.TaskID})
		}

		pending, err := s.verifiedNativeInitPendingOperations(ctx, current, failure, excludedOperationID)
		if err != nil {
			return nil, err
		}
		if s.Guest == nil {
			return nil, nativeInitRecoveryBlocked("native-init absence cannot be verified without native access", map[string]any{"task_id": current.TaskID})
		}
		if _, ownershipErr := s.verifyOwnership(ctx, current); ownershipErr == nil {
			return nil, nativeInitRecoveryBlocked("native-init recovery found a present managed instance", map[string]any{
				"task_id": current.TaskID, "native_present": true, "outcome_verified": true,
			})
		} else {
			control := protocol.AsError(ownershipErr)
			detail := map[string]any{"task_id": current.TaskID, "cause": ownershipErr.Error()}
			if nativeDetail, ok := control.Detail.(map[string]any); ok {
				for key, value := range nativeDetail {
					detail[key] = value
				}
			}
			if detail["native_missing"] != true {
				return nil, nativeInitRecoveryBlocked("native-init native identity is not confirmed absent", detail)
			}
		}
		ports, err := s.Store.ListPorts(ctx, current.TaskID)
		if err != nil {
			return nil, s.sqliteError(err)
		}
		if len(ports) != 0 {
			return nil, nativeInitRecoveryBlocked("native-init recovery found durable port mappings", map[string]any{
				"task_id": current.TaskID, "port_count": len(ports),
			})
		}

		operationIDs := operationIDs(pending)
		recoveredDetail := nativeInitRecoveryDetail(failure, current, operationIDs)
		recoveredLastError := mustJSON(recoveredDetail)
		recoveredTask := *current
		recoveredTask.State = stateAbsent
		recoveredTask.VMIP = nil
		recoveredTask.SSHHostPort = nil
		recoveredTask.LeaseExpiresAt = nil
		recoveredTask.InactiveSince = nil
		recoveredTask.CleanupAfter = nil
		recoveredTask.ReservationBytes = 0
		recoveredTask.PreviousGeneration = nil
		recoveredTask.ResourcesJSON = mustJSON(zeroResources())
		recoveredTask.ResourceReservationJSON = "{}"
		recoveredTask.LastError = &recoveredLastError
		recoveryResult, err := s.summary(ctx, &recoveredTask)
		if err != nil {
			return nil, err
		}
		latest, err := s.Store.GetTask(ctx, map[string]any{"task_id": current.TaskID})
		if err != nil {
			return nil, s.sqliteError(err)
		}
		if latest == nil || latest.Generation != current.Generation || latest.State != stateFailed && latest.State != stateClosing {
			return nil, nativeInitRecoveryBlocked("native-init recovery task changed before journal finalization", map[string]any{
				"task_id": current.TaskID, "expected_generation": current.Generation,
			})
		}

		// Journal finalization is deliberately complete before the conditional
		// task update. If the task commit fails, the durable terminal outcomes
		// remain evidence for explicit inspection instead of being replayed.
		for _, operation := range pending {
			response, responseErr := nativeInitRecoveryOperation(operation, recoveredDetail, recoveryResult, s.Release)
			if responseErr != nil {
				return nil, responseErr
			}
			if err := s.Store.RecordOperation(ctx, operation.OperationID, operation.Action, operation.SessionID, operation.Generation, response); err != nil {
				return nil, s.sqliteError(err)
			}
		}

		updated, err := s.Store.UpdateTaskIf(ctx, current.TaskID, state.TaskCondition{
			Generation: &current.Generation,
			States:     []string{stateFailed, stateClosing},
		}, map[string]any{
			"state":                     stateAbsent,
			"vm_ip":                     nil,
			"ssh_host_port":             nil,
			"lease_expires_at":          nil,
			"inactive_since":            nil,
			"cleanup_after":             nil,
			"resources_json":            mustJSON(zeroResources()),
			"resource_reservation_json": "{}",
			"reservation_bytes":         int64(0),
			"previous_generation":       nil,
			"last_error":                recoveredLastError,
			"updated_at":                nowString(s.Clock()),
		})
		if err != nil {
			return nil, s.sqliteError(err)
		}
		if !updated {
			return nil, nativeInitRecoveryBlocked("native-init recovery task changed before commit", map[string]any{"task_id": current.TaskID})
		}
		return nil, nil
	})
	return true, err
}

func nativeInitFailureRecord(lastError *string) (map[string]any, bool) {
	if lastError == nil || strings.TrimSpace(*lastError) == "" {
		return nil, false
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(*lastError), &value); err != nil {
		return nil, false
	}
	phase := stringValue(value["phase"])
	operationPhase := stringValue(value["operation_phase"])
	if phase != "native-init" || operationPhase != "" && operationPhase != "native-init" {
		return nil, false
	}
	return value, true
}

func (s *Service) verifiedNativeInitPendingOperations(ctx context.Context, task *state.Task, failure map[string]any, excludedOperationID string) ([]state.Operation, error) {
	pending, err := s.Store.PendingOperations(ctx)
	if err != nil {
		return nil, s.sqliteError(err)
	}
	previousGeneration := task.Generation - 1
	matched := make([]state.Operation, 0)
	acquireCount := 0
	for _, operation := range pending {
		if operation.SessionID == nil || *operation.SessionID != task.SessionID {
			continue
		}
		if excludedOperationID != "" && operation.OperationID == excludedOperationID {
			continue
		}
		detail := map[string]any{"operation_id": operation.OperationID, "task_id": task.TaskID}
		if operation.Generation == nil || !nativeInitOperationGenerationMatches(operation, task, failure, previousGeneration) {
			return nil, nativeInitOperationBlocked("pending operation generation is outside the recovery window", detail)
		}
		switch operation.Action {
		case "acquire", "release", "destroy":
		default:
			return nil, nativeInitOperationBlocked("pending operation action is not allowed for native-init recovery", map[string]any{"operation_id": operation.OperationID, "action": operation.Action})
		}
		if operation.Action == "acquire" {
			acquireCount++
		}
		if err := verifyNativeInitWorker(operation); err != nil {
			return nil, nativeInitOperationBlocked(err.Error(), detail)
		}
		requestValue, ok := operation.Response["_request"].(map[string]any)
		if !ok {
			return nil, nativeInitOperationBlocked("pending operation request is missing", detail)
		}
		digest := stringValue(operation.Response["_request_digest"])
		if strings.TrimSpace(digest) == "" {
			return nil, nativeInitOperationBlocked("pending operation request digest is missing", detail)
		}
		request := protocol.Request(requestValue)
		if validationErr := protocol.ValidateRequest(request); validationErr != nil {
			return nil, nativeInitOperationBlocked("pending operation request is invalid", map[string]any{"operation_id": operation.OperationID, "cause": validationErr.Error()})
		}
		requestGeneration, generationPresent := request["generation"]
		parsedGeneration, generationValid := protocol.Integer(requestGeneration)
		if !generationPresent || !generationValid || stringValue(request["operation_id"]) != operation.OperationID ||
			stringValue(request["action"]) != operation.Action ||
			stringValue(request["session_id"]) != task.SessionID ||
			parsedGeneration != *operation.Generation {
			return nil, nativeInitOperationBlocked("pending operation identity does not match its journal", detail)
		}
		if requestTaskID := stringValue(request["task_id"]); requestTaskID != "" && requestTaskID != task.TaskID {
			return nil, nativeInitOperationBlocked("pending operation task identity does not match", detail)
		}
		calculated, digestErr := protocol.Digest(request)
		if digestErr != nil || calculated != digest {
			return nil, nativeInitOperationBlocked("pending operation request digest does not match", detail)
		}
		matched = append(matched, operation)
	}
	if acquireCount > 1 {
		return nil, nativeInitOperationBlocked("multiple pending acquire operations match native-init recovery", map[string]any{"task_id": task.TaskID, "count": acquireCount})
	}
	if len(matched) == 0 {
		return nil, nativeInitOperationBlocked("native-init recovery has no matching pending journal", map[string]any{"task_id": task.TaskID})
	}
	return matched, nil
}

func verifyNativeInitWorker(operation state.Operation) error {
	value, ok := operation.Response["worker"]
	worker, workerOK := value.(map[string]any)
	if !ok || !workerOK {
		return errors.New("pending native worker observation is missing")
	}
	pid, pidOK := protocol.Integer(worker["pid"])
	startTime, startOK := worker["start_time"].(string)
	if !pidOK || pid <= 0 || !startOK || strings.TrimSpace(startTime) == "" {
		return errors.New("pending native worker identity is incomplete")
	}
	if processGroup, present := worker["process_group"]; present {
		group, valid := protocol.Integer(processGroup)
		if !valid || group < 0 {
			return errors.New("pending native worker process group is malformed")
		}
	}
	if timedOut, present := worker["timed_out"]; present && timedOut == true {
		return errors.New("pending native worker timed out before terminal observation")
	}
	switch stringValue(worker["state"]) {
	case "finished":
		if !validWorkerTimestamp(worker["finished_at"]) {
			return errors.New("pending native worker completion is not durably observed")
		}
	case "exited":
		if !validWorkerTimestamp(worker["observed_at"]) {
			return errors.New("pending native worker exit is not durably observed")
		}
	default:
		return errors.New("pending native worker has not reached a verified terminal state")
	}
	return nil
}

func validWorkerTimestamp(value any) bool {
	text, ok := value.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, text)
	return err == nil
}

func nativeInitOperationGenerationMatches(operation state.Operation, task *state.Task, failure map[string]any, previousGeneration int64) bool {
	if operation.Generation == nil {
		return false
	}
	if *operation.Generation == task.Generation || task.Generation > 0 && *operation.Generation == previousGeneration {
		return true
	}
	// A newly allocated task can follow a deleted task whose generation
	// tombstone is greater than one, while the public acquire request still
	// starts at generation zero. Bind that exceptional window to the exact
	// operation recorded in the native-init failure; never infer it from the
	// session alone.
	return operation.Action == "acquire" && *operation.Generation == 0 && task.Generation > 1 &&
		stringValue(failure["operation_id"]) == operation.OperationID
}

func nativeInitRecoveryDetail(previous map[string]any, task *state.Task, operationIDs []string) map[string]any {
	detail := cloneMap(previous)
	detail["phase"] = "native-init"
	detail["operation_phase"] = "native-init"
	detail["native_init_recovered_absent"] = true
	detail["outcome_verified"] = true
	detail["task_id"] = task.TaskID
	detail["task_generation"] = task.Generation
	if len(operationIDs) > 0 {
		detail["native_init_recovery_operation_ids"] = operationIDs
	}
	return detail
}

func nativeInitRecoveryOperation(operation state.Operation, detail map[string]any, result map[string]any, release *protocol.ReleaseMetadata) (map[string]any, error) {
	digest := stringValue(operation.Response["_request_digest"])
	if operation.Action == "acquire" {
		return map[string]any{
			"outcome":         "error",
			"error":           protocol.NewError("acquire_partial", "native initialization did not complete", detail),
			"_request_digest": digest,
		}, nil
	}
	rawRequest, ok := operation.Response["_request"].(map[string]any)
	if !ok {
		return nil, nativeInitOperationBlocked("pending operation request is missing", map[string]any{"operation_id": operation.OperationID})
	}
	response := protocol.SuccessWithRelease(protocol.Request(rawRequest), result, release, nil)
	response["_request_digest"] = digest
	return response, nil
}

func nativeInitRecoveryBlocked(message string, detail map[string]any) error {
	detail = cloneMap(detail)
	detail["phase"] = "native-init"
	detail["operation_phase"] = "native-init"
	detail["native_init_recovered_absent"] = false
	detail["outcome_verified"] = false
	return protocol.NewError("cleanup_incomplete", message, detail)
}

func nativeInitOperationBlocked(message string, detail map[string]any) error {
	detail = cloneMap(detail)
	detail["phase"] = "native-init"
	detail["operation_phase"] = "native-init"
	detail["native_init_recovered_absent"] = false
	detail["outcome_verified"] = false
	return protocol.NewError("operation_unverified", message, detail)
}
