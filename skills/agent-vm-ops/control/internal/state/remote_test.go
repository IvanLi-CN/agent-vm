package state

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRemoteMigrationPreservesStateAndSerializesConcurrentOperations(t *testing.T) {
	rqliteURL := os.Getenv("AGENT_VM_RQLITE_URL")
	if rqliteURL == "" {
		t.Skip("AGENT_VM_RQLITE_URL is required for the rqlite integration contract")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	t.Log("creating SQLite source")
	sourcePath := filepath.Join(t.TempDir(), "state.sqlite3")
	source, err := OpenForTest(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := "migration-session"
	generation, err := source.NextGeneration(ctx, sessionID)
	if err != nil {
		source.Close()
		t.Fatal(err)
	}
	if err := source.InsertTask(ctx, Task{
		TaskID:                  "migration-task",
		SessionID:               sessionID,
		Generation:              generation,
		State:                   "active",
		InstanceName:            "agent-vm-migration-task",
		CreatedAt:               "2026-10-29T00:00:00Z",
		UpdatedAt:               "2026-10-29T00:00:00Z",
		ImageFingerprint:        strings.Repeat("a", 64),
		ResourcesJSON:           "{}",
		ResourceReservationJSON: "{}",
	}); err != nil {
		source.Close()
		t.Fatal(err)
	}
	if err := source.SeedLegacyImage(ctx, strings.Repeat("a", 64)); err != nil {
		source.Close()
		t.Fatal(err)
	}
	operationID := "migration-operation"
	if _, err := source.ClaimOperation(ctx, operationID, "acquire", &sessionID, &generation, map[string]any{"outcome": "success"}); err != nil {
		source.Close()
		t.Fatal(err)
	}
	if err := source.Audit(ctx, "acquire", "migration-task", sessionID, "success", &generation, map[string]any{"actor": "legacy_unknown"}); err != nil {
		source.Close()
		t.Fatal(err)
	}
	if _, err := source.ClaimManagementOperation(ctx, ManagementOperation{
		OperationID: "migration-management-operation",
		Action:      "image-import",
		TargetKind:  "image",
		TargetID:    "migration-image",
		Actor:       "migration-test",
		Reason:      "exercise migration fence",
		Phase:       "queued",
		Status:      "queued",
		Request:     map[string]any{"fingerprint": strings.Repeat("a", 64)},
		CreatedAt:   "2026-10-29T00:00:00Z",
		UpdatedAt:   "2026-10-29T00:00:00Z",
	}); err != nil {
		source.Close()
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	if err := MigrateSQLiteToRQLite(ctx, sourcePath, rqliteURL, SQLiteMigrationGate{
		StateWriteFenced:    true,
		WorkersQuiescent:    true,
		SQLiteWritersClosed: true,
	}); err == nil {
		t.Fatal("migration with a queued management operation succeeded")
	}
	source, err = OpenForTest(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.UpdateManagementOperation(ctx, "migration-management-operation", func(value *ManagementOperation) error {
		value.Phase = "completed"
		value.Status = "succeeded"
		value.Result = map[string]any{"outcome": "success"}
		value.UpdatedAt = "2026-10-29T00:00:01Z"
		return nil
	}); err != nil {
		source.Close()
		t.Fatal(err)
	}
	expectedEpoch, err := source.StateEpoch(ctx)
	if err != nil {
		source.Close()
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	if err := MigrateSQLiteToRQLite(ctx, sourcePath, rqliteURL, SQLiteMigrationGate{}); err == nil {
		t.Fatal("migration without all fence proofs succeeded")
	}
	if err := MigrateSQLiteToRQLite(ctx, sourcePath, rqliteURL, SQLiteMigrationGate{
		StateWriteFenced:    true,
		WorkersQuiescent:    true,
		SQLiteWritersClosed: true,
	}); err != nil {
		t.Fatal(err)
	}
	t.Log("migration complete")

	first, err := OpenForTest(rqliteURL)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	t.Log("first remote store open")
	second, err := OpenForTest(rqliteURL)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	t.Log("second remote store open")

	gotEpoch, err := first.StateEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gotEpoch != expectedEpoch {
		t.Fatalf("state epoch changed during migration: got %q want %q", gotEpoch, expectedEpoch)
	}
	gotTask, err := second.GetTask(ctx, map[string]any{"task_id": "migration-task"})
	if err != nil {
		t.Fatal(err)
	}
	if gotTask == nil || gotTask.Generation != generation || gotTask.SessionID != sessionID {
		t.Fatalf("migrated task = %#v", gotTask)
	}
	gotOperation, err := first.GetOperation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOperation == nil || gotOperation.Response["outcome"] != "success" {
		t.Fatalf("migrated operation = %#v", gotOperation)
	}
	t.Log("migration contents verified")
	if _, err := first.remote.request(ctx, []remoteStatement{
		{query: "INSERT INTO meta(key, value) VALUES('remote-atomicity-test', 'first')"},
		{query: "INSERT INTO meta(key, value) VALUES('remote-atomicity-test', 'second')"},
	}, true); err == nil {
		t.Fatal("failing rqlite transaction succeeded")
	}
	if _, exists, err := first.Meta(ctx, "remote-atomicity-test"); err != nil {
		t.Fatal(err)
	} else if exists {
		t.Fatal("failing rqlite transaction left a partial meta row")
	}

	const concurrentAllocations = 8
	generations := make([]int64, concurrentAllocations)
	errorsByWorker := make([]error, concurrentAllocations)
	stores := []*Store{first, second}
	var wait sync.WaitGroup
	for index := 0; index < concurrentAllocations; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			generations[index], errorsByWorker[index] = stores[index%len(stores)].NextGeneration(ctx, "concurrent-session")
		}(index)
	}
	wait.Wait()
	t.Log("concurrent generations complete")
	for index, err := range errorsByWorker {
		if err != nil {
			t.Fatalf("concurrent generation %d: %v", index, err)
		}
	}
	sort.Slice(generations, func(left, right int) bool { return generations[left] < generations[right] })
	for index, value := range generations {
		want := int64(index + 1)
		if value != want {
			t.Fatalf("concurrent generation[%d] = %d, want %d; all=%v", index, value, want, generations)
		}
	}

	condition := TaskCondition{Generation: &generation, States: []string{"active"}}
	deleted := make(chan bool, 2)
	go func() {
		value, err := first.DeleteTaskIf(ctx, "migration-task", condition)
		if err != nil {
			t.Errorf("first conditional delete: %v", err)
		}
		deleted <- value
	}()
	go func() {
		value, err := second.DeleteTaskIf(ctx, "migration-task", condition)
		if err != nil {
			t.Errorf("second conditional delete: %v", err)
		}
		deleted <- value
	}()
	firstDelete, secondDelete := <-deleted, <-deleted
	if firstDelete == secondDelete {
		t.Fatalf("conditional deletes = %v, %v; exactly one writer should win", firstDelete, secondDelete)
	}
	t.Log("conditional deletes complete")
	lastGeneration, exists, err := first.LastGeneration(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || lastGeneration != generation {
		t.Fatalf("generation tombstone = %d, %v; want %d, true", lastGeneration, exists, generation)
	}
}
