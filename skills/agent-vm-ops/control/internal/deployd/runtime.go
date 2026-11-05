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
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/observe"
)

type HostRuntime struct {
	Config Config

	mu                  sync.Mutex
	metrics             map[string]map[string]map[string]string
	sources             map[string]SourceObservation
	stagedProcess       map[string]processIdentity
	quiesced            map[string]quiescenceEvidence
	legacyAuthorityLock *native.AuthorityLock
}

type processIdentity struct {
	PID       int64
	StartTime string
}

type quiescenceEvidence struct {
	Process       processIdentity
	StopRequested bool
}

const deploydDrainSession = "release-agent-vm-deployd"

func NewHostRuntime(config Config) *HostRuntime {
	return &HostRuntime{Config: config, metrics: map[string]map[string]map[string]string{}, sources: map[string]SourceObservation{}, stagedProcess: map[string]processIdentity{}, quiesced: map[string]quiescenceEvidence{}}
}

func (runtime *HostRuntime) Observe(ctx context.Context) (RuntimeSnapshot, error) {
	now := time.Now().UTC()
	active, activeErr := runtime.activeSlot()
	if activeErr != nil {
		active = "unknown"
	}
	snapshot := RuntimeSnapshot{ObservedAt: now, FreshUntil: now.Add(2 * runtime.Config.ObserveInterval()), ActiveSlot: active, ControllerSlot: "none", Consistent: activeErr == nil, Slots: map[string]SlotObservation{}, Sources: map[string]SourceObservation{}}
	sourceTimes := map[string]time.Time{}
	setSource := func(name string, observedAt time.Time, up bool, cause error) {
		observation := SourceObservation{Up: up}
		if up {
			observation.ObservedAt = observedAt
			if observation.ObservedAt.IsZero() {
				observation.ObservedAt = now
			}
			observation.FreshUntil = observation.ObservedAt.Add(runtime.sourceFreshness())
			runtime.rememberSource(name, observation)
		} else {
			previous := runtime.source(name)
			observation.ObservedAt = previous.ObservedAt
			observation.FreshUntil = previous.FreshUntil
			if cause != nil {
				observation.Error = "observation unavailable"
			}
		}
		snapshot.Sources[name] = observation
	}
	if activeErr != nil {
		setSource("active_link", now, false, activeErr)
	} else {
		setSource("active_link", now, true, nil)
	}
	slotContractsUp := true
	systemdObservationUp := true
	nativeObservationUp := true
	inventoryObservationUp := true
	inventoryComplete := true
	for _, slot := range []string{"blue", "green"} {
		observation := SlotObservation{Slot: slot, ObservedUnix: now.Unix()}
		body, status, requestErr := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodGet, "/internal/v1/health", nil)
		if requestErr != nil || (status != http.StatusOK && status != http.StatusServiceUnavailable) || !healthEnvelopeValid(body) {
			if requestErr != nil && runtime.systemdSlotStopped(ctx, slot) {
				runtime.mu.Lock()
				evidence := runtime.quiesced[slot]
				runtime.mu.Unlock()
				if quiescenceEvidenceProvesStopped(evidence) {
					observation.CountersKnown = true
				}
				if slot == active {
					slotContractsUp = false
					snapshot.Consistent = false
				}
				observation.MetricFamilies = runtime.slotMetrics(slot)
				snapshot.Slots[slot] = observation
				continue
			}
			if requestErr == nil {
				requestErr = fmt.Errorf("slot %s health returned HTTP %d or an invalid health envelope", slot, status)
			}
			systemdObservationUp = false
			slotContractsUp = false
			snapshot.Consistent = false
			runtime.invalidateMetrics(slot, "health")
			observation.MetricFamilies = runtime.slotMetrics(slot)
			snapshot.Slots[slot] = observation
			continue
		}
		processReady, processReadyOK := body["process_ready"].(bool)
		if !processReadyOK {
			runtime.mu.Lock()
			runtime.quiesced[slot] = quiescenceEvidence{}
			runtime.mu.Unlock()
			slotContractsUp = false
			snapshot.Consistent = false
			observation.MetricFamilies = runtime.slotMetrics(slot)
			snapshot.Slots[slot] = observation
			continue
		}
		observation.ProcessUp = status == http.StatusOK || status == http.StatusServiceUnavailable
		observation.ProcessReady = processReady
		observation.Ready, _ = body["ready"].(bool)
		observation.ControllerOwned, _ = body["controller_owned"].(bool)
		observation.TrafficAccepting, _ = body["traffic_accepting"].(bool)
		observation.ReleaseID, _ = body["release_id"].(string)
		observation.SourceRevision, _ = body["source_revision"].(string)
		process, processOK := processIdentityFromHealth(body)
		systemdErr := runtime.systemdProcessMatches(ctx, slot, process)
		if !processOK || systemdErr != nil {
			runtime.mu.Lock()
			runtime.quiesced[slot] = quiescenceEvidence{}
			runtime.mu.Unlock()
			systemdObservationUp = false
			slotContractsUp = false
			snapshot.Consistent = false
			observation.ProcessUp = false
			observation.ProcessReady = false
			observation.Ready = false
			observation.ControllerOwned = false
			observation.TrafficAccepting = false
			runtime.invalidateMetrics(slot, "health")
			observation.MetricFamilies = runtime.slotMetrics(slot)
			snapshot.Slots[slot] = observation
			continue
		}
		runtime.mu.Lock()
		if evidence := runtime.quiesced[slot]; evidence.Process.PID > 0 && !sameProcess(evidence.Process, process) {
			runtime.quiesced[slot] = quiescenceEvidence{}
		}
		runtime.mu.Unlock()
		metrics, metricsAt, metricsErr := runtime.observeSource(ctx, slot, "capacity", "admin-metrics")
		if metricsErr == nil {
			inFlight, inFlightOK := integerValue(metrics["in_flight_operations"])
			pending, pendingOK := arrayLengthValue(metrics["pending_operations"])
			stateEpoch, epochOK := metrics["observation_epoch"].(string)
			if !inFlightOK || !pendingOK || !epochOK || stateEpoch == "" {
				nativeObservationUp = false
				runtime.invalidateMetrics(slot, "capacity")
				snapshot.Consistent = false
			} else {
				runtime.rememberMetrics(slot, "capacity", observe.MetricsForSnapshot("capacity", metrics))
				observation.CountersKnown = true
				observation.InFlightRequests = inFlight
				observation.PendingOperations = pending
				observation.NativeWorkers = nativeWorkerCount(metrics["pending_operations"])
				observation.StateEpoch = stateEpoch
				sourceTimes["native_observation"] = minTime(sourceTimes["native_observation"], metricsAt)
			}
		} else {
			nativeObservationUp = false
			runtime.invalidateMetrics(slot, "capacity")
			snapshot.Consistent = false
		}
		inventory, inventoryAt, inventoryErr := runtime.observeSource(ctx, slot, "inventory", "admin-status")
		if inventoryErr != nil {
			inventoryObservationUp = false
			runtime.invalidateMetrics(slot, "inventory")
			snapshot.Consistent = false
		} else {
			inventoryConsistent := inventoryObservationCoherent(inventory)
			if !inventoryConsistent {
				snapshot.Consistent = false
			}
			if inventoryConsistent {
				if !inventoryObservationConsistent(inventory) {
					inventoryComplete = false
				}
				runtime.rememberMetrics(slot, "inventory", observe.MetricsForSnapshot("inventory", inventory))
				sourceTimes["inventory_observation"] = minTime(sourceTimes["inventory_observation"], inventoryAt)
			} else {
				inventoryObservationUp = false
				runtime.invalidateMetrics(slot, "inventory")
			}
		}
		doctor, doctorAt, doctorErr := runtime.observeSource(ctx, slot, "health", "doctor")
		if doctorErr != nil {
			nativeObservationUp = false
			runtime.invalidateMetrics(slot, "health")
			snapshot.Consistent = false
		} else {
			doctorValid := true
			doctorEpoch, epochOK := doctor["state_epoch"].(string)
			if !epochOK || doctorEpoch == "" || (observation.StateEpoch != "" && doctorEpoch != observation.StateEpoch) {
				doctorValid = false
				nativeObservationUp = false
				snapshot.Consistent = false
				runtime.invalidateMetrics(slot, "health")
			} else if observation.StateEpoch == "" {
				observation.StateEpoch = doctorEpoch
			}
			health := cloneMap(doctor)
			health["up"] = true
			if doctorValid {
				runtime.rememberMetrics(slot, "health", observe.MetricsForSnapshot("health", health))
			}
			sourceTimes["native_observation"] = minTime(sourceTimes["native_observation"], doctorAt)
		}
		drainStatus, _, drainErr := runtime.control(ctx, runtime.socket(slot), map[string]any{"protocol": 4, "action": "drain-status"})
		if drainErr != nil {
			nativeObservationUp = false
			snapshot.Consistent = false
		} else if result, ok := drainStatus["result"].(map[string]any); ok {
			draining, drainingOK := result["draining"].(bool)
			if !drainingOK {
				nativeObservationUp = false
				snapshot.Consistent = false
			} else {
				observation.Draining = draining
			}
			capabilities := stringSlice(result["state_capabilities"])
			if len(capabilities) == 0 {
				nativeObservationUp = false
				snapshot.Consistent = false
			} else {
				observation.StateCapabilities = capabilities
			}
		} else {
			nativeObservationUp = false
			snapshot.Consistent = false
		}
		observation.MetricFamilies = runtime.slotMetrics(slot)
		snapshot.Slots[slot] = observation
	}
	endActive, endActiveErr := runtime.activeSlot()
	if !activeLinkStable(active, activeErr, endActive, endActiveErr) {
		snapshot.Consistent = false
		if endActiveErr != nil {
			snapshot.ActiveSlot = "unknown"
			setSource("active_link", now, false, endActiveErr)
		} else {
			snapshot.ActiveSlot = endActive
			setSource("active_link", now, false, errors.New("active link changed during observation"))
		}
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
		setSource("slot_contracts", now, false, errors.New("one or more slot health contracts could not be observed"))
	} else {
		setSource("slot_contracts", now, true, nil)
	}
	if !systemdObservationUp {
		setSource("systemd", now, false, errors.New("one or more systemd slot identities could not be observed"))
	} else {
		setSource("systemd", now, true, nil)
	}
	if !nativeObservationUp {
		setSource("native_observation", sourceTimes["native_observation"], false, errors.New("one or more native observation lanes failed"))
	} else {
		setSource("native_observation", sourceTimes["native_observation"], true, nil)
	}
	if !inventoryObservationUp {
		setSource("inventory_observation", sourceTimes["inventory_observation"], false, errors.New("one or more inventory observation lanes failed"))
	} else {
		setSource("inventory_observation", sourceTimes["inventory_observation"], true, nil)
	}
	snapshot.FreshUntil = composedFreshUntil(snapshot.Sources, now)
	if !inventoryComplete {
		snapshot.ControllerRecoveryOnly = snapshot.Consistent
		snapshot.Consistent = false
	}
	return snapshot, nil
}

