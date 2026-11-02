package deployd

import (
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

func TestMetricsHaveOneDeploydSeriesPerSlot(t *testing.T) {
	metrics := NewMetrics(nil)
	metrics.Set(RuntimeSnapshot{
		ObservedAt: time.Unix(100, 0), Consistent: true, ActiveSlot: "green",
		Sources: map[string]SourceObservation{"active_link": {Up: true, ObservedAt: time.Unix(100, 0)}},
		Slots: map[string]SlotObservation{
			"blue":  {ProcessUp: true, ProcessReady: true, ControllerOwned: false, TrafficAccepting: false, InFlightRequests: 2, NativeWorkers: 1, PendingOperations: 1, MetricFamilies: map[string]string{"health": "agent_vm_up 1\n"}},
			"green": {ProcessUp: true, ProcessReady: true, ControllerOwned: true, TrafficAccepting: true},
		},
	})
	text := metrics.Render()
	if strings.Count(text, "agent_vm_deployd_slot_controller_owned{slot=") != 2 {
		t.Fatalf("controller series count = %d\n%s", strings.Count(text, "agent_vm_deployd_slot_controller_owned{slot="), text)
	}
	if !strings.Contains(text, `agent_vm_deployd_active_slot{slot="green"} 1`) || !strings.Contains(text, `agent_vm_deployd_active_slot{slot="blue"} 0`) {
		t.Fatalf("active slot series missing:\n%s", text)
	}
	if strings.Contains(text, "\nagent_vm_up ") {
		t.Fatal("deployd exposition leaked an unlabelled slot metric")
	}
	if !strings.Contains(text, `agent_vm_up{slot="blue"} 1`) {
		t.Fatal("deployd did not preserve the existing slot metric with a slot label")
	}
	if !strings.Contains(text, `agent_vm_deployd_observation_up{source="active_link"} 1`) {
		t.Fatalf("source observation series missing:\n%s", text)
	}
	if !strings.Contains(text, `agent_vm_deployd_slot_native_workers{slot="blue"} 1`) {
		t.Fatalf("native worker series missing:\n%s", text)
	}
	metrics.Set(RuntimeSnapshot{}, []store.Transaction{{Phase: PhaseQuiescence, RecoveryRequired: true}})
	text = metrics.Render()
	if !strings.Contains(text, `agent_vm_deployd_transaction_phase{phase="quiescence"} 1`) || !strings.Contains(text, "agent_vm_deployd_recovery_required 1") {
		t.Fatalf("transaction series missing:\n%s", text)
	}
}
