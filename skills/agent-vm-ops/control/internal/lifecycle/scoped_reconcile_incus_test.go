package lifecycle

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
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
	for _, name := range []string{"expired-peer", "active-peer"} {
		config, err := guest.Config(ctx, "instance-"+name)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = config
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
