package deployd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	releaseManifest "github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/manifest"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/observe"
)

type HostRuntime struct {
	Config Config

	mu      sync.Mutex
	metrics map[string]map[string]map[string]string
}

func NewHostRuntime(config Config) *HostRuntime {
	return &HostRuntime{Config: config, metrics: map[string]map[string]map[string]string{}}
}

func (runtime *HostRuntime) Observe(ctx context.Context) (RuntimeSnapshot, error) {
	now := time.Now().UTC()
	active, activeErr := runtime.activeSlot()
	if activeErr != nil {
		active = "unknown"
	}
	snapshot := RuntimeSnapshot{ObservedAt: now, FreshUntil: now.Add(2 * runtime.Config.ObserveInterval()), ActiveSlot: active, ControllerSlot: "none", Consistent: activeErr == nil, Slots: map[string]SlotObservation{}, Sources: map[string]SourceObservation{}}
	setSource := func(name string, up bool, cause error) {
		observation := SourceObservation{Up: up, ObservedAt: now}
		if cause != nil {
			observation.Error = cause.Error()
		}
		snapshot.Sources[name] = observation
	}
	if activeErr != nil {
		setSource("active_link", false, activeErr)
	} else {
		setSource("active_link", true, nil)
	}
	slotContractsUp := true
	nativeObservationUp := true
	inventoryObservationUp := true
	for _, slot := range []string{"blue", "green"} {
		observation := SlotObservation{Slot: slot, TrafficAccepting: slot == active, ObservedUnix: now.Unix()}
		body, status, requestErr := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodGet, "/internal/v1/health", nil)
		if requestErr != nil {
			if slot == active || active == "none" || active == "unknown" {
				slotContractsUp = false
				snapshot.Consistent = false
			}
			observation.MetricFamilies = runtime.slotMetrics(slot)
			snapshot.Slots[slot] = observation
			continue
		}
		processReady, processReadyOK := body["process_ready"].(bool)
		if !processReadyOK {
			if slot == active || active == "none" || active == "unknown" {
				slotContractsUp = false
				snapshot.Consistent = false
			}
			observation.MetricFamilies = runtime.slotMetrics(slot)
			snapshot.Slots[slot] = observation
			continue
		}
		observation.ProcessUp = status == http.StatusOK || status == http.StatusServiceUnavailable
		observation.ProcessReady = processReady
		observation.Ready, _ = body["ready"].(bool)
		observation.ControllerOwned, _ = body["controller_owned"].(bool)
		if metrics, metricsStatus, metricsErr := runtime.control(ctx, runtime.socket(slot), map[string]any{"protocol": 4, "action": "admin-metrics"}); metricsErr == nil && metricsStatus < 500 {
			result, _ := metrics["result"].(map[string]any)
			runtime.rememberMetrics(slot, "capacity", observe.MetricsForSnapshot("capacity", result))
			observation.InFlightRequests = integer(result["in_flight_operations"])
			observation.PendingOperations = arrayLength(result["pending_operations"])
			observation.NativeWorkers = nativeWorkerCount(result["pending_operations"])
			if doctor, ok := result["doctor"].(map[string]any); ok {
				if epoch, ok := doctor["state_epoch"].(string); ok {
					observation.StateEpoch = epoch
				}
			}
		} else {
			nativeObservationUp = false
			runtime.invalidateMetrics(slot, "capacity")
			snapshot.Consistent = false
		}
		if inventory, inventoryStatus, inventoryErr := runtime.control(ctx, runtime.socket(slot), map[string]any{"protocol": 4, "action": "admin-status"}); inventoryErr != nil || inventoryStatus >= 500 {
			inventoryObservationUp = false
			runtime.invalidateMetrics(slot, "inventory")
			snapshot.Consistent = false
		} else if result, ok := inventory["result"].(map[string]any); ok {
			runtime.rememberMetrics(slot, "inventory", observe.MetricsForSnapshot("inventory", result))
		}
		if doctor, doctorStatus, doctorErr := runtime.control(ctx, runtime.socket(slot), map[string]any{"protocol": 4, "action": "doctor"}); doctorErr != nil || doctorStatus >= 500 {
			nativeObservationUp = false
			runtime.invalidateMetrics(slot, "health")
			snapshot.Consistent = false
		} else if result, ok := doctor["result"].(map[string]any); ok {
			health := cloneMap(result)
			health["up"] = true
			runtime.rememberMetrics(slot, "health", observe.MetricsForSnapshot("health", health))
		}
		observation.MetricFamilies = runtime.slotMetrics(slot)
		snapshot.Slots[slot] = observation
	}
	controllerCount := 0
	for slot, observation := range snapshot.Slots {
		if observation.ControllerOwned {
			controllerCount++
			snapshot.ControllerSlot = slot
		}
	}
	if controllerCount > 1 {
		snapshot.ControllerSlot = "unknown"
		snapshot.Consistent = false
	}
	if !slotContractsUp {
		setSource("slot_contracts", false, errors.New("one or more slot health contracts could not be observed"))
	} else {
		setSource("slot_contracts", true, nil)
	}
	if !nativeObservationUp {
		setSource("native_observation", false, errors.New("one or more native observation lanes failed"))
	} else {
		setSource("native_observation", true, nil)
	}
	if !inventoryObservationUp {
		setSource("inventory_observation", false, errors.New("one or more inventory observation lanes failed"))
	} else {
		setSource("inventory_observation", true, nil)
	}
	return snapshot, nil
}

