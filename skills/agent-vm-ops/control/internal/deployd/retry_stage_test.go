package deployd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

type acceptanceRecoveryRuntime struct {
	*acceptanceRuntime
	points       map[string]recoveryPoint
	createCalls  int
	checkCalls   int
	prepareCalls int
}

func newAcceptanceRecoveryRuntime() *acceptanceRecoveryRuntime {
	return &acceptanceRecoveryRuntime{
		acceptanceRuntime: newAcceptanceRuntime(),
		points:            map[string]recoveryPoint{},
	}
}

func (runtime *acceptanceRecoveryRuntime) RecoveryAuthority() string { return "authority-1" }

func (runtime *acceptanceRecoveryRuntime) CreateRecoveryPoint(_ context.Context, operation, purpose, authority string, _ map[string]bool, _ time.Time) error {
	runtime.createCalls++
	if authority != runtime.RecoveryAuthority() {
		return errors.New("recovery authority mismatch")
	}
	key := operation + ":" + purpose
	if _, exists := runtime.points[key]; !exists {
		runtime.points[key] = recoveryPoint{ID: "point-original", Operation: operation, Purpose: purpose, Authority: authority, SHA256: "digest", Boundary: "epoch:point-original"}
	}
	return nil
}

func (runtime *acceptanceRecoveryRuntime) ValidateRecoveryPoint(_ context.Context, operation, purpose, authority string) (recoveryPoint, error) {
	runtime.checkCalls++
	if authority != runtime.RecoveryAuthority() {
		return recoveryPoint{}, errors.New("recovery authority mismatch")
	}
	point, ok := runtime.points[operation+":"+purpose]
	if !ok {
		return recoveryPoint{}, errors.New("original recovery point is missing")
	}
	return point, nil
}

func (runtime *acceptanceRecoveryRuntime) Preflight(context.Context, string, string) error {
	return nil
}

func (runtime *acceptanceRecoveryRuntime) PrepareRecoveryController(ctx context.Context, slot, bundle string) error {
	runtime.prepareCalls++
	return runtime.Verify(ctx, slot, bundle)
}

