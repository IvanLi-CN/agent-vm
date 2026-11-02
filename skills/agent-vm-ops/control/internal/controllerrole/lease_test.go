package controllerrole

import (
	"errors"
	"testing"
	"time"
)

func TestGateRequiresFreshLeaseAndFencesOnExpiry(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	gate := New("blue", func() time.Time { return now })
	lease := Lease{Slot: "blue", Term: 1, LeaseID: "lease-1", Expiry: now.Add(time.Second), Generation: 3, Nonce: "nonce-1"}
	if err := gate.Grant(lease); err != nil {
		t.Fatal(err)
	}
	if !gate.Valid() {
		t.Fatal("fresh lease was not accepted")
	}
	now = now.Add(2 * time.Second)
	if gate.Valid() {
		t.Fatal("expired lease remained valid")
	}
	if _, ok := gate.Snapshot(); ok {
		t.Fatal("expired lease remained in the snapshot")
	}
}

func TestGateRejectsOlderTermAndMismatchedRevoke(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	gate := New("green", func() time.Time { return now })
	lease := Lease{Slot: "green", Term: 4, LeaseID: "lease-4", Expiry: now.Add(time.Minute), Generation: 1, Nonce: "nonce-4"}
	if err := gate.Grant(lease); err != nil {
		t.Fatal(err)
	}
	older := lease
	older.Term = 3
	older.LeaseID = "lease-3"
	if err := gate.Grant(older); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("older term error = %v", err)
	}
	if err := gate.Revoke(older); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("mismatched revoke error = %v", err)
	}
	if !gate.Valid() {
		t.Fatal("mismatched revoke removed the current lease")
	}
	if err := gate.Revoke(lease); err != nil {
		t.Fatal(err)
	}
	if gate.Valid() {
		t.Fatal("revoked lease remained valid")
	}
}
