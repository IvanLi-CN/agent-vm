package deployd

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// resumeRollbackDrainExit completes only the original metadata-only release
// intent after both traffic/controller roles are fenced. It never edits state
// or resumes an Incus operation; the business service checks the saved digest.
func (runtime *HostRuntime) resumeRollbackDrainExit(ctx context.Context, slot string) error {
	result, err := runtime.quiescenceMetrics(ctx, slot)
	if err != nil {
		return err
	}
	pending, ok := result["pending_operations"].([]any)
	if !ok {
		return errors.New("rollback pending operations missing")
	}
	if len(pending) == 0 {
		return nil
	}
	inFlight, ok := integerValue(result["in_flight_operations"])
	if !ok || inFlight != 0 || len(pending) > 16 {
		return errors.New("rollback administrative recovery is not quiescent")
	}
	for _, name := range []string{"blue", "green"} {
		if runtime.systemdSlotStopped(ctx, name) {
			continue
		}
		health, status, err := runtime.slotRequest(ctx, runtime.socket(name), http.MethodGet, "/internal/v1/health", nil)
		if err != nil || (status != http.StatusOK && status != http.StatusServiceUnavailable) || !healthEnvelopeValid(health) || health["controller_owned"] != false || health["traffic_accepting"] != false {
			return errors.New("rollback administrative recovery requires fenced roles")
		}
	}
	requests := []map[string]any{}
	for _, raw := range pending {
		row, ok := raw.(map[string]any)
		generation, validGeneration := integerValue(row["generation"])
		id, _ := row["operation_id"].(string)
		if !ok || row["action"] != "drain-exit" || row["session_id"] != deploydDrainSession || !validGeneration || generation != 0 || !strings.HasPrefix(id, "deployd-drain-exit-") {
			return errors.New("rollback pending operation requires explicit business recovery")
		}
		requests = append(requests, map[string]any{"protocol": 4, "action": "drain-exit", "session_id": deploydDrainSession, "operation_id": id, "generation": int64(0), "preserve_traffic": true})
	}
	for _, request := range requests {
		if _, _, err := runtime.control(ctx, runtime.socket(slot), request); err != nil {
			return err
		}
	}
	return nil
}

// Reuse the coherent read-only lane. Standalone legacy admin-metrics may make
// many remote authority round trips and includes an unrelated capacity probe.
func (runtime *HostRuntime) quiescenceMetrics(ctx context.Context, slot string) (map[string]any, error) {
	bundle, err := runtime.observeSlotSources(ctx, slot)
	if err != nil {
		return nil, err
	}
	if bundle != nil {
		value, ok := bundle["capacity"].(map[string]any)
		if !ok {
			return nil, errors.New("quiescence capacity lane missing")
		}
		if err := validateSourceResult("admin-metrics", value); err != nil {
			return nil, err
		}
		return value, nil
	}
	body, _, err := runtime.control(ctx, runtime.socket(slot), map[string]any{"protocol": 4, "action": "admin-metrics"})
	if err != nil {
		return nil, err
	}
	value, ok := body["result"].(map[string]any)
	if !ok {
		return nil, errors.New("quiescence counters missing")
	}
	return value, nil
}
