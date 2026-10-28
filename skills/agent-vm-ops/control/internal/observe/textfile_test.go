package observe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSnapshotStorePublishesIndependentJSONAndTextfileSources(t *testing.T) {
	store := NewSnapshotStore(t.TempDir())
	if err := store.Publish("capacity", map[string]any{"up": true, "admission_ready": true, "observed_at": "2026-10-26T00:00:00+00:00", "resources": map[string]any{"allocated": map[string]any{"memory_mib": int64(4), "disk_gib": int64(2)}, "reserved": map[string]any{"memory_mib": int64(2), "disk_gib": int64(1)}, "allocated_and_reserved": map[string]any{"memory_mib": int64(6), "disk_gib": int64(3)}, "remaining": map[string]any{"memory_mib": int64(10), "disk_gib": int64(7)}, "limits": map[string]any{"memory_mib": int64(16), "disk_gib": int64(10)}}, "ct_cpu": map[string]any{"startup_pressure": map[string]any{"verified": false, "startup_blocked": nil, "threshold_full_avg10_percent": int64(50)}}, "disk_admission": map[string]any{"physical_admission": map[string]any{"allocated_bytes": int64(100), "reserved_bytes": int64(20), "allocated_and_reserved_bytes": int64(120), "remaining_bytes": int64(80), "limit_bytes": int64(180), "capacity_bytes": int64(200), "low_watermark_bytes": int64(40), "target_watermark_bytes": int64(60)}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Publish("inventory", map[string]any{"up": true, "tasks": []map[string]any{{
		"task_id": "task-1", "session_id": "session-1", "state": "active", "instance_name": "instance-1",
		"native_status": "Running", "guest_available": true, "effective_lease": true,
		"resources":        map[string]any{"cpu": int64(2), "memory_mib": int64(4), "disk_gib": int64(3)},
		"lease_expires_at": "2026-10-26T00:00:10Z", "last_error": `{"code":"resize_partial","phase":"native-config"}`,
		"ports": []map[string]any{{"port_name": "ssh", "protocol": "tcp", "device_name": "proxy-ssh-tcp"}},
	}, {
		"task_id": "task-unknown", "session_id": "session-unknown", "state": "mystery", "instance_name": "instance-unknown",
	}}}); err != nil {
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
	if !strings.Contains(string(inventory), "agent_vm_inventory_up 1") {
		t.Fatalf("inventory metrics = %s", inventory)
	}
	for _, metric := range []string{
		`agent_vm_task_info{task_id="task-1",state="active",instance_name="instance-1",session_id="session-1",native_status="Running",guest_available="true",effective_lease="true"} 1`,
		`agent_vm_task_cpu_vcpu{task_id="task-1",state="active"} 2`,
		`agent_vm_task_memory_bytes{task_id="task-1",state="active"} 4194304`,
		`agent_vm_task_disk_bytes{task_id="task-1",state="active"} 3221225472`,
		`agent_vm_task_lease_expires_at_seconds{task_id="task-1",state="active"} 1792972810`,
		`agent_vm_task_last_error_info{task_id="task-1",state="active",code="resize_partial",phase="native-config"} 1`,
		`agent_vm_task_ssh_mapping{task_id="task-1",state="active",port_name="ssh",protocol="tcp",device_name="proxy-ssh-tcp"} 1`,
	} {
		if !strings.Contains(string(inventory), metric) {
			t.Fatalf("inventory metrics missing %q: %s", metric, inventory)
		}
	}
	if !strings.Contains(string(inventory), `agent_vm_task_info{task_id="task-unknown",state="mystery",instance_name="instance-unknown",session_id="session-unknown",native_status="unknown",guest_available="unknown",effective_lease="unknown"} 1`) {
		t.Fatalf("inventory metrics lost unknown task observations = %s", inventory)
	}
	if strings.Contains(string(inventory), "agent_vm_scrape_timestamp_seconds") {
		t.Fatalf("inventory metrics reused an unscoped family = %s", inventory)
	}
	if capacityMetrics, err := os.ReadFile(filepath.Join(store.Directory, "capacity.prom")); err != nil || !strings.Contains(string(capacityMetrics), "agent_vm_admission_memory_allocated_bytes 4194304") {
		t.Fatalf("capacity admission metrics = %s err=%v", capacityMetrics, err)
	}
	capacityText, err := os.ReadFile(filepath.Join(store.Directory, "capacity.prom"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(capacityText), "agent_vm_admission_disk_allocated_bytes 100") || strings.Contains(string(capacityText), "agent_vm_admission_disk_allocated_bytes 2147483648") {
		t.Fatalf("capacity disk domain metrics = %s", capacityText)
	}
	for _, metric := range []string{
		"agent_vm_admission_disk_reserved_bytes 20",
		"agent_vm_admission_disk_allocated_and_reserved_bytes 120",
		"agent_vm_admission_disk_remaining_bytes 80",
		"agent_vm_admission_disk_limit_bytes 180",
		"agent_vm_admission_disk_capacity_bytes 200",
		"agent_vm_admission_disk_low_watermark_bytes 40",
		"agent_vm_admission_disk_target_watermark_bytes 60",
	} {
		if !strings.Contains(string(capacityText), metric) {
			t.Fatalf("capacity metrics missing %q: %s", metric, capacityText)
		}
	}
	if strings.Contains(string(capacityText), "agent_vm_cpu_startup_blocked") {
		t.Fatalf("unverified startup pressure was published as known: %s", capacityText)
	}
	failedStore := NewSnapshotStore(t.TempDir())
	if err := failedStore.Publish("capacity", map[string]any{"up": false, "observed_at": "2026-10-26T00:00:00Z", "error": map[string]any{"code": "resource_unverified"}}); err != nil {
		t.Fatal(err)
	}
	failedMetrics, err := os.ReadFile(filepath.Join(failedStore.Directory, "capacity.prom"))
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range []string{"agent_vm_admission_ready", "agent_vm_admission_draining", "agent_vm_observation_consistent"} {
		if strings.Contains(string(failedMetrics), metric) {
			t.Fatalf("failed capacity scrape fabricated %s: %s", metric, failedMetrics)
		}
	}
	if _, err := store.Read("unknown"); err == nil {
		t.Fatal("unknown snapshot source unexpectedly accepted")
	}
}
