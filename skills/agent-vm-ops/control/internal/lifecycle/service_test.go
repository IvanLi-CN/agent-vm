package lifecycle

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-toolkit/skills/agent-vm-ops/control/internal/state"
)

func TestLeaseJournalAndBuiltInCleanup(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.ImageFingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cfg.CacheSeconds = 60
	service := New(store, cfg, Options{Clock: func() time.Time { return now }})
	ctx := context.Background()
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "demo", "generation": int64(0), "operation_id": "acquire-demo"}
	first, err := service.Dispatch(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first["outcome"] != "success" || first["result"].(map[string]any)["state"] != stateActive {
		t.Fatalf("acquire = %#v", first)
	}
	replay, err := service.Dispatch(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replay["outcome"] != "replayed" {
		t.Fatalf("replay = %#v", replay)
	}
	status, err := service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "status", "session_id": "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if status["result"].(map[string]any)["effective_lease"] != true {
		t.Fatalf("status = %#v", status)
	}
	release, err := service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "release", "session_id": "demo", "generation": int64(1), "operation_id": "release-demo"})
	if err != nil {
		t.Fatal(err)
	}
	if release["result"].(map[string]any)["state"] != stateCached {
		t.Fatalf("release = %#v", release)
	}
	if err := store.UpdateTask(ctx, "avm-", map[string]any{}); err != nil {
		_ = err
	}
	if _, err := service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "drain-enter", "generation": int64(0), "operation_id": "drain"}); err != nil {
		t.Fatal(err)
	}
	_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "blocked", "generation": int64(0), "operation_id": "blocked"})
	if control := protocol.AsError(err); control.Code != "draining" {
		t.Fatalf("drain error = %#v", control)
	}
}

func TestAdmissionAndLeaseTransitionsRemainDurable(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.MemoryBudgetMiB = 4096
	service := New(store, cfg, Options{Clock: func() time.Time { return now }})
	ctx := context.Background()
	_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "first", "generation": int64(0), "operation_id": "acquire-first"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "second", "generation": int64(0), "operation_id": "acquire-second"})
	if control := protocol.AsError(err); control.Code != "resource_insufficient" {
		t.Fatalf("second acquire error = %#v", control)
	}
	if task, getErr := store.GetTask(ctx, map[string]any{"session_id": "second"}); getErr != nil || task != nil {
		t.Fatalf("rejected acquire left durable task: task=%#v err=%v", task, getErr)
	}
	first, err := store.GetTask(ctx, map[string]any{"session_id": "first"})
	if err != nil || first == nil {
		t.Fatalf("first task = %#v err=%v", first, err)
	}
	if err := store.UpdateTask(ctx, first.TaskID, map[string]any{"lease_expires_at": nowString(now.Add(-time.Second))}); err != nil {
		t.Fatal(err)
	}
	_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "renew", "session_id": "first", "generation": first.Generation, "operation_id": "renew-expired"})
	if control := protocol.AsError(err); control.Code != "lease_expired" {
		t.Fatalf("expired renew error = %#v", control)
	}
	result, err := service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "timer-expire", "task_id": first.TaskID, "generation": first.Generation})
	if err != nil {
		t.Fatal(err)
	}
	if result["result"].(map[string]any)["state"] != stateCached {
		t.Fatalf("expired cleanup result = %#v", result)
	}
}

func TestConcurrentOperationRetryDoesNotDuplicateLifecycleMutation(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, config.Defaults(), Options{})
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "concurrent", "generation": int64(0), "operation_id": "same-operation"}
	responses := make(chan map[string]any, 2)
	errors := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			response, dispatchErr := service.Dispatch(context.Background(), request)
			responses <- response
			errors <- dispatchErr
		}()
	}
	group.Wait()
	close(responses)
	close(errors)
	successes, replays := 0, 0
	for response := range responses {
		if response == nil {
			continue
		}
		switch response["outcome"] {
		case "success":
			successes++
		case "replayed":
			replays++
		}
	}
	for dispatchErr := range errors {
		if dispatchErr != nil {
			t.Fatalf("concurrent dispatch error = %v", dispatchErr)
		}
	}
	if successes != 1 || replays != 1 {
		t.Fatalf("successes=%d replays=%d", successes, replays)
	}
}

func TestResidentServiceRefusesAdmissionBeforeStartupReconcile(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, config.Defaults(), Options{Resident: true})
	_, err = service.Dispatch(context.Background(), protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "before-ready", "generation": int64(0), "operation_id": "before-ready"})
	if control := protocol.AsError(err); control.Code != "startup_reconcile_required" {
		t.Fatalf("not-ready error = %#v", control)
	}
	if service.Ready() {
		t.Fatal("service became ready before startup reconciliation")
	}
}
