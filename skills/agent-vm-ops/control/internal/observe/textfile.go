package observe

import (
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
	object, _ := value.(map[string]any)
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
		result["agent_vm_admission_ready"] = fmt.Sprintf("agent_vm_admission_ready %d", boolNumber(object["admission_ready"], false))
		result["agent_vm_admission_draining"] = fmt.Sprintf("agent_vm_admission_draining %d", boolNumber(object["draining"], false))
		result["agent_vm_observation_consistent"] = fmt.Sprintf("agent_vm_observation_consistent %d", boolNumber(object["observation_consistent"], false))
		if resources, ok := objectMap(object["resources"]); ok {
			addResourceMetric(result, "cpu", "cpu", resources, "vcpu", 1)
			addResourceMetric(result, "memory", "memory_mib", resources, "bytes", 1024*1024)
			addResourceMetric(result, "disk", "disk_gib", resources, "bytes", 1024*1024*1024)
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
	}
	if source == "health" {
		result["agent_vm_up"] = fmt.Sprintf("agent_vm_up %d", up)
		result["agent_vm_health_ready"] = fmt.Sprintf("agent_vm_health_ready %d", boolNumber(object["ready"], false))
	}
	return result
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
