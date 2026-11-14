package deployd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
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
	preflightError        error
	preflightCalls        int
	inconsistent          bool
	partialInventory      bool
	identityError         error
	stateEpoch            string
	capabilities          []string
	mutateContractOnGrant bool
	invalidCapabilities   bool
	unknownCounters       bool
	failRevoke            bool
	failDisableTraffic    bool
	failGrant             bool
	readyAfterGrant       bool
	enableRequiresReady   bool
	sequence              []string
	traffic               map[string]bool
	releaseIDs            map[string]string
	legacyGrant           bool
	grantedLeases         []controllerrole.Lease
}

func (runtime *fakeRuntime) RenewTransactionLease(_ context.Context, lease controllerrole.Lease) (controllerrole.Lease, error) {
	lease.Expiry = lease.Expiry.Add(time.Minute)
	return lease, nil
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
		if value.StateAuthority == "" {
			value.StateAuthority = "authority-1"
		}
		if runtime.stateEpoch != "" {
			value.StateEpoch = runtime.stateEpoch
		}
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
	return RuntimeSnapshot{ObservedAt: now, FreshUntil: now.Add(time.Minute), ActiveSlot: runtime.active, ControllerSlot: runtime.active, Consistent: !runtime.inconsistent && !runtime.partialInventory, ControllerRecoveryOnly: runtime.partialInventory && !runtime.inconsistent, Slots: slots, Sources: sources}, nil
}
func (runtime *fakeRuntime) Stage(context.Context, string, string) error {
	runtime.sequence = append(runtime.sequence, "stage")
	return runtime.stageError
}
func (runtime *fakeRuntime) Preflight(context.Context, string, string) error {
	runtime.preflightCalls++
	return runtime.preflightError
}
func (runtime *fakeRuntime) Verify(context.Context, string, string) error {
	runtime.sequence = append(runtime.sequence, "verify")
	return nil
}
func (runtime *fakeRuntime) VerifySlotIdentity(context.Context, string, string, string, []string) error {
	runtime.sequence = append(runtime.sequence, "verify-identity")
	return runtime.identityError
}
func (runtime *fakeRuntime) GrantController(_ context.Context, slot string, lease controllerrole.Lease) error {
	if runtime.legacyGrant && len(runtime.grantedLeases) > 0 && lease.Term <= runtime.grantedLeases[len(runtime.grantedLeases)-1].Term {
		return controllerrole.ErrLeaseMismatch
	}
	runtime.grantedLeases = append(runtime.grantedLeases, lease)
	runtime.sequence = append(runtime.sequence, "grant")
	if runtime.mutateContractOnGrant {
		runtime.capabilities = []string{"changed@1"}
	}
	if observation, ok := runtime.observations[slot]; ok {
		observation.ControllerOwned = true
		observation.ControllerLease = &lease
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
	if runtime.failDisableTraffic {
		return errors.New("traffic disable failed")
	}
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
func (runtime *fakeRuntime) Rollback(_ context.Context, target, previous string, _ controllerrole.Lease, _ string) error {
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
	runtime := &fakeRuntime{active: "blue", readyAfterGrant: true, observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: false},
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

func TestManagerRejectsIncompatibleCandidateBeforeCreatingTransaction(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", preflightError: &DeploymentCompatibilityError{Cause: errors.New("deployment contract major 2 is incompatible with Deployd v1")}, observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	if _, err := manager.Stage(context.Background(), "green", "/tmp/incompatible.tar"); !errors.As(err, new(*DeploymentCompatibilityError)) {
		t.Fatalf("stage error = %v, want deployment compatibility blocker", err)
	}
	active, err := database.ListActive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Fatalf("active transactions = %#v, want none", active)
	}
	if runtime.preflightCalls != 1 || len(runtime.sequence) != 0 {
		t.Fatalf("preflight calls=%d runtime sequence=%v, want one preflight and no effects", runtime.preflightCalls, runtime.sequence)
	}
}

func TestStageDrainedControllerForRecovery(t *testing.T) {
	for _, mode := range []string{"drained", "not-drained", "in-flight", "worker", "unknown-counters", "no-controller", "target-controller"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(100, 0).UTC()
			database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			active := SlotObservation{Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: false, Draining: true, ControllerOwned: true, PendingOperations: 1}
			target := SlotObservation{Slot: "green", ProcessUp: true, ProcessReady: true}
			runtime := &fakeRuntime{active: "blue", traffic: map[string]bool{}, observations: map[string]SlotObservation{}}
			switch mode {
			case "not-drained":
				active.Draining = false
			case "in-flight":
				active.InFlightRequests = 1
			case "worker":
				active.NativeWorkers = 1
			case "unknown-counters":
				runtime.unknownCounters = true
			case "no-controller":
				active.ControllerOwned = false
			case "target-controller":
				target.ControllerOwned = true
			}
			runtime.observations["blue"], runtime.observations["green"] = active, target
			manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
			transaction, err := manager.Stage(context.Background(), "green", "/tmp/recovery.tar")
			if mode == "drained" {
				if err != nil || transaction.Phase != PhaseStandbyValidation {
					t.Fatalf("stage = %#v, %v", transaction, err)
				}
			} else if err == nil || len(runtime.sequence) != 0 {
				t.Fatalf("unsafe stage: %#v, %v, %v", transaction, err, runtime.sequence)
			}
		})
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

func TestManagerVerifiesFencedStandbyCandidate(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", observations: map[string]SlotObservation{
		"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true, ReleaseID: "old", SourceRevision: "old", StateEpoch: "epoch", StateCapabilities: []string{"cap@1"}},
		"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: false, ControllerOwned: false, TrafficAccepting: false, ReleaseID: "new", SourceRevision: "new", StateEpoch: "epoch", StateCapabilities: []string{"cap@1"}},
	}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	transaction, err := manager.Stage(context.Background(), "green", "/tmp/release.tar")
	if err != nil {
		t.Fatal(err)
	}
	verified, err := manager.Verify(context.Background(), transaction.ID)
	if err != nil {
		t.Fatalf("verify fenced standby: %v", err)
	}
	if verified.Phase != PhaseControllerHandoff || verified.State != "active" {
		t.Fatalf("verified transaction = %#v", verified)
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

func TestQuiescenceEvidenceAllowsTransientGlobalInconsistency(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	snapshot := RuntimeSnapshot{
		ObservedAt: now,
		FreshUntil: now.Add(time.Minute),
		Consistent: false,
		Sources: map[string]SourceObservation{
			"active_link":    {Up: true, ObservedAt: now, FreshUntil: now.Add(time.Minute)},
			"slot_contracts": {Up: true, ObservedAt: now, FreshUntil: now.Add(time.Minute)},
		},
	}
	if !quiescenceObservationValid(snapshot, now) {
		t.Fatal("fresh source evidence should be sufficient for previous-slot quiescence")
	}
	snapshot.Sources["slot_contracts"] = SourceObservation{Up: false, ObservedAt: now, FreshUntil: now.Add(time.Minute)}
	if quiescenceObservationValid(snapshot, now) {
		t.Fatal("down source must invalidate quiescence evidence")
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
		"previous_release_id":      "release-blue",
		"previous_source_revision": "revision-1",
		"previous_state_authority": "authority-1", "previous_state_epoch": "epoch-1",
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
	cause := errors.New("counter proof failed\n/var/lib/agent-vm/private/release.tar")
	if err := manager.failure(context.Background(), transaction.ID, PhaseStage, cause); !errors.Is(err, cause) {
		t.Fatalf("failure returned %v, want %v", err, cause)
	}
	evidence, err := database.LatestFailureEvidence(context.Background(), transaction.ID)
	if err != nil || evidence == nil {
		t.Fatalf("failure evidence = %#v err=%v", evidence, err)
	}
	if evidence.Payload["cause"] != "counter proof failed <private-path>" {
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
	if err := database.Create(context.Background(), transaction, map[string]any{"previous_release_id": "release-blue", "previous_source_revision": "revision-1", "previous_state_authority": "authority-1", "previous_state_epoch": "epoch-1", "previous_state_capabilities": []string{"reads@1", "writes@1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginRollback(context.Background(), transaction.ID, transaction.Phase, PhaseRollback, "rollback_started", "test", nil, now); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"message": "deployment state could not be verified"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	if err := database.RecordFailure(context.Background(), transaction.ID, PhaseRollback, "state_unverified", "test", hex.EncodeToString(digest[:]), payload, now); err != nil {
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

type delayedControllerRuntime struct {
	*fakeRuntime
	renewals atomic.Int32
}

func (runtime *delayedControllerRuntime) Observe(ctx context.Context) (RuntimeSnapshot, error) {
	snapshot, err := runtime.fakeRuntime.Observe(ctx)
	if snapshot.Slots["blue"].ControllerOwned {
		timer := time.NewTimer(90 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return RuntimeSnapshot{}, ctx.Err()
		case <-timer.C:
		}
		value := snapshot.Slots["blue"]
		value.Ready = true
		snapshot.Slots["blue"] = value
	}
	return snapshot, err
}

func (runtime *delayedControllerRuntime) RenewTransactionLease(ctx context.Context, lease controllerrole.Lease) (controllerrole.Lease, error) {
	runtime.renewals.Add(1)
	return runtime.fakeRuntime.RenewTransactionLease(ctx, lease)
}

func TestControllerMaintenanceRenewsWhileWaitingForReadiness(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &delayedControllerRuntime{fakeRuntime: &fakeRuntime{active: "blue", observations: map[string]SlotObservation{
		"blue": {Slot: "blue", ProcessUp: true, ProcessReady: true},
	}}}
	manager := NewManager(database, runtime, 30*time.Millisecond, func() time.Time { return now })
	if err := manager.MaintainController(t.Context()); err != nil {
		t.Fatal(err)
	}
	if runtime.renewals.Load() < 2 || !runtime.traffic["blue"] {
		t.Fatalf("readiness did not retain its granted authority: renewals=%d traffic=%v", runtime.renewals.Load(), runtime.traffic)
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

type changingObservationRuntime struct {
	*fakeRuntime
	reads             int
	unstableReads     int
	onRead            func(int)
	invalidRoleSource string
}

func TestTransactionObservationRenewsOnlyVerifiedLiveCandidate(t *testing.T) {
	start := controllerrole.ProcessStartToken(os.Getpid())
	if start == "" {
		t.Skip("Linux process-incarnation evidence required")
	}
	for _, mode := range []string{"live", "transient", "guarded", "inconsistent", "expired", "other-owner"} {
		t.Run(mode, func(t *testing.T) {
			now := time.Unix(100, 0).UTC()
			database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), time.Hour, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			transaction := store.Transaction{ID: "tx-observe", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/tmp/recovery.tar", Phase: PhaseQuiescence, State: "active", Generation: 1, CreatedAt: now, UpdatedAt: now}
			if err := database.Create(context.Background(), transaction, nil); err != nil {
				t.Fatal(err)
			}
			lease := controllerrole.Lease{Slot: "green", Term: 1, Generation: 1, LeaseID: "lease", Nonce: "nonce", OwnerPID: os.Getpid(), OwnerStartTime: start, Expiry: now.Add(time.Second)}
			if mode == "expired" {
				lease.Expiry = now.Add(-time.Second)
			}
			if mode == "other-owner" {
				lease.OwnerStartTime = "other"
			}
			runtime := &changingObservationRuntime{fakeRuntime: &fakeRuntime{active: "green", observations: map[string]SlotObservation{
				"green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true, LeaseRenewalSupported: true, ControllerLease: &lease},
				"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true},
			}}}
			if mode == "transient" {
				runtime.unstableReads = 1
			}
			if mode == "inconsistent" {
				runtime.unstableReads = 10
			}
			manager := NewManager(database, runtime, 15*time.Second, func() time.Time { return now })
			observationContext := context.Background()
			if mode == "guarded" {
				var stop func() error
				observationContext, stop = manager.beginTransactionRenewal(observationContext, lease)
				defer stop()
			}
			_, err = manager.observe(observationContext, transaction.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantGrant := mode == "live" || mode == "transient"
			if (len(runtime.grantedLeases) == 1) != wantGrant {
				t.Fatalf("grants=%v reads=%d", runtime.grantedLeases, runtime.reads)
			}
			if wantGrant {
				renewed := runtime.grantedLeases[0]
				if !renewed.Expiry.Equal(now.Add(15 * time.Second)) {
					t.Fatalf("expiry=%v", renewed.Expiry)
				}
				renewed.Expiry = lease.Expiry
				if renewed != lease {
					t.Fatal("renewal changed controller identity")
				}
			}
		})
	}
}

func (runtime *changingObservationRuntime) Observe(ctx context.Context) (RuntimeSnapshot, error) {
	runtime.reads++
	if runtime.onRead != nil {
		runtime.onRead(runtime.reads)
	}
	snapshot, err := runtime.fakeRuntime.Observe(ctx)
	if runtime.invalidRoleSource != "" {
		snapshot.Consistent = false
		source := snapshot.Sources[runtime.invalidRoleSource]
		source.Up = false
		snapshot.Sources[runtime.invalidRoleSource] = source
	}
	if runtime.reads <= runtime.unstableReads {
		snapshot.Consistent = false
		source := snapshot.Sources["native_observation"]
		source.Up = false
		snapshot.Sources["native_observation"] = source
	}
	return snapshot, err
}

func TestMaintainControllerRetriesChangingStateBeforeRevokingLiveLease(t *testing.T) {
	if controllerrole.ProcessStartToken(os.Getpid()) == "" {
		t.Skip("Linux process-incarnation evidence required")
	}
	for _, test := range []struct {
		name      string
		unstable  int
		corrupt   string
		wantReads int
		wantError bool
		preserve  bool
	}{
		{"one-state-change", 1, "", 2, false, false},
		{"two-state-changes", 2, "", 3, false, false},
		{"persistent-inconsistency", 10, "", 3, true, true},
		{"traffic-disable-failure", 10, "traffic", 3, true, false},
		{"active-link-unavailable", 10, "source-active_link", 1, true, false},
		{"systemd-unavailable", 10, "source-systemd", 1, true, false},
		{"role-channel-unavailable", 10, "source-slot_contracts", 1, true, false},
		{"foreign-owner", 1, "owner", 1, true, false},
		{"missing-lease", 1, "lease", 1, true, false},
		{"expired-lease", 1, "expired", 1, true, false},
		{"expiry-during-retry", 1, "expiry-during-retry", 2, true, false},
		{"identity-change-during-retry", 1, "identity-during-retry", 2, true, false},
		{"second-controller", 1, "second", 1, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(100, 0).UTC()
			database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			lease := controllerrole.Lease{Slot: "blue", Term: 7, LeaseID: "existing-lease", Nonce: "existing-nonce", Generation: 11, Expiry: now.Add(time.Minute), OwnerPID: os.Getpid(), OwnerStartTime: controllerrole.ProcessStartToken(os.Getpid())}
			if test.preserve {
				lease.Expiry = now.Add(5 * time.Second)
			}
			runtime := &changingObservationRuntime{fakeRuntime: &fakeRuntime{active: "blue", observations: map[string]SlotObservation{
				"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true, LeaseRenewalSupported: true, ControllerLease: &lease, NativeWorkers: 1, PendingOperations: 1},
				"green": {Slot: "green", ProcessUp: true, ProcessReady: true},
			}}, unstableReads: test.unstable}
			if strings.HasPrefix(test.corrupt, "source-") {
				runtime.invalidRoleSource = strings.TrimPrefix(test.corrupt, "source-")
			}
			switch test.corrupt {
			case "traffic":
				runtime.failDisableTraffic = true
			case "owner":
				lease.OwnerPID++
			case "lease":
				active := runtime.observations["blue"]
				active.ControllerLease = nil
				runtime.observations["blue"] = active
			case "expired":
				lease.Expiry = now
			case "expiry-during-retry":
				lease.Expiry = now.Add(time.Second)
				runtime.onRead = func(read int) {
					if read == 2 {
						now = now.Add(2 * time.Second)
					}
				}
			case "identity-during-retry":
				runtime.onRead = func(read int) {
					if read == 2 {
						lease.LeaseID = "unexpected-replacement"
					}
				}
			case "second":
				standby := runtime.observations["green"]
				standby.ControllerOwned = true
				runtime.observations["green"] = standby
			}
			manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
			err = manager.MaintainController(context.Background())
			if (err != nil) != test.wantError {
				t.Fatalf("maintain error=%v sequence=%v", err, runtime.sequence)
			}
			if runtime.reads != test.wantReads {
				t.Fatalf("observation reads=%d want=%d", runtime.reads, test.wantReads)
			}
			if test.preserve {
				if len(runtime.grantedLeases) != 1 || !runtime.observations["blue"].ControllerOwned || runtime.grantedLeases[0].LeaseID != lease.LeaseID {
					t.Fatal("business observation failure interrupted the verified live authority")
				}
				if !runtime.grantedLeases[0].Expiry.After(lease.Expiry) {
					t.Fatal("business observation failure did not extend the live writer deadline")
				}
				for _, action := range runtime.sequence {
					if action == "fence" || action == "traffic" {
						t.Fatalf("inconsistent business reads performed role effects: %v", runtime.sequence)
					}
				}
				if runtime.traffic["blue"] || runtime.traffic["green"] {
					t.Fatal("inconsistent business reads left admission open")
				}
				// A subsequent coherent sample must recover without changing the
				// mutation's identity. Lost process evidence still fences authority.
				runtime.unstableReads = runtime.reads
				if err := manager.MaintainController(context.Background()); err != nil {
					t.Fatalf("coherent next tick did not recover: %v", err)
				}
				if len(runtime.grantedLeases) != 2 || runtime.grantedLeases[1].LeaseID != lease.LeaseID {
					t.Fatal("next tick replaced the existing lease")
				}
				now = runtime.observations["blue"].ControllerLease.Expiry.Add(time.Second)
				runtime.unstableReads = runtime.reads + 10
				if err := manager.MaintainController(context.Background()); err == nil || runtime.observations["blue"].ControllerOwned {
					t.Fatal("expired lease survived persistent observation failure")
				}
				return
			}
			if test.wantError {
				if len(runtime.grantedLeases) != 0 {
					t.Fatal("invalid observation renewed authority")
				}
				for _, value := range runtime.observations {
					if value.ControllerOwned {
						t.Fatal("unresolved observation retained authority")
					}
				}
				return
			}
			if len(runtime.grantedLeases) != 1 || runtime.grantedLeases[0].LeaseID != lease.LeaseID || runtime.grantedLeases[0].Term != lease.Term || runtime.grantedLeases[0].Generation != lease.Generation || runtime.grantedLeases[0].Nonce != lease.Nonce {
				t.Fatal("retry changed the running mutation's lease identity")
			}
			for _, action := range runtime.sequence {
				if action == "fence" {
					t.Fatal("normal state change revoked the live controller")
				}
			}
		})
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
	// A fenced process exposes its process contract, but scheduler startup and
	// admission readiness require the new controller grant first.
	target := runtime.observations["green"]
	target.Ready = false
	runtime.observations["green"] = target
	runtime.readyAfterGrant = true
	runtime.enableRequiresReady = true
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

func TestControllerRecoveryRetainsOnlyDrainedAuthority(t *testing.T) {
	for _, test := range []struct {
		name                                     string
		partial, draining, inconsistent, allowed bool
	}{
		{"complete-drain", false, true, false, true},
		{"partial-drain", true, true, false, true},
		{"partial-without-drain", true, false, false, false},
		{"inconsistent-drain", true, true, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Unix(100, 0).UTC()
			database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			runtime := &fakeRuntime{active: "blue", partialInventory: test.partial, inconsistent: test.inconsistent, observations: map[string]SlotObservation{
				"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: false, Draining: test.draining},
				"green": {Slot: "green", ProcessUp: true, ProcessReady: true, ControllerOwned: true},
			}}
			manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
			err = manager.MaintainController(context.Background())
			if (err == nil) != test.allowed {
				t.Fatalf("maintenance err=%v; sequence=%v", err, runtime.sequence)
			}
			if runtime.traffic["blue"] || runtime.traffic["green"] {
				t.Fatalf("recovery enabled traffic: %v", runtime.traffic)
			}
			if runtime.observations["blue"].ControllerOwned != test.allowed || runtime.observations["green"].ControllerOwned {
				t.Fatalf("unexpected ownership: %+v", runtime.observations)
			}
			snapshot, _ := runtime.Observe(context.Background())
			if test.partial && validObservation(snapshot, now) {
				t.Fatal("partial recovery view authorized deployment")
			}
			if !test.allowed {
				return
			}
			// Once reconciliation and the operator's drain exit are observed, ordinary
			// readiness checks may restore traffic without changing active routing.
			runtime.partialInventory = false
			active := runtime.observations["blue"]
			active.Draining = false
			active.Ready = true
			runtime.observations["blue"] = active
			if err := manager.MaintainController(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !runtime.traffic["blue"] || runtime.traffic["green"] {
				t.Fatalf("traffic was not restored exclusively: %v", runtime.traffic)
			}
		})
	}
}

func TestOnlyCurrentProcessLiveLeaseCanBeRenewed(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	manager := NewManager(database, nil, 15*time.Second, func() time.Time { return now })
	lease, err := manager.newLease(context.Background(), "blue", 7)
	if err != nil {
		t.Fatal(err)
	}
	if lease.OwnerStartTime == "" {
		t.Skip("process-incarnation evidence is unavailable on this platform")
	}
	now = now.Add(time.Second)
	target := SlotObservation{Slot: "blue", LeaseRenewalSupported: true, ControllerOwned: true, ControllerLease: &lease}
	renewed, err := manager.renewOrAllocateLease(context.Background(), target, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	expected := lease
	expected.Expiry = now.Add(15 * time.Second)
	if renewed != expected {
		t.Fatalf("renewal changed authority identity: got %+v want %+v", renewed, expected)
	}
	for name, mutate := range map[string]func(*SlotObservation){
		"foreign process":      func(s *SlotObservation) { s.ControllerLease.OwnerPID++ },
		"previous incarnation": func(s *SlotObservation) { s.ControllerLease.OwnerStartTime = "previous" },
		"expired":              func(s *SlotObservation) { s.ControllerLease.Expiry = now },
		"wrong slot":           func(s *SlotObservation) { s.ControllerLease.Slot = "green" },
		"not owned":            func(s *SlotObservation) { s.ControllerOwned = false },
	} {
		t.Run(name, func(t *testing.T) {
			copyLease := lease
			candidate := target
			candidate.ControllerLease = &copyLease
			mutate(&candidate)
			fresh, err := manager.renewOrAllocateLease(context.Background(), candidate, 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.Term <= lease.Term || fresh.LeaseID == lease.LeaseID || fresh.Nonce == lease.Nonce {
				t.Fatal("unowned or stale authority reused")
			}
		})
	}
}

func TestMaintainControllerNegotiatesLegacyAndRenewalSlots(t *testing.T) {
	for _, renewal := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "renewal"}[renewal], func(t *testing.T) {
			if renewal && controllerrole.ProcessStartToken(os.Getpid()) == "" {
				t.Skip("Linux process-incarnation evidence required")
			}
			now := time.Unix(100, 0).UTC()
			database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			runtime := &fakeRuntime{active: "blue", legacyGrant: !renewal, partialInventory: true, observations: map[string]SlotObservation{
				"blue": {Slot: "blue", ProcessUp: true, ProcessReady: true, Draining: true, LeaseRenewalSupported: renewal},
			}}
			manager := NewManager(database, runtime, 15*time.Second, func() time.Time { return now })
			for i := 0; i < 3; i++ {
				if err := manager.MaintainController(context.Background()); err != nil {
					t.Fatalf("round %d: %v", i, err)
				}
				now = now.Add(time.Second)
			}
			if len(runtime.grantedLeases) != 3 {
				t.Fatalf("grants=%d", len(runtime.grantedLeases))
			}
			first, last := runtime.grantedLeases[0], runtime.grantedLeases[2]
			if renewal {
				if first.Term != last.Term || first.LeaseID != last.LeaseID || first.Nonce != last.Nonce || !last.Expiry.After(first.Expiry) {
					t.Fatal("modern renewal replaced authority or failed to extend it")
				}
			} else if last.Term <= first.Term {
				t.Fatal("legacy slot received unsupported same-term renewal")
			}
			if runtime.traffic["blue"] {
				t.Fatal("drained slot accepted traffic")
			}
			if strings.Contains(strings.Join(runtime.sequence, ","), "fence") {
				t.Fatal("continuous maintenance fenced a compatible slot")
			}
		})
	}
}

func TestMaintainControllerRenewsLiveAuthorityDuringPartialInventory(t *testing.T) {
	if controllerrole.ProcessStartToken(os.Getpid()) == "" {
		t.Skip("Linux process-incarnation evidence required")
	}
	for _, invalid := range []string{"", "expired", "foreign-owner", "old-incarnation", "legacy", "second-controller", "disable-failure", "grant-failure"} {
		t.Run(invalid, func(t *testing.T) {
			now := time.Unix(100, 0).UTC()
			database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			runtime := &fakeRuntime{active: "blue", partialInventory: true, observations: map[string]SlotObservation{
				"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: false, ControllerOwned: true, LeaseRenewalSupported: true, NativeWorkers: 1, PendingOperations: 1},
				"green": {Slot: "green", ProcessUp: true, ProcessReady: true},
			}}
			manager := NewManager(database, runtime, 15*time.Second, func() time.Time { return now })
			lease, err := manager.newLease(context.Background(), "blue", 7)
			if err != nil {
				t.Fatal(err)
			}
			switch invalid {
			case "expired":
				lease.Expiry = now
			case "foreign-owner":
				lease.OwnerPID++
			case "old-incarnation":
				lease.OwnerStartTime = "old"
			case "legacy":
				value := runtime.observations["blue"]
				value.LeaseRenewalSupported = false
				runtime.observations["blue"] = value
			case "disable-failure":
				runtime.failDisableTraffic = true
			case "grant-failure":
				runtime.failGrant = true
			case "second-controller":
				value := runtime.observations["green"]
				value.ControllerOwned = true
				runtime.observations["green"] = value
			}
			value := runtime.observations["blue"]
			value.ControllerLease = &lease
			runtime.observations["blue"] = value
			original := lease
			now = now.Add(time.Second)
			err = manager.MaintainController(context.Background())
			if invalid != "" {
				if err == nil || (invalid != "grant-failure" && len(runtime.grantedLeases) != 0) || runtime.observations["blue"].ControllerOwned || runtime.observations["green"].ControllerOwned {
					t.Fatalf("invalid authority retained: err=%v sequence=%v", err, runtime.sequence)
				}
				return
			}
			if err != nil {
				t.Fatalf("normal in-flight partial inventory cancelled the controller: %v; sequence=%v", err, runtime.sequence)
			}
			for round := 0; round < 4; round++ {
				if runtime.traffic["blue"] || runtime.traffic["green"] {
					t.Fatal("partial inventory admitted traffic")
				}
				current := *runtime.observations["blue"].ControllerLease
				expected := original
				expected.Expiry = current.Expiry
				if current != expected || !current.Expiry.After(original.Expiry) {
					t.Fatal("renewal replaced authority identity or failed to extend expiry")
				}
				if !runtime.observations["blue"].ControllerOwned || runtime.observations["green"].ControllerOwned {
					t.Fatal("single controller lost")
				}
				snapshot, _ := runtime.Observe(context.Background())
				if validObservation(snapshot, now) {
					t.Fatal("partial inventory became deployment readiness proof")
				}
				now = now.Add(time.Second)
				if err := manager.MaintainController(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			runtime.partialInventory = false
			value = runtime.observations["blue"]
			value.Ready = true
			runtime.observations["blue"] = value
			if err := manager.MaintainController(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !runtime.traffic["blue"] || runtime.traffic["green"] {
				t.Fatal("completed inventory did not restore active traffic")
			}
			if strings.Contains(strings.Join(runtime.sequence, ","), "fence") {
				t.Fatal("ordinary lifecycle was fenced")
			}
		})
	}
}

func TestWaitingDeploymentRetainsOnlyExistingActiveLease(t *testing.T) {
	start := controllerrole.ProcessStartToken(os.Getpid())
	if start == "" {
		t.Skip("Linux process-incarnation evidence required")
	}
	for _, phase := range []string{PhaseStandbyValidation, PhaseControllerHandoff, PhaseActiveSwitch} {
		t.Run(phase, func(t *testing.T) {
			now := time.Unix(100, 0).UTC()
			db, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), time.Hour, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			tx := store.Transaction{ID: "waiting", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/candidate.tar", Phase: phase, State: "active", Generation: 2, CreatedAt: now, UpdatedAt: now}
			if err := db.Create(context.Background(), tx, nil); err != nil {
				t.Fatal(err)
			}
			lease := controllerrole.Lease{Slot: "blue", Term: 7, Generation: 1, LeaseID: "live", Nonce: "nonce", Expiry: now.Add(time.Minute), OwnerPID: os.Getpid(), OwnerStartTime: start}
			runtime := &fakeRuntime{active: "blue", observations: map[string]SlotObservation{"blue": {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true, LeaseRenewalSupported: true, ControllerLease: &lease}, "green": {Slot: "green", ProcessUp: true, ProcessReady: true}}}
			manager := NewManager(db, runtime, time.Minute, func() time.Time { return now })
			err = manager.MaintainController(context.Background())
			if phase == PhaseActiveSwitch {
				if err == nil {
					t.Fatal("uncertain switch retained authority")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(runtime.grantedLeases) != 1 || runtime.grantedLeases[0].LeaseID != lease.LeaseID || runtime.observations["green"].ControllerOwned {
				t.Fatalf("waiting transaction changed authority: %+v", runtime.grantedLeases)
			}
		})
	}
}

func TestStagingEffectKeepsExistingControllerLeaseRenewed(t *testing.T) {
	start := controllerrole.ProcessStartToken(os.Getpid())
	if start == "" {
		t.Skip("Linux process-incarnation evidence required")
	}
	now := time.Unix(100, 0).UTC()
	runtime := &delayedControllerRuntime{fakeRuntime: &fakeRuntime{}}
	manager := NewManager(nil, runtime, 30*time.Millisecond, func() time.Time { return now })
	lease := controllerrole.Lease{Slot: "blue", Term: 1, Generation: 1, LeaseID: "staging", Nonce: "nonce", Expiry: now.Add(time.Minute), OwnerPID: os.Getpid(), OwnerStartTime: start}
	target := SlotObservation{Slot: "blue", ControllerOwned: true, LeaseRenewalSupported: true, ControllerLease: &lease}
	err := manager.withControllerRenewal(t.Context(), target, func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(90 * time.Millisecond):
			return nil
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.renewals.Load() < 2 {
		t.Fatalf("slow staging lost heartbeat: renewals=%d", runtime.renewals.Load())
	}
}

func TestVerifyUsesAuthorityInsteadOfChangingEpoch(t *testing.T) {
	for _, differentAuthority := range []bool{false, true} {
		t.Run(fmt.Sprint(differentAuthority), func(t *testing.T) {
			now := time.Unix(100, 0).UTC()
			database, err := store.Open(filepath.Join(t.TempDir(), "deploy.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			runtime := &fakeRuntime{active: "blue", observations: map[string]SlotObservation{"blue": {ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true, StateEpoch: "epoch-before"}, "green": {ProcessUp: true, ProcessReady: true, Ready: true, StateEpoch: "epoch-after"}}}
			if differentAuthority {
				v := runtime.observations["green"]
				v.StateAuthority = "different-backend"
				runtime.observations["green"] = v
			}
			manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
			transaction, err := manager.Stage(context.Background(), "green", "/bundle")
			if err != nil {
				t.Fatal(err)
			}
			_, err = manager.Verify(context.Background(), transaction.ID)
			if (err != nil) != differentAuthority {
				t.Fatalf("verify differentAuthority=%v: %v", differentAuthority, err)
			}
			if !differentAuthority {
				v := runtime.observations["green"]
				v.StateAuthority = "replaced-after-verify"
				runtime.observations["green"] = v
				if _, err := manager.Activate(context.Background(), transaction.ID); err == nil {
					t.Fatal("activation accepted changed authority")
				}
				for _, step := range runtime.sequence {
					if step == "grant" || step == "revoke" {
						t.Fatal("authority mismatch caused role mutation")
					}
				}
			}
		})
	}
}

func TestVerifyRejectsBothSlotsChangingAuthorityAfterStage(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	database, err := store.Open(filepath.Join(t.TempDir(), "deploy.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := &fakeRuntime{active: "blue", observations: map[string]SlotObservation{"blue": {ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true}, "green": {ProcessUp: true, ProcessReady: true, Ready: true}}}
	manager := NewManager(database, runtime, time.Minute, func() time.Time { return now })
	transaction, err := manager.Stage(context.Background(), "green", "/bundle")
	if err != nil {
		t.Fatal(err)
	}
	for slot, value := range runtime.observations {
		value.StateAuthority = "reconfigured-backend"
		runtime.observations[slot] = value
	}
	if _, err := manager.Verify(context.Background(), transaction.ID); err == nil {
		t.Fatal("verify accepted both slots changing backend after stage")
	}
}

type slowActivationRuntime struct {
	*fakeRuntime
	renewals atomic.Int32
}

func (r *slowActivationRuntime) Quiesce(ctx context.Context, slot string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(120 * time.Millisecond):
		return r.fakeRuntime.Quiesce(ctx, slot)
	}
}
func (r *slowActivationRuntime) RenewTransactionLease(ctx context.Context, lease controllerrole.Lease) (controllerrole.Lease, error) {
	r.renewals.Add(1)
	return r.fakeRuntime.RenewTransactionLease(ctx, lease)
}
func TestActivationRenewsCandidateThroughoutOldSlotQuiescence(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	db, err := store.Open(filepath.Join(t.TempDir(), "deploy.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := &slowActivationRuntime{fakeRuntime: &fakeRuntime{active: "blue", observations: map[string]SlotObservation{"blue": {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true}, "green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true}}}}
	m := NewManager(db, r, 30*time.Millisecond, func() time.Time { return now })
	tx, err := m.Stage(context.Background(), "green", "bundle")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Verify(context.Background(), tx.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Activate(context.Background(), tx.ID); err != nil {
		t.Fatal(err)
	}
	if r.renewals.Load() < 3 {
		t.Fatalf("candidate renewal stopped during quiescence: %d", r.renewals.Load())
	}
}

func TestCancelledActivationCompensationPersistsAndFencesWithoutRollback(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	db, err := store.Open(filepath.Join(t.TempDir(), "deploy.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := &fakeRuntime{active: "blue", observations: map[string]SlotObservation{"blue": {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true}, "green": {Slot: "green", ProcessUp: true, ProcessReady: true, Ready: true}}}
	m := NewManager(db, r, time.Second, func() time.Time { return now })
	tx, err := m.Stage(context.Background(), "green", "bundle")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.rollbackOrFence(ctx, *tx, context.Canceled); err == nil {
		t.Fatal("cancelled activation reported success")
	}
	saved, err := db.Get(context.Background(), tx.ID)
	if err != nil || saved.State != "needs_recovery" || saved.RollbackUsed {
		t.Fatalf("cancelled compensation=%v err=%v", saved, err)
	}
	for _, value := range r.observations {
		if value.ControllerOwned {
			t.Fatal("cancelled request left controller active")
		}
	}
}
func TestReadOnlyStatusDoesNotReplacePublishedLatestObservation(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	db, err := store.Open(filepath.Join(t.TempDir(), "deploy.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := &fakeRuntime{active: "blue", observations: map[string]SlotObservation{"blue": {Slot: "blue", ProcessUp: true}, "green": {Slot: "green", ProcessUp: true}}}
	m := NewManager(db, r, time.Second, func() time.Time { return now })
	if _, err := m.observe(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	before, err := db.Observation(context.Background(), store.LatestObservationID)
	if err != nil {
		t.Fatal(err)
	}
	r.active = "green"
	status, err := m.Status(context.Background())
	if err != nil || status.Observed.ActiveSlot != "green" {
		t.Fatalf("status=%v err=%v", status, err)
	}
	after, err := db.Observation(context.Background(), store.LatestObservationID)
	if err != nil || after.Digest != before.Digest {
		t.Fatalf("read-only status replaced published observation: %v", err)
	}
}
