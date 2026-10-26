package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/lifecycle"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

func TestSchedulerReconcilesExpiryAndAgeGC(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "scheduler.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	cfg := config.Defaults()
	cfg.CacheSeconds = 0
	service := lifecycle.New(store, cfg, lifecycle.Options{Clock: func() time.Time { return now }})
	if _, err := service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version, "action": "acquire", "session_id": "scheduled", "generation": int64(0), "operation_id": "scheduled-acquire",
	}); err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(context.Background(), map[string]any{"session_id": "scheduled"})
	if err != nil || task == nil {
		t.Fatalf("task = %#v, err = %v", task, err)
	}
	if err := store.UpdateTask(context.Background(), task.TaskID, map[string]any{"lease_expires_at": now.Add(-time.Second).Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}

	instance := New(service)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- instance.run(ctx, 5*time.Millisecond, 5*time.Millisecond) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := store.GetTask(context.Background(), map[string]any{"session_id": "scheduled"})
		if getErr != nil {
			t.Fatal(getErr)
		}
		if current == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	current, err := store.GetTask(context.Background(), map[string]any{"session_id": "scheduled"})
	if err != nil {
		t.Fatal(err)
	}
	if current != nil {
		t.Fatalf("scheduler left task after expiry and age GC: %#v", current)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
