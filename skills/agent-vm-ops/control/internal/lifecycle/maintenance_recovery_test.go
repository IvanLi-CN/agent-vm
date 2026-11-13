package lifecycle

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

func TestMaintenanceUnverifiedJournalDoesNotStarveExpiry(t *testing.T) {
	f := newScopedFixture(t)
	ctx := context.Background()
	session := "blocked"
	generation := int64(1)
	if err := f.store.RecordOperation(ctx, "blocked", "release", &session, &generation, map[string]any{"outcome": "pending", "worker": map[string]any{"state": "exited", "start_attempted": false}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		_ = f.service.RunMaintenance(ctx, maintenanceReconcileLane)
		f.now = f.now.Add(65 * time.Second)
	}
	for _, name := range []string{"target", "expired-peer"} {
		task, err := f.store.GetTask(ctx, map[string]any{"session_id": name})
		if err != nil || task.State != stateCached {
			t.Fatalf("expired task %s starved: %#v %v", name, task, err)
		}
	}
	peer, _ := f.store.GetTask(ctx, map[string]any{"session_id": "active-peer"})
	if peer.State != stateActive {
		t.Fatal("effective lease was changed")
	}
	pending, _ := f.store.GetOperation(ctx, "blocked")
	if pending.Response["outcome"] != "pending" {
		t.Fatal("unverified intent was discarded")
	}
}

func TestManualReconcileBadCachedAcquireDoesNotPreventExpiry(t *testing.T) {
	f := newScopedFixture(t)
	ctx := context.Background()
	task := state.Task{TaskID: "bad-cache", SessionID: "bad-cache", Generation: 2, State: stateCached, InstanceName: "instance-bad-cache", CreatedAt: nowString(f.now), UpdatedAt: nowString(f.now), ResourcesJSON: "{}", ResourceReservationJSON: "{}"}
	if err := f.store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	request := protocol.Request{"protocol": 4, "action": "acquire", "session_id": task.SessionID, "generation": int64(1), "operation_id": "bad-acquire"}
	digest, _ := protocol.Digest(request)
	old := int64(1)
	if err := f.store.RecordOperation(ctx, "bad-acquire", "acquire", &task.SessionID, &old, map[string]any{"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest, "_task_generation": old, "_task_id": task.TaskID, "worker": map[string]any{"state": "exited", "start_attempted": false}}); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.Dispatch(ctx, protocol.Request{"protocol": 4, "action": "admin-reconcile", "session_id": "operator", "generation": 0, "operation_id": "global-reconcile", "apply": true})
	if err == nil {
		t.Fatal("invalid binding was accepted")
	}
	current, _ := f.store.GetTask(ctx, map[string]any{"session_id": "target"})
	if current.State != stateCached {
		t.Fatalf("unrelated expired target was not closed: %s (%v)", current.State, err)
	}
}

func TestGlobalGarbageCollectionPreviewDoesNotRequireOperation(t *testing.T) {
	f := newScopedFixture(t)
	response, err := f.service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version, "action": "admin-gc", "session_id": "operator", "generation": int64(0), "apply": false,
	})
	if err != nil {
		t.Fatalf("read-only admin gc preview failed: %v", err)
	}
	if response == nil {
		t.Fatal("read-only admin gc preview returned no response")
	}
}

func newCachedReleaseFixture(t *testing.T, mutations ...func(map[string]any)) *scopedFixture {
	f := newScopedFixture(t)
	ctx := context.Background()
	task := state.Task{TaskID: "cached-release", SessionID: "cached-release", Generation: 4, State: stateCached, InstanceName: "instance-cached-release", CreatedAt: nowString(f.now), UpdatedAt: nowString(f.now), ResourcesJSON: "{}", ResourceReservationJSON: "{}"}
	if err := f.store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	f.guest.tasks[task.InstanceName] = task
	f.guest.stopped = append(f.guest.stopped, task.InstanceName)
	request := protocol.Request{"protocol": 4, "action": "release", "session_id": task.SessionID, "generation": task.Generation, "operation_id": "cached-release"}
	digest, _ := protocol.Digest(request)
	response := map[string]any{"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest, "worker": map[string]any{"state": "finished", "pid": int64(2147483647), "process_group": int64(2147483647), "start_time": "original"}}
	for _, mutate := range mutations {
		mutate(response)
	}
	if err := f.store.RecordOperation(ctx, "cached-release", "release", &task.SessionID, &task.Generation, response); err != nil {
		t.Fatal(err)
	}

	return f
}

func TestMaintenanceFinalizesVerifiedCachedRelease(t *testing.T) {
	f := newCachedReleaseFixture(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		_ = f.service.RunMaintenance(ctx, maintenanceReconcileLane)
		f.now = f.now.Add(65 * time.Second)
	}
	op, _ := f.store.GetOperation(ctx, "cached-release")
	if op.Response["outcome"] == "pending" {
		t.Fatal("verified cached release journal remains pending")
	}
	worker, _ := op.Response["worker"].(map[string]any)
	if worker["start_time"] != "original" {
		t.Fatal("original worker evidence lost")
	}
}

func TestMaintenanceFailureClosesAdmissionAndKeepsRepairRunning(t *testing.T) {
	f := newScopedFixture(t)
	ctx := context.Background()
	session := "blocked"
	generation := int64(1)
	if err := f.store.RecordOperation(ctx, "blocked", "release", &session, &generation, map[string]any{"outcome": "pending", "worker": map[string]any{"state": "exited", "start_attempted": false}}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.RunMaintenance(ctx, maintenanceReconcileLane); err == nil {
		t.Fatal("unverified operation was hidden")
	}
	health, err := f.service.doctor(ctx)
	if err != nil || health["ready"] != false || !f.service.Ready() {
		t.Fatalf("health must fail without resetting scheduler startup: %#v %v", health, err)
	}
	metrics, err := f.service.adminMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	doctor, ok := metrics["doctor"].(map[string]any)
	if !ok || doctor["ready"] != false {
		t.Fatalf("admin metrics doctor health ignored maintenance failure: %#v", metrics["doctor"])
	}
	_, err = f.service.acquire(ctx, protocol.Request{"session_id": "new", "generation": 0})
	if protocol.AsError(err).Code != "maintenance_unhealthy" {
		t.Fatalf("new admission was not fenced: %v", err)
	}
	_, err = f.service.restart(ctx, protocol.Request{"session_id": "active-peer", "generation": 2})
	if protocol.AsError(err).Code != "maintenance_unhealthy" {
		t.Fatalf("restart was not fenced: %v", err)
	}
	if _, err := f.service.status(ctx, "active-peer"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.renew(ctx, protocol.Request{"session_id": "active-peer", "generation": 2}); err != nil {
		t.Fatalf("maintenance failure blocked valid lease renewal: %v", err)
	}
	// An explicit, verified repair is simulated here; maintenance itself must
	// never discard the unverified record just to open admission.
	if err := f.store.RecordOperation(ctx, "blocked", "release", &session, &generation, map[string]any{"outcome": "error", "error": protocol.NewError("lease_released", "fixture repair", nil)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		f.now = f.now.Add(65 * time.Second)
		_ = f.service.RunMaintenance(ctx, maintenanceReconcileLane)
	}
	if err := f.service.maintenanceAdmission(ctx); err != nil {
		t.Fatalf("clean repair cycles did not restore admission: %v", err)
	}
}

func TestMaintenanceAdmissionFencePreventsDispatchJournal(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		action     string
		session    string
		generation int64
	}{
		{name: "acquire", action: "acquire", session: "new", generation: 0},
		{name: "restart", action: "restart", session: "active-peer", generation: 2},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			f := newScopedFixture(t)
			ctx := context.Background()
			cursor, err := f.store.MaintenanceCursor(ctx)
			if err != nil {
				t.Fatal(err)
			}
			cursor.Reconcile.LastErrorCode = "operation_unverified"
			cursor.Reconcile.LastErrorPhase = maintenanceReconcileLane
			cursor.Reconcile.CycleFailed = true
			if err := f.store.SetMaintenanceCursor(ctx, cursor); err != nil {
				t.Fatal(err)
			}

			operationID := "blocked-" + testCase.name
			request := protocol.Request{
				"protocol":     protocol.Version,
				"action":       testCase.action,
				"session_id":   testCase.session,
				"generation":   testCase.generation,
				"operation_id": operationID,
			}
			_, err = f.service.Dispatch(ctx, request)
			if protocol.AsError(err).Code != "maintenance_unhealthy" {
				t.Fatalf("dispatch error = %v", err)
			}
			operation, err := f.store.GetOperation(ctx, operationID)
			if err != nil {
				t.Fatal(err)
			}
			if operation != nil {
				t.Fatalf("maintenance fence claimed an operation: %#v", operation)
			}
		})
	}
}

func TestRecoveryFinalizesUnattemptedRestartBehindMaintenanceFence(t *testing.T) {
	f := newScopedFixture(t)
	ctx := context.Background()
	cursor, err := f.store.MaintenanceCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cursor.Reconcile.LastErrorCode = "operation_unverified"
	cursor.Reconcile.LastErrorPhase = maintenanceReconcileLane
	cursor.Reconcile.CycleFailed = true
	if err := f.store.SetMaintenanceCursor(ctx, cursor); err != nil {
		t.Fatal(err)
	}

	session, generation, operationID := "active-peer", int64(2), "old-restart"
	request := protocol.Request{
		"protocol":     protocol.Version,
		"action":       "restart",
		"session_id":   session,
		"generation":   generation,
		"operation_id": operationID,
	}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordOperation(ctx, operationID, "restart", &session, &generation, map[string]any{
		"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest,
		"worker": map[string]any{"state": "starting", "start_attempted": false},
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.reconcilePendingWorker(ctx, operationID); err != nil {
		t.Fatal(err)
	}
	pending, err := f.store.PendingOperations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.recoverPending(ctx, pending); err != nil {
		t.Fatal(err)
	}
	operation, err := f.store.GetOperation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "error" {
		t.Fatalf("unattempted restart remained unresolved: %#v", operation)
	}
	control, ok := operation.Response["error"].(map[string]any)
	if !ok || control["code"] != "maintenance_unhealthy" {
		t.Fatalf("recovered restart error = %#v", operation.Response["error"])
	}
}

func TestResizeAllowRestartRollsBackReservationWhenMaintenanceIsUnhealthy(t *testing.T) {
	f := newScopedFixture(t)
	f.service.Config.StoragePath = ""
	ctx := context.Background()
	cursor, err := f.store.MaintenanceCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cursor.Reconcile.LastErrorCode = "operation_unverified"
	cursor.Reconcile.LastErrorPhase = maintenanceReconcileLane
	cursor.Reconcile.CycleFailed = true
	if err := f.store.SetMaintenanceCursor(ctx, cursor); err != nil {
		t.Fatal(err)
	}

	_, err = f.service.resize(ctx, protocol.Request{
		"session_id": "active-peer", "generation": int64(2), "allow_restart": true,
		"memory_mib": int64(10240),
	})
	if protocol.AsError(err).Code != "maintenance_unhealthy" {
		t.Fatalf("resize error = %v", err)
	}
	task, err := f.store.GetTask(ctx, map[string]any{"session_id": "active-peer"})
	if err != nil {
		t.Fatal(err)
	}
	if task.ResourceReservationJSON != "{}" || task.ReservationBytes != 0 {
		t.Fatalf("maintenance-fenced resize left reservation: %#v", task)
	}
}

func TestMaintenanceFenceAllowsExistingOperationReplay(t *testing.T) {
	f := newScopedFixture(t)
	ctx := context.Background()
	cursor, err := f.store.MaintenanceCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cursor.Reconcile.LastErrorCode = "operation_unverified"
	cursor.Reconcile.LastErrorPhase = maintenanceReconcileLane
	cursor.Reconcile.CycleFailed = true
	if err := f.store.SetMaintenanceCursor(ctx, cursor); err != nil {
		t.Fatal(err)
	}
	request := protocol.Request{"protocol": protocol.Version, "action": "restart", "session_id": "active-peer", "generation": int64(2), "operation_id": "replay-restart"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"task_id": "active-peer", "session_id": "active-peer", "generation": int64(2), "state": stateActive, "effective_lease": true}
	response := protocol.Success(request, result)
	response["_request"] = map[string]any(request)
	response["_request_digest"] = digest
	if err := f.store.RecordOperation(ctx, "replay-restart", "restart", stringPointer("active-peer"), generationPointer(2, true), response); err != nil {
		t.Fatal(err)
	}
	got, err := f.service.Dispatch(ctx, request)
	if err != nil {
		t.Fatalf("existing operation replay was fenced: %v", err)
	}
	if got["operation_id"] != "replay-restart" || got["outcome"] != "replayed" {
		t.Fatalf("unexpected replay response: %#v", got)
	}
}

func TestCachedReleaseRecoveryRejectsUnverifiedEvidence(t *testing.T) {
	for _, scenario := range []string{"digest", "binding", "request", "worker", "generation", "running", "lease", "reservation", "ports", "native-owner", "native-unavailable", "native-proxy"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f := newCachedReleaseFixture(t, func(r map[string]any) {
				switch scenario {
				case "digest":
					r["_request_digest"] = "corrupt"
				case "binding":
					r["_task_id"] = "foreign"
				case "request":
					r["_request"].(map[string]any)["session_id"] = "foreign"
				case "worker":
					r["worker"] = map[string]any{"state": "starting", "start_attempted": true}
				}
			})
			updates := map[string]any{}
			switch scenario {
			case "generation":
				updates["generation"] = int64(5)
			case "running":
				f.guest.stopped = nil
			case "lease":
				updates["lease_expires_at"] = nowString(f.now.Add(time.Hour))
			case "reservation":
				updates["reservation_bytes"] = int64(1)
			case "ports":
				if err := f.store.UpsertPort(ctx, state.Port{TaskID: "cached-release", Generation: 4, PortName: "ssh", Protocol: "tcp", HostPort: 20010, VMPort: 22, DeviceName: "proxy-ssh-tcp", State: "active"}); err != nil {
					t.Fatal(err)
				}
			case "native-owner":
				task := f.guest.tasks["instance-cached-release"]
				task.Generation = 5
				f.guest.tasks[task.InstanceName] = task
			case "native-unavailable":
				f.service.Guest = nil
			case "native-proxy":
				f.service.Guest = &proxyCachedGuest{f.guest}
			}
			if len(updates) > 0 {
				if err := f.store.UpdateTask(ctx, "cached-release", updates); err != nil {
					t.Fatal(err)
				}
			}
			_ = f.service.reconcileCachedRelease(ctx, "cached-release")
			op, _ := f.store.GetOperation(ctx, "cached-release")
			if op.Response["outcome"] != "pending" {
				t.Fatalf("unsafe release was finalized: %#v", op.Response)
			}
		})
	}
}

func TestMaintenanceScansPastFullPageOfUnverifiedJournals(t *testing.T) {
	f := newScopedFixture(t)
	ctx := context.Background()
	generation := int64(1)
	for i := 0; i < 8; i++ {
		session := fmt.Sprintf("blocked-%02d", i)
		if err := f.store.RecordOperation(ctx, session, "release", &session, &generation, map[string]any{"outcome": "pending", "worker": map[string]any{"state": "exited", "start_attempted": false}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i++ {
		_ = f.service.RunMaintenance(ctx, maintenanceReconcileLane)
		f.now = f.now.Add(65 * time.Second)
	}
	for _, session := range []string{"target", "expired-peer"} {
		task, _ := f.store.GetTask(ctx, map[string]any{"session_id": session})
		if task.State != stateCached {
			t.Fatalf("full pending page starved %s: %s", session, task.State)
		}
	}
	if f.service.maintenanceAdmission(ctx) == nil {
		t.Fatal("other successful pages hid unresolved maintenance failure")
	}
}

type proxyCachedGuest struct{ *scopedGuest }

func (g *proxyCachedGuest) Config(ctx context.Context, name string) (map[string]any, error) {
	info, err := g.scopedGuest.Config(ctx, name)
	if err == nil && name == "instance-cached-release" {
		info["expanded_devices"] = map[string]any{"foreign-proxy": map[string]any{"type": "proxy"}}
	}
	return info, err
}