func (runtime *HostRuntime) rememberMetrics(slot, source string, families map[string]string) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.metrics[slot] == nil {
		runtime.metrics[slot] = map[string]map[string]string{}
	}
	runtime.metrics[slot][source] = cloneFamilies(families)
}

func (runtime *HostRuntime) invalidateMetrics(slot, source string) {
	runtime.rememberMetrics(slot, source, nil)
}

func (runtime *HostRuntime) slotMetrics(slot string) map[string]string {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	result := map[string]string{}
	for _, families := range runtime.metrics[slot] {
		for name, value := range families {
			result[name] = value
		}
	}
	return result
}

func cloneFamilies(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source)+1)
	for key, value := range source {
		result[key] = value
	}
	return result
}

func (runtime *HostRuntime) Stage(ctx context.Context, slot, bundle string) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	active, err := runtime.activeSlot()
	if err != nil {
		return fmt.Errorf("read active slot before staging: %w", err)
	}
	if active == slot {
		return fmt.Errorf("cannot stage over active slot %s", slot)
	}
	if err := runtime.stageBundle(slot, bundle); err != nil {
		return err
	}
	return runtime.systemctl(ctx, "start", "agent-vm-native@"+slot+".service")
}

func (runtime *HostRuntime) Verify(ctx context.Context, slot, bundle string) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	temporary, err := os.MkdirTemp("", "agent-vm-deployd-verify-staged-")
	if err != nil {
		return fmt.Errorf("create staged bundle verification directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	if _, err := extractAndVerifyBundle(bundle, temporary); err != nil {
		return err
	}
	staged := filepath.Join(runtime.Config.ReleaseRoot, "slots", slot)
	if err := compareReleaseTrees(temporary, staged); err != nil {
		return fmt.Errorf("staged slot %s does not match the verified bundle: %w", slot, err)
	}
	return nil
}

const (
	maxBundleBytes = 128 * 1024 * 1024
	maxBundleFile  = 64 * 1024 * 1024
)

func inspectBundle(bundle string) (map[string]any, []byte, []byte, os.FileMode, error) {
	temporary, err := os.MkdirTemp("", "agent-vm-deployd-verify-")
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("create bundle verification directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	manifest, err := extractAndVerifyBundle(bundle, temporary)
	if err != nil {
		return nil, nil, nil, 0, err
	}
	manifestBytes, err := os.ReadFile(filepath.Join(temporary, "release-manifest.json"))
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("read verified bundle manifest: %w", err)
	}
	controlPath := filepath.Join(temporary, "bin", "agent-vm-control")
	controlInfo, err := os.Stat(controlPath)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("stat verified control binary: %w", err)
	}
	controlBytes, err := os.ReadFile(controlPath)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("read verified control binary: %w", err)
	}
	return manifest, manifestBytes, controlBytes, controlInfo.Mode().Perm(), nil
}

