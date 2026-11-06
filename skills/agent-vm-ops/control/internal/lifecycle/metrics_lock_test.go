package lifecycle

import (
	"context"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
	"path/filepath"
	"testing"
	"time"
)

func TestMetricsRemainsAvailableDuringNativeMutation(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, config.Defaults(), Options{})
	// Acquire keeps the release drain boundary locked throughout native startup.
	unlock, err := service.lockActionWithContext(context.Background(), "acquire", protocol.Request{"session_id": "starting"})
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	response, err := service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "admin-metrics"})
	if err != nil {
		t.Fatalf("read-only lease observation blocked by native startup: %v", err)
	}
	if response["ok"] != true {
		t.Fatalf("metrics response = %#v", response)
	}
}

func TestReleaseSealRequiresUnlockedCurrentDrain(t *testing.T) {
	for _, scenario := range []string{"busy", "changed-epoch", "owner-exited", "quiescent"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			service := New(store, config.Defaults(), Options{})
			req := protocol.Request{"session_id": "release-owner"}
			if _, err := service.drain(ctx, true, req); err != nil {
				t.Fatal(err)
			}
			epoch, err := store.StateEpoch(ctx)
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "busy":
				unlock := service.lockNamedAction("drain")
				defer unlock()
			case "changed-epoch":
				if err := store.SetMeta(ctx, "concurrent-change", "1"); err != nil {
					t.Fatal(err)
				}
			case "owner-exited":
				if _, err := service.drain(ctx, false, req); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.trySealReleaseDrain(ctx, epoch); err != nil {
				t.Fatal(err)
			}
			value, _, err := store.Meta(ctx, "drain_sealed")
			if err != nil {
				t.Fatal(err)
			}
			if (value == "1") != (scenario == "quiescent") {
				t.Fatalf("seal=%q scenario=%s", value, scenario)
			}
		})
	}
}
