package observe

import (
	"strings"
	"testing"
)

func TestIncusMetricsRejectForeignDuplicateAndMalformedSamples(t *testing.T) {
	valid := "# HELP incus_cpu_seconds_total CPU\nincus_cpu_seconds_total{project=\"my project\",name=\"guest\"} 1.25\n"
	families, err := ParseIncusMetrics(valid)
	if err != nil || !strings.Contains(families["incus_cpu_seconds_total"], "my project") {
		t.Fatalf("valid samples: %v %v", families, err)
	}
	for _, input := range []string{valid + valid, "agent_vm_up 1\n", "incus_cpu{a=\"unterminated} 1\n", "incus_cpu NaNa\n", "incus_cpu 1\nincus_cpu 2\n", ""} {
		if _, err := ParseIncusMetrics(input); err == nil {
			t.Errorf("accepted invalid samples: %q", input)
		}
	}
	failed := MetricsForSnapshot("capacity", map[string]any{"incus_metrics_up": true, "incus_metrics": "agent_vm_up 1\n"})
	if failed["agent_vm_incus_metrics_up"] != "agent_vm_incus_metrics_up 0" || failed["agent_vm_up"] != "" {
		t.Fatal("invalid Incus batch did not fail closed")
	}
}