func (runtime *HostRuntime) stageBundle(slot, bundle string) error {
	root := filepath.Join(runtime.Config.ReleaseRoot, "slots")
	if err := os.MkdirAll(root, 0o750); err != nil {
		return fmt.Errorf("prepare slot root: %w", err)
	}
	temporary, err := os.MkdirTemp(root, ".stage-"+slot+"-")
	if err != nil {
		return fmt.Errorf("prepare slot staging directory: %w", err)
	}
	keepTemporary := true
	defer func() {
		if keepTemporary {
			_ = os.RemoveAll(temporary)
		}
	}()
	if _, err := extractAndVerifyBundle(bundle, temporary); err != nil {
		return err
	}
	if err := syncDirectory(temporary); err != nil {
		return err
	}
	target := filepath.Join(root, slot)
	backup := filepath.Join(root, "."+slot+".previous-stage")
	if info, statErr := os.Lstat(target); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("slot target is not a directory: %s", slot)
		}
		_ = os.RemoveAll(backup)
		if err := os.Rename(target, backup); err != nil {
			return fmt.Errorf("move previous slot stage: %w", err)
		}
		defer func() { _ = os.RemoveAll(backup) }()
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	if err := os.Rename(temporary, target); err != nil {
		if _, backupErr := os.Lstat(backup); backupErr == nil {
			_ = os.Rename(backup, target)
		}
		return fmt.Errorf("activate staged slot files: %w", err)
	}
	keepTemporary = false
	return syncDirectory(root)
}

func extractAndVerifyBundle(bundle, destination string) (map[string]any, error) {
	if !filepath.IsAbs(bundle) {
		return nil, errors.New("bundle must be an absolute path")
	}
	info, err := os.Stat(bundle)
	if err != nil {
		return nil, fmt.Errorf("bundle is not available: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("bundle must be a regular file")
	}
	if info.Size() > maxBundleBytes {
		return nil, errors.New("release bundle exceeds the size limit")
	}
	if err := os.MkdirAll(destination, 0o750); err != nil {
		return nil, fmt.Errorf("prepare bundle verification directory: %w", err)
	}
	file, err := os.Open(bundle)
	if err != nil {
		return nil, fmt.Errorf("open release bundle: %w", err)
	}
	defer file.Close()
	reader := tar.NewReader(io.LimitReader(file, maxBundleBytes+1))
	members := make(map[string]struct{})
	var total int64
	for {
		header, headerErr := reader.Next()
		if errors.Is(headerErr, io.EOF) {
			break
		}
		if headerErr != nil {
			return nil, fmt.Errorf("release bundle is not a valid tar archive: %w", headerErr)
		}
		name := filepath.ToSlash(header.Name)
		if !safeBundlePath(name) {
			return nil, fmt.Errorf("release bundle contains an unsafe path: %q", header.Name)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("release bundle contains a non-regular member: %q", header.Name)
		}
		if header.Size < 0 || header.Size > maxBundleFile || total > maxBundleBytes-header.Size {
			return nil, errors.New("release bundle exceeds the size limit")
		}
		if _, exists := members[name]; exists {
			return nil, fmt.Errorf("release bundle contains duplicate member: %q", header.Name)
		}
		members[name] = struct{}{}
		target := filepath.Join(destination, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return nil, fmt.Errorf("prepare release bundle member %q: %w", name, err)
		}
		mode := os.FileMode(header.Mode) & 0o777
		output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return nil, fmt.Errorf("create release bundle member %q: %w", name, err)
		}
		written, copyErr := io.CopyN(output, reader, header.Size)
		if copyErr == nil && written != header.Size {
			copyErr = io.ErrUnexpectedEOF
		}
		if syncErr := output.Sync(); copyErr == nil {
			copyErr = syncErr
		}
		if closeErr := output.Close(); copyErr == nil {
			copyErr = closeErr
		}
		if copyErr != nil {
			return nil, fmt.Errorf("write release bundle member %q: %w", name, copyErr)
		}
		if err := os.Chmod(target, mode); err != nil {
			return nil, fmt.Errorf("set release bundle member mode %q: %w", name, err)
		}
		total += header.Size
	}
	manifestPath := filepath.Join(destination, "release-manifest.json")
	manifest, err := readBundleManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	if format, ok := manifest["format"].(json.Number); !ok || format.String() != "2" {
		return nil, errors.New("bundle manifest format must be 2")
	}
	expected := map[string]struct{}{"release-manifest.json": {}, "client-release.json": {}}
	artifacts, ok := manifest["artifacts"].([]any)
	if !ok {
		return nil, errors.New("bundle manifest artifacts are invalid")
	}
	for _, raw := range artifacts {
		artifact, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("bundle manifest artifact is invalid")
		}
		path, ok := artifact["path"].(string)
		if !ok || !safeBundlePath(path) {
			return nil, errors.New("bundle manifest artifact path is invalid")
		}
		expected[path] = struct{}{}
	}
	if _, ok := members["release-manifest.json"]; !ok {
		return nil, errors.New("release bundle is missing release-manifest.json")
	}
	if _, ok := members["client-release.json"]; !ok {
		return nil, errors.New("release bundle is missing client-release.json")
	}
	for path := range expected {
		if _, ok := members[path]; !ok {
			return nil, fmt.Errorf("release bundle is missing %s", path)
		}
	}
	for path := range members {
		if _, ok := expected[path]; !ok {
			return nil, fmt.Errorf("release bundle contains an undeclared member: %s", path)
		}
	}
	if _, err := releaseManifest.LoadMetadata(manifestPath); err != nil {
		return nil, fmt.Errorf("release manifest verification failed: %w", err)
	}
	if err := syncDirectory(destination); err != nil {
		return nil, fmt.Errorf("persist verified release bundle: %w", err)
	}
	return manifest, nil
}

