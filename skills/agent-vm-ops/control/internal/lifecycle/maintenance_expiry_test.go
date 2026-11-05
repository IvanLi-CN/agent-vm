package lifecycle

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

func TestMaintenancePreparingExpirySurvivesEarlierObservation(t *testing.T) {
	ctx := context.Background()
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fingerprint := strings.Repeat("a", 64)
	if err := store.SeedLegacyImage(ctx, fingerprint); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	expires := nowString(now.Add(time.Minute))
	task := state.Task{TaskID: "preparing-expiry", SessionID: "preparing-expiry", Generation: 1, State: statePreparing, InstanceName: "preparing-expiry", LeaseExpiresAt: &expires, CreatedAt: nowString(now), UpdatedAt: nowString(now), ImageFingerprint: fingerprint, ResourcesJSON: "{}", ResourceReservationJSON: "{}"}
	if err := store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Clock: func() time.Time { return now }})
	if err := service.reconcileMaintenanceTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	if op, err := store.GetOperation(ctx, timerExpireOperationID(task.TaskID, task.Generation)); err != nil || op != nil {
		t.Fatalf("future deadline consumed expiry identity: %#v, %v", op, err)
	}
	now = now.Add(2 * time.Minute)
	if err := service.reconcileMaintenanceTask(ctx, &task); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetTask(ctx, map[string]any{"task_id": task.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if current.State != stateCached || current.LeaseExpiresAt != nil {
		t.Fatalf("expired preparing task remains %s with lease %#v", current.State, current.LeaseExpiresAt)
	}
}

func TestMaintenanceExpiryPreservesLegacyEvidence(t *testing.T) {
	for _, scenario := range []struct {
		name, outcome, workerState string
		started                    any
		expired                    any
		wantCached                 bool
	}{
		{"verified-no-effect", "success", "starting", false, false, true},
		{"pending", "pending", "starting", false, false, false},
		{"worker-attempted", "success", "starting", true, false, false},
		{"contradictory-worker", "success", "starting", false, false, false},
		{"worker-unverified", "success", "starting", nil, false, false},
		{"result-unverified", "success", "starting", false, nil, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			fp := strings.Repeat("a", 64)
			if err := store.SeedLegacyImage(ctx, fp); err != nil {
				t.Fatal(err)
			}
			now := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
			deadline := nowString(now)
			task := state.Task{TaskID: "legacy-expiry", SessionID: "legacy-expiry", Generation: 1, State: statePreparing, InstanceName: "legacy-expiry", LeaseExpiresAt: &deadline, CreatedAt: deadline, UpdatedAt: deadline, ImageFingerprint: fp, ResourcesJSON: "{}", ResourceReservationJSON: "{}"}
			if err := store.InsertTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			id := timerExpireOperationID(task.TaskID, task.Generation)
			req := protocol.Request{"protocol": protocol.Version, "action": "timer-expire", "task_id": task.TaskID, "session_id": task.SessionID, "generation": task.Generation, "operation_id": id}
			digest, _ := protocol.Digest(req)
			original := map[string]any{"ok": true, "outcome": scenario.outcome, "_request_digest": digest, "_request": map[string]any(req), "result": map[string]any{"expired": scenario.expired}, "worker": map[string]any{"state": scenario.workerState, "start_attempted": scenario.started}}
			if scenario.name == "contradictory-worker" {
				original["worker"].(map[string]any)["pid"] = int64(123)
			}
			if err := store.RecordOperation(ctx, id, "timer-expire", &task.SessionID, &task.Generation, original); err != nil {
				t.Fatal(err)
			}
			service := New(store, config.Defaults(), Options{Clock: func() time.Time { return now }})
			err = service.reconcileMaintenanceTask(ctx, &task)
			if scenario.wantCached && err != nil {
				t.Fatal(err)
			}
			if scenario.outcome == "pending" && err == nil {
				t.Fatal("pending evidence was bypassed")
			}
			current, err := store.GetTask(ctx, map[string]any{"task_id": task.TaskID})
			if err != nil {
				t.Fatal(err)
			}
			if (current.State == stateCached) != scenario.wantCached {
				t.Fatalf("state=%s, cached expected=%v", current.State, scenario.wantCached)
			}
			saved, err := store.GetOperation(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if saved.Response["outcome"] != scenario.outcome {
				t.Fatal("old outcome overwritten")
			}
			worker := saved.Response["worker"].(map[string]any)
			if worker["start_attempted"] != scenario.started {
				t.Fatal("old worker evidence overwritten")
			}
		})
	}
}