func inventoryObservationCoherent(inventory map[string]any) bool {
	_, partialOK := inventory["partial"].(bool)
	consistent, consistentOK := inventory["observation_consistent"].(bool)
	return partialOK && consistentOK && consistent
}

func inventoryObservationConsistent(inventory map[string]any) bool {
	partial, partialOK := inventory["partial"].(bool)
	consistent, consistentOK := inventory["observation_consistent"].(bool)
	return partialOK && !partial && consistentOK && consistent
}

func (runtime *HostRuntime) sourceFreshness() time.Duration {
	interval := runtime.Config.ObserveInterval()
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return 2 * interval
}

func (runtime *HostRuntime) source(name string) SourceObservation {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return runtime.sources[name]
}

func (runtime *HostRuntime) rememberSource(name string, observation SourceObservation) {
	runtime.mu.Lock()
	runtime.sources[name] = observation
	runtime.mu.Unlock()
}

func composedFreshUntil(sources map[string]SourceObservation, now time.Time) time.Time {
	var result time.Time
	for _, source := range sources {
		if source.FreshUntil.IsZero() {
			return now
		}
		if result.IsZero() || source.FreshUntil.Before(result) {
			result = source.FreshUntil
		}
	}
	if result.IsZero() {
		return now
	}
	return result
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
	previousProcess, previousProcessOK := runtime.readProcessIdentity(ctx, slot)
	if err := runtime.stageBundle(slot, bundle); err != nil {
		return err
	}
	if err := runtime.systemctl(ctx, "restart", "agent-vm-native@"+slot+".service"); err != nil {
		return runtime.restoreAfterStageFailure(ctx, slot, previousProcess, previousProcessOK, err)
	}
	currentProcess, err := runtime.waitSlotIdentity(ctx, slot, previousProcess, previousProcessOK)
	if err != nil {
		return runtime.restoreAfterStageFailure(ctx, slot, previousProcess, previousProcessOK, err)
	}
	if err := runtime.ExitDrain(ctx, slot); err != nil {
		return runtime.restoreAfterStageFailure(ctx, slot, previousProcess, previousProcessOK, err)
	}
	if err := runtime.discardStagedBackup(slot); err != nil {
		return &UncertainError{Cause: fmt.Errorf("stage completed but previous slot backup could not be removed: %w", err)}
	}
	runtime.mu.Lock()
	runtime.stagedProcess[slot] = currentProcess
	runtime.mu.Unlock()
	return nil
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
	manifest, err := extractAndVerifyBundle(bundle, temporary)
	if err != nil {
		return err
	}
	if err := runtime.ensureSlotSocketLink(temporary, slot); err != nil {
		return err
	}
	staged := filepath.Join(runtime.Config.ReleaseRoot, "slots", slot)
	if err := runtime.verifySlotSocketLink(staged, slot); err != nil {
		return err
	}
	if err := compareReleaseTrees(temporary, staged); err != nil {
		return fmt.Errorf("staged slot %s does not match the verified bundle: %w", slot, err)
	}
	releaseID, err := manifestString(manifest, "release_id")
	if err != nil {
		return err
	}
	sourceRevision, err := manifestString(manifest, "source_revision")
	if err != nil {
		return err
	}
	body, status, err := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodGet, "/internal/v1/health", nil)
	if err != nil || (status != http.StatusOK && status != http.StatusServiceUnavailable) || !healthEnvelopeValid(body) {
		if err != nil {
			return fmt.Errorf("verify slot %s health: %w", slot, err)
		}
		return fmt.Errorf("verify slot %s health returned HTTP %d or an invalid health envelope", slot, status)
	}
	if body["release_id"] != releaseID || body["source_revision"] != sourceRevision {
		return fmt.Errorf("slot %s process identity does not match the verified release manifest", slot)
	}
	if process, ok := processIdentityFromHealth(body); !ok || !processMatchesStaged(runtime, slot, process) {
		return fmt.Errorf("slot %s process identity was not verified after restart", slot)
	}
	return nil
}

