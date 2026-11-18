package install

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
)

func TestPreparedAccountCandidateKeepsIncumbentServing(t *testing.T) {
	now := time.Now().UTC()
	report := Report{
		ActiveSlot: "blue", PreviousSource: "previous", SourceRevision: "candidate",
		previousAuthority: "rqlite", previousReleaseID: "previous-release",
	}
	tests := []struct {
		name   string
		mutate func(*deployd.RuntimeSnapshot, map[string]any)
	}{
		{name: "serving incumbent and verified standby"},
		{name: "wrong transaction", mutate: func(_ *deployd.RuntimeSnapshot, tx map[string]any) {
			tx["transaction_id"] = "another"
		}},
		{name: "wrong bundle", mutate: func(_ *deployd.RuntimeSnapshot, tx map[string]any) {
			tx["staging_id"] = ".update-account-another"
		}},
		{name: "recovery required", mutate: func(_ *deployd.RuntimeSnapshot, tx map[string]any) {
			tx["recovery_required"] = true
		}},
		{name: "incumbent stopped serving", mutate: func(snapshot *deployd.RuntimeSnapshot, _ map[string]any) {
			slot := snapshot.Slots["blue"]
			slot.TrafficAccepting = false
			snapshot.Slots["blue"] = slot
		}},
		{name: "candidate claims controller", mutate: func(snapshot *deployd.RuntimeSnapshot, _ map[string]any) {
			slot := snapshot.Slots["green"]
			slot.ControllerOwned = true
			snapshot.Slots["green"] = slot
		}},
		{name: "candidate uses another database", mutate: func(snapshot *deployd.RuntimeSnapshot, _ map[string]any) {
			slot := snapshot.Slots["green"]
			slot.StateAuthority = "another"
			snapshot.Slots["green"] = slot
		}},
		{name: "observation expired", mutate: func(snapshot *deployd.RuntimeSnapshot, _ map[string]any) {
			snapshot.FreshUntil = now
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := deployd.RuntimeSnapshot{
				Consistent: true, ActiveSlot: "blue", ControllerSlot: "blue", FreshUntil: now.Add(time.Second),
				Slots: map[string]deployd.SlotObservation{
					"blue": {ProcessUp: true, Ready: true, ControllerOwned: true, TrafficAccepting: true,
						ReleaseID: report.previousReleaseID, SourceRevision: report.PreviousSource, StateAuthority: report.previousAuthority},
					"green": {ProcessUp: true, ProcessReady: true, SourceRevision: report.SourceRevision, StateAuthority: report.previousAuthority},
				},
			}
			tx := map[string]any{"transaction_id": "original", "staging_id": ".update-account-expected",
				"previous_slot": "blue", "target_slot": "green", "state": "active", "phase": deployd.PhaseControllerHandoff}
			if tt.mutate != nil {
				tt.mutate(&snapshot, tx)
			}
			payload, err := json.Marshal(map[string]any{"ok": true, "value": map[string]any{
				"observation": snapshot, "transactions": []map[string]any{tx},
			}})
			if err != nil {
				t.Fatal(err)
			}
			err = checkPreparedAccountCandidate(payload, "original", ".update-account-expected", report, "green", now)
			if (err != nil) != (tt.mutate != nil) {
				t.Fatalf("unexpected verification result: %v", err)
			}
		})
	}
}

func TestAccountHandoffRequiresFinalizedActivationAndMatchingLiveLease(t *testing.T) {
	now := time.Now().UTC()
	report := Report{ActiveSlot: "blue", SourceRevision: "candidate", previousAuthority: "rqlite"}
	const id, staging = "tx-original", ".update-account-expected"
	for _, scenario := range []string{"verified", "unfinished", "wrong transaction", "compensated", "recovery pending", "stale lease generation", "previous still running", "wrong backend"} {
		t.Run(scenario, func(t *testing.T) {
			lease := &controllerrole.Lease{Slot: "green", Term: 2, LeaseID: "lease", Nonce: "nonce", Generation: 10,
				Expiry: now.Add(time.Minute), OwnerPID: 42, OwnerStartTime: "start"}
			tx := accountMigrationTransaction{
				ID: id, StagingID: staging, Previous: "blue", Target: "green", State: "finalized", Phase: deployd.PhaseFinalize, Generation: 10,
			}
			active := deployd.SlotObservation{ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true,
				TrafficAccepting: true, SourceRevision: report.SourceRevision, StateAuthority: report.previousAuthority, ControllerLease: lease}
			old := deployd.SlotObservation{CountersKnown: true}
			switch scenario {
			case "unfinished":
				tx.State, tx.Phase = "active", deployd.PhaseControllerHandoff
			case "wrong transaction":
				tx.ID = "another"
			case "compensated":
				tx.RollbackUsed = true
			case "recovery pending":
				tx.RecoveryRequired = true
			case "stale lease generation":
				lease.Generation--
			case "previous still running":
				old.ProcessUp = true
			case "wrong backend":
				active.StateAuthority = "another"
			}
			journal, err := json.Marshal(map[string]any{"ok": true, "value": map[string]any{"transaction": tx}})
			if err != nil {
				t.Fatal(err)
			}
			var status accountMigrationStatus
			status.OK = true
			status.Value.Observation = deployd.RuntimeSnapshot{Consistent: true, ActiveSlot: "green", ControllerSlot: "green",
				FreshUntil: now.Add(time.Second), Slots: map[string]deployd.SlotObservation{"green": active, "blue": old}}
			payload, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			handoff, err := accountHandoffAfterActivation(journal, payload, id, staging, "green", report, now)
			if scenario == "verified" {
				if err != nil || handoff.Lease != *lease || handoff.StateAuthority != report.previousAuthority {
					t.Fatalf("handoff=%+v, err=%v", handoff, err)
				}
			} else if err == nil {
				t.Fatal("unverified activation produced handoff credentials")
			}
		})
	}
}
