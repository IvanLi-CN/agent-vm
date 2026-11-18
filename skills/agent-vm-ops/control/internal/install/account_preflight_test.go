package install

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd"
)

func TestAccountMigrationRequiresUncontestedActiveSlot(t *testing.T) {
	now := time.Now()
	active := deployd.SlotObservation{ProcessUp: true, Ready: true, ControllerOwned: true, TrafficAccepting: true, StateAuthority: "rqlite", SourceRevision: "prior", ReleaseID: "prior"}
	for _, test := range []struct {
		name               string
		other              deployd.SlotObservation
		activeTransactions []string
		until              time.Time
		want               bool
	}{
		{name: "ready", until: now.Add(time.Minute), want: true},
		{name: "dual controller", other: deployd.SlotObservation{ControllerOwned: true}, until: now.Add(time.Minute)},
		{name: "transaction pending", activeTransactions: []string{"update-1"}, until: now.Add(time.Minute)},
		{name: "expired", until: now},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := deployd.RuntimeSnapshot{ActiveSlot: "blue", ControllerSlot: "blue", Consistent: true, FreshUntil: test.until,
				Slots: map[string]deployd.SlotObservation{"blue": active, "green": test.other}}
			data, err := json.Marshal(map[string]any{"ok": true, "value": map[string]any{"observation": snapshot, "transactions": test.activeTransactions}})
			if err != nil {
				t.Fatal(err)
			}
			_, prior, err := accountMigrationActive(data, now)
			if (err == nil) != test.want || (test.want && prior.SourceRevision != "prior") {
				t.Fatalf("prior=%+v err=%v", prior, err)
			}
		})
	}
}

func TestAccountMigrationRejectsAuthorityOrProjectChange(t *testing.T) {
	prior := config.Config{Slot: "blue", StateDB: "https://first", Project: "original"}
	next := prior
	if !sameAccountMigrationEnvironment("blue", prior, next) {
		t.Fatal("unchanged slot rejected")
	}
	next.StateDB = "https://second"
	if sameAccountMigrationEnvironment("blue", prior, next) {
		t.Fatal("database replacement permitted")
	}
	next = prior
	next.Project = "other"
	if sameAccountMigrationEnvironment("blue", prior, next) {
		t.Fatal("project change permitted")
	}
}
