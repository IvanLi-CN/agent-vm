package state

import (
	"context"
	"database/sql"
	"encoding/json"
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

func TestManagementConfirmationWritesDoNotInvalidateSystemVersion(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	before, err := store.StateEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	confirmation := ManagementConfirmation{
		ConfirmationDigest: strings.Repeat("a", 64), Subject: "admin@example", Action: "reconcile", Target: "system",
		TargetVersion: "system:" + before, ParamsDigest: strings.Repeat("b", 64), CreatedAt: "2026-10-31T00:00:00Z", ExpiresAt: "2026-11-01T00:00:00Z",
	}
	if err := store.InsertManagementConfirmation(ctx, confirmation); err != nil {
		t.Fatal(err)
	}
	afterInsert, err := store.StateEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterInsert != before {
		t.Fatalf("confirmation insert advanced system version: %q -> %q", before, afterInsert)
	}
	consumed, err := store.ConsumeManagementConfirmation(ctx, confirmation.ConfirmationDigest, confirmation.Subject, confirmation.Action, confirmation.Target, confirmation.TargetVersion, confirmation.ParamsDigest, "2026-10-31T00:01:00Z")
	if err != nil || !consumed {
		t.Fatalf("confirmation consume = %v, err=%v", consumed, err)
	}
	afterConsume, err := store.StateEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if afterConsume != before {
		t.Fatalf("confirmation consume advanced system version: %q -> %q", before, afterConsume)
	}
}

func TestAuditSummaryAggregatesStableObservabilityLabels(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Audit(ctx, "acquire", "task", "session", "error", intPointer(1), map[string]any{
		"code": "image_unpack_failed", "phase": "dispatch", "message": "image rootfs unpack failed",
		"detail":   map[string]any{"phase": "native-unpack"},
		"warnings": []map[string]any{{"code": "capacity_insufficient"}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Audit(ctx, "acquire", "task", "session", "success", intPointer(1), map[string]any{"phase": "dispatch"}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureManagementSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordManagementAudit(ctx, ManagementAudit{
		EventID: "management-image-unpack-failure", Action: "image_import", Outcome: "failed", Phase: "image_import",
		Target: map[string]any{"kind": "image_catalog"}, Detail: map[string]any{"code": "internal_error", "message": "image rootfs unpack failed"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordManagementAudit(ctx, ManagementAudit{
		EventID: "management-image-catalog-failure", Action: "image_import", Outcome: "failed", Phase: "reconciliation",
		Target: map[string]any{"kind": "image_catalog"}, Detail: map[string]any{"code": "internal_error", "message": "catalog persistence failed"},
	}); err != nil {
		t.Fatal(err)
	}
	summary, err := store.AuditSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Operations["acquire|error|native-unpack"] != 1 || summary.Operations["acquire|success|dispatch"] != 1 || summary.Operations["image_import|error|image_import"] != 1 {
		t.Fatalf("operation summary = %#v", summary.Operations)
	}
	if summary.ImageUnpackFailures["native-unpack|image_unpack_failed"] != 1 || summary.ImageUnpackFailures["image_import|internal_error"] != 1 || summary.Warnings["capacity_insufficient"] != 1 {
		t.Fatalf("failure summary = %#v warnings=%#v", summary.ImageUnpackFailures, summary.Warnings)
	}
	if _, exists := summary.ImageUnpackFailures["reconciliation|internal_error"]; exists {
		t.Fatalf("catalog persistence failure was misclassified as unpack failure: %#v", summary.ImageUnpackFailures)
	}
}

func TestAuditSummaryUpdatesAfterCacheInitialization(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Audit(ctx, "acquire", "task", "session", "success", intPointer(1), map[string]any{"phase": "dispatch"}); err != nil {
		t.Fatal(err)
	}
	first, err := store.AuditSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if first.Operations["acquire|success|dispatch"] != 1 {
		t.Fatalf("initial audit summary = %#v", first.Operations)
	}
	if err := store.Audit(ctx, "release", "task", "session", "error", intPointer(1), map[string]any{"phase": "cleanup", "code": "capacity_insufficient"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordManagementAudit(ctx, ManagementAudit{
		EventID: "incremental-management-audit", Action: "image_import", Outcome: "failed", Phase: "reconciliation",
		Target: map[string]any{"kind": "image_catalog"}, Detail: map[string]any{"code": "internal_error", "message": "catalog persistence failed"},
	}); err != nil {
		t.Fatal(err)
	}
	second, err := store.AuditSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if second.Operations["release|error|cleanup"] != 1 || second.Operations["image_import|error|reconciliation"] != 1 || second.Warnings["capacity_insufficient"] != 1 {
		t.Fatalf("incremental audit summary = %#v warnings=%#v", second.Operations, second.Warnings)
	}
}

func TestMissingDatabaseFailsClosed(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "missing.sqlite3"), false)
	if err == nil {
		t.Fatal("missing database unexpectedly opened")
	}
}

func TestCreateOpenDefersManagementMigration(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "legacy.sqlite3"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	usable, err := store.CachedImageUsable(context.Background(), strings.Repeat("a", 64), false)
	if err != nil || !usable {
		t.Fatalf("legacy cache compatibility = %v, err=%v", usable, err)
	}
	capabilities, err := store.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if capabilities != nil {
		t.Fatalf("management capabilities = %#v, want deferred migration", capabilities)
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

func TestGenerationTombstoneAdvancesAfterTaskDeletion(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "generations.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	task := Task{TaskID: "avm-generation", SessionID: "generation-session", Generation: 7, State: "absent", InstanceName: "agent-vm-generation", CreatedAt: "now", UpdatedAt: "now", ImageFingerprint: "image", ResourcesJSON: "{}", ResourceReservationJSON: "{}"}
	if err := store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteTaskIf(ctx, task.TaskID, TaskCondition{Generation: &task.Generation, States: []string{"absent"}})
	if err != nil || !deleted {
		t.Fatalf("delete task = %v, err=%v", deleted, err)
	}
	next, err := store.NextGeneration(ctx, task.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if next != 8 {
		t.Fatalf("next generation = %d, want 8", next)
	}
	next, err = store.NextGeneration(ctx, task.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if next != 9 {
		t.Fatalf("second next generation = %d, want 9", next)
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
	if typed.Uncertain() {
		t.Fatal("proven rollback was classified as uncertain")
	}
	if !(&TransactionError{StateCommitted: nil}).Uncertain() {
		t.Fatal("unknown commit was not classified as uncertain")
	}
	if _, found, err := store.Meta(context.Background(), "transaction-test"); err != nil || found {
		t.Fatalf("rolled back metadata found=%v err=%v", found, err)
	}
}

func TestMaintenanceCursorIsAdditiveAndValidated(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "maintenance-cursor.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	cursor, err := store.MaintenanceCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.Reconcile.NextAttemptAt == "" || cursor.AgeGC.NextAttemptAt == "" {
		t.Fatalf("default maintenance cursor = %#v", cursor)
	}
	cursor.Reconcile.Attempt = 3
	cursor.Reconcile.ScanAfterTaskID = "task-15"
	cursor.Reconcile.LastErrorCode = "native_unavailable"
	cursor.Reconcile.LastErrorPhase = "reconcile"
	if err := store.SetMaintenanceCursor(ctx, cursor); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.MaintenanceCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Reconcile.Attempt != 3 || loaded.Reconcile.ScanAfterTaskID != "task-15" || loaded.Reconcile.LastErrorCode != "native_unavailable" {
		t.Fatalf("loaded maintenance cursor = %#v", loaded)
	}
	if err := store.SetMeta(ctx, MaintenanceCursorMetaKey, `{"reconcile":{"attempt":7,"next_attempt_at":"2026-11-03T00:00:00Z","updated_at":"2026-11-03T00:00:00Z"},"age_gc":{"attempt":0,"next_attempt_at":"2026-11-03T00:00:00Z","updated_at":"2026-11-03T00:00:00Z"}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MaintenanceCursor(ctx); err == nil {
		t.Fatal("invalid maintenance cursor was accepted")
	}
}

func TestOperationIdentityAndWorkerIdentityRemainImmutable(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "operation-identity.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	session := "identity-session"
	generation := int64(2)
	if err := store.RecordOperation(ctx, "identity-operation", "acquire", &session, &generation, map[string]any{
		"outcome": "pending", "_request_digest": "identity-digest", "worker": map[string]any{"state": "starting", "start_attempted": false},
	}); err != nil {
		t.Fatal(err)
	}
	identity := map[string]any{"state": "running", "pid": int64(11), "start_time": "start-11", "process_group": int64(11)}
	if err := store.RecordOperationWorker(ctx, "identity-operation", identity); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperationWorker(ctx, "identity-operation", map[string]any{"state": "running", "pid": int64(12), "start_time": "start-12", "process_group": int64(12)}); err == nil {
		t.Fatal("worker identity overwrite was accepted")
	}
	if err := store.RecordOperation(ctx, "identity-operation", "acquire", &session, &generation, map[string]any{
		"outcome": "success", "worker": map[string]any{"state": "finished", "pid": int64(12), "start_time": "start-12", "process_group": int64(12)},
	}); err == nil {
		t.Fatal("terminal worker identity overwrite was accepted")
	}
	if err := store.RecordOperation(ctx, "identity-operation", "acquire", &session, &generation, map[string]any{
		"outcome": "success", "_request_digest": "different-digest",
	}); err == nil {
		t.Fatal("operation request digest overwrite was accepted")
	}
	if err := store.UpdateOperationResponse(ctx, "identity-operation", func(response map[string]any) error {
		response["_request_digest"] = "different-digest"
		return nil
	}); err == nil {
		t.Fatal("pending operation request digest mutation was accepted")
	}
	if err := store.RecordOperation(ctx, "identity-operation", "release", &session, &generation, map[string]any{"outcome": "success"}); err == nil {
		t.Fatal("operation identity overwrite was accepted")
	}
	if err := store.RecordOperation(ctx, "identity-operation", "acquire", &session, &generation, map[string]any{"outcome": "success"}); err != nil {
		t.Fatal(err)
	}
	operation, err := store.GetOperation(ctx, "identity-operation")
	if err != nil {
		t.Fatal(err)
	}
	worker, ok := operation.Response["worker"].(map[string]any)
	if !ok || worker["pid"] != json.Number("11") {
		t.Fatalf("finalized worker evidence = %#v", operation.Response)
	}
	if operation.Response["_request_digest"] != "identity-digest" {
		t.Fatalf("finalized request digest = %#v", operation.Response["_request_digest"])
	}
}

func TestOperationWorkerIdentityCanRearmAfterVerifiedFinish(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "operation-worker-rearm.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.RecordOperation(ctx, "rearm-operation", "acquire", stringPointer("rearm-session"), intPointer(1), map[string]any{
		"outcome": "pending",
		"worker":  map[string]any{"state": "starting"},
	}); err != nil {
		t.Fatal(err)
	}
	first := map[string]any{"state": "finished", "pid": int64(11), "start_time": "start-11", "process_group": int64(11)}
	if err := store.RecordOperationWorker(ctx, "rearm-operation", first); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperationWorker(ctx, "rearm-operation", map[string]any{"state": "starting"}); err != nil {
		t.Fatalf("verified worker did not rearm: %v", err)
	}
	second := map[string]any{"state": "running", "pid": int64(12), "start_time": "start-12", "process_group": int64(12)}
	if err := store.RecordOperationWorker(ctx, "rearm-operation", second); err != nil {
		t.Fatalf("new worker identity was rejected after verified finish: %v", err)
	}
	operation, err := store.GetOperation(ctx, "rearm-operation")
	if err != nil {
		t.Fatal(err)
	}
	worker, ok := operation.Response["worker"].(map[string]any)
	if !ok || worker["pid"] != json.Number("12") || worker["start_time"] != "start-12" {
		t.Fatalf("rearmed worker identity = %#v, want second worker", operation.Response)
	}
}

func stringPointer(value string) *string { return &value }
func intPointer(value int64) *int64      { return &value }

func TestWorkerRecoveryAuditAndBarrierCommitAtomically(t *testing.T) {
	for _, scenario := range []string{"original-success", "original-failure", "rearm-success", "rearm-failure"} {
		t.Run(scenario, func(t *testing.T) {
			fail, rearm := strings.HasSuffix(scenario, "failure"), strings.HasPrefix(scenario, "rearm")
			ctx := context.Background()
			store, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			session, generation := "session", int64(2)
			if err := store.RecordOperation(ctx, "destroy", "destroy", &session, &generation, map[string]any{"outcome": "pending", "worker": map[string]any{"state": "starting", "start_attempted": true}}); err != nil {
				t.Fatal(err)
			}
			evidenceKey := "_worker_recovery_evidence"
			recoverWorker := store.RecoverOperationWorker
			if rearm {
				evidenceKey = "_worker_rearm_recovery_evidence"
				recoverWorker = store.RecoverOperationWorkerRearm
				if err := store.UpdateOperationResponse(ctx, "destroy", func(response map[string]any) error {
					response["_worker_recovery_evidence"] = map[string]any{"outcome": "unknown"}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if fail {
				if err := store.withWrite(ctx, func(tx *Tx) error {
					_, err := tx.exec("CREATE TRIGGER fail_recovery_audit BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT, 'audit unavailable'); END")
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			err = recoverWorker(ctx, "destroy", "task", func(response map[string]any) error {
				response[evidenceKey] = map[string]any{"original_worker": response["worker"], "outcome": "unknown"}
				response["worker"] = map[string]any{"state": "exited", "start_attempted": true}
				return nil
			})
			if (err != nil) != fail {
				t.Fatalf("recovery error=%v fail=%v", err, fail)
			}
			operation, err := store.GetOperation(ctx, "destroy")
			if err != nil {
				t.Fatal(err)
			}
			worker := operation.Response["worker"].(map[string]any)
			expected := "exited"
			if fail {
				expected = "starting"
			}
			if worker["state"] != expected || operation.Response["outcome"] != "pending" {
				t.Fatalf("non-atomic recovery: %#v", operation.Response)
			}
			var count int
			if err := store.withRead(ctx, func(conn *sql.Conn) error {
				return conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit WHERE outcome = 'unknown' AND json_extract(detail_json, '$.operation_id') = 'destroy'").Scan(&count)
			}); err != nil {
				t.Fatal(err)
			}
			if (count == 1) == fail {
				t.Fatalf("audit count=%d", count)
			}
			if (operation.Response[evidenceKey] == nil) != fail {
				t.Fatal("recovery evidence did not commit with audit")
			}
			if !fail {
				if err := recoverWorker(ctx, "destroy", "task", func(map[string]any) error { return nil }); err == nil {
					t.Fatal("repeat recovery accepted")
				}
				if err := store.RecordOperation(ctx, "destroy", "destroy", &session, &generation, map[string]any{"outcome": "success", evidenceKey: map[string]any{"outcome": "invented"}}); err == nil {
					t.Fatal("recovery evidence replacement accepted")
				}
			}
		})
	}
}
func fmtBool(value bool) string {
	if value {
		return "audit-failure"
	}
	return "atomic-success"
}

func TestMarkHistoricalUnknownReconciledPreservesJournal(t *testing.T) {
	store, err := OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	session, generation := "history-session", int64(3)
	response := map[string]any{
		"outcome":         "error",
		"error":           map[string]any{"code": "operation_outcome_unknown", "message": "original outcome is unknown"},
		"_request_digest": "digest",
	}
	if err := store.RecordOperation(ctx, "history-op", "acquire", &session, &generation, response); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkHistoricalUnknownReconciled(ctx, "history-op", map[string]any{"status": "reconciled", "reason": "operator evidence"}); err != nil {
		t.Fatal(err)
	}
	operation, err := store.GetOperation(ctx, "history-op")
	if err != nil || operation == nil {
		t.Fatalf("operation=%#v err=%v", operation, err)
	}
	if operation.Response["outcome"] != "error" || operation.Response["_request_digest"] != "digest" {
		t.Fatalf("journal was rewritten: %#v", operation.Response)
	}
	if marker, ok := operation.Response["_historical_reconciled"].(map[string]any); !ok || marker["status"] != "reconciled" {
		t.Fatalf("missing reconciliation marker: %#v", operation.Response)
	}
	unknown, err := store.ReconciledUnknownOperations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 0 {
		t.Fatalf("reconciled operation remains a startup blocker: %#v", unknown)
	}
}