func (runtime *HostRuntime) VerifySlotIdentity(_ context.Context, slot, releaseID, sourceRevision string, capabilities []string) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	if releaseID == "" || sourceRevision == "" {
		return errors.New("slot identity is incomplete")
	}
	root := filepath.Join(runtime.Config.ReleaseRoot, "slots", slot)
	if err := runtime.verifySlotSocketLink(root, slot); err != nil {
		return err
	}
	metadata, err := releaseManifest.LoadMetadata(filepath.Join(root, "release-manifest.json"))
	if err != nil {
		return fmt.Errorf("verify slot %s release manifest: %w", slot, err)
	}
	if metadata.ReleaseID != releaseID || metadata.SourceRevision != sourceRevision {
		return fmt.Errorf("slot %s release identity does not match the verified rollback target", slot)
	}
	if err := releaseManifest.ValidateStateCapabilities(metadata, capabilities); err != nil {
		return fmt.Errorf("verify slot %s state compatibility: %w", slot, err)
	}
	return nil
}

func manifestString(manifest map[string]any, key string) (string, error) {
	value, ok := manifest[key].(string)
	if !ok || value == "" {
		return "", fmt.Errorf("release manifest %s is missing or invalid", key)
	}
	return value, nil
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
	if err := runtime.ensureSlotSocketLink(temporary, slot); err != nil {
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
		if err := os.RemoveAll(backup); err != nil {
			return fmt.Errorf("remove previous slot backup: %w", err)
		}
		if err := os.Rename(target, backup); err != nil {
			return fmt.Errorf("move previous slot stage: %w", err)
		}
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
	if err := syncDirectory(root); err != nil {
		if restoreErr := restoreStagedSlot(root, slot); restoreErr != nil {
			return &UncertainError{Cause: fmt.Errorf("sync staged slot: %w; restore previous slot: %v", err, restoreErr)}
		}
		return fmt.Errorf("sync staged slot: %w", err)
	}
	return nil
}

func restoreStagedSlot(root, slot string) error {
	target := filepath.Join(root, slot)
	backup := filepath.Join(root, "."+slot+".previous-stage")
	if _, err := os.Lstat(backup); err != nil {
		return fmt.Errorf("previous stage backup is unavailable: %w", err)
	}
	if err := os.RemoveAll(target); err != nil {
		return fmt.Errorf("remove failed staged slot: %w", err)
	}
	if err := os.Rename(backup, target); err != nil {
		return fmt.Errorf("restore previous staged slot: %w", err)
	}
	return syncDirectory(root)
}

func (runtime *HostRuntime) restoreAfterStageFailure(ctx context.Context, slot string, previousProcess processIdentity, previousProcessOK bool, cause error) error {
	if err := runtime.systemctl(ctx, "stop", "agent-vm-native@"+slot+".service"); err != nil {
		return &UncertainError{Cause: fmt.Errorf("stage failed and slot could not be stopped: %w; original error: %v", err, cause)}
	}
	if !runtime.systemdSlotStopped(ctx, slot) {
		return &UncertainError{Cause: fmt.Errorf("stage failed but slot stop was not verified; original error: %v", cause)}
	}
	root := filepath.Join(runtime.Config.ReleaseRoot, "slots")
	if err := restoreStagedSlot(root, slot); err != nil {
		return &UncertainError{Cause: fmt.Errorf("stage failed and previous slot could not be restored: %w; original error: %v", err, cause)}
	}
	if err := runtime.systemctl(ctx, "restart", "agent-vm-native@"+slot+".service"); err != nil {
		return &UncertainError{Cause: fmt.Errorf("stage failed and restored slot could not be restarted: %w; original error: %v", err, cause)}
	}
	if _, err := runtime.waitSlotIdentity(ctx, slot, previousProcess, previousProcessOK); err != nil {
		return &UncertainError{Cause: fmt.Errorf("stage failed and restored slot did not satisfy its process contract: %w; original error: %v", err, cause)}
	}
	return fmt.Errorf("stage failed and previous slot was restored: %w", cause)
}

func (runtime *HostRuntime) discardStagedBackup(slot string) error {
	root := filepath.Join(runtime.Config.ReleaseRoot, "slots")
	backup := filepath.Join(root, "."+slot+".previous-stage")
	if err := os.RemoveAll(backup); err != nil {
		return err
	}
	return syncDirectory(root)
}

func (runtime *HostRuntime) ensureSlotSocketLink(root, slot string) error {
	link := filepath.Join(root, "app.sock")
	if _, err := os.Lstat(link); err == nil {
		return fmt.Errorf("staged slot already contains app.sock")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Symlink(runtime.socket(slot), link); err != nil {
		return fmt.Errorf("create %s app.sock link: %w", slot, err)
	}
	// MkdirTemp uses 0700 even when the daemon's group is the proxy read group.
	// The proxy must traverse the published slot to reach this socket link.
	if err := os.Chmod(root, 0o750); err != nil {
		return fmt.Errorf("prepare %s proxy socket traversal: %w", slot, err)
	}
	return nil
}

func (runtime *HostRuntime) verifySlotSocketLink(root, slot string) error {
	info, err := os.Lstat(root)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o750 {
		return fmt.Errorf("slot %s directory must allow only owner and proxy read-group traversal", slot)
	}
	target, err := os.Readlink(filepath.Join(root, "app.sock"))
	if err != nil {
		return fmt.Errorf("slot %s app.sock link is unavailable: %w", slot, err)
	}
	if target != runtime.socket(slot) {
		return fmt.Errorf("slot %s app.sock link points to an unexpected socket", slot)
	}
	return nil
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
	if format, ok := manifest["format"].(json.Number); !ok || !supportedBundleManifestFormat(format) {
		return nil, errors.New("bundle manifest format is unsupported")
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

func supportedBundleManifestFormat(format json.Number) bool {
	value, err := format.Int64()
	if err != nil {
		return false
	}
	return value == releaseManifest.Format || value == releaseManifest.LegacyProductFormat || value == releaseManifest.ProductFormat
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
			if path == filepath.Join(root, "app.sock") {
				return nil
			}
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
	if status != http.StatusOK || !responseOK(body) {
		return fmt.Errorf("slot %s rejected controller lease with HTTP %d", slot, status)
	}
	valid, ok := body["valid"].(bool)
	if !ok || !valid {
		return fmt.Errorf("slot %s did not confirm controller lease", slot)
	}
	return nil
}

func (runtime *HostRuntime) RevokeController(ctx context.Context, slot string, term uint64) error {
	if slot == "" || slot == "none" {
		return nil
	}
	body, status, err := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodPost, "/internal/v1/controller-role", map[string]any{"action": "revoke", "lease": map[string]any{"slot": slot, "term": term}})
	if err != nil {
		return err
	}
	if status != http.StatusOK || !responseOK(body) {
		return fmt.Errorf("slot %s rejected controller revoke with HTTP %d", slot, status)
	}
	return nil
}

func (runtime *HostRuntime) FenceController(ctx context.Context, slot string) error {
	if slot == "" || slot == "none" {
		return nil
	}
	if runtime.systemdSlotStopped(ctx, slot) {
		return nil
	}
	body, status, err := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodPost, "/internal/v1/controller-role", map[string]any{"action": "fence"})
	if err == nil && status == http.StatusOK && responseOK(body) {
		if valid, ok := body["valid"].(bool); ok && !valid {
			return nil
		}
		err = errors.New("slot did not confirm hard controller fencing")
	} else if err == nil {
		err = fmt.Errorf("slot %s rejected hard controller fencing with HTTP %d", slot, status)
	}
	if stopErr := runtime.Stop(ctx, slot); stopErr != nil {
		return fmt.Errorf("hard fence slot %s: %w; stop fallback: %v", slot, err, stopErr)
	}
	return nil
}

func (runtime *HostRuntime) EnableTraffic(ctx context.Context, slot string) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	body, status, err := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodPost, "/internal/v1/traffic-role", map[string]any{"action": "enable"})
	if err != nil {
		return err
	}
	if status != http.StatusOK || !responseOK(body) {
		return fmt.Errorf("slot %s rejected traffic role with HTTP %d", slot, status)
	}
	if accepting, ok := body["traffic_accepting"].(bool); !ok || !accepting {
		return fmt.Errorf("slot %s did not confirm traffic role", slot)
	}
	return nil
}