func compareReleaseTrees(expectedRoot, actualRoot string) error {
	expected, err := readReleaseTree(expectedRoot)
	if err != nil {
		return fmt.Errorf("read verified bundle: %w", err)
	}
	actual, err := readReleaseTree(actualRoot)
	if err != nil {
		return fmt.Errorf("read staged slot: %w", err)
	}
	if len(expected) != len(actual) {
		return fmt.Errorf("file count differs: bundle=%d staged=%d", len(expected), len(actual))
	}
	for path, expectedFile := range expected {
		actualFile, ok := actual[path]
		if !ok {
			return fmt.Errorf("staged slot is missing %s", path)
		}
		if expectedFile.mode != actualFile.mode {
			return fmt.Errorf("mode differs for %s", path)
		}
		if !bytes.Equal(expectedFile.content, actualFile.content) {
			return fmt.Errorf("content differs for %s", path)
		}
	}
	return nil
}

type releaseFile struct {
	content []byte
	mode    os.FileMode
}

func readReleaseTree(root string) (map[string]releaseFile, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("release root is not a directory")
	}
	files := map[string]releaseFile{}
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("release tree contains a symlink: %s", path)
		}
		if info.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("release tree contains a non-regular file: %s", path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(relative)] = releaseFile{content: content, mode: info.Mode().Perm()}
		return nil
	})
	return files, err
}

func readBundleManifest(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read release bundle manifest: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var manifest map[string]any
	if err := decoder.Decode(&manifest); err != nil || len(manifest) == 0 {
		if err == nil {
			err = errors.New("manifest is empty")
		}
		return nil, fmt.Errorf("bundle manifest is invalid: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("bundle manifest contains trailing JSON")
	}
	return manifest, nil
}

func safeBundlePath(value string) bool {
	if value == "" || filepath.IsAbs(value) || strings.Contains(value, "\\") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (runtime *HostRuntime) GrantController(ctx context.Context, slot string, lease controllerrole.Lease) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	body, status, err := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodPost, "/internal/v1/controller-role", map[string]any{"action": "grant", "lease": lease})
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("slot %s rejected controller lease with HTTP %d", slot, status)
	}
	valid, ok := body["valid"].(bool)
	if !ok || !valid {
		return fmt.Errorf("slot %s did not confirm controller lease", slot)
	}
	return nil
}

