package deployd

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

type fakeRuntime struct {
	active                string
	observations          map[string]SlotObservation
	failSwitch            bool
	stageError            error
	inconsistent          bool
	identityError         error
	stateEpoch            string
	capabilities          []string
	mutateContractOnGrant bool
	invalidCapabilities   bool
	unknownCounters       bool
	failRevoke            bool
	failGrant             bool
	readyAfterGrant       bool
	enableRequiresReady   bool
	sequence              []string
	traffic               map[string]bool
	releaseIDs            map[string]string
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
		if releaseID := runtime.releaseIDs[slot]; releaseID != "" {
			value.ReleaseID = releaseID
		}
		value.SourceRevision = "revision-1"
		value.StateEpoch = runtime.stateEpoch
		if value.StateEpoch == "" {
			value.StateEpoch = "epoch-1"
		}
		if !runtime.invalidCapabilities {
			value.StateCapabilities = append([]string(nil), runtime.capabilities...)
			if len(value.StateCapabilities) == 0 {
				value.StateCapabilities = []string{"reads@1", "writes@1"}
			}
		}
		value.CountersKnown = !runtime.unknownCounters
		slots[slot] = value
	}
	now := time.Unix(100, 0).UTC()
	sources := map[string]SourceObservation{}
	for _, source := range []string{"active_link", "systemd", "slot_contracts", "native_observation", "inventory_observation"} {
		sources[source] = SourceObservation{Up: true, ObservedAt: now, FreshUntil: now.Add(time.Minute)}
	}
	return RuntimeSnapshot{ObservedAt: now, FreshUntil: now.Add(time.Minute), ActiveSlot: runtime.active, ControllerSlot: runtime.active, Consistent: !runtime.inconsistent, Slots: slots, Sources: sources}, nil
}
func (runtime *fakeRuntime) Stage(context.Context, string, string) error {
	runtime.sequence = append(runtime.sequence, "stage")
	return runtime.stageError
}
func (runtime *fakeRuntime) Verify(context.Context, string, string) error {
	runtime.sequence = append(runtime.sequence, "verify")
	return nil
}
func (runtime *fakeRuntime) VerifySlotIdentity(context.Context, string, string, string, []string) error {
	runtime.sequence = append(runtime.sequence, "verify-identity")
	return runtime.identityError
}
func (runtime *fakeRuntime) GrantController(_ context.Context, slot string, _ controllerrole.Lease) error {
	runtime.sequence = append(runtime.sequence, "grant")
	if runtime.mutateContractOnGrant {
		runtime.stateEpoch = "epoch-2"
	}
	if observation, ok := runtime.observations[slot]; ok {
		observation.ControllerOwned = true
		if runtime.readyAfterGrant {
			observation.Ready = true
		}
		runtime.observations[slot] = observation
	}
	if runtime.failGrant {
		return errors.New("grant response lost")
	}
	return nil
}
func (runtime *fakeRuntime) RevokeController(_ context.Context, slot string, _ uint64) error {
	runtime.sequence = append(runtime.sequence, "revoke")
	if runtime.failRevoke {
		return errors.New("revoke failed")
	}
	if observation, ok := runtime.observations[slot]; ok {
		observation.ControllerOwned = false
		runtime.observations[slot] = observation
	}
	return nil
}
func (runtime *fakeRuntime) FenceController(_ context.Context, slot string) error {
	runtime.sequence = append(runtime.sequence, "fence")
	if observation, ok := runtime.observations[slot]; ok {
		observation.ControllerOwned = false
		runtime.observations[slot] = observation
	}
	return nil
}
func (runtime *fakeRuntime) EnableTraffic(_ context.Context, slot string) error {
	runtime.sequence = append(runtime.sequence, "traffic")
	if runtime.enableRequiresReady && !runtime.observations[slot].Ready {
		return errors.New("slot is not ready")
	}
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
func (runtime *fakeRuntime) ExitDrain(context.Context, string) error {
	runtime.sequence = append(runtime.sequence, "drain-exit")
	return nil
}
func (runtime *fakeRuntime) Quiesce(context.Context, string) error {
	runtime.sequence = append(runtime.sequence, "quiesce")
	return nil
}
func (runtime *fakeRuntime) Stop(_ context.Context, slot string) error {
	runtime.sequence = append(runtime.sequence, "stop")
	value := runtime.observations[slot]
	value.ProcessUp = false
	value.ProcessReady = false
	value.Ready = false
	value.ControllerOwned = false
	runtime.observations[slot] = value
	return nil
}
func (runtime *fakeRuntime) Rollback(_ context.Context, target, previous string, _ controllerrole.Lease) error {
	runtime.sequence = append(runtime.sequence, "rollback")
	runtime.active = previous
	targetObservation := runtime.observations[target]
	targetObservation.ProcessUp = false
	targetObservation.ProcessReady = false
	targetObservation.Ready = false
	targetObservation.ControllerOwned = false
	targetObservation.TrafficAccepting = false
	runtime.observations[target] = targetObservation
	previousObservation := runtime.observations[previous]
	previousObservation.ProcessUp = true
	previousObservation.ProcessReady = true
	previousObservation.Ready = true
	previousObservation.ControllerOwned = true
	runtime.observations[previous] = previousObservation
	if runtime.traffic == nil {
		runtime.traffic = map[string]bool{}
	}
	runtime.traffic[previous] = true
	runtime.traffic[target] = false
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

func TestManagerFencesUncertainStageFailure(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{
		active:     "blue",
		stageError: &UncertainError{Cause: errors.New("restored process contract was not verified")},
		observations: map[string]SlotObservation{
			"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
			"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
		},
	}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	if _, err := manager.Stage(context.Background(), "green", "/tmp/release.tar"); err == nil {
		t.Fatal("uncertain stage failure unexpectedly succeeded")
	}
	transaction, err := database.ListActive(context.Background())
	if err != nil || len(transaction) != 1 || transaction[0].State != "needs_recovery" || !transaction[0].RecoveryRequired {
		t.Fatalf("uncertain stage transaction = %#v err=%v", transaction, err)
	}
	if !containsSequence(runtime.sequence, "fence") {
		t.Fatalf("uncertain stage failure did not fence slots: %v", runtime.sequence)
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

func TestManagerRejectsStageWhenTargetStillServesTraffic(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", traffic: map[string]bool{"blue": true, "green": true}, observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	if _, err := manager.Stage(context.Background(), "green", "/tmp/release.tar"); err == nil {
		t.Fatal("stage unexpectedly succeeded while the target accepted traffic")
	}
	if len(runtime.sequence) != 0 {
		t.Fatalf("stage mutated a serving target: %v", runtime.sequence)
	}
}

func TestManagerFencesWhenQuiescenceCountersAreUnknown(t *testing.T) {
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
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(context.Background(), transaction.ID); err != nil {
		t.Fatal(err)
	}
	runtime.unknownCounters = true
	if _, err := manager.Activate(context.Background(), transaction.ID); err == nil {
		t.Fatal("activation unexpectedly succeeded with unknown quiescence counters")
	}
	current, err := database.Get(context.Background(), transaction.ID)
	if err != nil || current == nil || current.State != "needs_recovery" || !current.RecoveryRequired {
		t.Fatalf("unknown-counter transaction = %#v err=%v", current, err)
	}
}

func TestManagerRollsBackFinalizedTransaction(t *testing.T) {
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
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(context.Background(), transaction.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background(), transaction.ID); err != nil {
		t.Fatal(err)
	}
	updated, err := manager.Rollback(context.Background(), transaction.ID)
	if err != nil {
		t.Fatalf("rollback finalized transaction: %v", err)
	}
	if updated.State != "finalized" || updated.Phase != PhaseFinalize || !updated.RollbackUsed {
		t.Fatalf("rolled back transaction = %#v", updated)
	}
}

func TestManagerFencesWhenRollbackStateCannotBePersisted(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{active: "green", observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
	}}
	transaction := store.Transaction{ID: "tx-rollback-persist-failure", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/bundle", Phase: PhaseFinalize, State: "finalized", Generation: 1, CreatedAt: now, UpdatedAt: now}
	if err := database.Create(context.Background(), transaction, map[string]any{
		"previous_release_id":         "release-blue",
		"previous_source_revision":    "revision-1",
		"previous_state_epoch":        "epoch-1",
		"previous_state_capabilities": []string{"reads@1", "writes@1"},
	}); err != nil {
		t.Fatal(err)
	}
	closed := false
	clock := func() time.Time {
		if !closed {
			closed = true
			_ = database.Close()
		}
		return now
	}
	manager := NewManager(database, runtime, time.Minute, clock)
	if _, err := manager.Rollback(context.Background(), transaction.ID); err == nil {
		t.Fatal("rollback unexpectedly succeeded after its state store was closed")
	} else if !strings.Contains(err.Error(), "persist rollback state") {
		t.Fatalf("rollback persistence error = %v", err)
	}
	for _, slot := range []string{"blue", "green"} {
		if runtime.observations[slot].ControllerOwned || runtime.traffic[slot] {
			t.Fatalf("slot %s remained active after rollback persistence failure: %#v", slot, runtime.observations[slot])
		}
	}
	if len(runtime.sequence) != 5 || runtime.sequence[0] != "verify-identity" || runtime.sequence[1] != "traffic-disable" || runtime.sequence[2] != "traffic-disable" || runtime.sequence[3] != "fence" || runtime.sequence[4] != "fence" {
		t.Fatalf("rollback persistence fencing sequence = %v", runtime.sequence)
	}
}

func TestManagerPersistsSanitizedFailureCause(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	transaction := store.Transaction{ID: "tx-failure-cause", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/bundle", Phase: PhaseStage, State: "active", Generation: 1, CreatedAt: now, UpdatedAt: now}
	if err := database.Create(context.Background(), transaction, nil); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(database, &fakeRuntime{}, time.Minute, func() time.Time { return now })
	cause := errors.New("counter proof failed\nslot=green")
	if err := manager.failure(context.Background(), transaction.ID, PhaseStage, cause); !errors.Is(err, cause) {
		t.Fatalf("failure returned %v, want %v", err, cause)
	}
	evidence, err := database.LatestFailureEvidence(context.Background(), transaction.ID)
	if err != nil || evidence == nil {
		t.Fatalf("failure evidence = %#v err=%v", evidence, err)
	}
	if evidence.Payload["cause"] != "counter proof failed slot=green" {
		t.Fatalf("failure cause = %#v", evidence.Payload["cause"])
	}
	if evidence.Payload["cause_type"] != "*errors.errorString" {
		t.Fatalf("failure cause type = %#v", evidence.Payload["cause_type"])
	}
}

func TestManagerResumesRollbackAfterRestart(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "green", observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
	}}
	transaction := store.Transaction{ID: "tx-resume-rollback", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/bundle", Phase: PhaseActiveSwitch, State: "active", Generation: 1, CreatedAt: now, UpdatedAt: now}
	if err := database.Create(context.Background(), transaction, map[string]any{"previous_release_id": "release-blue", "previous_source_revision": "revision-1", "previous_state_epoch": "epoch-1", "previous_state_capabilities": []string{"reads@1", "writes@1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginRollback(context.Background(), transaction.ID, transaction.Phase, PhaseRollback, "rollback_started", "test", nil, now); err != nil {
		t.Fatal(err)
	}
	if err := database.RecordFailure(context.Background(), transaction.ID, PhaseRollback, "state_unverified", "test", "digest", map[string]any{"message": "deployment state could not be verified"}, now); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	updated, err := manager.Rollback(context.Background(), transaction.ID)
	if err != nil {
		t.Fatalf("rollback resume failed: %v", err)
	}
	if updated.State != "finalized" || updated.Phase != PhaseFinalize || !updated.RollbackUsed {
		t.Fatalf("resumed rollback transaction = %#v", updated)
	}
	if runtime.active != "blue" || !runtime.traffic["blue"] {
		t.Fatalf("resumed rollback runtime active=%s traffic=%#v", runtime.active, runtime.traffic)
	}
}

func TestManagerRejectsRollbackWithoutPreviousBeforeEffects(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "green", observations: map[string]SlotObservation{
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
	}}
	transaction := store.Transaction{ID: "tx-no-previous", TargetSlot: "green", PreviousSlot: "none", Bundle: "/bundle", Phase: PhaseActiveSwitch, State: "active", Generation: 1, CreatedAt: now, UpdatedAt: now}
	if err := database.Create(context.Background(), transaction, nil); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	if _, err := manager.Rollback(context.Background(), transaction.ID); err == nil {
		t.Fatal("rollback without a previous slot unexpectedly succeeded")
	}
	if len(runtime.sequence) != 0 {
		t.Fatalf("rollback without previous slot caused effects: %v", runtime.sequence)
	}
	current, err := database.Get(context.Background(), transaction.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.RollbackUsed || current.Phase != PhaseActiveSwitch {
		t.Fatalf("rollback without previous slot changed transaction: %#v", current)
	}
}

func TestManagerRejectsCandidateIdentityChangedAfterVerify(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", releaseIDs: map[string]string{}, observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	transaction, err := manager.Stage(context.Background(), "green", "/bundle")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(context.Background(), transaction.ID); err != nil {
		t.Fatal(err)
	}
	runtime.releaseIDs["green"] = "release-replaced"
	if _, err := manager.Activate(context.Background(), transaction.ID); err == nil {
		t.Fatal("activation accepted a release changed after verify")
	}
	if len(runtime.sequence) != 3 || runtime.sequence[2] != "verify" {
		t.Fatalf("activation performed native effects after identity mismatch: %v", runtime.sequence)
	}
}

func TestManagerRejectsCandidateStateContractChangedAfterVerify(t *testing.T) {
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
	transaction, err := manager.Stage(context.Background(), "green", "/bundle")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(context.Background(), transaction.ID); err != nil {
		t.Fatal(err)
	}
	runtime.capabilities = []string{"legacy-agent-vm-core@1"}
	if _, err := manager.Activate(context.Background(), transaction.ID); err == nil {
		t.Fatal("candidate state contract change unexpectedly activated")
	}
	for _, action := range runtime.sequence {
		if action == "revoke" || action == "grant" {
			t.Fatalf("state contract mismatch caused controller effect: %v", runtime.sequence)
		}
	}
}

func TestManagerFencesWhenCandidateStateContractChangesAfterGrant(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", mutateContractOnGrant: true, observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	transaction, err := manager.Stage(context.Background(), "green", "/bundle")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(context.Background(), transaction.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background(), transaction.ID); err == nil {
		t.Fatal("candidate state contract change after grant unexpectedly activated")
	}
	for _, action := range runtime.sequence {
		if action == "switch" {
			t.Fatalf("state contract mismatch after grant caused an active-link switch: %v", runtime.sequence)
		}
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

func TestManagerWaitsForReadyAfterGrantBeforeTraffic(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", readyAfterGrant: true, enableRequiresReady: true, observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: false},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	if err := manager.MaintainController(context.Background()); err != nil {
		t.Fatalf("controller maintenance failed after waiting for readiness: %v; sequence=%v", err, runtime.sequence)
	}
	if !runtime.traffic["blue"] {
		t.Fatalf("active traffic was not enabled after readiness: %v", runtime.sequence)
	}
}

func TestManagerHardFencesInconsistentObservation(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", inconsistent: true, observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	if err := manager.MaintainController(context.Background()); err == nil {
		t.Fatal("inconsistent observation unexpectedly maintained a controller")
	}
	for slot, observation := range runtime.observations {
		if observation.ControllerOwned {
			t.Fatalf("slot %s retained controller ownership after hard fencing", slot)
		}
	}
	term, err := database.NextFenceTerm(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if term != 1 {
		t.Fatalf("hard fencing allocated a durable term: %d", term)
	}
}

func TestManagerHardFencesWhenObservationPersistenceFails(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	runtime := &fakeRuntime{active: "blue", observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := manager.MaintainController(context.Background()); err == nil {
		t.Fatal("database failure unexpectedly maintained a controller")
	}
	for slot, observation := range runtime.observations {
		if observation.ControllerOwned {
			t.Fatalf("slot %s retained controller ownership after persistence failure", slot)
		}
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
	wantSequence := []string{"traffic-disable", "traffic-disable", "fence", "fence"}
	if len(runtime.sequence) != len(wantSequence) {
		t.Fatalf("fencing sequence = %v", runtime.sequence)
	}
	for index, want := range wantSequence {
		if runtime.sequence[index] != want {
			t.Fatalf("fencing sequence = %v, want %v", runtime.sequence, wantSequence)
		}
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
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	transaction, err := manager.Stage(context.Background(), "green", "/bundle")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(context.Background(), transaction.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Transition(context.Background(), transaction.ID, PhaseControllerHandoff, PhaseActiveSwitch, "active", "active_switched", "test", nil, now); err != nil {
		t.Fatal(err)
	}
	transaction, err = database.Get(context.Background(), transaction.ID)
	if err != nil || transaction == nil {
		t.Fatalf("read recovery transaction: %#v err=%v", transaction, err)
	}
	if err := manager.RecoverOnStart(context.Background()); err == nil || !errors.Is(err, ErrUncertain) {
		t.Fatalf("restart recovery error = %v", err)
	}
	runtime.active = "green"
	runtime.traffic = map[string]bool{"blue": false, "green": false}
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
	if !runtime.traffic["green"] {
		t.Fatal("recovery finalize did not restore target traffic")
	}
	if runtime.observations["blue"].ProcessUp || runtime.observations["blue"].ControllerOwned {
		t.Fatalf("recovery finalize left the previous slot running: %#v", runtime.observations["blue"])
	}
	if !containsSequence(runtime.sequence, "stop") {
		t.Fatalf("recovery finalize did not stop the previous slot: %v", runtime.sequence)
	}
}

func TestManagerRecoveryFinalizeFencesWhenPreviousRevokeFails(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", failRevoke: true, observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	transaction, err := manager.Stage(context.Background(), "green", "/bundle")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(context.Background(), transaction.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Transition(context.Background(), transaction.ID, PhaseControllerHandoff, PhaseActiveSwitch, "active", "active_switched", "test", nil, now); err != nil {
		t.Fatal(err)
	}
	transaction, err = database.Get(context.Background(), transaction.ID)
	if err != nil || transaction == nil {
		t.Fatalf("read recovery transaction: %#v err=%v", transaction, err)
	}
	if err := manager.RecoverOnStart(context.Background()); err == nil || !errors.Is(err, ErrUncertain) {
		t.Fatalf("restart recovery error = %v", err)
	}
	runtime.active = "green"
	runtime.traffic = map[string]bool{"blue": false, "green": false}
	if _, err := manager.Recover(context.Background(), transaction.ID, "finalize"); err == nil || !errors.Is(err, ErrUncertain) {
		t.Fatalf("recovery finalize error = %v", err)
	}
	if runtime.observations["blue"].ControllerOwned || runtime.observations["green"].ControllerOwned {
		t.Fatalf("recovery revoke failure did not fence both slots: %#v", runtime.observations)
	}
}

func TestManagerRecoveryFinalizeFencesWhenTargetGrantIsUncertain(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", failGrant: true, observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	transaction, err := manager.Stage(context.Background(), "green", "/bundle")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Verify(context.Background(), transaction.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Transition(context.Background(), transaction.ID, PhaseControllerHandoff, PhaseActiveSwitch, "active", "active_switched", "test", nil, now); err != nil {
		t.Fatal(err)
	}
	transaction, err = database.Get(context.Background(), transaction.ID)
	if err != nil || transaction == nil {
		t.Fatalf("read recovery transaction: %#v err=%v", transaction, err)
	}
	if err := manager.RecoverOnStart(context.Background()); err == nil || !errors.Is(err, ErrUncertain) {
		t.Fatalf("restart recovery error = %v", err)
	}
	runtime.active = "green"
	runtime.traffic = map[string]bool{"blue": false, "green": false}
	if _, err := manager.Recover(context.Background(), transaction.ID, "finalize"); err == nil || !errors.Is(err, ErrUncertain) {
		t.Fatalf("recovery finalize error = %v", err)
	}
	if runtime.observations["blue"].ControllerOwned || runtime.observations["green"].ControllerOwned {
		t.Fatalf("uncertain target grant did not fence both slots: %#v", runtime.observations)
	}
}

func TestManagerDoesNotMaintainControllerWithoutStateCapabilities(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", invalidCapabilities: true, observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	if err := manager.MaintainController(context.Background()); err == nil {
		t.Fatal("controller maintenance succeeded without state capabilities")
	}
	if runtime.observations["blue"].ControllerOwned || runtime.observations["green"].ControllerOwned {
		t.Fatalf("controller roles were not fenced without state capabilities: %#v", runtime.observations)
	}
}

func containsSequence(sequence []string, expected string) bool {
	for _, item := range sequence {
		if item == expected {
			return true
		}
	}
	return false
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