func (runtime *HostRuntime) DisableTraffic(ctx context.Context, slot string) error {
	if slot == "" || slot == "none" {
		return nil
	}
	body, status, err := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodPost, "/internal/v1/traffic-role", map[string]any{"action": "disable"})
	if err != nil {
		if runtime.systemdSlotStopped(ctx, slot) {
			return nil
		}
		return err
	}
	if status != http.StatusOK || !responseOK(body) {
		return fmt.Errorf("slot %s rejected traffic disable with HTTP %d", slot, status)
	}
	if accepting, ok := body["traffic_accepting"].(bool); !ok || accepting {
		return fmt.Errorf("slot %s did not confirm traffic was disabled", slot)
	}
	return nil
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
	statusBody, status, err := runtime.control(ctx, runtime.socket(slot), map[string]any{"protocol": 4, "action": "drain-enter", "session_id": deploydDrainSession, "operation_id": "deployd-drain-" + strconv.FormatInt(time.Now().UnixNano(), 10), "generation": int64(0)})
	if err != nil {
		return err
	}
	if status != http.StatusOK || !responseOK(statusBody) {
		return fmt.Errorf("slot %s rejected drain with HTTP %d", slot, status)
	}
	return nil
}

func (runtime *HostRuntime) ExitDrain(ctx context.Context, slot string) error {
	if slot == "" || slot == "none" {
		return nil
	}
	_, status, err := runtime.control(ctx, runtime.socket(slot), map[string]any{"protocol": 4, "action": "drain-exit", "session_id": deploydDrainSession, "operation_id": "deployd-drain-exit-" + strconv.FormatInt(time.Now().UnixNano(), 10), "generation": int64(0), "preserve_traffic": true})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("slot %s rejected drain exit with HTTP %d", slot, status)
	}
	runtime.mu.Lock()
	runtime.quiesced[slot] = quiescenceEvidence{}
	runtime.mu.Unlock()
	return nil
}

