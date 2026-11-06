package deployd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
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

func TestMetricsRejectFutureSourceObservationAsFresh(t *testing.T) {
	now := time.Now().UTC()
	sources := map[string]SourceObservation{}
	for _, name := range []string{"active_link", "systemd", "slot_contracts", "native_observation"} {
		sources[name] = SourceObservation{Up: true, ObservedAt: now, FreshUntil: now.Add(time.Minute)}
	}
	sources["inventory_observation"] = SourceObservation{Up: true, ObservedAt: now.Add(time.Second), FreshUntil: now.Add(2 * time.Minute)}
	metrics := NewMetrics(nil)
	metrics.Set(RuntimeSnapshot{ObservedAt: now, FreshUntil: now.Add(time.Minute), ActiveSlot: "blue", Consistent: true, Sources: sources})
	text := metrics.Render()
	if !strings.Contains(text, `agent_vm_deployd_observation_fresh{source="inventory_observation"} 0`) || strings.Contains(text, "agent_vm_deployd_snapshot_fresh 1") || strings.Contains(text, `agent_vm_deployd_observation_timestamp_seconds{source="inventory_observation"}`) {
		t.Fatalf("future observation was reported as fresh:\n%s", text)
	}
}

func TestMetricsRejectFutureTopLevelObservationAsFresh(t *testing.T) {
	now := time.Now().UTC()
	sources := map[string]SourceObservation{}
	for _, name := range []string{"active_link", "systemd", "slot_contracts", "native_observation", "inventory_observation"} {
		sources[name] = SourceObservation{Up: true, ObservedAt: now, FreshUntil: now.Add(time.Minute)}
	}
	metrics := NewMetrics(nil)
	metrics.Set(RuntimeSnapshot{ObservedAt: now.Add(time.Second), FreshUntil: now.Add(2 * time.Minute), ActiveSlot: "blue", Consistent: true, Sources: sources})
	text := metrics.Render()
	if strings.Contains(text, "agent_vm_deployd_snapshot_fresh 1") || strings.Contains(text, `agent_vm_deployd_active_slot{slot="blue"}`) {
		t.Fatalf("future top-level observation was reported as fresh:\n%s", text)
	}
}

func TestSharedIncusMetricsAreEmittedOnceFromActiveSlot(t *testing.T) {
	now := time.Now().UTC()
	sources := map[string]SourceObservation{}
	for _, name := range []string{"active_link", "systemd", "slot_contracts", "native_observation", "inventory_observation"} {
		sources[name] = SourceObservation{Up: true, ObservedAt: now, FreshUntil: now.Add(time.Minute)}
	}
	snapshot := RuntimeSnapshot{ObservedAt: now, FreshUntil: now.Add(time.Minute), Consistent: true, ActiveSlot: "green", Sources: sources, Slots: map[string]SlotObservation{
		"blue":  {MetricFamilies: map[string]string{"incus_cpu_seconds_total": "incus_cpu_seconds_total{project=\"test\"} 1\n"}},
		"green": {MetricFamilies: map[string]string{"incus_cpu_seconds_total": "incus_cpu_seconds_total{project=\"test\"} 2\n"}},
	}}
	metrics := NewMetrics(nil)
	metrics.Set(snapshot)
	text := metrics.Render()
	if strings.Count(text, "incus_cpu_seconds_total{") != 1 || !strings.Contains(text, "incus_cpu_seconds_total{project=\"test\"} 2\n") {
		t.Fatalf("shared metrics duplicated or wrong slot:\n%s", text)
	}
	snapshot.FreshUntil = now.Add(-time.Second)
	metrics.Set(snapshot)
	if strings.Contains(metrics.Render(), "incus_cpu_seconds_total{") {
		t.Fatal("stale shared metrics emitted")
	}
}

func TestPublicationKeepsLiveTransactionObservableButFencesRecovery(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "recovery"}[recovery], func(t *testing.T) {
			now := time.Now().UTC()
			ctx := context.Background()
			db, err := store.Open(filepath.Join(t.TempDir(), "deploy.sqlite3"), 30*24*time.Hour, time.Now)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			tx := store.Transaction{ID: "tx-publication", Bundle: "bundle", TargetSlot: "green", PreviousSlot: "blue", Phase: PhaseStandbyValidation, State: "active", Generation: 1, CreatedAt: now, UpdatedAt: now}
			if recovery {
				tx.State = "needs_recovery"
				tx.RecoveryRequired = true
			}
			if err := db.Create(ctx, tx, map[string]any{}); err != nil {
				t.Fatal(err)
			}
			sources := map[string]SourceObservation{}
			for _, name := range []string{"active_link", "systemd", "slot_contracts", "native_observation", "inventory_observation"} {
				sources[name] = SourceObservation{Up: true, ObservedAt: now, FreshUntil: now.Add(time.Minute)}
			}
			snapshot := RuntimeSnapshot{ObservedAt: now, FreshUntil: now.Add(time.Minute), ActiveSlot: "blue", Consistent: true, Sources: sources, Slots: map[string]SlotObservation{"blue": {Slot: "blue", ProcessUp: true, ControllerOwned: true}, "green": {Slot: "green", ProcessUp: true}}}
			raw, _ := json.Marshal(snapshot)
			var payload map[string]any
			json.Unmarshal(raw, &payload)
			canonical, _ := json.Marshal(payload)
			digest := sha256.Sum256(canonical)
			if err := db.SaveObservation(ctx, store.LatestObservationID, store.Observation{ObservedAt: now, FreshUntil: now.Add(time.Minute), Digest: hex.EncodeToString(digest[:]), Payload: payload}); err != nil {
				t.Fatal(err)
			}
			server := NewServer(NewManager(db, &fakeRuntime{}, time.Second, time.Now), nil)
			// Publication must not wait for an in-progress transaction writer.
			server.Manager.mu.Lock()
			err = server.RestoreLatest(ctx)
			server.Manager.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			text := server.Metrics.Render()
			fresh := strings.Contains(text, "agent_vm_deployd_snapshot_fresh 1")
			if fresh == recovery {
				t.Fatalf("incorrect publication recovery=%t: %s", recovery, text)
			}
		})
	}
}