func (runtime *HostRuntime) RevokeController(ctx context.Context, slot string) error {
	if slot == "" || slot == "none" {
		return nil
	}
	_, status, err := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodPost, "/internal/v1/controller-role", map[string]any{"action": "revoke", "lease": map[string]any{"slot": slot}})
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("slot %s rejected controller revoke with HTTP %d", slot, status)
	}
	return nil
}

func (runtime *HostRuntime) EnableTraffic(ctx context.Context, slot string) error {
	_, err := runtime.activeSlot()
	if err != nil {
		return err
	}
	return validSlot(slot)
}

func (runtime *HostRuntime) SwitchActive(ctx context.Context, slot string) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	target := filepath.Join(runtime.Config.ReleaseRoot, "slots", slot)
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("slot directory is not available: %s", target)
	}
	parent := filepath.Dir(runtime.Config.ActivePath)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return err
	}
	if info, err := os.Lstat(runtime.Config.ActivePath); err == nil && info.Mode()&os.ModeSymlink == 0 {
		return errors.New("active path is not a symlink")
	}
	temporary := filepath.Join(parent, ".active."+strconv.FormatInt(time.Now().UnixNano(), 10))
	if err := os.Symlink(target, temporary); err != nil {
		return err
	}
	defer os.Remove(temporary)
	if err := os.Rename(temporary, runtime.Config.ActivePath); err != nil {
		return &UncertainError{Cause: err}
	}
	directory, err := os.Open(parent)
	if err != nil {
		return &UncertainError{Cause: err}
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return &UncertainError{Cause: err}
	}
	return nil
}

func (runtime *HostRuntime) Drain(ctx context.Context, slot string) error {
	if slot == "" || slot == "none" {
		return nil
	}
	_, status, err := runtime.control(ctx, runtime.socket(slot), map[string]any{"protocol": 4, "action": "drain-enter", "operation_id": "deployd-drain-" + strconv.FormatInt(time.Now().UnixNano(), 10), "generation": int64(0)})
	if err != nil {
		return err
	}
	if status >= 300 {
		return fmt.Errorf("slot %s rejected drain with HTTP %d", slot, status)
	}
	return nil
}

func (runtime *HostRuntime) Quiesce(ctx context.Context, slot string) error {
	if slot == "" || slot == "none" {
		return nil
	}
	status, statusCode, err := runtime.control(ctx, runtime.socket(slot), map[string]any{"protocol": 4, "action": "drain-status"})
	if err != nil {
		return err
	}
	if statusCode >= 300 {
		return fmt.Errorf("slot %s drain status returned HTTP %d", slot, statusCode)
	}
	result, _ := status["result"].(map[string]any)
	if draining, _ := result["draining"].(bool); !draining {
		return errors.New("old slot is not fenced for drain")
	}
	metrics, metricsCode, err := runtime.control(ctx, runtime.socket(slot), map[string]any{"protocol": 4, "action": "admin-metrics"})
	if err != nil {
		return err
	}
	if metricsCode >= 300 {
		return fmt.Errorf("slot %s metrics returned HTTP %d", slot, metricsCode)
	}
	value, _ := metrics["result"].(map[string]any)
	if arrayLength(value["pending_operations"]) != 0 || integer(value["in_flight_operations"]) != 0 || arrayLength(value["historical_unknown_operations"]) != 0 {
		return errors.New("old slot still has pending or in-flight operations")
	}
	return nil
}

func (runtime *HostRuntime) Stop(ctx context.Context, slot string) error {
	if slot == "" || slot == "none" {
		return nil
	}
	return runtime.systemctl(ctx, "stop", "agent-vm-native@"+slot+".service")
}

