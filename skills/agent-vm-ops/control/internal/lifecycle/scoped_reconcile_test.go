package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

type scopedGuest struct {
	*startupGuest
	stopped    []string
	removed    []string
	tasks      map[string]state.Task
	realWorker bool
}

func (g *scopedGuest) Config(_ context.Context, name string) (map[string]any, error) {
	task := g.tasks[name]
	return map[string]any{"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": task.TaskID, "user.agent-vm.session": task.SessionID, "user.agent-vm.generation": fmt.Sprint(task.Generation)}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}, nil
}
func (g *scopedGuest) Status(_ context.Context, name string) (map[string]any, error) {
	for _, n := range g.stopped {
		if n == name {
			return map[string]any{"status": "Stopped"}, nil
		}
	}
	return map[string]any{"status": "Running"}, nil
}
func (g *scopedGuest) Stop(ctx context.Context, name string) error {
	if g.stopErr != nil {
		return g.stopErr
	}
	if g.realWorker {
		if _, err := native.NewRunner().Run(ctx, []string{"/bin/sh", "-c", "sleep 0.03"}, nil); err != nil {
			return err
		}
	}
	g.stopped = append(g.stopped, name)
	return nil
}

func TestScopedReconcileNativeWorkerBelongsToMaintenanceNotAcquire(t *testing.T) {
	f := newScopedFixture(t)
	f.guest.realWorker = true
	if _, err := f.service.Dispatch(context.Background(), f.apply()); err != nil {
		t.Fatal(err)
	}
	maintenance, _ := f.store.GetOperation(context.Background(), "scoped-recovery")
	worker := maintenance.Response["worker"].(map[string]any)
	if worker["state"] != "finished" || worker["pid"] == nil || worker["start_time"] == nil {
		t.Fatalf("cleanup worker not journaled: %#v", worker)
	}
	acquire, _ := f.store.GetOperation(context.Background(), "pending-target")
	original := acquire.Response["worker"].(map[string]any)
	if original["pid"] != nil || original["start_attempted"] != false {
		t.Fatalf("cleanup replayed acquire worker: %#v", original)
	}
}
func (g *scopedGuest) DeviceRemove(_ context.Context, name, device string) error {
	g.removed = append(g.removed, name+":"+device)
	return nil
}

type scopedFixture struct {
	service *Service
	store   *state.Store
	guest   *scopedGuest
	now     time.Time
	request protocol.Request
}

