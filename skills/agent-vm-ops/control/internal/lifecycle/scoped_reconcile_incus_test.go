package lifecycle

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
)

// The opt-in fixture must live in the validation guest's own nested Incus.
// Its three containers carry the same ownership labels as newScopedFixture.
func TestScopedReconcileIsolatedIncus(t *testing.T) {
	project := os.Getenv("AGENT_VM_SCOPED_INCUS_PROJECT")
	if project == "" {
		t.Skip("requires an isolated nested Incus fixture")
	}
	if !strings.HasPrefix(project, "scoped-reconcile-test-") {
		t.Fatal("refusing a non-test project")
	}
	f := newScopedFixture(t)
	ctx := context.Background()
	guest := native.NewIncusClient(native.NewRunner(), project, native.ClientOptions{})
	f.service.Guest = guest
	f.service.Config.Project = project
	f.service.Config.HostAddress = "10.252.1.1"
	before := map[string]map[string]any{}
	for _, name := range []string{"target", "expired-peer", "active-peer"} {
		config, err := guest.Config(ctx, "instance-"+name)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = config
		status, err := guest.Status(ctx, "instance-"+name)
		value, parseErr := native.ParseStatus(status)
		if err != nil || parseErr != nil || value != "Running" {
			t.Fatalf("fixture must start Running: %s %v %v", name, status, err)
		}
	}
	for index, name := range []string{"target", "expired-peer", "active-peer"} {
		address := "10.252.1." + []string{"100", "101", "102"}[index]
		if err := f.store.UpdateTask(ctx, name, map[string]any{"vm_ip": address}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := f.snapshot(t)
	if response, err := f.service.Dispatch(ctx, f.request); err != nil {
		t.Fatal(err)
	} else {
		t.Logf("preview: %s", mustJSON(response))
	}
	if f.snapshot(t) != snapshot {
		t.Fatal("real Incus preview changed durable state")
	}
	if os.Getenv("AGENT_VM_SCOPED_INCUS_MODE") == "maintenance" {
		session := "blocked-history"
		generation := int64(1)
		if err := f.store.RecordOperation(ctx, "blocked-history", "release", &session, &generation, map[string]any{"outcome": "pending", "worker": map[string]any{"state": "exited", "start_attempted": false}}); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 8; i++ {
			if err := f.service.RunMaintenance(ctx, maintenanceReconcileLane); err != nil {
				t.Logf("maintenance round %d: %v", i, err)
			}
			f.now = f.now.Add(65 * time.Second)
		}
		for _, name := range []string{"target", "expired-peer"} {
			task, err := f.store.GetTask(ctx, map[string]any{"session_id": name})
			if err != nil || task.State != stateCached {
				pending, _ := f.store.PendingOperations(ctx)
				t.Logf("pending: %s", mustJSON(pending))
				t.Fatalf("real maintenance failed %s: %#v %v", name, task, err)
			}
			status, err := guest.Status(ctx, "instance-"+name)
			value, parseErr := native.ParseStatus(status)
			if err != nil || parseErr != nil || value != "Stopped" {
				t.Fatalf("real native expiry failed %s: %v %v", name, status, err)
			}
			config, err := guest.Config(ctx, "instance-"+name)
			if err != nil {
				t.Fatal(err)
			}
			if _, exists := config["expanded_devices"].(map[string]any)["proxy-ssh-tcp"]; exists {
				t.Fatal("expired proxy remains")
			}
			op, err := f.store.GetOperation(ctx, "pending-"+name)
			if err != nil || op.Response["outcome"] == "pending" {
				t.Fatalf("expired acquire not finalized %s: %v", name, err)
			}
		}
		after, err := guest.Config(ctx, "instance-active-peer")
		if err != nil || !reflect.DeepEqual(before["active-peer"], after) {
			t.Fatal("valid lease config changed", err)
		}
		status, err := guest.Status(ctx, "instance-active-peer")
		value, parseErr := native.ParseStatus(status)
		if err != nil || parseErr != nil || value != "Running" {
			t.Fatal("valid lease stopped", err)
		}
		op, _ := f.store.GetOperation(ctx, "blocked-history")
		if op.Response["outcome"] != "pending" {
			t.Fatal("unknown history was discarded")
		}
		// Recreate the journal-write crash window after the real native cleanup,
		// retaining its actual completed process identity for the read-only proof.
		timer, err := f.store.GetOperation(ctx, timerExpireOperationID("target", 2))
		if err != nil || timer == nil {
			t.Fatal("missing native expiry journal", err)
		}
		worker, ok := timer.Response["worker"].(map[string]any)
		if !ok {
			t.Fatal("missing real worker evidence")
		}
		request := protocol.Request{"protocol": 4, "action": "release", "session_id": "target", "generation": int64(2), "operation_id": "cached-release-proof"}
		digest, _ := protocol.Digest(request)
		session = "target"
		generation = int64(2)
		if err := f.store.RecordOperation(ctx, "cached-release-proof", "release", &session, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest, "worker": worker}); err != nil {
			t.Fatal(err)
		}
		if err := f.service.reconcileCachedRelease(ctx, "cached-release-proof"); err != nil {
			t.Fatal("real cached release proof", err)
		}
		recovered, _ := f.store.GetOperation(ctx, "cached-release-proof")
		if recovered.Response["outcome"] != "success" || !reflect.DeepEqual(worker, recovered.Response["worker"]) {
			t.Fatal("real cached release did not preserve evidence")
		}
		if f.service.maintenanceAdmission(ctx) == nil {
			t.Fatal("unknown history reported healthy")
		}
		return
	}
	if _, err := f.service.Dispatch(ctx, f.apply()); err != nil {
		t.Fatal(err)
	}
	status, err := guest.Status(ctx, "instance-target")
	if err != nil {
		t.Fatal(err)
	}
	if value, err := native.ParseStatus(status); err != nil || value != "Stopped" {
		t.Fatalf("target not stopped: %v %v", status, err)
	}
	config, err := guest.Config(ctx, "instance-target")
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := config["expanded_devices"].(map[string]any)["proxy-ssh-tcp"]; exists {
		t.Fatal("target proxy remains")
	}
	for _, name := range []string{"expired-peer", "active-peer"} {
		after, err := guest.Config(ctx, "instance-"+name)
		if err != nil || !reflect.DeepEqual(before[name], after) {
			t.Fatalf("peer config changed: %s %v", name, err)
		}
		status, err := guest.Status(ctx, "instance-"+name)
		if value, parseErr := native.ParseStatus(status); err != nil || parseErr != nil || value != "Running" {
			t.Fatalf("peer stopped: %s %v %v", name, status, err)
		}
	}
	if _, err := f.service.Dispatch(ctx, f.apply()); err != nil {
		t.Fatal(err)
	}
}