func (runtime *HostRuntime) Quiesce(ctx context.Context, slot string) error {
	if slot == "" || slot == "none" {
		return nil
	}
	runtime.mu.Lock()
	runtime.quiesced[slot] = quiescenceEvidence{}
	runtime.mu.Unlock()
	status, statusCode, err := runtime.control(ctx, runtime.socket(slot), map[string]any{"protocol": 4, "action": "drain-status"})
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("slot %s drain status returned HTTP %d", slot, statusCode)
	}
	result, ok := status["result"].(map[string]any)
	if !ok {
		return errors.New("old slot drain status result is missing")
	}
	draining, ok := result["draining"].(bool)
	if !ok || !draining {
		return errors.New("old slot is not fenced for drain")
	}
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		metrics, metricsCode, err := runtime.control(ctx, runtime.socket(slot), map[string]any{"protocol": 4, "action": "admin-metrics"})
		if err != nil {
			return err
		}
		if metricsCode != http.StatusOK {
			return fmt.Errorf("slot %s metrics returned HTTP %d", slot, metricsCode)
		}
		value, ok := metrics["result"].(map[string]any)
		if !ok {
			return errors.New("old slot metrics result is missing")
		}
		pending, pendingOK := arrayLengthValue(value["pending_operations"])
		inFlight, inFlightOK := integerValue(value["in_flight_operations"])
		unknown, unknownOK := arrayLengthValue(value["historical_unknown_operations"])
		if !pendingOK || !inFlightOK || !unknownOK {
			return errors.New("old slot quiescence counters are incomplete")
		}
		if pending == 0 && inFlight == 0 && unknown == 0 {
			identity, identityOK := runtime.readProcessIdentity(ctx, slot)
			if !identityOK {
				return errors.New("old slot process identity is unavailable after quiescence")
			}
			if err := runtime.systemdProcessMatches(ctx, slot, identity); err != nil {
				return fmt.Errorf("old slot process identity is not verified after quiescence: %w", err)
			}
			runtime.mu.Lock()
			runtime.quiesced[slot] = quiescenceEvidence{Process: identity}
			runtime.mu.Unlock()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("old slot did not quiesce before the bounded deadline")
		case <-ticker.C:
		}
	}
}

