package controllerrole

import (
	"errors"
	"os"
	"sync/atomic"
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

func TestGateRejectsPIDReuseWithDifferentProcessStartToken(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	gate := New("blue", func() time.Time { return now })
	lease := Lease{Slot: "blue", Term: 1, LeaseID: "lease-1", Expiry: now.Add(time.Minute), Generation: 1, Nonce: "nonce-1", OwnerPID: os.Getpid(), OwnerStartTime: "not-the-current-process"}
	if err := gate.Grant(lease); err != nil {
		t.Fatal(err)
	}
	if gate.Valid() {
		t.Fatal("lease with a reused PID token remained valid")
	}
}

func TestGateFencePreservesTermForFutureGrants(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	gate := New("blue", func() time.Time { return now })
	lease := Lease{Slot: "blue", Term: 4, LeaseID: "lease-4", Expiry: now.Add(time.Minute), Generation: 1, Nonce: "nonce-4"}
	if err := gate.Grant(lease); err != nil {
		t.Fatal(err)
	}
	gate.Fence()
	if gate.Valid() {
		t.Fatal("hard-fenced lease remained valid")
	}
	if err := gate.Grant(lease); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("same-term grant after hard fence = %v", err)
	}
	lease.Term = 5
	if err := gate.Grant(lease); err != nil {
		t.Fatalf("newer-term grant after hard fence: %v", err)
	}
}

func TestGateCommitSerializesFenceAfterDurableCommit(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	gate := New("blue", func() time.Time { return now })
	lease := Lease{Slot: "blue", Term: 1, LeaseID: "lease-1", Expiry: now.Add(time.Minute), Generation: 1, Nonce: "nonce-1"}
	if err := gate.Grant(lease); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var committed atomic.Bool
	commitDone := make(chan error, 1)
	go func() {
		commitDone <- gate.WithValid(func() error {
			close(started)
			<-release
			committed.Store(true)
			return nil
		})
	}()
	<-started
	fenceDone := make(chan struct{})
	go func() {
		gate.Fence()
		close(fenceDone)
	}()
	select {
	case <-fenceDone:
		t.Fatal("fence overtook the durable commit")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-commitDone; err != nil {
		t.Fatal(err)
	}
	<-fenceDone
	if !committed.Load() {
		t.Fatal("durable commit callback did not run")
	}
	if gate.Valid() {
		t.Fatal("fenced lease remained valid")
	}
}

func TestRenewalPreservesWorkButCannotResurrectFencedAuthority(t *testing.T) {
	now := time.Now().UTC()
	gate := New("blue", func() time.Time { return now })
	lease := Lease{Slot: "blue", Term: 1, LeaseID: "live", Nonce: "nonce", Generation: 1, Expiry: now.Add(time.Second)}
	if err := gate.Grant(lease); err != nil {
		t.Fatal(err)
	}
	changes := gate.Changes()
	now = now.Add(500 * time.Millisecond)
	lease.Expiry = now.Add(time.Second)
	if err := gate.Grant(lease); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
		t.Fatal("renewal canceled in-flight work")
	default:
	}
	now = now.Add(750 * time.Millisecond)
	if !gate.Valid() {
		t.Fatal("renewal did not extend authority beyond original expiry")
	}
	for _, mutate := range []func(*Lease){
		func(l *Lease) { l.Nonce = "other" },
		func(l *Lease) { l.LeaseID = "other" },
		func(l *Lease) { l.Generation++ },
		func(l *Lease) { l.OwnerPID++ },
		func(l *Lease) { l.OwnerStartTime = "other" },
		func(l *Lease) { l.Expiry = l.Expiry.Add(-time.Millisecond) },
	} {
		invalid := lease
		mutate(&invalid)
		if err := gate.Grant(invalid); !errors.Is(err, ErrLeaseMismatch) {
			t.Fatalf("changed identity or shrinking expiry accepted: %v", err)
		}
	}
	now = lease.Expiry.Add(time.Millisecond)
	lease.Expiry = now.Add(time.Second)
	if err := gate.Grant(lease); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("expired authority resurrected: %v", err)
	}
	select {
	case <-changes:
	default:
		t.Fatal("expiry did not cancel work")
	}
	lease.Term++
	if err := gate.Grant(lease); err != nil {
		t.Fatal(err)
	}
	gate.Fence()
	if err := gate.Grant(lease); !errors.Is(err, ErrLeaseMismatch) {
		t.Fatalf("fenced authority resurrected: %v", err)
	}
}
