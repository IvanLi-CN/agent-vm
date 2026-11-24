package deployd

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

func TestRetryStageRecoveryRejectsActiveNativeWork(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*acceptanceRecoveryRuntime, *SlotObservation)
	}{
		{name: "in-flight request", setup: func(_ *acceptanceRecoveryRuntime, previous *SlotObservation) { previous.InFlightRequests = 1 }},
		{name: "native worker", setup: func(_ *acceptanceRecoveryRuntime, previous *SlotObservation) { previous.NativeWorkers = 1 }},
		{name: "pending operation", setup: func(_ *acceptanceRecoveryRuntime, previous *SlotObservation) { previous.PendingOperations = 1 }},
		{name: "unknown counters", setup: func(runtime *acceptanceRecoveryRuntime, _ *SlotObservation) { runtime.unknownCounters = true }},
		{name: "duplicate controller", setup: func(runtime *acceptanceRecoveryRuntime, _ *SlotObservation) {
			blue := runtime.slots["blue"]
			blue.ControllerOwned = true
			runtime.slots["blue"] = blue
			green := runtime.slots["green"]
			green.ControllerOwned = true
			runtime.slots["green"] = green
		}},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			database, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			runtime := newAcceptanceRecoveryRuntime()
			runtime.stageUncertain = true
			manager := NewManager(database, runtime, time.Minute, time.Now)
			if _, err := manager.Stage(ctx, "green", "/verified/green-v1.10.0.tar"); err == nil {
				t.Fatal("initial uncertain stage unexpectedly succeeded")
			}
			transactions, err := database.ListActive(ctx)
			if err != nil || len(transactions) != 1 || transactions[0].State != "needs_recovery" {
				t.Fatalf("stage recovery transaction = %#v, %v", transactions, err)
			}
			transactionID := transactions[0].ID
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
			testCase.setup(runtime, &previous)
			runtime.slots["blue"] = previous
			if testCase.name == "duplicate controller" {
				candidate = runtime.slots["green"]
				candidate.ControllerOwned = true
				runtime.slots["green"] = candidate
			}
			runtime.traffic["blue"] = false
			runtime.traffic["green"] = false
			runtime.draining["blue"] = false
			runtime.stageUncertain = false
			callsBefore := runtime.stageCalls
			blueControllerBefore := runtime.slots["blue"].ControllerOwned
			greenControllerBefore := runtime.slots["green"].ControllerOwned
			blueTrafficBefore := runtime.traffic["blue"]
			greenTrafficBefore := runtime.traffic["green"]
			runtime.mu.Unlock()

			if _, err := manager.Recover(ctx, transactionID, "retry-stage"); err == nil {
				t.Fatal("stage recovery accepted an invalid pre-recovery observation")
			}
			current, err := database.Get(ctx, transactionID)
			if err != nil || current == nil || current.State != "needs_recovery" || current.Phase != PhaseStage {
				t.Fatalf("unsafe retry changed transaction = %#v, %v", current, err)
			}
			prepared, err := database.LatestEvent(ctx, transactionID, PhaseStage, "retry_stage_prepared")
			if err != nil || prepared != nil {
				t.Fatalf("unsafe retry persisted a preparation event = %#v, %v", prepared, err)
			}
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			if runtime.stageCalls != callsBefore || runtime.slots["blue"].ControllerOwned != blueControllerBefore || runtime.slots["green"].ControllerOwned != greenControllerBefore || runtime.traffic["blue"] != blueTrafficBefore || runtime.traffic["green"] != greenTrafficBefore {
				t.Fatalf("unsafe retry had effects: stages=%d/%d slots=%#v traffic=%#v", runtime.stageCalls, callsBefore, runtime.slots, runtime.traffic)
			}
		})
	}
}
