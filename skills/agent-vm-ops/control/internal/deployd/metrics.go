package deployd

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

type Metrics struct {
	latest       RuntimeSnapshot
	transactions []store.Transaction
}

func NewMetrics(_ *Manager) *Metrics { return &Metrics{} }

func (metrics *Metrics) Set(snapshot RuntimeSnapshot, transactions ...[]store.Transaction) {
	metrics.latest = snapshot
	if len(transactions) > 0 {
		metrics.transactions = append([]store.Transaction(nil), transactions[0]...)
	} else {
		metrics.transactions = nil
	}
}

func (metrics *Metrics) Render() string {
	var output strings.Builder
	output.WriteString("# HELP agent_vm_deployd_up Deployd process and metrics publisher health.\n")
	output.WriteString("# TYPE agent_vm_deployd_up gauge\n")
	output.WriteString("agent_vm_deployd_up 1\n")
	output.WriteString("# HELP agent_vm_deployd_snapshot_fresh Whether the composed observation is within its freshness bound.\n")
	output.WriteString("# TYPE agent_vm_deployd_snapshot_fresh gauge\n")
	snapshotFresh := composedSnapshotFresh(metrics.latest, time.Now().UTC())
	output.WriteString(fmt.Sprintf("agent_vm_deployd_snapshot_fresh %d\n", boolInt(snapshotFresh)))
	output.WriteString("# HELP agent_vm_deployd_observation_up Whether the latest composed observation is valid.\n")
	output.WriteString("# TYPE agent_vm_deployd_observation_up gauge\n")
	output.WriteString("# HELP agent_vm_deployd_observation_fresh Whether a source observation is within the composed freshness bound.\n")
	output.WriteString("# TYPE agent_vm_deployd_observation_fresh gauge\n")
	for _, source := range []string{"active_link", "slot_contracts", "native_observation", "inventory_observation"} {
		observation := metrics.latest.Sources[source]
		output.WriteString(fmt.Sprintf("agent_vm_deployd_observation_up{source=%s} %d\n", quote(source), boolInt(observation.Up)))
		freshUntil := observation.FreshUntil
		if freshUntil.IsZero() {
			freshUntil = metrics.latest.FreshUntil
		}
		fresh := observation.Up && !observation.ObservedAt.IsZero() && !freshUntil.IsZero() && time.Now().UTC().Before(freshUntil)
		output.WriteString(fmt.Sprintf("agent_vm_deployd_observation_fresh{source=%s} %d\n", quote(source), boolInt(fresh)))
	}
	output.WriteString("# HELP agent_vm_deployd_observation_timestamp_seconds Unix timestamp of the composed observation.\n")
	output.WriteString("# TYPE agent_vm_deployd_observation_timestamp_seconds gauge\n")
	for _, source := range []string{"active_link", "slot_contracts", "native_observation", "inventory_observation"} {
		observation := metrics.latest.Sources[source]
		if !observation.ObservedAt.IsZero() {
			output.WriteString(fmt.Sprintf("agent_vm_deployd_observation_timestamp_seconds{source=%s} %d\n", quote(source), observation.ObservedAt.Unix()))
		}
	}
	output.WriteString("# HELP agent_vm_deployd_active_slot Active slot selected by the protected active link.\n")
	output.WriteString("# TYPE agent_vm_deployd_active_slot gauge\n")
	for _, slot := range []string{"blue", "green"} {
		value := 0
		if metrics.latest.ActiveSlot == slot {
			value = 1
		}
		output.WriteString(fmt.Sprintf("agent_vm_deployd_active_slot{slot=%s} %d\n", quote(slot), value))
	}
	output.WriteString("# HELP agent_vm_deployd_slot_process_up Whether a slot control process responded.\n")
	output.WriteString("# TYPE agent_vm_deployd_slot_process_up gauge\n")
	output.WriteString("# HELP agent_vm_deployd_slot_ready Whether a slot is ready for its assigned role.\n")
	output.WriteString("# TYPE agent_vm_deployd_slot_ready gauge\n")
	output.WriteString("# HELP agent_vm_deployd_slot_selected Whether active routing selects a slot.\n")
	output.WriteString("# TYPE agent_vm_deployd_slot_selected gauge\n")
	output.WriteString("# HELP agent_vm_deployd_slot_controller_owned Whether a slot currently owns the controller lease.\n")
	output.WriteString("# TYPE agent_vm_deployd_slot_controller_owned gauge\n")
	output.WriteString("# HELP agent_vm_deployd_slot_traffic_accepting Whether a slot is allowed to accept new traffic.\n")
	output.WriteString("# TYPE agent_vm_deployd_slot_traffic_accepting gauge\n")
	output.WriteString("# HELP agent_vm_deployd_slot_in_flight_requests Number of observed in-flight operations.\n")
	output.WriteString("# TYPE agent_vm_deployd_slot_in_flight_requests gauge\n")
	output.WriteString("# HELP agent_vm_deployd_slot_pending_operations Number of pending durable operations.\n")
	output.WriteString("# TYPE agent_vm_deployd_slot_pending_operations gauge\n")
	output.WriteString("# HELP agent_vm_deployd_slot_native_workers Number of native workers that have crossed the effect barrier.\n")
	output.WriteString("# TYPE agent_vm_deployd_slot_native_workers gauge\n")
	for _, slot := range []string{"blue", "green"} {
		observation := metrics.latest.Slots[slot]
		output.WriteString(fmt.Sprintf("agent_vm_deployd_slot_process_up{slot=%s} %d\n", quote(slot), boolInt(observation.ProcessUp)))
		output.WriteString(fmt.Sprintf("agent_vm_deployd_slot_ready{slot=%s} %d\n", quote(slot), boolInt(observation.Ready)))
		output.WriteString(fmt.Sprintf("agent_vm_deployd_slot_selected{slot=%s} %d\n", quote(slot), boolInt(metrics.latest.ActiveSlot == slot)))
		output.WriteString(fmt.Sprintf("agent_vm_deployd_slot_controller_owned{slot=%s} %d\n", quote(slot), boolInt(observation.ControllerOwned)))
		output.WriteString(fmt.Sprintf("agent_vm_deployd_slot_traffic_accepting{slot=%s} %d\n", quote(slot), boolInt(observation.TrafficAccepting)))
		output.WriteString(fmt.Sprintf("agent_vm_deployd_slot_in_flight_requests{slot=%s} %d\n", quote(slot), observation.InFlightRequests))
		output.WriteString(fmt.Sprintf("agent_vm_deployd_slot_pending_operations{slot=%s} %d\n", quote(slot), observation.PendingOperations))
		output.WriteString(fmt.Sprintf("agent_vm_deployd_slot_native_workers{slot=%s} %d\n", quote(slot), observation.NativeWorkers))
		for _, family := range sortedFamilies(observation.MetricFamilies) {
			output.WriteString(withSlotLabel(observation.MetricFamilies[family], slot))
		}
	}
	output.WriteString("# HELP agent_vm_deployd_transaction_phase Current deployment transaction phase.\n")
	output.WriteString("# TYPE agent_vm_deployd_transaction_phase gauge\n")
	phases := []string{PhaseStage, PhaseStandbyValidation, PhaseControllerHandoff, PhaseTrafficEnable, PhaseActiveSwitch, PhaseDrainOld, PhaseQuiescence, PhaseRollback, PhaseFinalize}
	currentPhase := ""
	if len(metrics.transactions) > 0 {
		currentPhase = metrics.transactions[0].Phase
	}
	for _, phase := range phases {
		value := 0
		if currentPhase == phase {
			value = 1
		}
		output.WriteString(fmt.Sprintf("agent_vm_deployd_transaction_phase{phase=%s} %d\n", quote(phase), value))
	}
	output.WriteString("# HELP agent_vm_deployd_recovery_required Whether deployment recovery is blocking role ownership.\n")
	output.WriteString("# TYPE agent_vm_deployd_recovery_required gauge\n")
	recoveryRequired := 0
	if len(metrics.transactions) > 0 && metrics.transactions[0].RecoveryRequired {
		recoveryRequired = 1
	}
	output.WriteString(fmt.Sprintf("agent_vm_deployd_recovery_required %d\n", recoveryRequired))
	return output.String()
}

