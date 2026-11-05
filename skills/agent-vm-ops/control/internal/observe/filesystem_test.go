package observe

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesystemsExposeRealCapacityAndFailedReadSeparately(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	rows := Filesystems(root, missing, root, "")
	if len(rows) != 2 || rows[0]["up"] != true || rows[1]["up"] != false {
		t.Fatalf("filesystem observations = %#v", rows)
	}
	if rows[0]["size_bytes"].(int64) <= 0 || rows[0]["used_bytes"].(int64) < 0 || rows[0]["available_bytes"].(int64) < 0 {
		t.Fatalf("invalid real filesystem capacity: %#v", rows[0])
	}
	metrics := MetricsForSnapshot("capacity", map[string]any{"up": true, "filesystems": rows})
	if !strings.Contains(metrics["agent_vm_filesystem_up"], `mountpoint="`+missing+`"} 0`) {
		t.Fatalf("missing filesystem health: %#v", metrics)
	}
	for _, name := range []string{"size_bytes", "used_bytes", "available_bytes"} {
		if strings.Contains(metrics["agent_vm_filesystem_"+name], missing) {
			t.Fatalf("failed read invented filesystem capacity: %#v", metrics)
		}
	}
}

func TestCompleteGlobalTaskTotalsIncludeZerosWithoutMaskingFailure(t *testing.T) {
	for _, up := range []bool{false, true} {
		metrics := MetricsForSnapshot("capacity", map[string]any{"up": up, "observation_consistent": true, "task_states": map[string]int64{"active": 40}, "image_unpack_failures": map[string]int64{}})
		states := metrics["agent_vm_task_states"]
		if !up && states != "" {
			t.Fatal("failed observation invented task totals")
		}
		if up && (!strings.Contains(states, `state="active"} 40`) || !strings.Contains(states, `state="failed"} 0`)) {
			t.Fatalf("global totals missing: %s", states)
		}
		if metrics["agent_vm_image_unpack_failures_count"] != "agent_vm_image_unpack_failures_count 0" {
			t.Fatal("verified empty audit must expose zero failures")
		}
	}
	metrics := MetricsForSnapshot("capacity", map[string]any{"up": false})
	if _, ok := metrics["agent_vm_image_unpack_failures_count"]; ok {
		t.Fatal("missing audit invented zero failures")
	}
}
