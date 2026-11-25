package deployd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

func fencedForwardTransaction(t *testing.T, database *store.Store) store.Transaction {
	t.Helper()
	transaction := store.Transaction{
		ID: "tx-forward", TargetSlot: "blue", PreviousSlot: "green",
		Bundle: "/var/lib/agent-vm-native/release-staging/.update-1/release.tar",
		Phase:  PhaseRollback, State: "needs_recovery", RecoveryRequired: true, Generation: 1,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	evidence := map[string]any{
		"active_slot":                 "green",
		"previous_release_id":         "release-green",
		"previous_source_revision":    "revision-1",
		"previous_state_epoch":        "epoch-1",
		"previous_state_authority":    "authority-1",
		"previous_state_capabilities": []string{"reads@1"},
	}
	if err := database.Create(context.Background(), transaction, evidence); err != nil {
		t.Fatal(err)
	}
	stored, err := database.Get(context.Background(), transaction.ID)
	if err != nil || stored == nil {
		t.Fatalf("stored transaction = %#v, %v", stored, err)
	}
	return *stored
}

func fencedForwardRuntime() *acceptanceRecoveryRuntime {
	runtime := newAcceptanceRecoveryRuntime()
	runtime.active = "green"
	runtime.capabilitiesBySlot = map[string][]string{"blue": {"reads@1"}, "green": {"reads@1"}}
	for _, slot := range []string{"blue", "green"} {
		value := runtime.slots[slot]
		value.ProcessUp = false
		value.ProcessReady = false
		value.Ready = false
		value.ControllerOwned = false
		runtime.slots[slot] = value
		runtime.traffic[slot] = false
		runtime.draining[slot] = false
	}
	return runtime
}

func TestCompleteForwardFinishesFencedTransaction(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	transaction := fencedForwardTransaction(t, database)
	runtime := fencedForwardRuntime()
	manager := NewManager(database, runtime, time.Minute, time.Now)

	if _, err := manager.CompleteForward(ctx, transaction.ID, "release.tar"); err == nil {
		t.Fatal("complete-forward accepted a non-absolute bundle")
	}
	finished, err := manager.CompleteForward(ctx, transaction.ID, "/verified/blue-1.10.0.tar")
	if err != nil {
		t.Fatalf("complete-forward: %v", err)
	}
	if finished.Phase != PhaseFinalize || finished.State != "finalized" || finished.RecoveryRequired {
		t.Fatalf("forward recovery transaction = %#v", finished)
	}
	if active, err := database.ListActive(ctx); err != nil || len(active) != 0 {
		t.Fatalf("active transactions = %#v, %v", active, err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	target := runtime.slots["blue"]
	if runtime.active != "blue" || !target.ProcessUp || !target.Ready || !target.ControllerOwned || !runtime.traffic["blue"] {
		t.Fatalf("forward recovery roles = active=%s slots=%#v traffic=%#v", runtime.active, runtime.slots, runtime.traffic)
	}
	if runtime.slots["green"].ControllerOwned || runtime.traffic["green"] {
		t.Fatalf("previous slot retained a role: slots=%#v traffic=%#v", runtime.slots, runtime.traffic)
	}
}

func TestCompleteForwardRequiresStoppedPreviousSlot(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	transaction := fencedForwardTransaction(t, database)
	runtime := fencedForwardRuntime()
	runtime.mu.Lock()
	running := runtime.slots["green"]
	running.ProcessUp = true
	running.ProcessReady = true
	runtime.slots["green"] = running
	runtime.mu.Unlock()
	manager := NewManager(database, runtime, time.Minute, time.Now)

	if _, err := manager.CompleteForward(ctx, transaction.ID, "/verified/blue-1.10.0.tar"); err == nil {
		t.Fatal("complete-forward accepted a running previous slot")
	}
	unchanged, err := database.Get(ctx, transaction.ID)
	if err != nil || unchanged == nil || unchanged.State != "needs_recovery" || unchanged.Phase != PhaseRollback {
		t.Fatalf("refused forward recovery changed the transaction = %#v, %v", unchanged, err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.active != "green" || runtime.stageCalls != 0 || runtime.slots["blue"].ProcessUp {
		t.Fatalf("refused forward recovery had effects: active=%s stages=%d slots=%#v", runtime.active, runtime.stageCalls, runtime.slots)
	}
}

func TestCompleteForwardAcceptsUndeclaredStateCapabilities(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	transaction := fencedForwardTransaction(t, database)
	runtime := fencedForwardRuntime()
	runtime.mu.Lock()
	runtime.capabilitiesBySlot["blue"] = []string{"other@1"}
	runtime.mu.Unlock()
	manager := NewManager(database, runtime, time.Minute, time.Now)

	finished, err := manager.CompleteForward(ctx, transaction.ID, "/verified/blue-1.10.0.tar")
	if err != nil {
		t.Fatalf("state markers must not gate forward recovery: %v", err)
	}
	if finished.State != "finalized" {
		t.Fatalf("forward recovery transaction = %#v", finished)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.active != "blue" || !runtime.traffic["blue"] {
		t.Fatalf("forward recovery did not switch the active slot: active=%s traffic=%#v", runtime.active, runtime.traffic)
	}
}

func TestRecoverEndpointRequiresBundleOnlyForCompleteForward(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	transaction := fencedForwardTransaction(t, database)
	manager := NewManager(database, fencedForwardRuntime(), time.Minute, time.Now)
	httpServer := httptest.NewServer(NewServer(manager, NewMetrics(manager)).Handler())
	defer httpServer.Close()

	for name, body := range map[string]map[string]string{
		"missing bundle": {"transaction_id": transaction.ID, "action": "complete-forward"},
		"foreign bundle": {"transaction_id": transaction.ID, "action": "retry-stage", "bundle": "/verified/blue.tar"},
	} {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.Post(httpServer.URL+"/v1/recover", "application/json", strings.NewReader(string(encoded)))
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: status=%d, want 400", name, response.StatusCode)
		}
	}
	unchanged, err := database.Get(ctx, transaction.ID)
	if err != nil || unchanged == nil || unchanged.State != "needs_recovery" {
		t.Fatalf("invalid recovery requests changed the transaction = %#v, %v", unchanged, err)
	}
}

func forwardSnapshot(now time.Time) RuntimeSnapshot {
	sources := map[string]SourceObservation{
		"active_link":    {Up: true, ObservedAt: now, FreshUntil: now.Add(30 * time.Second)},
		"systemd":        {Up: true, ObservedAt: now, FreshUntil: now.Add(30 * time.Second)},
		"slot_contracts": {Up: false, ObservedAt: now, FreshUntil: now.Add(30 * time.Second), Error: "one or more slot health contracts could not be observed"},
	}
	// A stopped active slot leaves the composed window and the slot health
	// contract stale, exactly like the live host this recovery targets.
	return RuntimeSnapshot{
		ObservedAt: now, FreshUntil: now,
		ActiveSlot: "green", ControllerSlot: "none", Consistent: false,
		Slots:   map[string]SlotObservation{"blue": {Slot: "blue", ObservedUnix: now.Unix()}, "green": {Slot: "green", ObservedUnix: now.Unix()}},
		Sources: sources,
	}
}

func TestForwardRecoveryObservationAcceptsStoppedActiveSlot(t *testing.T) {
	now := time.Now().UTC()
	snapshot := forwardSnapshot(now)
	transaction := store.Transaction{TargetSlot: "blue", PreviousSlot: "green"}
	if err := forwardRecoveryObservation(snapshot, transaction, now); err != nil {
		t.Fatalf("stopped active slot was rejected: %v", err)
	}
	running := forwardSnapshot(now)
	previous := running.Slots["green"]
	previous.ProcessUp = true
	previous.ProcessReady = true
	running.Slots["green"] = previous
	if err := forwardRecoveryObservation(running, transaction, now); err == nil {
		t.Fatal("running previous slot was accepted")
	}
	controlled := forwardSnapshot(now)
	controlled.ControllerSlot = "blue"
	if err := forwardRecoveryObservation(controlled, transaction, now); err == nil {
		t.Fatal("controller role was accepted")
	}
	stale := forwardSnapshot(now)
	staleSource := stale.Sources["systemd"]
	staleSource.FreshUntil = now.Add(-time.Second)
	stale.Sources["systemd"] = staleSource
	if err := forwardRecoveryObservation(stale, transaction, now); err == nil {
		t.Fatal("stale systemd evidence was accepted")
	}
}

func TestCompleteForwardAcceptsTrafficEnablePhase(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	transaction := fencedForwardTransaction(t, database)
	if _, err := database.Transition(ctx, transaction.ID, PhaseRollback, PhaseTrafficEnable, "needs_recovery", "candidate_traffic_unobserved", "deployd", nil, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	runtime := fencedForwardRuntime()
	manager := NewManager(database, runtime, time.Minute, time.Now)
	finished, err := manager.CompleteForward(ctx, transaction.ID, "/verified/blue-1.12.0.tar")
	if err != nil {
		t.Fatalf("complete-forward at traffic_enable: %v", err)
	}
	if finished.Phase != PhaseFinalize || finished.State != "finalized" {
		t.Fatalf("forward recovery transaction = %#v", finished)
	}
}
