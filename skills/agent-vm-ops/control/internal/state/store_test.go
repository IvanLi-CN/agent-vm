package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestLeaseConditionsPreserveSubSecondPrecision(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	base := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	lease := base.Add(900 * time.Nanosecond)
	task := Task{TaskID: "subsecond", SessionID: "subsecond", Generation: 1, State: "active", InstanceName: "agent-vm-subsecond", CreatedAt: base.Format(time.RFC3339Nano), UpdatedAt: base.Format(time.RFC3339Nano), ImageFingerprint: strings.Repeat("a", 64), ResourcesJSON: `{"cpu":0,"memory_mib":0,"disk_gib":0}`, ResourceReservationJSON: `{}`, LeaseExpiresAt: stringPointer(lease.Format(time.RFC3339Nano))}
	if err := store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	activeAfter := base.Add(901 * time.Nanosecond)
	changed, err := store.UpdateTaskIf(ctx, task.TaskID, TaskCondition{LeaseActiveAfter: &activeAfter}, map[string]any{"state": "closing"})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("lease was treated as active after its fractional deadline")
	}
	expiredAt := base.Add(899 * time.Nanosecond)
	changed, err = store.UpdateTaskIf(ctx, task.TaskID, TaskCondition{LeaseExpiredAt: &expiredAt}, map[string]any{"state": "closing"})
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("lease was treated as expired before its fractional deadline")
	}
	expiredAt = base.Add(900 * time.Nanosecond)
	changed, err = store.UpdateTaskIf(ctx, task.TaskID, TaskCondition{LeaseExpiredAt: &expiredAt}, map[string]any{"state": "closing"})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("lease was not treated as expired after its fractional deadline")
	}
}

func TestLegacyBaselineGetsInitialEpochOnReadOnlyOpen(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("..", "..", "..", "tests", "fixtures", "control-baseline.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "legacy.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(fixture)); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	epoch, err := store.StateEpoch(context.Background())
	if err != nil || epoch != "initial" {
		t.Fatalf("legacy epoch = %q, err=%v", epoch, err)
	}
}

func TestStateEpochRejectsMalformedValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "epoch.sqlite3")
	store, err := OpenForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE meta SET value = 'not-an-epoch' WHERE key = 'state_epoch'"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.StateEpoch(context.Background())
	var epochErr *StateEpochError
	if !errors.As(err, &epochErr) {
		t.Fatalf("epoch error = %v", err)
	}
}

func TestClaimOperationIsAtomicAcrossConcurrentRetries(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "operations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	pending := map[string]any{"outcome": "pending", "_request_digest": "digest"}
	var group sync.WaitGroup
	var mu sync.Mutex
	claims := 0
	seenExisting := 0
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			existing, claimErr := store.ClaimOperation(context.Background(), "same", "acquire", stringPointer("session"), intPointer(0), pending)
			if claimErr != nil {
				t.Errorf("claim: %v", claimErr)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if existing == nil {
				claims++
			} else {
				seenExisting++
			}
		}()
	}
	group.Wait()
	if claims != 1 || seenExisting != 1 {
		t.Fatalf("claims=%d existing=%d", claims, seenExisting)
	}
}

func TestTransactionErrorRetainsRollbackPhaseAndCommitState(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "transaction.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	transactionErr := store.withWrite(context.Background(), func(tx *Tx) error {
		if _, err := tx.exec("INSERT INTO meta(key, value) VALUES('transaction-test', '1')"); err != nil {
			return err
		}
		return errors.New("synthetic callback failure")
	})
	var typed *TransactionError
	if !errors.As(transactionErr, &typed) {
		t.Fatalf("transaction error = %v", transactionErr)
	}
	if typed.Phase != "write" || typed.SQLOperation != "write" || typed.StateCommitted == nil || *typed.StateCommitted {
		t.Fatalf("transaction metadata = %#v", typed)
	}
	if _, found, err := store.Meta(context.Background(), "transaction-test"); err != nil || found {
		t.Fatalf("rolled back metadata found=%v err=%v", found, err)
	}
}

func stringPointer(value string) *string { return &value }
func intPointer(value int64) *int64      { return &value }
