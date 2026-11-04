package deployd

import (
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

func TestMetricsHaveOneDeploydSeriesPerSlot(t *testing.T) {
	metrics := NewMetrics(nil)
	now := time.Now().UTC()
	sources := map[string]SourceObservation{}
	for _, name := range []string{"active_link", "systemd", "slot_contracts", "native_observation", "inventory_observation"} {
		sources[name] = SourceObservation{Up: true, ObservedAt: now, FreshUntil: now.Add(time.Minute)}
	}
	metrics.Set(RuntimeSnapshot{
		ObservedAt: now, FreshUntil: now.Add(time.Minute), Consistent: true, ActiveSlot: "green", Sources: sources,
		Slots: map[string]SlotObservation{
			"blue":  {ProcessUp: true, ProcessReady: true, ControllerOwned: false, TrafficAccepting: false, CountersKnown: true, InFlightRequests: 2, NativeWorkers: 1, PendingOperations: 1, MetricFamilies: map[string]string{"health": "agent_vm_up 1\n"}},
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

func TestMetricsOmitUnknownSlotCounters(t *testing.T) {
	metrics := NewMetrics(nil)
	metrics.Set(RuntimeSnapshot{Slots: map[string]SlotObservation{
		"blue": {ProcessUp: true},
	}})
	text := metrics.Render()
	for _, name := range []string{
		"agent_vm_deployd_slot_in_flight_requests{slot=\"blue\"}",
		"agent_vm_deployd_slot_pending_operations{slot=\"blue\"}",
		"agent_vm_deployd_slot_native_workers{slot=\"blue\"}",
	} {
		if strings.Contains(text, name) {
			t.Fatalf("unknown slot counter was exposed: %s\n%s", name, text)
		}
	}
}

func TestMetricsOmitSlotStateWhenSnapshotIsNotFresh(t *testing.T) {
	metrics := NewMetrics(nil)
	metrics.Set(RuntimeSnapshot{
		ObservedAt: time.Now().UTC().Add(-time.Minute),
		FreshUntil: time.Now().UTC().Add(-time.Second),
		Consistent: false,
		ActiveSlot: "blue",
		Slots: map[string]SlotObservation{
			"blue":  {ProcessUp: true, Ready: true, ControllerOwned: true, TrafficAccepting: true},
			"green": {ProcessUp: false},
		},
	})
	text := metrics.Render()
	for _, name := range []string{
		"agent_vm_deployd_active_slot{slot=",
		"agent_vm_deployd_slot_process_up{slot=",
		"agent_vm_deployd_slot_ready{slot=",
		"agent_vm_deployd_slot_selected{slot=",
		"agent_vm_deployd_slot_controller_owned{slot=",
		"agent_vm_deployd_slot_traffic_accepting{slot=",
	} {
		if strings.Contains(text, name) {
			t.Fatalf("stale slot state was exposed: %s\n%s", name, text)
		}
	}
}

func TestMetricsMarkComposedSnapshotStaleWhenASourceFails(t *testing.T) {
	now := time.Now().UTC()
	metrics := NewMetrics(nil)
	sources := map[string]SourceObservation{}
	for _, name := range []string{"active_link", "systemd", "slot_contracts", "native_observation", "inventory_observation"} {
		sources[name] = SourceObservation{Up: true, ObservedAt: now, FreshUntil: now.Add(time.Minute)}
	}
	sources["native_observation"] = SourceObservation{Up: false, ObservedAt: now, FreshUntil: now.Add(time.Minute), Error: "observation unavailable"}
	metrics.Set(RuntimeSnapshot{ObservedAt: now, FreshUntil: now.Add(time.Minute), Consistent: false, Sources: sources})
	if strings.Contains(metrics.Render(), "agent_vm_deployd_snapshot_fresh 1") {
		t.Fatal("failed source was exposed as a fresh composed snapshot")
	}
}

func TestMetricsRetainCapacityFamiliesWhenInventoryFails(t *testing.T) {
	now := time.Now().UTC()
	sources := map[string]SourceObservation{}
	for _, name := range []string{"active_link", "systemd", "slot_contracts", "native_observation"} {
		sources[name] = SourceObservation{Up: true, ObservedAt: now, FreshUntil: now.Add(time.Minute)}
	}
	sources["inventory_observation"] = SourceObservation{Up: false, ObservedAt: now, FreshUntil: now.Add(time.Minute), Error: "observation unavailable"}
	metrics := NewMetrics(nil)
	metrics.Set(RuntimeSnapshot{
		ObservedAt: now, FreshUntil: now.Add(time.Minute), Consistent: false, ActiveSlot: "blue", Sources: sources,
		Slots: map[string]SlotObservation{
			"blue": {CountersKnown: true, InFlightRequests: 2, PendingOperations: 3, NativeWorkers: 1, MetricFamilies: map[string]string{"capacity": "agent_vm_capacity 1\n"}},
		},
	})
	text := metrics.Render()
	if !strings.Contains(text, `agent_vm_deployd_slot_in_flight_requests{slot="blue"} 2`) || !strings.Contains(text, `agent_vm_capacity{slot="blue"} 1`) {
		t.Fatalf("capacity metrics were removed with failed inventory observation:\n%s", text)
	}
	if strings.Contains(text, `agent_vm_deployd_active_slot{slot="blue"}`) {
		t.Fatal("stale composed active-slot state was exposed")
	}
}
