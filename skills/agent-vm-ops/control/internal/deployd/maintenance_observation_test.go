package deployd

import (
	"context"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
)

type slowOwnedRuntime struct {
	*fakeRuntime
	mu       sync.Mutex
	lease    controllerrole.Lease
	renewals int
}

func (r *slowOwnedRuntime) CurrentOwnedControllerLease(context.Context) (controllerrole.Lease, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lease, r.lease.Validate(time.Now(), "blue") == nil, nil
}
func (r *slowOwnedRuntime) RenewTransactionLease(_ context.Context, expected controllerrole.Lease) (controllerrole.Lease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !sameTransactionLease(r.lease, expected) {
		return expected, controllerrole.ErrLeaseMismatch
	}
	if err := r.lease.Validate(time.Now(), "blue"); err != nil {
		return expected, err
	}
	r.lease.Expiry = time.Now().Add(300 * time.Millisecond)
	r.renewals++
	return r.lease, nil
}
func (r *slowOwnedRuntime) Observe(ctx context.Context) (RuntimeSnapshot, error) {
	original, _, _ := r.CurrentOwnedControllerLease(ctx)
	select {
	case <-ctx.Done():
		return RuntimeSnapshot{}, ctx.Err()
	case <-time.After(700 * time.Millisecond):
	}
	_, live, _ := r.CurrentOwnedControllerLease(ctx)
	if !live {
		return RuntimeSnapshot{}, controllerrole.ErrLeaseExpired
	}
	snapshot, err := r.fakeRuntime.Observe(ctx)
	target := snapshot.Slots["blue"]
	target.ControllerLease = &original
	snapshot.Slots["blue"] = target
	return snapshot, err
}
func TestMaintenanceObservationRenewsWhileBusinessProbeIsSlow(t *testing.T) {
	for _, maintenance := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-only", true: "maintenance"}[maintenance], func(t *testing.T) {
			runtime := &slowOwnedRuntime{fakeRuntime: &fakeRuntime{active: "blue", observations: map[string]SlotObservation{"blue": {Slot: "blue", ProcessUp: true, ProcessReady: true, ControllerOwned: true}}}, lease: controllerrole.Lease{Slot: "blue", Term: 1, LeaseID: "live", Nonce: "nonce", Generation: 1, Expiry: time.Now().Add(300 * time.Millisecond)}}
			manager := NewManager(nil, runtime, 300*time.Millisecond, time.Now)
			ctx := context.Background()
			if maintenance {
				ctx = context.WithValue(ctx, controllerMaintenanceKey{}, controllerMaintenancePolicy{})
			}
			snapshot, err := manager.observeRuntime(ctx)
			runtime.mu.Lock()
			count := runtime.renewals
			runtime.mu.Unlock()
			if maintenance {
				if err != nil || count < 2 || snapshot.Slots["blue"].ControllerLease.Validate(time.Now(), "blue") != nil {
					t.Fatalf("slow observation lost authority: count=%d err=%v", count, err)
				}
			} else if err == nil || count != 0 {
				t.Fatalf("read-only observation extended authority: count=%d err=%v", count, err)
			}
		})
	}
}

func TestRecoveryTransactionFencesBeforeSlowObservationOrRenewal(t *testing.T) {
	now := time.Now().UTC()
	db, err := store.Open(filepath.Join(t.TempDir(), "deploy.sqlite3"), 30*24*time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx := store.Transaction{ID: "tx-recovery", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/bundle", State: "needs_recovery", Phase: PhaseControllerHandoff, Generation: 1, RecoveryRequired: true, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(context.Background(), tx, nil); err != nil {
		t.Fatal(err)
	}
	runtime := &slowOwnedRuntime{fakeRuntime: &fakeRuntime{active: "blue", observations: map[string]SlotObservation{"blue": {ControllerOwned: true}}}}
	manager := NewManager(db, runtime, 300*time.Millisecond, time.Now)
	start := time.Now()
	if err := manager.MaintainController(context.Background()); err == nil {
		t.Fatal("recovery transaction retained controller")
	}
	if runtime.renewals != 0 || time.Since(start) > 500*time.Millisecond {
		t.Fatal("recovery policy checked after slow observation")
	}
	if runtime.observations["blue"].ControllerOwned {
		t.Fatal("recovery transaction did not fence")
	}
}

func TestDaemonRestartCannotRenewWithoutInMemoryProcessPin(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "active")
	if err := os.Symlink(filepath.Join(root, "slots", "blue"), active); err != nil {
		t.Fatal(err)
	}
	runtime := NewHostRuntime(Config{ReleaseRoot: root, ActivePath: active})
	if _, owned, err := runtime.CurrentOwnedControllerLease(context.Background()); err != nil || owned {
		t.Fatalf("unobserved restarted daemon resumed authority: %v %v", owned, err)
	}
}
