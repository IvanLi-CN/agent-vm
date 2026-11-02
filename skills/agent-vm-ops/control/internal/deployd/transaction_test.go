package deployd

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

type fakeRuntime struct {
	active       string
	observations map[string]SlotObservation
	failSwitch   bool
	sequence     []string
}

func (runtime *fakeRuntime) Observe(context.Context) (RuntimeSnapshot, error) {
	return RuntimeSnapshot{ObservedAt: time.Unix(100, 0), FreshUntil: time.Unix(110, 0), ActiveSlot: runtime.active, Consistent: true, Slots: runtime.observations}, nil
}
func (runtime *fakeRuntime) Stage(context.Context, string, string) error {
	runtime.sequence = append(runtime.sequence, "stage")
	return nil
}
func (runtime *fakeRuntime) Verify(context.Context, string, string) error {
	runtime.sequence = append(runtime.sequence, "verify")
	return nil
}
func (runtime *fakeRuntime) GrantController(_ context.Context, slot string, _ controllerrole.Lease) error {
	runtime.sequence = append(runtime.sequence, "grant")
	if observation, ok := runtime.observations[slot]; ok {
		observation.ControllerOwned = true
		runtime.observations[slot] = observation
	}
	return nil
}
func (runtime *fakeRuntime) RevokeController(_ context.Context, slot string) error {
	runtime.sequence = append(runtime.sequence, "revoke")
	if observation, ok := runtime.observations[slot]; ok {
		observation.ControllerOwned = false
		runtime.observations[slot] = observation
	}
	return nil
}
func (runtime *fakeRuntime) EnableTraffic(context.Context, string) error {
	runtime.sequence = append(runtime.sequence, "traffic")
	return nil
}
func (runtime *fakeRuntime) SwitchActive(_ context.Context, slot string) error {
	runtime.sequence = append(runtime.sequence, "switch")
	if runtime.failSwitch {
		return &UncertainError{Cause: errors.New("switch result was lost")}
	}
	runtime.active = slot
	return nil
}
func (runtime *fakeRuntime) Drain(context.Context, string) error {
	runtime.sequence = append(runtime.sequence, "drain")
	return nil
}
func (runtime *fakeRuntime) Quiesce(context.Context, string) error {
	runtime.sequence = append(runtime.sequence, "quiesce")
	return nil
}
func (runtime *fakeRuntime) Stop(context.Context, string) error {
	runtime.sequence = append(runtime.sequence, "stop")
	return nil
}
func (runtime *fakeRuntime) Rollback(context.Context, string, string, controllerrole.Lease) error {
	runtime.sequence = append(runtime.sequence, "rollback")
	runtime.active = "blue"
	return nil
}

func TestManagerCompletesBlueGreenTransactionWithOneRuntime(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	transaction, err := manager.Stage(context.Background(), "green", "/tmp/release.tar")
	if err != nil || transaction.Phase != PhaseStandbyValidation {
		t.Fatalf("stage transaction=%#v err=%v", transaction, err)
	}
	transaction, err = manager.Verify(context.Background(), transaction.ID)
	if err != nil || transaction.Phase != PhaseControllerHandoff {
		t.Fatalf("verify transaction=%#v err=%v", transaction, err)
	}
	transaction, err = manager.Activate(context.Background(), transaction.ID)
	if err != nil || transaction.Phase != PhaseFinalize || transaction.State != "finalized" {
		t.Fatalf("activate transaction=%#v err=%v", transaction, err)
	}
	if runtime.active != "green" || len(runtime.sequence) < 8 {
		t.Fatalf("runtime active=%s sequence=%v", runtime.active, runtime.sequence)
	}
}

func TestManagerFencesUncertainActiveSwitch(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", failSwitch: true, observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	transaction, err := manager.Stage(context.Background(), "green", "/tmp/release.tar")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(context.Background(), transaction.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background(), transaction.ID); err == nil {
		t.Fatal("uncertain activation unexpectedly succeeded")
	}
	current, err := database.Get(context.Background(), transaction.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != "needs_recovery" || !current.RecoveryRequired {
		t.Fatalf("uncertain transaction was not fenced: %#v", current)
	}
}

func TestManagerReconcilesControllerOnlyAfterFreshObservation(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	if err := manager.MaintainController(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runtime.sequence) != 2 || runtime.sequence[0] != "revoke" || runtime.sequence[1] != "grant" {
		t.Fatalf("controller maintenance sequence = %v", runtime.sequence)
	}
}

func TestManagerMarksActiveTransactionForRecoveryAfterRestart(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	transaction := store.Transaction{ID: "tx-restart", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/bundle", Phase: PhaseStandbyValidation, Generation: 1, CreatedAt: now, UpdatedAt: now}
	if err := database.Create(context.Background(), transaction, nil); err != nil {
		t.Fatal(err)
	}
	if err := manager.RecoverOnStart(context.Background()); err == nil || !errors.Is(err, ErrUncertain) {
		t.Fatalf("restart recovery error = %v", err)
	}
	current, err := database.Get(context.Background(), transaction.ID)
	if err != nil || current == nil || current.State != "needs_recovery" {
		t.Fatalf("restart transaction = %#v err=%v", current, err)
	}
	if err := manager.MaintainController(context.Background()); err == nil {
		t.Fatal("controller was maintained while recovery was required")
	}
	if len(runtime.sequence) != 2 || runtime.sequence[0] != "revoke" || runtime.sequence[1] != "revoke" {
		t.Fatalf("fencing sequence = %v", runtime.sequence)
	}
}

func TestManagerRecoveryFinalizeReestablishesControllerAfterRestart(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	transaction := store.Transaction{ID: "tx-restart-finalize", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/bundle", Phase: PhaseActiveSwitch, State: "active", Generation: 1, CreatedAt: now, UpdatedAt: now}
	if err := database.Create(context.Background(), transaction, nil); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	if err := manager.RecoverOnStart(context.Background()); err == nil || !errors.Is(err, ErrUncertain) {
		t.Fatalf("restart recovery error = %v", err)
	}
	runtime.active = "green"
	updated, err := manager.Recover(context.Background(), transaction.ID, "finalize")
	if err != nil {
		t.Fatal(err)
	}
	if updated.State != "finalized" || updated.Phase != PhaseFinalize {
		t.Fatalf("recovered transaction = %#v", updated)
	}
	if !runtime.observations["green"].ControllerOwned || runtime.observations["blue"].ControllerOwned {
		t.Fatalf("controller ownership after recovery = %#v", runtime.observations)
	}
}

func TestManagerRejectsConcurrentDeploymentTransactions(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	if _, err := manager.Stage(context.Background(), "green", "/tmp/release-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stage(context.Background(), "green", "/tmp/release-b"); err == nil {
		t.Fatal("concurrent deployment transaction was accepted")
	}
}
