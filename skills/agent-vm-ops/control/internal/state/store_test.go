package state

import (
	"context"
	"path/filepath"
	"testing"
)

func TestExistingSchemaLifecycleAndOpaqueEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite3")
	store, err := OpenForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	epoch, err := store.StateEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if epoch != "initial" {
		t.Fatalf("initial epoch = %q", epoch)
	}
	task := Task{TaskID: "avm-test", SessionID: "session", Generation: 1, State: "active", InstanceName: "agent-vm-avm-test", CreatedAt: "2026-10-26T00:00:00+00:00", UpdatedAt: "2026-10-26T00:00:00+00:00", ImageFingerprint: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`}
	if err := store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	changed, err := store.StateEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed == "initial" || len(changed) < 10 {
		t.Fatalf("epoch did not advance opaquely: %q", changed)
	}
	if err := store.SetMeta(ctx, "same", "value"); err != nil {
		t.Fatal(err)
	}
	metaEpoch, err := store.StateEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMeta(ctx, "same", "value"); err != nil {
		t.Fatal(err)
	}
	unchanged, err := store.StateEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != metaEpoch {
		t.Fatalf("no-op metadata write advanced epoch: %q -> %q", metaEpoch, unchanged)
	}
	if err := store.UpdateTask(ctx, "avm-test", map[string]any{"state": "active", "updated_at": "2026-10-26T00:00:00+00:00"}); err != nil {
		t.Fatal(err)
	}
	unchanged, err = store.StateEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged != metaEpoch {
		t.Fatalf("no-op task write advanced epoch: %q -> %q", metaEpoch, unchanged)
	}
	loaded, err := store.GetTask(ctx, map[string]any{"session_id": "session"})
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.Generation != 1 || loaded.State != "active" {
		t.Fatalf("loaded task = %#v", loaded)
	}
	op := map[string]any{"outcome": "pending", "_request_digest": "digest"}
	if err := store.RecordOperation(ctx, "op", "acquire", stringPointer("session"), intPointer(1), op); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingOperations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].OperationID != "op" {
		t.Fatalf("pending = %#v", pending)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.GetOperation(ctx, "op"); err != nil {
		t.Fatal(err)
	}
}

func TestMissingDatabaseFailsClosed(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "missing.sqlite3"), false)
	if err == nil {
		t.Fatal("missing database unexpectedly opened")
	}
}

func stringPointer(value string) *string { return &value }
func intPointer(value int64) *int64      { return &value }