func (runtime *HostRuntime) Stop(ctx context.Context, slot string) error {
	if slot == "" || slot == "none" {
		return nil
	}
	runtime.mu.Lock()
	evidence := runtime.quiesced[slot]
	runtime.mu.Unlock()
	if evidence.Process.PID > 0 {
		current, ok := runtime.readProcessIdentity(ctx, slot)
		if !ok || !sameProcess(current, evidence.Process) {
			runtime.mu.Lock()
			runtime.quiesced[slot] = quiescenceEvidence{}
			runtime.mu.Unlock()
			return fmt.Errorf("slot %s process identity changed before stop", slot)
		}
		if err := runtime.systemdProcessMatches(ctx, slot, current); err != nil {
			runtime.mu.Lock()
			runtime.quiesced[slot] = quiescenceEvidence{}
			runtime.mu.Unlock()
			return fmt.Errorf("slot %s process identity is not verified before stop: %w", slot, err)
		}
	}
	if err := runtime.systemctl(ctx, "stop", "agent-vm-native@"+slot+".service"); err != nil {
		runtime.mu.Lock()
		runtime.quiesced[slot] = quiescenceEvidence{}
		runtime.mu.Unlock()
		return err
	}
	if evidence.Process.PID > 0 {
		runtime.mu.Lock()
		currentEvidence := runtime.quiesced[slot]
		if sameProcess(currentEvidence.Process, evidence.Process) {
			currentEvidence.StopRequested = true
			runtime.quiesced[slot] = currentEvidence
		}
		runtime.mu.Unlock()
	}
	return nil
}

func quiescenceEvidenceProvesStopped(evidence quiescenceEvidence) bool {
	return evidence.StopRequested && evidence.Process.PID > 0 && evidence.Process.StartTime != ""
}

