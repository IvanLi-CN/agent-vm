package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreMigratesSchemaVersionOneForMigrationRetryCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment.sqlite3")
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.Exec(`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE deployment_transactions (
  transaction_id TEXT PRIMARY KEY,
  target_slot TEXT NOT NULL,
  previous_slot TEXT NOT NULL,
  bundle TEXT NOT NULL,
  phase TEXT NOT NULL,
  state TEXT NOT NULL,
  generation INTEGER NOT NULL,
  rollback_used INTEGER NOT NULL DEFAULT 0,
  recovery_required INTEGER NOT NULL DEFAULT 0,
  error_code TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
INSERT INTO meta(key, value) VALUES('schema_version', '1');`)
	if err != nil {
		_ = database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(path, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	transaction := Transaction{ID: "migration-retry-schema", TargetSlot: "state", PreviousSlot: "legacy", Bundle: "sqlite-to-rqlite", Phase: "migration_import", State: "needs_recovery", RecoveryRequired: true, Generation: 1}
	if err := store.Create(context.Background(), transaction, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.ConsumeMigrationRetry(context.Background(), transaction.ID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get(context.Background(), transaction.ID)
	if err != nil || loaded == nil || loaded.MigrationRetryCount != 1 || loaded.MigrationRetryState != "in_progress" {
		t.Fatalf("migrated retry count = %#v, err=%v", loaded, err)
	}
	if err := store.ConsumeMigrationRetry(context.Background(), transaction.ID, time.Now().UTC()); err != nil {
		t.Fatalf("in-progress retry was not resumable: %v", err)
	}
	if err := store.FailMigrationRetry(context.Background(), transaction.ID, time.Now().UTC()); err != nil {
		t.Fatalf("failed retry was not durably exhausted: %v", err)
	}
	if err := store.ConsumeMigrationRetry(context.Background(), transaction.ID, time.Now().UTC()); err == nil {
		t.Fatal("exhausted retry was accepted")
	}
}

func TestStoreRejectsActiveTransactionAcrossStoreInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment.sqlite3")
	first, err := Open(path, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	transaction := Transaction{ID: "tx-first", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/bundle", Phase: "stage", Generation: 1}
	if err := first.Create(context.Background(), transaction, nil); err != nil {
		t.Fatal(err)
	}
	transaction.ID = "tx-second"
	if err := second.Create(context.Background(), transaction, nil); err == nil {
		t.Fatal("second store created an active transaction")
	}
}

func TestStorePersistsLatestObservationSeparatelyFromTransactions(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s, err := Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	payload := map[string]any{"active_slot": "blue", "sources": map[string]any{"health": map[string]any{"up": true}}}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	want := Observation{ObservedAt: now, FreshUntil: now.Add(time.Minute), Digest: hex.EncodeToString(digest[:]), Payload: payload}
	if err := s.SaveObservation(context.Background(), LatestObservationID, want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Observation(context.Background(), LatestObservationID)
	if err != nil || got == nil {
		t.Fatalf("latest observation = %#v err=%v", got, err)
	}
	if got.Digest != want.Digest || got.Payload["active_slot"] != "blue" {
		t.Fatalf("latest observation = %#v", got)
	}
	if transactionObservation, err := s.Observation(context.Background(), "missing"); err != nil || transactionObservation != nil {
		t.Fatalf("missing transaction observation = %#v err=%v", transactionObservation, err)
	}
}

func TestStoreRejectsObservationDigestMismatch(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	payload := map[string]any{"active_slot": "blue"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	if err := s.SaveObservation(context.Background(), LatestObservationID, Observation{ObservedAt: time.Now().UTC(), FreshUntil: time.Now().UTC().Add(time.Minute), Digest: hex.EncodeToString(digest[:]), Payload: payload}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(context.Background(), "UPDATE deployment_latest_observation SET payload_json = ? WHERE id = 1", `{"active_slot":"green"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Observation(context.Background(), LatestObservationID); err == nil {
		t.Fatal("observation with a mismatched digest was accepted")
	}
}

func TestStoreBeginRollbackAtomicallyConsumesRollbackBudget(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s, err := Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	transaction := Transaction{ID: "tx-rollback", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/bundle", Phase: "active_switch", Generation: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.Create(context.Background(), transaction, nil); err != nil {
		t.Fatal(err)
	}
	updated, err := s.BeginRollback(context.Background(), transaction.ID, transaction.Phase, "rollback", "rollback_started", "test", map[string]any{"bounded": true}, now)
	if err != nil || updated == nil || !updated.RollbackUsed || updated.Phase != "rollback" {
		t.Fatalf("rollback transaction = %#v err=%v", updated, err)
	}
	if _, err := s.BeginRollback(context.Background(), transaction.ID, "rollback", "rollback", "retry", "test", nil, now); err == nil {
		t.Fatalf("second rollback unexpectedly succeeded: %v", err)
	}
}

func TestStoreTransitionsAndCleansOnlyFinalizedHistory(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s, err := Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	transaction := Transaction{ID: "tx-1", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/bundle", Phase: "stage", Generation: 1, CreatedAt: now.Add(-31 * 24 * time.Hour), UpdatedAt: now.Add(-31 * 24 * time.Hour)}
	if err := s.Create(context.Background(), transaction, map[string]any{"slot": "green"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(context.Background(), transaction.ID, "stage", "standby_validation", "active", "verified", "test", nil, transaction.UpdatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(context.Background(), transaction.ID, "standby_validation", "finalize", "finalized", "finalized", "test", nil, transaction.UpdatedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Cleanup(context.Background(), now)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
	missing, err := s.Get(context.Background(), transaction.ID)
	if err != nil {
		t.Fatal(err)
	}
	if missing != nil {
		t.Fatalf("finalized transaction was retained: %#v", missing)
	}
}

func TestStoreKeepsNeedsRecoveryTransaction(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s, err := Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	transaction := Transaction{ID: "tx-recovery", TargetSlot: "blue", PreviousSlot: "green", Bundle: "/bundle", Phase: "activate", Generation: 2, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)}
	if err := s.Create(context.Background(), transaction, nil); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"uncertain": true}
	if err := s.RecordFailure(context.Background(), transaction.ID, "activate", "state_unverified", "test", testPayloadDigest(t, payload), payload, now.Add(-90*time.Minute)); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Cleanup(context.Background(), now)
	if err != nil || removed != 0 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
	kept, err := s.Get(context.Background(), transaction.ID)
	if err != nil || kept == nil || !kept.RecoveryRequired {
		t.Fatalf("recovery transaction = %#v err=%v", kept, err)
	}
}

func TestStoreRetainsTheSingleFinalizedStateMigration(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s, err := Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	transaction := Transaction{ID: "migration-old", TargetSlot: "state", PreviousSlot: "legacy", Bundle: "sqlite-to-rqlite", Phase: "migration_prepare", Generation: 1, CreatedAt: now.Add(-31 * 24 * time.Hour), UpdatedAt: now.Add(-31 * 24 * time.Hour)}
	if err := s.Create(context.Background(), transaction, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(context.Background(), transaction.ID, "migration_prepare", "migration_finalize", "active", "imported", "test", nil, transaction.UpdatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Transition(context.Background(), transaction.ID, "migration_finalize", "finalize", "finalized", "completed", "test", nil, transaction.UpdatedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Cleanup(context.Background(), now)
	if err != nil || removed != 0 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
	kept, err := s.Get(context.Background(), transaction.ID)
	if err != nil || kept == nil || kept.State != "finalized" {
		t.Fatalf("finalized state migration = %#v err=%v", kept, err)
	}
	duplicate := transaction
	duplicate.ID = "migration-second"
	duplicate.CreatedAt = now
	duplicate.UpdatedAt = now
	if err := s.Create(context.Background(), duplicate, nil); err == nil {
		t.Fatal("duplicate state migration transaction was accepted")
	}
}

func TestStoreCleansDeterministicallyFailedTransaction(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s, err := Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	transaction := Transaction{ID: "tx-failed", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/bundle", Phase: "stage", Generation: 1, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour)}
	if err := s.Create(context.Background(), transaction, nil); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"uncertain": false}
	if err := s.RecordFailure(context.Background(), transaction.ID, "stage", "deployment_failed", "test", testPayloadDigest(t, payload), payload, now.Add(-90*time.Minute)); err != nil {
		t.Fatal(err)
	}
	active, err := s.ListActive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("deterministically failed transaction remained active: %#v", active)
	}
	removed, err := s.Cleanup(context.Background(), now)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup removed=%d err=%v", removed, err)
	}
}

func TestStorePersistsFenceTermsAndMakesRecoveryFailuresIdempotent(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "deployment.sqlite3")
	s, err := Open(path, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.NextFenceTerm(context.Background())
	if err != nil || first != 1 {
		t.Fatalf("first fencing term = %d err=%v", first, err)
	}
	transaction := Transaction{ID: "tx-idempotent", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/bundle", Phase: "active_switch", Generation: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.Create(context.Background(), transaction, nil); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"message": "effect result was lost", "uncertain": true}
	if err := s.RecordFailure(context.Background(), transaction.ID, transaction.Phase, "state_unverified", "test", testPayloadDigest(t, payload), payload, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordFailure(context.Background(), transaction.ID, transaction.Phase, "state_unverified", "test", testPayloadDigest(t, payload), payload, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	current, err := s.Get(context.Background(), transaction.ID)
	if err != nil || current == nil || current.State != "needs_recovery" || !current.RecoveryRequired {
		t.Fatalf("recovery transaction = %#v err=%v", current, err)
	}
	var evidenceCount, eventCount int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM deployment_failure_evidence WHERE transaction_id = ?", transaction.ID).Scan(&evidenceCount); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM deployment_phase_events WHERE transaction_id = ?", transaction.ID).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if evidenceCount != 1 || eventCount != 2 {
		t.Fatalf("idempotent recovery records = evidence:%d events:%d", evidenceCount, eventCount)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path, time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	second, err := s.NextFenceTerm(context.Background())
	if err != nil || second != 2 {
		t.Fatalf("persisted fencing term = %d err=%v", second, err)
	}
}

func TestStoreRejectsFailureEvidenceDigestMismatch(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	s, err := Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	transaction := Transaction{ID: "tx-bad-failure-digest", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/bundle", Phase: "stage", Generation: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.Create(context.Background(), transaction, nil); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"message": "stage failed"}
	if err := s.RecordFailure(context.Background(), transaction.ID, transaction.Phase, "deployment_failed", "test", "wrong-digest", payload, now); err == nil {
		t.Fatal("mismatched failure evidence digest was accepted")
	}
	current, err := s.Get(context.Background(), transaction.ID)
	if err != nil || current == nil || current.State != "active" {
		t.Fatalf("transaction after rejected failure evidence = %#v err=%v", current, err)
	}
}

func testPayloadDigest(t *testing.T, payload map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