func (runtime *HostRuntime) Rollback(ctx context.Context, target, previous string, lease controllerrole.Lease) error {
	if err := runtime.RevokeController(ctx, target); err != nil {
		return err
	}
	if previous != "" && previous != "none" {
		if err := runtime.systemctl(ctx, "start", "agent-vm-native@"+previous+".service"); err != nil {
			return err
		}
		if err := runtime.waitSlot(ctx, previous); err != nil {
			return err
		}
		if err := runtime.GrantController(ctx, previous, lease); err != nil {
			return err
		}
	}
	if previous == "" || previous == "none" {
		return nil
	}
	if err := runtime.SwitchActive(ctx, previous); err != nil {
		return err
	}
	if target != "" && target != "none" && target != previous {
		if err := runtime.Drain(ctx, target); err != nil {
			return err
		}
		if err := runtime.Quiesce(ctx, target); err != nil {
			return err
		}
		if err := runtime.Stop(ctx, target); err != nil {
			return err
		}
	}
	return nil
}

func (runtime *HostRuntime) waitSlot(ctx context.Context, slot string) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		body, status, err := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodGet, "/internal/v1/health", nil)
		if err == nil && status > 0 {
			if _, ok := body["process_ready"].(bool); ok {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("slot %s did not expose its health contract after start", slot)
		case <-ticker.C:
		}
	}
}

func (runtime *HostRuntime) socket(slot string) string {
	if slot == "blue" {
		return runtime.Config.BlueSocket
	}
	return runtime.Config.GreenSocket
}

func (runtime *HostRuntime) activeSlot() (string, error) {
	target, err := os.Readlink(runtime.Config.ActivePath)
	if errors.Is(err, os.ErrNotExist) {
		return "none", nil
	}
	if err != nil {
		return "", err
	}
	target = filepath.Clean(target)
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(runtime.Config.ActivePath), target)
	}
	for _, slot := range []string{"blue", "green"} {
		if target == filepath.Join(runtime.Config.ReleaseRoot, "slots", slot) {
			return slot, nil
		}
	}
	return "", errors.New("active path points outside the verified slots")
}

func (runtime *HostRuntime) control(ctx context.Context, socket string, request map[string]any) (map[string]any, int, error) {
	return runtime.slotRequest(ctx, socket, http.MethodPost, "/internal/v1/control", request)
}

func (runtime *HostRuntime) slotRequest(ctx context.Context, socket, method, path string, value any) (map[string]any, int, error) {
	var body io.Reader
	if value != nil {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, 0, err
		}
		body = bytes.NewReader(encoded)
	}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var dialer net.Dialer
		return dialer.DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, body)
	if err != nil {
		return nil, 0, err
	}
	if value != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := (&http.Client{Transport: transport, Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 2*1024*1024+1))
	if err != nil {
		return nil, response.StatusCode, err
	}
	if len(payload) > 2*1024*1024 {
		return nil, response.StatusCode, errors.New("slot response exceeds limit")
	}
	var result map[string]any
	if len(bytes.TrimSpace(payload)) != 0 {
		if err := json.Unmarshal(payload, &result); err != nil {
			return nil, response.StatusCode, err
		}
	}
	return result, response.StatusCode, nil
}

func (runtime *HostRuntime) systemctl(ctx context.Context, action, unit string) error {
	if action != "start" && action != "stop" {
		return errors.New("unsupported systemd action")
	}
	if !strings.HasPrefix(unit, "agent-vm-native@") || !strings.HasSuffix(unit, ".service") {
		return errors.New("unsupported systemd unit")
	}
	command := exec.CommandContext(ctx, "systemctl", action, unit)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s %s: %w: %s", action, unit, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func integer(value any) int64 {
	switch typed := value.(type) {
	case float64:
		return int64(typed)
	case int64:
		return typed
	case json.Number:
		parsed, _ := typed.Int64()
		return parsed
	default:
		return 0
	}
}

func arrayLength(value any) int64 {
	items, ok := value.([]any)
	if !ok {
		return 0
	}
	return int64(len(items))
}

func nativeWorkerCount(value any) int64 {
	items, ok := value.([]any)
	if !ok {
		return 0
	}
	var count int64
	for _, item := range items {
		operation, ok := item.(map[string]any)
		if !ok {
			continue
		}
		worker, ok := operation["worker"].(map[string]any)
		if !ok {
			continue
		}
		switch worker["state"] {
		case "starting", "running", "reconciling":
			count++
		}
	}
	return count
}