func (runtime *HostRuntime) Rollback(ctx context.Context, target, previous string, lease controllerrole.Lease) error {
	if target != "" && target != "none" {
		if runtime.systemdSlotStopped(ctx, target) {
			if err := runtime.systemctl(ctx, "start", "agent-vm-native@"+target+".service"); err != nil {
				return fmt.Errorf("restart stopped rollback target: %w", err)
			}
			if err := runtime.waitSlot(ctx, target); err != nil {
				return fmt.Errorf("wait for restarted rollback target: %w", err)
			}
		}
		if err := runtime.DisableTraffic(ctx, target); err != nil {
			return err
		}
		if err := runtime.RevokeController(ctx, target, lease.Term); err != nil {
			if stopErr := runtime.Stop(ctx, target); stopErr != nil {
				return fmt.Errorf("revoke target controller: %w; stop target after revoke failure: %v", err, stopErr)
			}
		} else {
			if err := runtime.Drain(ctx, target); err != nil {
				return err
			}
			if err := runtime.Quiesce(ctx, target); err != nil {
				return err
			}
			if err := runtime.Stop(ctx, target); err != nil {
				return fmt.Errorf("stop target after revoke: %w", err)
			}
		}
	}
	if previous != "" && previous != "none" {
		// Restart even when systemd still reports the previous process as active:
		// controller fencing can leave a resident process alive but not ready, and
		// startup reconciliation is the only supported path back to admission.
		if err := runtime.systemctl(ctx, "restart", "agent-vm-native@"+previous+".service"); err != nil {
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
	if err := runtime.ExitDrain(ctx, previous); err != nil {
		return fmt.Errorf("exit rollback drain on slot %s: %w", previous, err)
	}
	return nil
}

func (runtime *HostRuntime) waitSlot(ctx context.Context, slot string) error {
	_, err := runtime.waitSlotIdentity(ctx, slot, processIdentity{}, false)
	return err
}

func (runtime *HostRuntime) readProcessIdentity(ctx context.Context, slot string) (processIdentity, bool) {
	body, status, err := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodGet, "/internal/v1/health", nil)
	if err != nil || (status != http.StatusOK && status != http.StatusServiceUnavailable) || !healthEnvelopeValid(body) {
		return processIdentity{}, false
	}
	identity, ok := processIdentityFromHealth(body)
	return identity, ok
}

func (runtime *HostRuntime) waitSlotIdentity(ctx context.Context, slot string, previous processIdentity, requireChanged bool) (processIdentity, error) {
	if err := validSlot(slot); err != nil {
		return processIdentity{}, err
	}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		body, status, err := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodGet, "/internal/v1/health", nil)
		if err == nil && (status == http.StatusOK || status == http.StatusServiceUnavailable) && healthEnvelopeValid(body) {
			if identity, ok := processIdentityFromHealth(body); ok && (!requireChanged || !sameProcess(identity, previous)) {
				if err := runtime.systemdProcessMatches(ctx, slot, identity); err == nil {
					return identity, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return processIdentity{}, ctx.Err()
		case <-deadline.C:
			if requireChanged {
				return processIdentity{}, fmt.Errorf("slot %s process identity did not change after restart", slot)
			}
			return processIdentity{}, fmt.Errorf("slot %s did not expose its health contract after start", slot)
		case <-ticker.C:
		}
	}
}

func processIdentityFromHealth(body map[string]any) (processIdentity, bool) {
	pid, pidOK := integerValue(body["process_id"])
	startTime, startOK := body["process_start_time"].(string)
	if !pidOK || pid <= 0 || !startOK || startTime == "" {
		return processIdentity{}, false
	}
	return processIdentity{PID: pid, StartTime: startTime}, true
}

func sameProcess(left, right processIdentity) bool {
	if left.PID <= 0 || right.PID <= 0 || left.StartTime == "" || right.StartTime == "" || left.PID != right.PID {
		return false
	}
	return left.StartTime == right.StartTime
}

func processMatchesStaged(runtime *HostRuntime, slot string, identity processIdentity) bool {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	expected, ok := runtime.stagedProcess[slot]
	return ok && sameProcess(identity, expected)
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
		return "", errors.New("active link is missing")
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
	body, status, err := runtime.slotRequest(ctx, socket, http.MethodPost, "/internal/v1/control", request)
	if err != nil {
		return nil, status, err
	}
	if status != http.StatusOK {
		return nil, status, fmt.Errorf("slot control action returned HTTP %d", status)
	}
	if !responseOK(body) {
		return nil, status, errors.New("slot control response is not successful")
	}
	protocolValue, ok := integerValue(body["protocol"])
	if !ok || protocolValue != 4 {
		return nil, status, errors.New("slot control response has an invalid protocol")
	}
	if action, ok := request["action"].(string); !ok || body["action"] != action {
		return nil, status, errors.New("slot control response action does not match the request")
	}
	if _, ok := body["result"].(map[string]any); !ok {
		return nil, status, errors.New("slot control response result is missing")
	}
	return body, status, nil
}

func (runtime *HostRuntime) observeSource(ctx context.Context, slot, source, action string) (map[string]any, time.Time, error) {
	if _, _, err := runtime.control(ctx, runtime.socket(slot), map[string]any{"protocol": 4, "action": action}); err != nil {
		return nil, time.Time{}, err
	}
	body, status, err := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodGet, "/internal/v1/snapshots/"+source, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	if status != http.StatusOK || !responseOK(body) {
		return nil, time.Time{}, fmt.Errorf("%s snapshot returned HTTP %d or an invalid envelope", source, status)
	}
	protocolValue, protocolOK := integerValue(body["protocol"])
	if !protocolOK || protocolValue != 4 {
		return nil, time.Time{}, fmt.Errorf("%s snapshot has an invalid protocol", source)
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		return nil, time.Time{}, fmt.Errorf("%s observation result is missing", source)
	}
	if err := validateSourceResult(action, result); err != nil {
		return nil, time.Time{}, fmt.Errorf("%s observation is invalid: %w", source, err)
	}
	observedAt, ok := validObservedAt(result["observed_at"], time.Now().UTC())
	if !ok {
		return nil, time.Time{}, fmt.Errorf("%s observation timestamp is missing", source)
	}
	return result, observedAt, nil
}

func validateSourceResult(action string, result map[string]any) error {
	up, ok := result["up"].(bool)
	if !ok {
		return errors.New("up is missing or invalid")
	}
	if !up && action != "admin-status" {
		return errors.New("up is false")
	}
	if _, ok := parseObservedAt(result["observed_at"]); !ok {
		return errors.New("observed_at is missing or invalid")
	}
	switch action {
	case "admin-metrics":
		observationConsistent, ok := result["observation_consistent"].(bool)
		if !ok || !observationConsistent {
			return errors.New("observation_consistent is missing or false")
		}
		observationEpoch, ok := result["observation_epoch"].(string)
		if !ok || observationEpoch == "" {
			return errors.New("observation_epoch is missing or invalid")
		}
		if _, ok := result["pending_operations"].([]any); !ok {
			return errors.New("pending_operations is missing or invalid")
		}
		if _, ok := result["historical_unknown_operations"].([]any); !ok {
			return errors.New("historical_unknown_operations is missing or invalid")
		}
		if _, ok := integerValue(result["in_flight_operations"]); !ok {
			return errors.New("in_flight_operations is missing or invalid")
		}
		doctor, ok := result["doctor"].(map[string]any)
		if !ok {
			return errors.New("doctor is missing or invalid")
		}
		if _, ok := doctor["issues"].([]any); !ok {
			return errors.New("doctor.issues is missing or invalid")
		}
	case "admin-status":
		if _, ok := result["tasks"].([]any); !ok {
			return errors.New("tasks is missing or invalid")
		}
		partial, partialOK := result["partial"].(bool)
		if !partialOK {
			return errors.New("partial is missing or invalid")
		}
		if !up && !partial {
			return errors.New("admin-status up is false without a partial observation")
		}
		if _, ok := result["observation_consistent"].(bool); !ok {
			return errors.New("observation_consistent is missing or invalid")
		}
		for _, field := range []string{"unknown", "probed", "verified", "timed_out"} {
			if _, ok := integerValue(result[field]); !ok {
				return fmt.Errorf("%s is missing or invalid", field)
			}
		}
	case "doctor":
		stateEpoch, ok := result["state_epoch"].(string)
		if !ok || stateEpoch == "" {
			return errors.New("state_epoch is missing or invalid")
		}
		if _, ok := result["issues"].([]any); !ok {
			return errors.New("issues is missing or invalid")
		}
	default:
		return errors.New("unsupported observation action")
	}
	return nil
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
	if result == nil {
		return nil, response.StatusCode, errors.New("slot response must be a JSON object")
	}
	return result, response.StatusCode, nil
}

func (runtime *HostRuntime) systemctl(ctx context.Context, action, unit string) error {
	if action != "start" && action != "stop" && action != "restart" {
		return errors.New("unsupported systemd action")
	}
	if !strings.HasPrefix(unit, "agent-vm-native@") || !strings.HasSuffix(unit, ".service") {
		return errors.New("unsupported systemd unit")
	}
	command := exec.CommandContext(ctx, "systemctl", action, unit)
	output, err := command.CombinedOutput()
	if err != nil {
		_ = output
		return fmt.Errorf("systemctl %s %s failed: %w", action, unit, err)
	}
	return nil
}

func (runtime *HostRuntime) systemdProcessMatches(ctx context.Context, slot string, identity processIdentity) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	unit := "agent-vm-native@" + slot + ".service"
	command := exec.CommandContext(ctx, "systemctl", "show", unit,
		"--property=ActiveState", "--property=SubState", "--property=MainPID",
		"--property=ControlGroup", "--property=FragmentPath", "--property=ExecStart")
	output, err := command.Output()
	if err != nil {
		return errors.New("systemd unit status is unavailable")
	}
	properties := parseSystemdProperties(string(output))
	if properties["ActiveState"] != "active" || properties["SubState"] != "running" {
		return errors.New("systemd slot unit is not active")
	}
	mainPID, err := strconv.ParseInt(properties["MainPID"], 10, 64)
	if err != nil || mainPID <= 0 || mainPID != identity.PID {
		return errors.New("systemd main process does not match slot health")
	}
	if !processStartTokenMatches(identity) {
		return errors.New("systemd process start token does not match slot health")
	}
	if filepath.Clean(properties["FragmentPath"]) != "/etc/systemd/system/agent-vm-native@.service" {
		return errors.New("systemd unit template is not the native slot unit")
	}
	if !strings.HasSuffix(properties["ControlGroup"], "/agent-vm-native@"+slot+".service") {
		return errors.New("systemd cgroup does not match the native slot unit")
	}
	expectedBinary := filepath.Join(runtime.Config.ReleaseRoot, "slots", slot, "bin", "agent-vm-control")
	expectedConfig := filepath.Join("/etc/agent-vm-native/slots", slot, "config.json")
	expectedManifest := filepath.Join(runtime.Config.ReleaseRoot, "slots", slot, "release-manifest.json")
	expectedCommand := expectedBinary + " serve --config " + expectedConfig + " --manifest " + expectedManifest
	if !systemdExecStartMatches(properties["ExecStart"], expectedCommand) {
		return errors.New("systemd executable does not match the native slot")
	}
	return nil
}

