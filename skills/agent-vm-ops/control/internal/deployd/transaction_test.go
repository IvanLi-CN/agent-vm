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
	traffic      map[string]bool
}

func (runtime *fakeRuntime) Observe(context.Context) (RuntimeSnapshot, error) {
	slots := make(map[string]SlotObservation, len(runtime.observations))
	for slot, value := range runtime.observations {
		if runtime.traffic == nil {
			value.TrafficAccepting = runtime.active == slot
		} else {
			value.TrafficAccepting = runtime.traffic[slot]
		}
		value.ReleaseID = "release-" + slot
		value.SourceRevision = "revision-1"
		value.StateEpoch = "epoch-1"
		value.StateCapabilities = []string{"reads@1", "writes@1"}
		slots[slot] = value
	}
	now := time.Unix(100, 0).UTC()
	sources := map[string]SourceObservation{}
	for _, source := range []string{"active_link", "slot_contracts", "native_observation", "inventory_observation"} {
		sources[source] = SourceObservation{Up: true, ObservedAt: now}
	}
	return RuntimeSnapshot{ObservedAt: now, FreshUntil: now.Add(time.Minute), ActiveSlot: runtime.active, ControllerSlot: runtime.active, Consistent: true, Slots: slots, Sources: sources}, nil
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
func (runtime *fakeRuntime) RevokeController(_ context.Context, slot string, _ uint64) error {
	runtime.sequence = append(runtime.sequence, "revoke")
	if observation, ok := runtime.observations[slot]; ok {
		observation.ControllerOwned = false
		runtime.observations[slot] = observation
	}
	return nil
}
func (runtime *fakeRuntime) EnableTraffic(_ context.Context, slot string) error {
	runtime.sequence = append(runtime.sequence, "traffic")
	if runtime.traffic == nil {
		runtime.traffic = map[string]bool{}
	}
	runtime.traffic[slot] = true
	return nil
}
func (runtime *fakeRuntime) DisableTraffic(_ context.Context, slot string) error {
	runtime.sequence = append(runtime.sequence, "traffic-disable")
	if runtime.traffic == nil {
		runtime.traffic = map[string]bool{}
	}
	runtime.traffic[slot] = false
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
	value := runtime.observations["blue"]
	value.ProcessUp = false
	value.ProcessReady = false
	value.Ready = false
	value.ControllerOwned = false
	runtime.observations["blue"] = value
	return nil
}
func (runtime *fakeRuntime) Rollback(context.Context, string, string, controllerrole.Lease) error {
	runtime.sequence = append(runtime.sequence, "rollback")
	runtime.active = "blue"
	if runtime.traffic == nil {
		runtime.traffic = map[string]bool{}
	}
	runtime.traffic["blue"] = true
	runtime.traffic["green"] = false
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
	wantSequence := []string{"traffic-disable", "revoke", "grant", "traffic"}
	if len(runtime.sequence) != len(wantSequence) {
		t.Fatalf("controller maintenance sequence = %v, want %v", runtime.sequence, wantSequence)
	}
	for index, want := range wantSequence {
		if runtime.sequence[index] != want {
			t.Fatalf("controller maintenance sequence = %v, want %v", runtime.sequence, wantSequence)
		}
	}
	if !runtime.traffic["blue"] {
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
	runtime.traffic = map[string]bool{"blue": false, "green": true}
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
