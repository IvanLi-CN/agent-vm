package observe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotStorePublishesIndependentJSONAndTextfileSources(t *testing.T) {
	store := NewSnapshotStore(t.TempDir())
	if err := store.Publish("capacity", map[string]any{"up": true, "admission_ready": true, "observed_at": "2026-10-26T00:00:00+00:00", "resources": map[string]any{"allocated": map[string]any{"memory_mib": int64(4)}, "reserved": map[string]any{"memory_mib": int64(2)}, "allocated_and_reserved": map[string]any{"memory_mib": int64(6)}, "remaining": map[string]any{"memory_mib": int64(10)}, "limits": map[string]any{"memory_mib": int64(16)}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish("inventory", map[string]any{"up": false}); err != nil {
		t.Fatal(err)
	}
	capacity, err := store.Read("capacity")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(capacity), "observed_at") {
		t.Fatalf("capacity snapshot = %s", capacity)
	}
	inventory, err := os.ReadFile(filepath.Join(store.Directory, "inventory.prom"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(inventory), `agent_vm_source_up{source="inventory"} 0`) {
		t.Fatalf("inventory metrics = %s", inventory)
	}
	if capacityMetrics, err := os.ReadFile(filepath.Join(store.Directory, "capacity.prom")); err != nil || !strings.Contains(string(capacityMetrics), "agent_vm_admission_memory_allocated_bytes 4194304") {
		t.Fatalf("capacity admission metrics = %s err=%v", capacityMetrics, err)
	}
	if _, err := store.Read("unknown"); err == nil {
		t.Fatal("unknown snapshot source unexpectedly accepted")
	}
}