func processStartTokenMatches(identity processIdentity) bool {
	if identity.PID <= 0 || identity.StartTime == "" {
		return false
	}
	return controllerrole.ProcessStartToken(int(identity.PID)) == identity.StartTime
}

func activeLinkStable(start string, startErr error, end string, endErr error) bool {
	return startErr == nil && endErr == nil && start != "" && start == end
}

func (runtime *HostRuntime) systemdSlotStopped(ctx context.Context, slot string) bool {
	if validSlot(slot) != nil {
		return false
	}
	unit := "agent-vm-native@" + slot + ".service"
	command := exec.CommandContext(ctx, "systemctl", "show", unit,
		"--property=LoadState", "--property=ActiveState", "--property=SubState",
		"--property=MainPID", "--property=FragmentPath")
	output, err := command.Output()
	if err != nil {
		return false
	}
	return systemdSlotStoppedProperties(parseSystemdProperties(string(output)))
}

func systemdSlotStoppedProperties(properties map[string]string) bool {
	return properties["LoadState"] == "loaded" &&
		properties["ActiveState"] == "inactive" &&
		properties["SubState"] == "dead" &&
		properties["MainPID"] == "0" &&
		filepath.Clean(properties["FragmentPath"]) == "/etc/systemd/system/agent-vm-native@.service"
}

func systemdExecStartMatches(value, expected string) bool {
	const marker = "argv[]="
	index := strings.Index(value, marker)
	if index < 0 {
		return false
	}
	command := value[index+len(marker):]
	if end := strings.Index(command, " ;"); end >= 0 {
		command = command[:end]
	}
	return command == expected
}

func parseSystemdProperties(output string) map[string]string {
	properties := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && key != "" {
			properties[key] = value
		}
	}
	return properties
}

func integer(value any) int64 {
	parsed, _ := integerValue(value)
	return parsed
}

func integerValue(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), typed == float64(int64(typed))
	case int64:
		return typed, true
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	case int:
		return int64(typed), true
	default:
		return 0, false
	}
}

func arrayLength(value any) int64 {
	parsed, _ := arrayLengthValue(value)
	return parsed
}

func arrayLengthValue(value any) (int64, bool) {
	items, ok := value.([]any)
	if !ok {
		return 0, false
	}
	return int64(len(items)), true
}

func responseOK(body map[string]any) bool {
	value, ok := body["ok"].(bool)
	return ok && value
}

func healthEnvelopeValid(body map[string]any) bool {
	envelopeOK, ok := body["ok"].(bool)
	protocolValue, protocolOK := integerValue(body["protocol"])
	processReady, processReadyOK := body["process_ready"].(bool)
	_, readyOK := body["ready"].(bool)
	_, controllerOK := body["controller_owned"].(bool)
	_, trafficOK := body["traffic_accepting"].(bool)
	_, processIDOK := integerValue(body["process_id"])
	processStartTime, processStartOK := body["process_start_time"].(string)
	return ok && envelopeOK && protocolOK && protocolValue == 4 && processReadyOK && readyOK && controllerOK && trafficOK && processIDOK && processStartOK && processStartTime != "" && processReady
}

func parseObservedAt(value any) (time.Time, bool) {
	text, ok := value.(string)
	if !ok {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	return parsed, err == nil
}

func validObservedAt(value any, now time.Time) (time.Time, bool) {
	parsed, ok := parseObservedAt(value)
	if !ok || parsed.After(now) {
		return time.Time{}, false
	}
	return parsed, true
}

func stringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok || text == "" {
			return nil
		}
		result = append(result, text)
	}
	return result
}

func minTime(left, right time.Time) time.Time {
	if left.IsZero() || (!right.IsZero() && right.Before(left)) {
		return right
	}
	return left
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
