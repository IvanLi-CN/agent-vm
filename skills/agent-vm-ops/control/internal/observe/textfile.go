package observe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

type Publisher struct{ Directory string }

func NewPublisher(directory string) *Publisher { return &Publisher{Directory: directory} }

func (publisher *Publisher) Publish(source string, families map[string]string) error {
	if publisher.Directory == "" {
		return fmt.Errorf("metrics directory is empty")
	}
	if source != "capacity" && source != "inventory" && source != "health" {
		return fmt.Errorf("unsupported metrics source: %s", source)
	}
	if err := os.MkdirAll(publisher.Directory, 0o750); err != nil {
		return err
	}
	keys := make([]string, 0, len(families))
	for key := range families {
		if !metricName.MatchString(key) {
			return fmt.Errorf("invalid metric family: %s", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var content strings.Builder
	for _, key := range keys {
		content.WriteString(families[key])
		if !strings.HasSuffix(families[key], "\n") {
			content.WriteByte('\n')
		}
	}
	name := filepath.Join(publisher.Directory, source+".prom")
	temporary, err := os.CreateTemp(publisher.Directory, "."+source+"-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o640); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(content.String()); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, name)
}

type SnapshotStore struct {
	Directory string
	Metrics   *Publisher
}

func NewSnapshotStore(directory string) *SnapshotStore {
	return &SnapshotStore{Directory: directory, Metrics: NewPublisher(directory)}
}

func (store *SnapshotStore) Publish(source string, value any) error {
	if !validSource(source) {
		return fmt.Errorf("unsupported snapshot source: %s", source)
	}
	if store == nil || store.Directory == "" {
		return fmt.Errorf("snapshot directory is empty")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(store.Directory, 0o750); err != nil {
		return err
	}
	if err := atomicFile(filepath.Join(store.Directory, source+".json"), raw, 0o640); err != nil {
		return err
	}
	metrics := store.Metrics
	if metrics == nil {
		metrics = NewPublisher(store.Directory)
	}
	return metrics.Publish(source, snapshotMetrics(source, value))
}

func snapshotMetrics(source string, value any) map[string]string {
	object := normalizedSnapshotObject(value)
	if nested, ok := object["result"].(map[string]any); ok {
		object = nested
	}
	up := boolNumber(object["up"], false)
	observed := time.Now().Unix()
	if raw, ok := object["observed_at"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			observed = parsed.Unix()
		}
	}
	result := map[string]string{
		"agent_vm_" + source + "_up":                       fmt.Sprintf("agent_vm_%s_up %d", source, up),
		"agent_vm_" + source + "_observed_at_seconds":      fmt.Sprintf("agent_vm_%s_observed_at_seconds %d", source, observed),
		"agent_vm_" + source + "_scrape_timestamp_seconds": fmt.Sprintf("agent_vm_%s_scrape_timestamp_seconds %d", source, observed),
	}
	if source == "capacity" {
		addBoolMetricIfPresent(result, "agent_vm_admission_ready", object, "admission_ready")
		addBoolMetricIfPresent(result, "agent_vm_admission_draining", object, "draining")
		addBoolMetricIfPresent(result, "agent_vm_observation_consistent", object, "observation_consistent")
		if resources, ok := objectMap(object["resources"]); ok {
			addResourceMetric(result, "cpu", "cpu", resources, "vcpu", 1)
			addResourceMetric(result, "memory", "memory_mib", resources, "bytes", 1024*1024)
		}
		if admission, ok := objectMap(object["disk_admission"]); ok {
			if physical, ok := objectMap(admission["physical_admission"]); ok {
				addPhysicalDiskMetric(result, physical)
			}
		}
		if instances, ok := objectMap(object["instances"]); ok {
			if admission, ok := objectMap(instances["admission"]); ok {
				addInstanceMetric(result, "allocated", admission["allocated"])
				addInstanceMetric(result, "reserved", admission["reserved"])
				addInstanceMetric(result, "allocated_and_reserved", admission["allocated_and_reserved"])
				addInstanceMetric(result, "remaining", admission["remaining"])
				addInstanceMetric(result, "limit", admission["limit"])
				addInstanceMetric(result, "capacity", admission["capacity"])
			}
		}
		if defaultAdmission, ok := objectMap(object["default_admission"]); ok {
			if requested, ok := objectMap(defaultAdmission["requested"]); ok {
				addMetric(result, "agent_vm_admission_default_request_cpu_vcpu", requested["cpu"], 1)
				addMetric(result, "agent_vm_admission_default_request_memory_bytes", requested["memory_mib"], 1024*1024)
				addMetric(result, "agent_vm_admission_default_request_disk_bytes", requested["disk_gib"], 1024*1024*1024)
			}
			addMetric(result, "agent_vm_admission_default_request_physical_cost_bytes", defaultAdmission["request_cost_bytes"], 1)
		}
		if value, exists := object["reclaim_required"]; exists && value != nil {
			addMetric(result, "agent_vm_admission_reclaim_required", value, 1)
		}
		if cpu, ok := objectMap(object["ct_cpu"]); ok {
			addMetric(result, "agent_vm_admission_cpu_capacity_vcpu", cpu["capacity_vcpu"], 1)
			if pressure, ok := objectMap(cpu["pressure"]); ok {
				addMetric(result, "agent_vm_cpu_pressure_some_avg10_percent", nestedMetricValue(pressure, "some", "avg10"), 1)
				addMetric(result, "agent_vm_cpu_pressure_full_avg10_percent", nestedMetricValue(pressure, "full", "avg10"), 1)
			}
			if startup, ok := objectMap(cpu["startup_pressure"]); ok {
				addMetric(result, "agent_vm_cpu_startup_pressure_threshold_percent", startup["threshold_full_avg10_percent"], 1)
				if startup["verified"] == true {
					addMetric(result, "agent_vm_cpu_startup_blocked", startup["startup_blocked"], 1)
				}
			}
		}
		if memory, ok := objectMap(object["ct_memory"]); ok {
			addMetric(result, "agent_vm_memory_current_bytes", memory["current_bytes"], 1)
			addMetric(result, "agent_vm_memory_available_bytes", memory["available_bytes"], 1)
			addMetric(result, "agent_vm_memory_pressure_threshold_bytes", memory["pressure_threshold_bytes"], 1)
			addMetric(result, "agent_vm_memory_pressure", memory["pressure"], 1)
		}
	}
	if source == "inventory" {
		states, _ := objectMap(object["task_states"])
		if len(states) == 0 {
			if tasks, ok := object["tasks"].([]any); ok {
				states = make(map[string]any)
				for _, item := range tasks {
					if task, ok := objectMap(item); ok {
						if state, ok := task["state"].(string); ok {
							states[state] = numericValue(states[state]) + 1
						}
					}
				}
			}
		}
		if len(states) > 0 {
			for state, count := range states {
				result["agent_vm_task_states"] += fmt.Sprintf("agent_vm_task_states{state=%s} %s\n", strconv.Quote(state), number(count))
			}
		}
		if tasks, ok := object["tasks"].([]any); ok {
			for _, rawTask := range tasks {
				task, ok := objectMap(rawTask)
				if !ok {
					continue
				}
				addTaskMetrics(result, task)
			}
		}
	}
	if source == "health" {
		result["agent_vm_up"] = fmt.Sprintf("agent_vm_up %d", up)
		addBoolMetricIfPresent(result, "agent_vm_health_ready", object, "ready")
	}
	return result
}

func addBoolMetricIfPresent(result map[string]string, name string, object map[string]any, key string) {
	value, ok := object[key].(bool)
	if !ok {
		return
	}
	result[name] = fmt.Sprintf("%s %d", name, boolNumber(value, false))
}

func normalizedSnapshotObject(value any) map[string]any {
	raw, err := json.Marshal(value)
	if err != nil {
		object, _ := value.(map[string]any)
		return object
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	object := map[string]any{}
	if err := decoder.Decode(&object); err != nil {
		return map[string]any{}
	}
	return object
}

func addPhysicalDiskMetric(result map[string]string, physical map[string]any) {
	for _, kind := range []string{"allocated", "reserved", "allocated_and_reserved", "remaining", "limit", "capacity"} {
		value, ok := physical[kind+"_bytes"]
		if !ok {
			continue
		}
		name := "agent_vm_admission_disk_" + kind + "_bytes"
		addMetric(result, name, value, 1)
	}
}

func addInstanceMetric(result map[string]string, kind string, value any) {
	addMetric(result, "agent_vm_admission_instances_"+kind+"_count", value, 1)
}

func addMetric(result map[string]string, name string, value any, multiplier float64) {
	if _, ok := numeric(value); !ok {
		if _, isBool := value.(bool); !isBool {
			return
		}
	}
	result[name] = fmt.Sprintf("%s %s", name, numberWithMultiplier(value, multiplier))
}

func nestedMetricValue(object map[string]any, outer, inner string) any {
	nested, ok := objectMap(object[outer])
	if !ok {
		return nil
	}
	return nested[inner]
}

func addTaskMetrics(result map[string]string, task map[string]any) {
	taskID := taskLabel(task["task_id"])
	state := taskLabel(task["state"])
	baseLabels := fmt.Sprintf("task_id=%s,state=%s", taskID, state)
	infoLabels := baseLabels + ",instance_name=" + taskLabel(task["instance_name"]) + ",session_id=" + taskLabel(task["session_id"]) + ",native_status=" + taskLabel(task["native_status"]) + ",guest_available=" + taskLabel(tristateLabel(task["guest_available"])) + ",effective_lease=" + taskLabel(task["effective_lease"])
	result["agent_vm_task_info"] += fmt.Sprintf("agent_vm_task_info{%s} 1\n", infoLabels)
	if resources, ok := objectMap(task["resources"]); ok {
		addLabeledMetric(result, "agent_vm_task_cpu_vcpu", baseLabels, resources["cpu"], 1)
		addLabeledMetric(result, "agent_vm_task_memory_bytes", baseLabels, resources["memory_mib"], 1024*1024)
		addLabeledMetric(result, "agent_vm_task_disk_bytes", baseLabels, resources["disk_gib"], 1024*1024*1024)
	}
	if timestamp, ok := timestampSeconds(task["lease_expires_at"]); ok {
		addLabeledMetric(result, "agent_vm_task_lease_expires_at_seconds", baseLabels, timestamp, 1)
	}
	if lastError, ok := taskError(task["last_error"]); ok {
		labels := baseLabels + ",code=" + taskLabel(lastError["code"]) + ",phase=" + taskLabel(lastError["phase"])
		result["agent_vm_task_last_error_info"] += fmt.Sprintf("agent_vm_task_last_error_info{%s} 1\n", labels)
	}
	if ports, ok := task["ports"].([]any); ok {
		for _, rawPort := range ports {
			port, ok := objectMap(rawPort)
			if !ok {
				continue
			}
			labels := baseLabels + ",port_name=" + taskLabel(port["port_name"]) + ",protocol=" + taskLabel(port["protocol"]) + ",device_name=" + taskLabel(port["device_name"])
			result["agent_vm_task_ssh_mapping"] += fmt.Sprintf("agent_vm_task_ssh_mapping{%s} 1\n", labels)
		}
	}
}

func addLabeledMetric(result map[string]string, name, labels string, value any, multiplier float64) {
	if _, ok := numeric(value); !ok {
		return
	}
	result[name] += fmt.Sprintf("%s{%s} %s\n", name, labels, numberWithMultiplier(value, multiplier))
}

func taskLabel(value any) string {
	switch typed := value.(type) {
	case nil:
		return strconv.Quote("unknown")
	case bool:
		if typed {
			return strconv.Quote("true")
		}
		return strconv.Quote("false")
	case string:
		if typed == "" {
			return strconv.Quote("unknown")
		}
		return strconv.Quote(typed)
	default:
		return strconv.Quote(fmt.Sprint(typed))
	}
}

func tristateLabel(value any) string {
	if value == nil {
		return "unknown"
	}
	if typed, ok := value.(bool); ok {
		if typed {
			return "true"
		}
		return "false"
	}
	if text, ok := value.(string); ok && text != "" {
		return text
	}
	return fmt.Sprint(value)
}

func timestampSeconds(value any) (int64, bool) {
	text, ok := value.(string)
	if !ok || text == "" {
		return 0, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return 0, false
	}
	return parsed.Unix(), true
}

func taskError(value any) (map[string]any, bool) {
	if value == nil {
		return nil, false
	}
	if object, ok := objectMap(value); ok {
		return object, true
	}
	text, ok := value.(string)
	if !ok || text == "" {
		return nil, false
	}
	var object map[string]any
	if json.Unmarshal([]byte(text), &object) != nil || object == nil {
		return nil, false
	}
	return object, true
}

func addResourceMetric(result map[string]string, dimension, inputKey string, resources map[string]any, suffix string, multiplier float64) {
	for _, kind := range []string{"allocated", "reserved", "allocated_and_reserved", "remaining", "limits"} {
		values, ok := objectMap(resources[kind])
		if !ok {
			continue
		}
		value, ok := values[inputKey]
		if !ok {
			continue
		}
		name := "agent_vm_admission_" + dimension + "_" + kind + "_" + suffix
		if kind == "limits" {
			name = "agent_vm_admission_" + dimension + "_limit_" + suffix
		}
		result[name] = fmt.Sprintf("%s %s", name, numberWithMultiplier(value, multiplier))
	}
}

func objectMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case map[string]int64:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = item
		}
		return result, true
	default:
		return nil, false
	}
}

func numericValue(value any) float64 {
	if parsed, ok := numeric(value); ok {
		return parsed
	}
	return 0
}

func number(value any) string { return numberWithMultiplier(value, 1) }

func numberWithMultiplier(value any, multiplier float64) string {
	valueFloat, ok := numeric(value)
	if !ok {
		return "NaN"
	}
	return strconv.FormatFloat(valueFloat*multiplier, 'f', -1, 64)
}

func numeric(value any) (float64, bool) {
	switch typed := value.(type) {
	case bool:
		if typed {
			return 1, true
		}
		return 0, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	default:
		return 0, false
	}
}

func boolNumber(value any, fallback bool) int {
	if typed, ok := value.(bool); ok {
		if typed {
			return 1
		}
		return 0
	}
	if fallback {
		return 1
	}
	return 0
}

func (store *SnapshotStore) Read(source string) ([]byte, error) {
	if store == nil || store.Directory == "" {
		return nil, fmt.Errorf("snapshot directory is empty")
	}
	if !validSource(source) {
		return nil, fmt.Errorf("unsupported snapshot source: %s", source)
	}
	path := filepath.Join(store.Directory, source+".json")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("snapshot is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	value, err := io.ReadAll(io.LimitReader(file, 2*1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(value) > 2*1024*1024 || !json.Valid(value) {
		return nil, fmt.Errorf("snapshot is invalid or too large")
	}
	return value, nil
}

func validSource(source string) bool {
	return source == "capacity" || source == "inventory" || source == "health"
}

func atomicFile(path string, content []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), ".snapshot-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}
