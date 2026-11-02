package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

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
	if err := s.RecordFailure(context.Background(), transaction.ID, "activate", "state_unverified", "test", "digest", map[string]any{"uncertain": true}, now.Add(-90*time.Minute)); err != nil {
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
	if err := s.RecordFailure(context.Background(), transaction.ID, "stage", "deployment_failed", "test", "digest", map[string]any{"uncertain": false}, now.Add(-90*time.Minute)); err != nil {
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