func newScopedFixture(t *testing.T) *scopedFixture {
	t.Helper()
	ctx := context.Background()
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "scope.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f := &scopedFixture{store: store, now: time.Date(2026, 11, 7, 8, 40, 0, 0, time.UTC)}
	f.guest = &scopedGuest{startupGuest: &startupGuest{}, tasks: map[string]state.Task{}}
	for _, name := range []string{"target", "expired-peer", "active-peer"} {
		deadline := nowString(f.now.Add(-time.Minute))
		if name == "active-peer" {
			deadline = nowString(f.now.Add(time.Hour))
		}
		task := state.Task{TaskID: name, SessionID: name, Generation: 2, State: stateActive, InstanceName: "instance-" + name, LeaseExpiresAt: &deadline, CreatedAt: nowString(f.now), UpdatedAt: nowString(f.now), ResourcesJSON: `{"cpu":4,"memory_mib":8192,"disk_gib":30}`, ResourceReservationJSON: `{}`}
		if err := store.InsertTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		f.guest.tasks[task.InstanceName] = task
		if err := store.UpsertPort(ctx, state.Port{TaskID: name, Generation: 2, PortName: "ssh", Protocol: "tcp", HostPort: 20000 + int64(len(f.guest.tasks)), VMPort: 22, DeviceName: "proxy-ssh-tcp", State: "active", CreatedAt: nowString(f.now)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"target", "expired-peer"} {
		request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": name, "generation": int64(2), "operation_id": "pending-" + name}
		digest, _ := protocol.Digest(request)
		if err := store.RecordOperation(ctx, "pending-"+name, "acquire", stringPointer(name), generationPointer(2, true), map[string]any{"outcome": "pending", "_request_digest": digest, "_request": map[string]any(request), "worker": map[string]any{"state": "starting", "start_attempted": false, "prepared_at": nowString(f.now)}}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.StoragePath = filepath.Join(t.TempDir(), "storage")
	cfg.Network = ""
	cfg.NetworkACL = ""
	f.service = New(store, cfg, Options{Guest: f.guest, Clock: func() time.Time { return f.now }})
	f.service.MarkReady()
	f.request = protocol.Request{"protocol": protocol.Version, "action": "admin-reconcile-session", "session_id": "target", "task_id": "target", "generation": int64(2), "pending_operation_id": "pending-target", "apply": false}
	return f
}
func (f *scopedFixture) apply() protocol.Request {
	r := cloneMap(f.request)
	r["apply"] = true
	r["operation_id"] = "scoped-recovery"
	return protocol.Request(r)
}
func (f *scopedFixture) snapshot(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	tasks, err := f.store.ListTasks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	operations, err := f.store.PendingOperations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := f.store.MaintenanceCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return mustJSON(map[string]any{"tasks": tasks, "pending": operations, "cursor": cursor})
}

func TestScopedReconcileRejectsUnexpiredLeaseWithAuthoritativeDiagnosis(t *testing.T) {
	f := newScopedFixture(t)
	request := cloneMap(f.request)
	request["task_id"] = "active-peer"
	request["session_id"] = "active-peer"
	request["pending_operation_id"] = "pending-target"
	err := func() error {
		_, err := f.service.Dispatch(context.Background(), protocol.Request(request))
		return err
	}()
	control := protocol.AsError(err)
	if control.Code != "stale_target" {
		t.Fatalf("code=%s want stale_target", control.Code)
	}
	detail, ok := control.Detail.(map[string]any)
	if !ok {
		t.Fatalf("detail=%#v", control.Detail)
	}
	diagnosis, ok := detail["diagnosis"].(map[string]any)
	if !ok || diagnosis["recovery_allowed"] != false || diagnosis["recovery_blocker"] != "lease_unexpired" || diagnosis["lease_expires_at"] != nowString(f.now.Add(time.Hour)) {
		t.Fatalf("diagnosis=%#v", detail["diagnosis"])
	}
}

func TestScopedReconcileDryRunApplyAndReplayKeepPeerAndCursor(t *testing.T) {
	f := newScopedFixture(t)
	ctx := context.Background()
	before := f.snapshot(t)
	wakes := 0
	f.service.SetWake(func() { wakes++ })
	if _, err := f.service.Dispatch(ctx, f.request); err != nil {
		t.Fatal(err)
	}
	if after := f.snapshot(t); before != after {
		t.Fatal("dry run mutated task, journal or cursor")
	}
	peerBefore, _ := f.store.GetTask(ctx, map[string]any{"session_id": "expired-peer"})
	activeBefore, _ := f.store.GetTask(ctx, map[string]any{"session_id": "active-peer"})
	pendingPeerBefore, _ := f.store.GetOperation(ctx, "pending-expired-peer")
	cursorBefore, _ := f.store.MaintenanceCursor(ctx)
	request := f.apply()
	response, err := f.service.Dispatch(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if response["ok"] != true {
		t.Fatalf("response: %#v", response)
	}
	task, _ := f.store.GetTask(ctx, map[string]any{"session_id": "target"})
	if task.State != stateCached || task.Generation != 2 || task.LeaseExpiresAt != nil || task.ResourcesJSON != f.guest.tasks[task.InstanceName].ResourcesJSON {
		t.Fatalf("cache state: %#v", task)
	}
	ports, _ := f.store.ListPorts(ctx, "target")
	if len(ports) != 0 {
		t.Fatalf("target ports remain: %#v", ports)
	}
	operation, _ := f.store.GetOperation(ctx, "pending-target")
	if operation.Response["outcome"] != "error" || operation.Response["_worker_recovery_evidence"] == nil {
		t.Fatalf("pending operation not terminal with evidence: %#v", operation)
	}
	evidence := operation.Response["_worker_recovery_evidence"].(map[string]any)
	prior := evidence["worker"].(map[string]any)
	if prior["state"] != "starting" || prior["start_attempted"] != false {
		t.Fatalf("prior worker lost: %#v", prior)
	}
	for _, item := range []struct {
		name   string
		before *state.Task
	}{{"expired-peer", peerBefore}, {"active-peer", activeBefore}} {
		after, _ := f.store.GetTask(ctx, map[string]any{"session_id": item.name})
		if !reflect.DeepEqual(item.before, after) {
			t.Fatalf("changed peer %s", item.name)
		}
		ports, _ := f.store.ListPorts(ctx, item.name)
		if len(ports) != 1 {
			t.Fatal("peer mapping changed")
		}
	}
	peerAfter, _ := f.store.GetOperation(ctx, "pending-expired-peer")
	cursorAfter, _ := f.store.MaintenanceCursor(ctx)
	if !reflect.DeepEqual(pendingPeerBefore, peerAfter) || !reflect.DeepEqual(cursorBefore, cursorAfter) {
		t.Fatal("changed peer journal or maintenance cursor")
	}
	if _, err := f.service.Dispatch(ctx, request); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.guest.stopped, []string{"instance-target"}) || f.guest.startCalls != 0 || f.guest.deleteCalls != 0 || wakes != 0 {
		t.Fatalf("unexpected native effects or scheduler wake: stopped=%v start=%d delete=%d wakes=%d", f.guest.stopped, f.guest.startCalls, f.guest.deleteCalls, wakes)
	}
}

func TestScopedReconcileRefusesUnsafeOrStaleTargetBeforeEffects(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		change func(*scopedFixture)
	}{
		{"active-lease", func(f *scopedFixture) {
			d := nowString(f.now.Add(time.Hour))
			_ = f.store.UpdateTask(context.Background(), "target", map[string]any{"lease_expires_at": d})
		}},
		{"wrong-generation", func(f *scopedFixture) { f.request["generation"] = int64(1) }},
		{"wrong-task", func(f *scopedFixture) { f.request["task_id"] = "expired-peer" }},
		{"foreign-native-generation", func(f *scopedFixture) {
			task := f.guest.tasks["instance-target"]
			task.Generation = 3
			f.guest.tasks["instance-target"] = task
		}},
		{"missing-lease-deadline", func(f *scopedFixture) {
			_ = f.store.UpdateTask(context.Background(), "target", map[string]any{"lease_expires_at": nil})
		}},
		{"cached-running-guest", func(f *scopedFixture) {
			_ = f.store.UpdateTask(context.Background(), "target", map[string]any{"state": stateCached, "lease_expires_at": nil})
		}},
		{"other-pending-in-session", func(f *scopedFixture) {
			_ = f.store.RecordOperation(context.Background(), "other-pending", "release", stringPointer("target"), generationPointer(2, true), map[string]any{"outcome": "pending"})
		}},
		{"foreign-operation", func(f *scopedFixture) { f.request["pending_operation_id"] = "pending-expired-peer" }},
		{"attempted-worker", func(f *scopedFixture) {
			_ = f.store.UpdateOperationResponse(context.Background(), "pending-target", func(r map[string]any) error { r["worker"].(map[string]any)["start_attempted"] = true; return nil })
		}},
		{"missing-attempt-evidence", func(f *scopedFixture) {
			_ = f.store.UpdateOperationResponse(context.Background(), "pending-target", func(r map[string]any) error { delete(r["worker"].(map[string]any), "start_attempted"); return nil })
		}},
		{"contradictory-worker", func(f *scopedFixture) {
			_ = f.store.UpdateOperationResponse(context.Background(), "pending-target", func(r map[string]any) error { r["worker"].(map[string]any)["pid"] = int64(123); return nil })
		}},
		{"bad-digest", func(f *scopedFixture) {
			_ = f.store.UpdateOperationResponse(context.Background(), "pending-target", func(r map[string]any) error { r["_request"].(map[string]any)["cpu"] = int64(99); return nil })
		}},
		{"foreign-recovery-evidence", func(f *scopedFixture) {
			_ = f.store.UpdateOperationResponse(context.Background(), "pending-target", func(r map[string]any) error {
				r["_worker_recovery_evidence"] = map[string]any{"kind": "foreign"}
				return nil
			})
		}},
		{"active-operation", func(f *scopedFixture) { done := f.service.markOperationActive("pending-target"); t.Cleanup(done) }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newScopedFixture(t)
			scenario.change(f)
			tasksBefore, _ := f.store.ListTasks(context.Background())
			pendingBefore, _ := f.store.GetOperation(context.Background(), "pending-target")
			if _, err := f.service.Dispatch(context.Background(), f.apply()); err == nil {
				t.Fatal("unsafe target accepted")
			}
			tasksAfter, _ := f.store.ListTasks(context.Background())
			pendingAfter, _ := f.store.GetOperation(context.Background(), "pending-target")
			if !reflect.DeepEqual(tasksBefore, tasksAfter) || !reflect.DeepEqual(pendingBefore, pendingAfter) || len(f.guest.stopped) != 0 {
				t.Fatal("unsafe request changed target or spawned native effects")
			}
		})
	}
}

func TestScopedReconcileCleanupFailureRetainsEvidenceAndCacheIdentity(t *testing.T) {
	f := newScopedFixture(t)
	f.guest.stopErr = errors.New("stop unverified")
	_, err := f.service.Dispatch(context.Background(), f.apply())
	if err == nil || protocol.AsError(err).Code != "cleanup_incomplete" {
		t.Fatalf("expected incomplete cleanup: %v", err)
	}
	op, _ := f.store.GetOperation(context.Background(), "pending-target")
	if op.Response["outcome"] != "pending" || op.Response["_worker_recovery_evidence"] == nil {
		t.Fatal("cleanup failure lost pending journal evidence")
	}
	task, _ := f.store.GetTask(context.Background(), map[string]any{"session_id": "target"})
	if task == nil || task.State != stateClosing || f.guest.deleteCalls != 0 {
		t.Fatal("uncertain cache was deleted or marked recovered")
	}
}

func TestScopedReconcileSessionLockAcrossServices(t *testing.T) {
	f := newScopedFixture(t)
	other := New(f.store, f.service.Config, Options{Guest: f.guest, Clock: func() time.Time { return f.now }})
	other.MarkReady()
	unlock, err := f.service.lockActionWithContext(context.Background(), "session", protocol.Request{"session_id": "target"})
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	before := f.snapshot(t)
	if _, err := other.Dispatch(ctx, f.request); err == nil {
		t.Fatal("scoped reconciliation bypassed another service's target-session lock")
	}
	if f.snapshot(t) != before || len(f.guest.stopped) != 0 {
		t.Fatal("contended dry run mutated state")
	}
}

func TestScopedReconcileSessionLockAcrossProcesses(t *testing.T) {
	if path := os.Getenv("AGENT_VM_SCOPED_LOCK_DB"); path != "" {
		store, err := state.Open(path, false)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		cfg := config.Defaults()
		cfg.StoragePath = os.Getenv("AGENT_VM_SCOPED_LOCK_STORAGE")
		service := New(store, cfg, Options{})
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		request := protocol.Request{"protocol": protocol.Version, "action": "admin-reconcile-session", "session_id": "target", "task_id": "target", "generation": int64(2), "pending_operation_id": "pending-target", "apply": false}
		if _, err := service.Dispatch(ctx, request); err == nil || protocol.AsError(err).Code != "operation_conflict" {
			t.Fatalf("cross-process lock bypass: %v", err)
		}
		return
	}
	f := newScopedFixture(t)
	unlock, err := f.service.lockActionWithContext(context.Background(), "session", protocol.Request{"session_id": "target"})
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	before := f.snapshot(t)
	command := exec.Command(os.Args[0], "-test.run=^TestScopedReconcileSessionLockAcrossProcesses$")
	command.Env = append(os.Environ(), "AGENT_VM_SCOPED_LOCK_DB="+f.store.Path(), "AGENT_VM_SCOPED_LOCK_STORAGE="+f.service.Config.StoragePath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("lock helper: %v %s", err, output)
	}
	if f.snapshot(t) != before {
		t.Fatal("contended process changed state")
	}
}

func TestScopedReconcileProtocolRequiresBoundScope(t *testing.T) {
	f := newScopedFixture(t)
	for _, key := range []string{"session_id", "task_id", "generation", "pending_operation_id", "apply"} {
		r := protocol.Request(cloneMap(f.request))
		delete(r, key)
		if err := protocol.ValidateRequest(r); err == nil {
			t.Fatalf("accepted missing %s", key)
		}
	}
	r := f.apply()
	r["pending_operation_id"] = r["operation_id"]
	if err := protocol.ValidateRequest(r); err == nil {
		t.Fatal("accepted self-targeting journal identity")
	}
	// A peer without the new action refuses it before reaching any mutation.
	f.service.Release = &protocol.ReleaseMetadata{Actions: map[string]protocol.ActionCompatibility{}}
	before := f.snapshot(t)
	if _, err := f.service.Dispatch(context.Background(), f.apply()); err == nil {
		t.Fatal("missing action capability silently downgraded")
	}
	if f.snapshot(t) != before {
		t.Fatal("incompatible action reached mutation")
	}
}
