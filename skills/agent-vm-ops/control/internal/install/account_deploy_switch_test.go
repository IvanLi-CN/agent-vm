package install

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
)

func TestDedicatedHandoffRequiresNewOwnerOnSameRouteAndAuthority(t *testing.T) {
	now := time.Now()
	prior := controllerrole.Lease{Slot: "green", Term: 10, Generation: 3, OwnerPID: 100, OwnerStartTime: "1000"}
	expect := deployd.AccountHandoff{Lease: prior, StateAuthority: "database"}
	for _, tc := range []struct {
		name      string
		dedicated bool
		route     string
		authority string
		pid       int
		term      uint64
		want      bool
	}{
		{"new owner", true, "green", "database", 101, 11, true},
		{"root daemon", false, "green", "database", 101, 11, false},
		{"old owner", true, "green", "database", 100, 11, false},
		{"old term", true, "green", "database", 101, 10, false},
		{"wrong route", true, "blue", "database", 101, 11, false},
		{"wrong database", true, "green", "other", 101, 11, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := controllerrole.Lease{Slot: "green", Term: tc.term, Generation: 3, LeaseID: "next", Nonce: "next", OwnerPID: tc.pid,
				OwnerStartTime: "1010", Expiry: now.Add(time.Minute)}
			snapshot := deployd.RuntimeSnapshot{ActiveSlot: tc.route, ControllerSlot: "green", Consistent: true, FreshUntil: now.Add(time.Minute),
				Slots: map[string]deployd.SlotObservation{"green": {ControllerLease: &lease, ProcessUp: true, Ready: true, TrafficAccepting: true,
					ControllerOwned: true, StateAuthority: tc.authority}}}
			payload, err := json.Marshal(map[string]any{"ok": true, "value": map[string]any{"dedicated_service_account": tc.dedicated, "observation": snapshot}})
			if err != nil {
				t.Fatal(err)
			}
			if got := dedicatedHandoffConfirmed(payload, expect, now); got != tc.want {
				t.Fatalf("confirmed=%v, want %v", got, tc.want)
			}
		})
	}
}