func TestRecoverRetriesStageOnlyOnOriginalTransaction(t *testing.T) {
	ctx := context.Background()
	database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runtime := newAcceptanceRecoveryRuntime()
	runtime.stageUncertain = true
	runtime.capabilitiesBySlot = map[string][]string{
		"blue":  {"reads@1", "writes@1", "vm-hold@1"},
		"green": {"reads@1", "writes@1"},
	}
	manager := NewManager(database, runtime, time.Minute, time.Now)
	failed, err := manager.Stage(ctx, "green", "/verified/green-v1.10.0.tar")
	if err == nil || failed != nil {
		t.Fatalf("initial stage = %#v, %v; want uncertain failure", failed, err)
	}
	transactions, err := database.ListActive(ctx)
	if err != nil || len(transactions) != 1 || transactions[0].State != "needs_recovery" || transactions[0].Phase != PhaseStage {
		t.Fatalf("stage recovery transaction = %#v, %v", transactions, err)
	}
	originalBundle := transactions[0].Bundle
	originalTarget := transactions[0].TargetSlot
	originalID := transactions[0].ID

	// Model the observed post-failure state: the selected incumbent remains
	// queryable but fenced, and the failed inactive candidate is stopped.
	runtime.mu.Lock()
	previous := runtime.slots["blue"]
	previous.Ready = false
	previous.ControllerOwned = false
	runtime.slots["blue"] = previous
	candidate := runtime.slots["green"]
	candidate.ProcessUp = false
	candidate.ProcessReady = false
	candidate.Ready = false
	candidate.ControllerOwned = false
	runtime.slots["green"] = candidate
	runtime.traffic["blue"] = false
	runtime.traffic["green"] = false
	runtime.draining["blue"] = false
	runtime.stageUncertain = false
	runtime.mu.Unlock()

	server := NewServer(manager, NewMetrics(manager))
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	post := func(path string, body map[string]string) map[string]any {
		t.Helper()
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		response, err := http.Post(httpServer.URL+path, "application/json", strings.NewReader(string(encoded)))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var envelope map[string]any
		if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("POST %s status=%d response=%#v", path, response.StatusCode, envelope)
		}
		value, ok := envelope["value"].(map[string]any)
		if !ok {
			t.Fatalf("POST %s response value = %#v", path, envelope["value"])
		}
		return value
	}
	stageCallsBefore, checksBefore := runtime.stageCalls, runtime.checkCalls
	replacementRequest, err := json.Marshal(map[string]any{
		"transaction_id": originalID, "action": "retry-stage",
		"target_slot": "blue", "bundle": "/unverified/replacement.tar",
	})
	if err != nil {
		t.Fatal(err)
	}
	rejectedResponse, err := http.Post(httpServer.URL+"/v1/recover", "application/json", strings.NewReader(string(replacementRequest)))
	if err != nil {
		t.Fatal(err)
	}
	var rejected map[string]any
	if err := json.NewDecoder(rejectedResponse.Body).Decode(&rejected); err != nil {
		_ = rejectedResponse.Body.Close()
		t.Fatal(err)
	}
	_ = rejectedResponse.Body.Close()
	if rejectedResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("recovery accepted caller-supplied replacement identity: status=%d response=%#v", rejectedResponse.StatusCode, rejected)
	}
	persisted, err := database.Get(ctx, originalID)
	if err != nil || persisted == nil || persisted.Bundle != originalBundle || persisted.TargetSlot != originalTarget || persisted.State != "needs_recovery" || persisted.Phase != PhaseStage {
		t.Fatalf("rejected replacement changed original transaction: %#v, %v", persisted, err)
	}
	if runtime.stageCalls != stageCallsBefore || runtime.checkCalls != checksBefore {
		t.Fatalf("rejected replacement reached recovery effects: stages=%d/%d checks=%d/%d", runtime.stageCalls, stageCallsBefore, runtime.checkCalls, checksBefore)
	}

	prepared := post("/v1/recover", map[string]string{"transaction_id": originalID, "action": "retry-stage"})
	if prepared["transaction_id"] != originalID || prepared["phase"] != PhaseControllerHandoff || prepared["state"] != "active" {
		t.Fatalf("recovered stage transaction = %#v", prepared)
	}
	persisted, err = database.Get(ctx, originalID)
	if err != nil || persisted == nil || persisted.Bundle != originalBundle || persisted.TargetSlot != originalTarget {
		t.Fatalf("forward recovery changed original bundle identity: %#v, %v", persisted, err)
	}
	if runtime.createCalls != 1 || runtime.checkCalls != 1 || len(runtime.points) != 1 || runtime.stageCalls != 2 || runtime.prepareCalls != 1 {
		t.Fatalf("recovery points created=%d checked=%d registered=%d stages=%d process rebinds=%d; want original point and candidate identity reused", runtime.createCalls, runtime.checkCalls, len(runtime.points), runtime.stageCalls, runtime.prepareCalls)
	}
	runtime.mu.Lock()
	if runtime.active != "blue" || runtime.slots["green"].ControllerOwned || runtime.slots["green"].TrafficAccepting || runtime.traffic["green"] {
		t.Fatalf("stage recovery changed active or granted roles: active=%s slots=%#v traffic=%#v", runtime.active, runtime.slots, runtime.traffic)
	}
	runtime.mu.Unlock()

	activated := post("/v1/activate", map[string]string{"transaction_id": originalID})
	if activated["transaction_id"] != originalID || activated["phase"] != PhaseFinalize || activated["state"] != "finalized" {
		t.Fatalf("activation did not finish the original transaction: %#v", activated)
	}
	if runtime.createCalls != 1 || len(runtime.points) != 1 {
		t.Fatalf("activation created another recovery point: created=%d registered=%d", runtime.createCalls, len(runtime.points))
	}
	if active, err := database.ListActive(ctx); err != nil || len(active) != 0 {
		t.Fatalf("active deployment transactions = %#v, %v", active, err)
	}
	runtime.mu.Lock()
	if runtime.active != "green" || !runtime.slots["green"].ControllerOwned || !runtime.traffic["green"] || runtime.slots["blue"].ControllerOwned || runtime.traffic["blue"] {
		t.Fatalf("forward recovery controller handoff = active %s slots=%#v traffic=%#v", runtime.active, runtime.slots, runtime.traffic)
	}
	runtime.mu.Unlock()
}