func sortedFamilies(families map[string]string) []string {
	keys := make([]string, 0, len(families))
	for key := range families {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func withSlotLabel(content, slot string) string {
	var output strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(content, "\n"), "\n") {
		if line == "" {
			output.WriteString(line)
			output.WriteByte('\n')
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		space := strings.IndexByte(line, ' ')
		if space <= 0 {
			continue
		}
		name := line[:space]
		if strings.HasSuffix(name, "}") {
			name = strings.TrimSuffix(name, "}") + `,slot="` + slot + `"}`
		} else {
			name += `{slot="` + slot + `"}`
		}
		output.WriteString(name)
		output.WriteString(line[space:])
		output.WriteByte('\n')
	}
	return output.String()
}

func quote(value string) string { return strconv.Quote(value) }

func composedSnapshotFresh(snapshot RuntimeSnapshot, now time.Time) bool {
	if !snapshot.Consistent || (snapshot.ActiveSlot != "blue" && snapshot.ActiveSlot != "green") || snapshot.ObservedAt.IsZero() || snapshot.FreshUntil.IsZero() || !snapshot.FreshUntil.After(now) {
		return false
	}
	for _, name := range []string{"active_link", "slot_contracts", "native_observation", "inventory_observation"} {
		source, ok := snapshot.Sources[name]
		if !ok || !source.Up || source.ObservedAt.IsZero() || source.ObservedAt.After(now) {
			return false
		}
		freshUntil := source.FreshUntil
		if freshUntil.IsZero() {
			freshUntil = snapshot.FreshUntil
		}
		if !freshUntil.After(now) {
			return false
		}
	}
	return true
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
