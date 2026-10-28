package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/config"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/native"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/observe"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/protocol"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

func missingInstanceError() error {
	err := exec.Command("false").Run()
	return &native.CommandError{Result: native.Result{Code: 1, Stderr: []byte("instance not found")}, Cause: err}
}

type staticCPUObserver struct {
	observation observe.CPUObservation
	err         error
}

type staticMemoryObserver struct {
	observation observe.MemoryObservation
	err         error
}

type capacityGuest struct {
	*startupGuest
	capacity        native.StorageCapacity
	capacityFunc    func() native.StorageCapacity
	capacityErr     error
	probeErr        error
	capacityEntered chan struct{}
	capacityRelease <-chan struct{}
	probeHook       func()
	capacityMu      sync.Mutex
	capacityCalls   int
}

func (guest *capacityGuest) StoragePoolCapacity(context.Context) (native.StorageCapacity, error) {
	guest.capacityMu.Lock()
	guest.capacityCalls++
	guest.capacityMu.Unlock()
	if guest.capacityEntered != nil {
		guest.capacityEntered <- struct{}{}
	}
	if guest.capacityRelease != nil {
		<-guest.capacityRelease
	}
	if guest.capacityFunc != nil {
		return guest.capacityFunc(), guest.capacityErr
	}
	return guest.capacity, guest.capacityErr
}

func (guest *capacityGuest) Probe(context.Context) error {
	if guest.probeHook != nil {
		guest.probeHook()
	}
	return guest.probeErr
}

type startupGuest struct {
	initErr             error
	status              string
	statusErr           error
	statuses            []map[string]any
	statusCalls         int
	deleteCalls         int
	config              map[string]any
	configErr           error
	configSetErr        error
	deviceSetErr        error
	deviceSetFailDevice string
	initEntered         chan<- struct{}
	initRelease         <-chan struct{}
	stopErr             error
	deleteErr           error
	deviceSets          []string
}

func (guest *startupGuest) Init(_ context.Context, _, _ string, ownership map[string]string) error {
	if guest.initEntered != nil {
		guest.initEntered <- struct{}{}
	}
	if guest.initRelease != nil {
		<-guest.initRelease
	}
	if guest.config != nil {
		config := guest.config["config"].(map[string]any)
		for key, value := range ownership {
			config[key] = value
		}
	}
	return guest.initErr
}
func (guest *startupGuest) Probe(context.Context) error { return nil }
func (guest *startupGuest) ConfigSet(_ context.Context, _ string, key, value string) error {
	if guest.configSetErr != nil {
		return guest.configSetErr
	}
	if guest.config != nil {
		if config, ok := guest.config["config"].(map[string]any); ok {
			config[key] = value
		}
	}
	return nil
}

func (guest *startupGuest) DeviceSet(_ context.Context, _ string, device, key, value string) error {
	if guest.deviceSetErr != nil && (guest.deviceSetFailDevice == "" || guest.deviceSetFailDevice == device) {
		return guest.deviceSetErr
	}
	guest.deviceSets = append(guest.deviceSets, device+":"+key+"="+value)
	return nil
}
func (guest *startupGuest) DeviceAdd(context.Context, string, string, string, map[string]string) error {
	return nil
}
func (guest *startupGuest) DeviceRemove(context.Context, string, string) error { return nil }
func (guest *startupGuest) Start(context.Context, string) error                { return nil }
func (guest *startupGuest) Stop(context.Context, string) error                 { return guest.stopErr }
func (guest *startupGuest) Delete(context.Context, string) error {
	guest.deleteCalls++
	return guest.deleteErr
}
func (guest *startupGuest) Status(context.Context, string) (map[string]any, error) {
	if guest.statusErr != nil {
		return nil, guest.statusErr
	}
	index := guest.statusCalls
	guest.statusCalls++
	if len(guest.statuses) > 0 {
		if index >= len(guest.statuses) {
			index = len(guest.statuses) - 1
		}
		return guest.statuses[index], nil
	}
	return map[string]any{"status": guest.status}, nil
}
func (guest *startupGuest) Config(context.Context, string) (map[string]any, error) {
	if guest.configErr != nil {
		return nil, guest.configErr
	}
	return guest.config, nil
}
func (guest *startupGuest) Exec(context.Context, string, []string, []byte) (native.Result, error) {
	return native.Result{}, errors.New("startup guest exec is not used")
}

func (observer staticCPUObserver) Observe(context.Context) (observe.CPUObservation, error) {
	return observer.observation, observer.err
}

func (observer staticMemoryObserver) Observe(context.Context) (observe.MemoryObservation, error) {
	return observer.observation, observer.err
}

func TestLeaseJournalAndBuiltInCleanup(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.ImageFingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cfg.CacheSeconds = 60
	service := New(store, cfg, Options{Clock: func() time.Time { return now }})
	ctx := context.Background()
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "demo", "generation": int64(0), "operation_id": "acquire-demo"}
	first, err := service.Dispatch(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if first["outcome"] != "success" || first["result"].(map[string]any)["state"] != stateActive {
		t.Fatalf("acquire = %#v", first)
	}
	replay, err := service.Dispatch(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if replay["outcome"] != "replayed" {
		t.Fatalf("replay = %#v", replay)
	}
	status, err := service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "status", "session_id": "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if status["result"].(map[string]any)["effective_lease"] != true {
		t.Fatalf("status = %#v", status)
	}
	release, err := service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "release", "session_id": "demo", "generation": int64(1), "operation_id": "release-demo"})
	if err != nil {
		t.Fatal(err)
	}
	if release["result"].(map[string]any)["state"] != stateCached {
		t.Fatalf("release = %#v", release)
	}
	if err := store.UpdateTask(ctx, "avm-", map[string]any{}); err != nil {
		_ = err
	}
	if _, err := service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "drain-enter", "generation": int64(0), "operation_id": "drain"}); err != nil {
		t.Fatal(err)
	}
	_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "blocked", "generation": int64(0), "operation_id": "blocked"})
	if control := protocol.AsError(err); control.Code != "draining" {
		t.Fatalf("drain error = %#v", control)
	}
}

func TestAcquirePersistsPhysicalReservationWhilePreparing(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.VMIPStart = "10.0.0.2"
	cfg.VMIPEnd = "10.0.0.254"
	guest := &capacityGuest{
		startupGuest: &startupGuest{initEntered: entered, initRelease: release},
		capacity:     native.StorageCapacity{TotalBytes: 100 * 1024 * 1024 * 1024, FreeBytes: 90 * 1024 * 1024 * 1024},
	}
	service := New(store, cfg, Options{Guest: guest})
	service.MarkReady()
	result := make(chan error, 1)
	go func() {
		_, dispatchErr := service.Dispatch(context.Background(), protocol.Request{
			"protocol": protocol.Version, "action": "acquire", "session_id": "preparing-reservation", "generation": int64(0), "operation_id": "preparing-reservation-acquire",
		})
		result <- dispatchErr
	}()
	<-entered
	task, err := store.GetTask(context.Background(), map[string]any{"session_id": "preparing-reservation"})
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.State != statePreparing || task.ReservationBytes != cfg.CreateCostBytes {
		t.Fatalf("preparing task reservation = %#v", task)
	}
	close(release)
	<-result
}

func TestReleaseDrainExitClaimsLegacyOwnerlessFence(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.SetMeta(ctx, "draining", "1"); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	result, err := service.drain(ctx, false, protocol.Request{"session_id": "release-first-management-aware"})
	if err != nil {
		t.Fatal(err)
	}
	if result["draining"] != false {
		t.Fatalf("drain result = %#v", result)
	}
	if value, found, err := store.Meta(ctx, "draining"); err != nil || !found || value != "0" {
		t.Fatalf("draining metadata = %q found=%v err=%v", value, found, err)
	}
	if owner, found, err := store.Meta(ctx, "drain_owner"); err != nil || (found && owner != "") {
		t.Fatalf("drain owner = %q found=%v err=%v", owner, found, err)
	}
}

func TestDrainOwnershipCannotBeReplacedByAnotherCaller(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	service := New(store, config.Defaults(), Options{})
	if _, err := service.drain(ctx, true, protocol.Request{"session_id": "release-owner"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.drain(ctx, true, protocol.Request{"session_id": "release-other"}); protocol.AsError(err).Code != "operation_conflict" {
		t.Fatalf("different release owner error = %#v", err)
	}
	if _, err := service.drain(ctx, true, protocol.Request{"operation_id": "browser-retry"}); protocol.AsError(err).Code != "operation_conflict" {
		t.Fatalf("browser owner error = %#v", err)
	}
	if owner, found, err := store.Meta(ctx, "drain_owner"); err != nil || !found || owner != "release-owner" {
		t.Fatalf("drain owner = %q found=%v err=%v", owner, found, err)
	}
	if _, err := service.drain(ctx, false, protocol.Request{"session_id": "release-other"}); protocol.AsError(err).Code != "permission_denied" {
		t.Fatalf("wrong release exit error = %#v", err)
	}
	if result, err := service.drain(ctx, false, protocol.Request{"session_id": "release-owner"}); err != nil || result["draining"] != false {
		t.Fatalf("owner exit result=%#v err=%v", result, err)
	}
}

func TestReleaseMayTakeOverBrowserDrainBeforeActivation(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	service := New(store, config.Defaults(), Options{})
	if _, err := service.drain(ctx, true, protocol.Request{"operation_id": "browser-drain"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.drain(ctx, true, protocol.Request{"session_id": "release-handoff"}); err != nil {
		t.Fatal(err)
	}
	if owner, found, err := store.Meta(ctx, "drain_owner"); err != nil || !found || owner != "release-handoff" {
		t.Fatalf("drain owner = %q found=%v err=%v", owner, found, err)
	}
}

func TestCachedAcquireReusesLegacyDefaultAfterMigration(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.ImageFingerprint = strings.Repeat("a", 64)
	if err := store.SeedLegacyImage(ctx, cfg.ImageFingerprint); err != nil {
		t.Fatal(err)
	}
	service := New(store, cfg, Options{})
	req := func(action, op string, generation int64) protocol.Request {
		return protocol.Request{"protocol": protocol.Version, "action": action, "session_id": "legacy-cache", "generation": generation, "operation_id": op}
	}
	if _, err := service.Dispatch(ctx, req("acquire", "legacy-first", 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Dispatch(ctx, req("release", "legacy-release", 1)); err != nil {
		t.Fatal(err)
	}
	result, err := service.Dispatch(ctx, req("acquire", "legacy-resume", 1))
	if err != nil {
		t.Fatal(err)
	}
	if result["result"].(map[string]any)["state"] != stateActive {
		t.Fatalf("reacquire = %#v", result)
	}
}

func TestCachedAcquireRejectsRetiredPinnedImage(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	legacy := strings.Repeat("a", 64)
	pinned := strings.Repeat("b", 64)
	if err := store.SeedLegacyImage(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertManagedImage(ctx, state.ManagedImage{Fingerprint: pinned, Architecture: "x86_64", InstanceType: "container", SourceKind: "local_upload", State: "available", Integrity: "passed", Suitability: "passed"}); err != nil {
		t.Fatal(err)
	}
	lease := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if err := store.InsertTask(ctx, state.Task{TaskID: taskID("retired-cache"), SessionID: "retired-cache", Generation: 1, State: stateCached, InstanceName: "agent-vm-retired-cache", CreatedAt: "2026-10-27T00:00:00Z", UpdatedAt: "2026-10-27T00:00:00Z", ImageFingerprint: pinned, ResourcesJSON: `{"cpu":1,"memory_mib":128,"disk_gib":1}`, ResourceReservationJSON: `{"cpu":1,"memory_mib":128,"disk_gib":1}`, LeaseExpiresAt: &lease}); err != nil {
		t.Fatal(err)
	}
	if err := store.RetireManagedImage(ctx, pinned); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.ImageFingerprint = legacy
	service := New(store, cfg, Options{})
	_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "retired-cache", "generation": int64(1), "operation_id": "acquire-retired-cache"})
	control := protocol.AsError(err)
	if control == nil || control.Code != "cache_stale" {
		t.Fatalf("retired cache acquire error = %#v", err)
	}
}

func TestAbsentStatusMatchesClientContract(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	service := New(store, cfg, Options{})
	response, err := service.Dispatch(context.Background(), protocol.Request{
		"protocol":   protocol.Version,
		"action":     "status",
		"session_id": "new-session",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := response["result"].(map[string]any)
	if result["state"] != stateAbsent || result["generation"] != int64(0) ||
		result["effective_lease"] != false || result["native_status"] != stateAbsent ||
		result["guest_available"] != false {
		t.Fatalf("absent status = %#v", result)
	}
}

func TestWaitForGuestReadyPollsUntilIPv4IsVisible(t *testing.T) {
	guest := &startupGuest{statuses: []map[string]any{
		{"status": "Running", "network": map[string]any{"eth0": map[string]any{"addresses": []any{}}}},
		{"status": "Running", "network": map[string]any{"eth0": map[string]any{"addresses": []any{
			map[string]any{"family": "inet", "address": "10.250.19.20"},
		}}}},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ip, err := waitForGuestReady(ctx, guest, "agent-ready")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.250.19.20" || guest.statusCalls != 2 {
		t.Fatalf("ip=%q status_calls=%d", ip, guest.statusCalls)
	}
}

func TestConfigureNetworkMatchesManagedContainerContract(t *testing.T) {
	guest := &startupGuest{config: map[string]any{"expanded_devices": map[string]any{
		"eth0": map[string]any{
			"type": "nic", "network": "agentsbr0", "ipv4.address": "10.250.19.20",
			"security.mac_filtering": "true", "security.ipv4_filtering": "true",
			"security.ipv6_filtering": "true", "security.port_isolation": "true",
			"security.acls":                        "agent-vm-acl",
			"security.acls.default.ingress.action": "allow",
			"security.acls.default.egress.action":  "allow",
		},
	}}}
	cfg := config.Defaults()
	cfg.Network = "agentsbr0"
	cfg.NetworkACL = "agent-vm-acl"
	service := &Service{Config: cfg, Guest: guest}
	if err := service.configureNetwork(context.Background(), &state.Task{InstanceName: "agent-ready", VMIP: stringPointer("10.250.19.20")}); err != nil {
		t.Fatal(err)
	}
	if len(guest.deviceSets) != 8 || guest.deviceSets[0] != "eth0:ipv4.address=10.250.19.20" {
		t.Fatalf("device sets = %#v", guest.deviceSets)
	}
}

func TestAdminMetricsIncludesPendingOperations(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	service := New(store, cfg, Options{})
	response, err := service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version,
		"action":   "admin-metrics",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := response["result"].(map[string]any)
	pending, ok := result["pending_operations"].([]any)
	if !ok || len(pending) != 0 {
		t.Fatalf("pending operations = %#v", result["pending_operations"])
	}
}

func TestAdmissionRejectsWhenPhysicalStorageCannotBeMeasured(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	service := New(store, cfg, Options{Guest: &startupGuest{}, Resident: true})
	service.MarkReady()
	err = service.checkAdmission(context.Background(), map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}, "", true)
	if control := protocol.AsError(err); control == nil || control.Code != "resource_unverified" {
		t.Fatalf("unmeasured storage error = %#v", err)
	}
}

func TestAdmissionRejectsLowPhysicalStorageWithoutTouchingEffectiveLease(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	lease := now.Add(time.Hour).Format(time.RFC3339Nano)
	active := state.Task{
		TaskID: "active-task", SessionID: "active-session", Generation: 1, State: stateActive,
		InstanceName: "active-instance", LeaseExpiresAt: &lease, CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: "{}",
	}
	if err := store.InsertTask(ctx, active); err != nil {
		t.Fatal(err)
	}
	guest := &capacityGuest{
		startupGuest: &startupGuest{},
		capacity:     native.StorageCapacity{TotalBytes: 100 * 1024 * 1024 * 1024, FreeBytes: 13 * 1024 * 1024 * 1024},
	}
	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.DiskLowBytes = 10 * 1024 * 1024 * 1024
	cfg.DiskTargetBytes = 20 * 1024 * 1024 * 1024
	service := New(store, cfg, Options{Guest: guest, Resident: true, Clock: func() time.Time { return now }})
	service.MarkReady()
	err = service.checkAdmission(ctx, map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}, "", true)
	if control := protocol.AsError(err); control == nil || control.Code != "resource_insufficient" {
		t.Fatalf("low storage error = %#v", err)
	}
	stored, err := store.GetTask(ctx, map[string]any{"task_id": active.TaskID})
	if err != nil || stored == nil || stored.State != stateActive || stored.LeaseExpiresAt == nil || *stored.LeaseExpiresAt != lease {
		t.Fatalf("effective lease changed after rejection: task=%#v err=%v", stored, err)
	}
}

func TestAdmissionRejectsIncompleteDurableDiskReservation(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	task := state.Task{TaskID: "incomplete-reservation-task", SessionID: "incomplete-reservation-session", State: stateActive, InstanceName: "incomplete-reservation-instance", ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{"cpu":4,"memory_mib":8192}`}
	if err := store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	guest := &capacityGuest{startupGuest: &startupGuest{}, capacity: native.StorageCapacity{TotalBytes: 100 * 1024 * 1024 * 1024, FreeBytes: 90 * 1024 * 1024 * 1024}}
	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	service := New(store, cfg, Options{Guest: guest, Resident: true})
	service.MarkReady()
	_, err = service.diskAdmission(ctx, "")
	if control := protocol.AsError(err); control == nil || control.Code != "resource_unverified" {
		t.Fatalf("incomplete disk reservation error = %#v", err)
	}
}

func TestAdmissionRejectsMalformedLogicalResources(t *testing.T) {
	cases := []struct {
		name         string
		resources    string
		reservations string
	}{
		{name: "incomplete resources", resources: `{"cpu":2,"memory_mib":4096}`, reservations: `{}`},
		{name: "reservation does not mask incomplete resources", resources: `{"cpu":2,"memory_mib":4096}`, reservations: `{"cpu":4,"memory_mib":8192,"disk_gib":20}`},
		{name: "null resource", resources: `{"cpu":null,"memory_mib":4096,"disk_gib":20}`, reservations: `{}`},
		{name: "negative reservation", resources: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, reservations: `{"cpu":-1,"memory_mib":4096,"disk_gib":20}`},
		{name: "resource sum overflow", resources: `{"cpu":9223372036854775807,"memory_mib":4096,"disk_gib":20}`, reservations: `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			task := state.Task{TaskID: "malformed-logical-task", SessionID: "malformed-logical-session", State: stateActive, InstanceName: "malformed-logical-instance", ResourcesJSON: tc.resources, ResourceReservationJSON: tc.reservations}
			if err := store.InsertTask(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			service := New(store, config.Defaults(), Options{})
			err = service.checkAdmission(context.Background(), map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}, "", true)
			if control := protocol.AsError(err); control == nil || control.Code != "resource_unverified" {
				t.Fatalf("malformed logical resource error = %#v", err)
			}
		})
	}
}

func TestAdmissionCountsOriginalResourcesWhenResizeReservationIsPending(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.CPUBudget = 5
	cfg.CPUOvercommitRatio = json.Number("1")
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "pending-resize-task", SessionID: "pending-resize-session", State: stateActive,
		InstanceName: "pending-resize-instance", ResourcesJSON: `{"cpu":4,"memory_mib":4096,"disk_gib":20}`,
		ResourceReservationJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`,
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, cfg, Options{})
	err = service.checkAdmission(context.Background(), map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}, "", true)
	if control := protocol.AsError(err); control == nil || control.Code != "resource_insufficient" {
		t.Fatalf("pending resize admission error = %#v", err)
	}
}

func TestFailedDiskResizeRetainsPhysicalCostReservation(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	lease := now.Add(time.Hour).Format(time.RFC3339Nano)
	if err := store.InsertTask(ctx, state.Task{
		TaskID: "failed-disk-resize-task", SessionID: "failed-disk-resize-session", Generation: 1, State: stateActive,
		InstanceName: "failed-disk-resize-instance", LeaseExpiresAt: &lease, CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	const gib = int64(1024 * 1024 * 1024)
	cfg := config.Defaults()
	cfg.DiskLowBytes = 200 * gib
	cfg.DiskTargetBytes = 300 * gib
	cfg.ResizeCostBytes = 7 * gib
	guest := &capacityGuest{
		startupGuest: &startupGuest{deviceSetErr: errors.New("disk resize unavailable")},
		capacity:     native.StorageCapacity{TotalBytes: 1024 * gib, FreeBytes: 210 * gib},
	}
	service := New(store, cfg, Options{Guest: guest, Resident: true, Clock: func() time.Time { return now }})
	service.MarkReady()
	_, err = service.resize(ctx, protocol.Request{"session_id": "failed-disk-resize-session", "generation": int64(1), "disk_gib": int64(30)})
	if control := protocol.AsError(err); control == nil || control.Code != "resize_partial" {
		t.Fatalf("failed disk resize error = %#v", err)
	}
	task, err := store.GetTask(ctx, map[string]any{"task_id": "failed-disk-resize-task"})
	if err != nil {
		t.Fatal(err)
	}
	if task == nil {
		t.Fatalf("failed disk resize task is missing")
	}
	reservation, reservationErr := decodeResourceMap(task.ResourceReservationJSON, "test reservation", false)
	if reservationErr != nil || task.ReservationBytes != 7*gib || reservation["cpu"] != 2 || reservation["memory_mib"] != 4096 || reservation["disk_gib"] != 30 {
		t.Fatalf("failed disk resize reservation = %#v", task)
	}
	disk, err := service.diskAdmission(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	physical := disk["physical_admission"].(map[string]any)
	if physical["reserved_bytes"] != int64(7*gib) {
		t.Fatalf("failed disk resize physical reservation = %#v", physical)
	}
	if control := protocol.AsError(service.checkAdmission(ctx, map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}, "", true)); control == nil || control.Code != "resource_insufficient" {
		t.Fatalf("failed disk resize follow-up admission = %#v", control)
	}
	guest.configSetErr = errors.New("CPU resize unavailable")
	_, err = service.resize(ctx, protocol.Request{"session_id": "failed-disk-resize-session", "generation": int64(1), "cpu": int64(3)})
	if control := protocol.AsError(err); control == nil || control.Code != "resize_partial" {
		t.Fatalf("failed follow-up CPU resize error = %#v", err)
	}
	task, err = store.GetTask(ctx, map[string]any{"task_id": "failed-disk-resize-task"})
	if err != nil {
		t.Fatal(err)
	}
	reservation, reservationErr = decodeResourceMap(task.ResourceReservationJSON, "test reservation", false)
	if reservationErr != nil || task.ReservationBytes != 7*gib || reservation["cpu"] != 3 || reservation["memory_mib"] != 4096 || reservation["disk_gib"] != 30 {
		t.Fatalf("failed follow-up CPU resize reservation = %#v", task)
	}
	guest.configSetErr = nil
	guest.deviceSetErr = nil
	if _, err := service.resize(ctx, protocol.Request{"session_id": "failed-disk-resize-session", "generation": int64(1), "cpu": int64(4)}); err != nil {
		t.Fatalf("follow-up CPU resize = %#v", err)
	}
	task, err = store.GetTask(ctx, map[string]any{"task_id": "failed-disk-resize-task"})
	if err != nil {
		t.Fatal(err)
	}
	reservation, reservationErr = decodeResourceMap(task.ResourceReservationJSON, "test reservation", false)
	if reservationErr != nil || task.ReservationBytes != 7*gib || reservation["cpu"] != 4 || reservation["memory_mib"] != 4096 || reservation["disk_gib"] != 30 {
		t.Fatalf("follow-up CPU resize reservation = %#v", task)
	}
	if _, err := service.resize(ctx, protocol.Request{"session_id": "failed-disk-resize-session", "generation": int64(1), "disk_gib": int64(31)}); err != nil {
		t.Fatalf("retry disk resize = %#v", err)
	}
	task, err = store.GetTask(ctx, map[string]any{"task_id": "failed-disk-resize-task"})
	if err != nil {
		t.Fatal(err)
	}
	if task.ReservationBytes != 0 || task.ResourceReservationJSON != "{}" {
		t.Fatalf("successful retry disk resize reservation = %#v", task)
	}
}

func TestInvalidLiveResizeDoesNotReclaimCachedCapacity(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	lease := now.Add(time.Hour).Format(time.RFC3339Nano)
	if err := store.InsertTask(ctx, state.Task{
		TaskID: "invalid-live-task", SessionID: "invalid-live-session", Generation: 1, State: stateActive,
		InstanceName: "agent-invalid-live", LeaseExpiresAt: &lease, CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertTask(ctx, state.Task{
		TaskID: "invalid-live-cache", SessionID: "invalid-live-cache-session", Generation: 1, State: stateCached,
		InstanceName: "agent-invalid-live-cache", InactiveSince: stringPointer(nowString(now)), CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	const gib = int64(1024 * 1024 * 1024)
	cfg := config.Defaults()
	cfg.DiskLowBytes = 20 * gib
	cfg.DiskTargetBytes = 30 * gib
	guest := &capacityGuest{
		startupGuest: &startupGuest{
			status: "Stopped",
			config: map[string]any{
				"config":  map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "invalid-live-cache", "user.agent-vm.session": "invalid-live-cache-session", "user.agent-vm.generation": "1"},
				"devices": map[string]any{}, "expanded_devices": map[string]any{},
			},
		},
		capacity: native.StorageCapacity{TotalBytes: 100 * gib, FreeBytes: 10 * gib},
	}
	service := New(store, cfg, Options{Guest: guest, Resident: true, Clock: func() time.Time { return now }})
	_, err = service.resize(ctx, protocol.Request{"session_id": "invalid-live-session", "generation": int64(1), "live_memory": true, "memory_mib": int64(8192), "disk_gib": int64(30)})
	control := protocol.AsError(err)
	if control == nil || control.Code != "invalid_request" {
		t.Fatalf("invalid live resize error = %#v", control)
	}
	if guest.deleteCalls != 0 {
		t.Fatalf("invalid live resize reclaimed %d cached guests", guest.deleteCalls)
	}
	if task, getErr := store.GetTask(ctx, map[string]any{"task_id": "invalid-live-cache"}); getErr != nil || task == nil {
		t.Fatalf("invalid live resize cache = %#v err=%v", task, getErr)
	}
}

func TestSuccessfulDiskResizeClearsPhysicalCostReservation(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)
	lease := now.Add(time.Hour).Format(time.RFC3339Nano)
	if err := store.InsertTask(ctx, state.Task{
		TaskID: "successful-disk-resize-task", SessionID: "successful-disk-resize-session", Generation: 1, State: stateActive,
		InstanceName: "successful-disk-resize-instance", LeaseExpiresAt: &lease, CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	const gib = int64(1024 * 1024 * 1024)
	cfg := config.Defaults()
	cfg.DiskLowBytes = 200 * gib
	cfg.DiskTargetBytes = 300 * gib
	cfg.ResizeCostBytes = 7 * gib
	guest := &capacityGuest{startupGuest: &startupGuest{}, capacity: native.StorageCapacity{TotalBytes: 1024 * gib, FreeBytes: 210 * gib}}
	service := New(store, cfg, Options{Guest: guest, Resident: true, Clock: func() time.Time { return now }})
	service.MarkReady()
	if _, err := service.resize(ctx, protocol.Request{"session_id": "successful-disk-resize-session", "generation": int64(1), "disk_gib": int64(30)}); err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(ctx, map[string]any{"task_id": "successful-disk-resize-task"})
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.ReservationBytes != 0 || task.ResourceReservationJSON != "{}" {
		t.Fatalf("successful disk resize reservation = %#v", task)
	}
	disk, err := service.diskAdmission(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	physical := disk["physical_admission"].(map[string]any)
	if physical["reserved_bytes"] != int64(0) {
		t.Fatalf("successful disk resize physical reservation = %#v", physical)
	}
}

func TestAdminMetricsRejectsMalformedLogicalResources(t *testing.T) {
	cases := []struct {
		name         string
		resources    string
		reservations string
	}{
		{name: "incomplete resources", resources: `{"cpu":2,"memory_mib":4096}`, reservations: `{}`},
		{name: "null resource", resources: `{"cpu":null,"memory_mib":4096,"disk_gib":20}`, reservations: `{}`},
		{name: "negative reservation", resources: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, reservations: `{"cpu":-1,"memory_mib":4096,"disk_gib":20}`},
		{name: "incomplete reservation", resources: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, reservations: `{"cpu":4,"memory_mib":8192}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			task := state.Task{TaskID: "malformed-metrics-task", SessionID: "malformed-metrics-session", State: stateActive, InstanceName: "malformed-metrics-instance", ResourcesJSON: tc.resources, ResourceReservationJSON: tc.reservations}
			if err := store.InsertTask(context.Background(), task); err != nil {
				t.Fatal(err)
			}
			service := New(store, config.Defaults(), Options{})
			service.MarkReady()
			_, err = service.adminMetrics(context.Background())
			if control := protocol.AsError(err); control == nil || control.Code != "resource_unverified" {
				t.Fatalf("malformed metrics error = %#v", err)
			}
		})
	}
}

func TestAdminMetricsRejectsDefaultProjectionOverflow(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.CPUBudget = maxInt64Value
	cfg.CPUOvercommitRatio = json.Number("1")
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "projection-overflow-task", SessionID: "projection-overflow-session", State: stateActive,
		InstanceName: "projection-overflow-instance", ResourcesJSON: `{"cpu":9223372036854775807,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, cfg, Options{})
	service.MarkReady()
	metrics, err := service.adminMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	admission := metrics["default_admission"].(map[string]any)
	if admission["ready"] != false {
		t.Fatalf("overflow default admission = %#v", admission)
	}
	reasons := admission["reasons"].([]any)
	found := false
	for _, raw := range reasons {
		if reason, ok := raw.(map[string]any); ok && reason["code"] == "resource_unverified" {
			found = true
		}
	}
	if !found {
		t.Fatalf("overflow default admission reasons = %#v", reasons)
	}
}

func TestStartupPressureDetailRejectsProjectionOverflow(t *testing.T) {
	service := New(nil, config.Defaults(), Options{})
	detail := service.startupPressureDetail(observe.CPUObservation{Capacity: 8}, maxInt64Value, 2)
	if detail["verified"] != false || detail["projected_cpu"] != nil || detail["startup_blocked"] != nil {
		t.Fatalf("overflow startup pressure detail = %#v", detail)
	}
}

func TestExternalPhysicalReservationSharesLifecycleAdmission(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	guest := &capacityGuest{
		startupGuest: &startupGuest{},
		capacity:     native.StorageCapacity{TotalBytes: 100 * 1024 * 1024 * 1024, FreeBytes: 90 * 1024 * 1024 * 1024},
	}
	service := New(store, cfg, Options{Guest: guest, Resident: true})
	service.MarkReady()
	release, err := service.ReservePhysicalStorage(ctx, 75*1024*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	err = service.checkAdmission(ctx, map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}, "", true)
	if control := protocol.AsError(err); control == nil || control.Code != "resource_insufficient" {
		t.Fatalf("shared physical reservation error = %#v", err)
	}
}

func TestConcurrentPhysicalReservationsSerialize(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	capacityEntered := make(chan struct{}, 2)
	capacityRelease := make(chan struct{})
	guest := &capacityGuest{startupGuest: &startupGuest{}, capacity: native.StorageCapacity{TotalBytes: 100 * 1024 * 1024 * 1024, FreeBytes: 90 * 1024 * 1024 * 1024}, capacityEntered: capacityEntered, capacityRelease: capacityRelease}
	service := New(store, cfg, Options{Guest: guest, Resident: true})
	service.MarkReady()
	type reservationResult struct {
		release func()
		err     error
	}
	results := make(chan reservationResult, 2)
	go func() {
		release, reserveErr := service.ReservePhysicalStorage(context.Background(), 75*1024*1024*1024)
		results <- reservationResult{release: release, err: reserveErr}
	}()
	select {
	case <-capacityEntered:
	case <-time.After(time.Second):
		t.Fatal("first reservation did not reach capacity probe")
	}
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		release, reserveErr := service.ReservePhysicalStorage(context.Background(), 75*1024*1024*1024)
		results <- reservationResult{release: release, err: reserveErr}
	}()
	<-secondStarted
	select {
	case <-capacityEntered:
		t.Fatal("second reservation entered the capacity probe before the first released the admission lock")
	case <-time.After(25 * time.Millisecond):
	}
	close(capacityRelease)
	first, second := <-results, <-results
	successes := 0
	for _, result := range []reservationResult{first, second} {
		if result.err == nil {
			successes++
			result.release()
			continue
		}
		if control := protocol.AsError(result.err); control == nil || control.Code != "resource_insufficient" {
			t.Fatalf("concurrent reservation error = %#v", result.err)
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent reservation successes = %d, want 1", successes)
	}
}

func TestExternalPhysicalReservationPreservesManagementFreeSpaceFloor(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.Management.Upload.MinimumFreeBytes = 30 * 1024 * 1024 * 1024
	guest := &capacityGuest{
		startupGuest: &startupGuest{},
		capacity:     native.StorageCapacity{TotalBytes: 100 * 1024 * 1024 * 1024, FreeBytes: 40 * 1024 * 1024 * 1024},
	}
	service := New(store, cfg, Options{Guest: guest, Resident: true})
	service.MarkReady()
	_, err = service.ReservePhysicalStorage(context.Background(), 20*1024*1024*1024)
	if control := protocol.AsError(err); control == nil || control.Code != "resource_insufficient" {
		t.Fatalf("management free-space floor error = %#v", err)
	}
}

func TestExternalPhysicalReservationRefusesAdmissionDrain(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.SetMeta(context.Background(), "draining", "1"); err != nil {
		t.Fatal(err)
	}
	guest := &capacityGuest{startupGuest: &startupGuest{}, capacity: native.StorageCapacity{TotalBytes: 100, FreeBytes: 90}}
	service := New(store, config.Defaults(), Options{Guest: guest})
	_, err = service.ReservePhysicalStorage(context.Background(), 1)
	control := protocol.AsError(err)
	if control == nil || control.Code != "draining" {
		t.Fatalf("draining reservation error = %#v", err)
	}
	if guest.capacityCalls != 0 {
		t.Fatalf("draining reservation probed capacity %d times", guest.capacityCalls)
	}
}

func TestCachedResumeWithDiskGrowthChargesBothPhysicalCosts(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.ResumeCostBytes = 10
	cfg.ResizeCostBytes = 7
	service := New(store, cfg, Options{})
	task := state.Task{TaskID: "cached-cost-task", SessionID: "cached-cost-session", State: stateCached, ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`}
	if err := store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	cost, err := service.admissionStorageCost(context.Background(), task.TaskID, map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 30}, true)
	if err != nil {
		t.Fatal(err)
	}
	if cost != 17 {
		t.Fatalf("cached resume cost = %d, want 17", cost)
	}
}

func TestFailedCachedResumeRetainsLastAppliedResourcesForRetryCost(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "failed-cached-resume.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	fingerprint := strings.Repeat("a", 64)
	if err := store.SeedLegacyImage(ctx, fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateManagedImageVerification(ctx, fingerprint, "passed", "passed", nil, "available"); err != nil {
		t.Fatal(err)
	}
	session := "failed-cached-resume-session"
	if err := store.InsertTask(ctx, state.Task{
		TaskID: "failed-cached-resume-task", SessionID: session, Generation: 1, State: stateCached,
		InstanceName: "agent-failed-cached-resume", VMIP: stringPointer("10.0.0.10"), ImageFingerprint: fingerprint,
		CreatedAt: nowString(time.Now()), UpdatedAt: nowString(time.Now()),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Network = ""
	cfg.NetworkACL = ""
	guest := &startupGuest{
		status:              "Stopped",
		deviceSetErr:        errors.New("disk resize unavailable"),
		deviceSetFailDevice: "root",
		config: map[string]any{
			"config": map[string]any{
				"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "failed-cached-resume-task",
				"user.agent-vm.session": session, "user.agent-vm.generation": "1",
			},
			"devices": map[string]any{},
			"expanded_devices": map[string]any{"eth0": map[string]any{
				"type": "nic", "network": "", "ipv4.address": "10.0.0.10",
				"security.mac_filtering": "true", "security.ipv4_filtering": "true",
				"security.ipv6_filtering": "true", "security.port_isolation": "true",
				"security.acls": "", "security.acls.default.ingress.action": "allow",
				"security.acls.default.egress.action": "allow",
			}},
		},
	}
	service := New(store, cfg, Options{Guest: guest})
	_, err = service.acquire(ctx, protocol.Request{"session_id": session, "generation": int64(1)})
	if protocol.AsError(err).Code != "native_failed" {
		t.Fatalf("failed cached resume error = %#v", err)
	}
	if _, err := service.reconcile(ctx, true); err != nil {
		t.Fatalf("failed cached resume cleanup = %v", err)
	}
	cached, err := store.GetTask(ctx, map[string]any{"session_id": session})
	if err != nil || cached == nil || cached.State != stateCached {
		t.Fatalf("cached failed resume task = %#v err=%v", cached, err)
	}
	resources, resourceErr := decodeResourceMap(cached.ResourcesJSON, "cached failed resume resources", false)
	if resourceErr != nil || !sameResources(resources, map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}) {
		t.Fatalf("cached failed resume resources = %s err=%v", cached.ResourcesJSON, resourceErr)
	}
	cost, err := service.admissionStorageCost(ctx, cached.TaskID, map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 40}, true)
	if err != nil {
		t.Fatal(err)
	}
	if cost != cfg.ResumeCostBytes+cfg.ResizeCostBytes {
		t.Fatalf("retry physical cost = %d, want %d", cost, cfg.ResumeCostBytes+cfg.ResizeCostBytes)
	}
}

func TestFailedInitialAcquireRetainsLastAppliedResourcesForRetryCost(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "failed-initial-acquire.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Network = ""
	cfg.NetworkACL = ""
	cfg.VMIPStart = "10.0.0.10"
	cfg.VMIPEnd = "10.0.0.20"
	guest := &startupGuest{
		status:              "Stopped",
		deviceSetErr:        errors.New("disk resize unavailable"),
		deviceSetFailDevice: "root",
		config: map[string]any{
			"config":  map[string]any{},
			"devices": map[string]any{},
			"expanded_devices": map[string]any{"eth0": map[string]any{
				"type": "nic", "network": "", "ipv4.address": "10.0.0.10",
				"security.mac_filtering": "true", "security.ipv4_filtering": "true",
				"security.ipv6_filtering": "true", "security.port_isolation": "true",
				"security.acls": "", "security.acls.default.ingress.action": "allow",
				"security.acls.default.egress.action": "allow",
			}},
		},
	}
	service := New(store, cfg, Options{Guest: guest})
	_, err = service.acquire(ctx, protocol.Request{"session_id": "failed-initial", "generation": int64(0), "disk_gib": int64(40)})
	if protocol.AsError(err).Code != "native_failed" {
		t.Fatalf("failed initial acquire error = %#v", err)
	}
	if _, err := service.reconcile(ctx, true); err != nil {
		t.Fatalf("failed initial acquire cleanup = %v", err)
	}
	cached, err := store.GetTask(ctx, map[string]any{"session_id": "failed-initial"})
	if err != nil || cached == nil || cached.State != stateCached {
		t.Fatalf("cached failed initial acquire task = %#v err=%v", cached, err)
	}
	resources, resourceErr := decodeResourceMap(cached.ResourcesJSON, "cached failed initial acquire resources", false)
	if resourceErr != nil || !sameResources(resources, map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 0}) {
		t.Fatalf("cached failed initial acquire resources = %s err=%v", cached.ResourcesJSON, resourceErr)
	}
	cost, err := service.admissionStorageCost(ctx, cached.TaskID, map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 40}, true)
	if err != nil {
		t.Fatal(err)
	}
	if cost != cfg.ResumeCostBytes+cfg.ResizeCostBytes {
		t.Fatalf("retry physical cost = %d, want %d", cost, cfg.ResumeCostBytes+cfg.ResizeCostBytes)
	}
}

func TestCachedReleaseRetainsResourcesForResumeCostClassification(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.ResumeCostBytes = 10
	cfg.ResizeCostBytes = 7
	service := New(store, cfg, Options{})
	task := state.Task{TaskID: "release-cache-task", SessionID: "release-cache-session", Generation: 1, State: stateActive, ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`}
	if err := store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if _, err := service.releaseUnlocked(context.Background(), protocol.Request{"session_id": task.SessionID, "generation": task.Generation}, false); err != nil {
		t.Fatal(err)
	}
	cached, err := store.GetTask(context.Background(), map[string]any{"task_id": task.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	if cached == nil || cached.ResourcesJSON != task.ResourcesJSON {
		t.Fatalf("cached resources = %#v, want %s", cached, task.ResourcesJSON)
	}
	sameSize, err := service.admissionStorageCost(context.Background(), task.TaskID, map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}, true)
	if err != nil {
		t.Fatal(err)
	}
	if sameSize != cfg.ResumeCostBytes {
		t.Fatalf("same-size cached resume cost = %d, want %d", sameSize, cfg.ResumeCostBytes)
	}
}

func TestReclaimCachedCapacitySkipsIneligibleGuest(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const gib = int64(1024 * 1024 * 1024)
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "running-cache", SessionID: "running-cache-session", Generation: 1, State: stateCached,
		InstanceName: "agent-running-cache", CreatedAt: nowString(time.Now()), UpdatedAt: nowString(time.Now()),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	guest := &capacityGuest{startupGuest: &startupGuest{
		status: "Running",
		config: map[string]any{
			"config": map[string]any{
				"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "running-cache",
				"user.agent-vm.session": "running-cache-session", "user.agent-vm.generation": "1",
			},
			"devices": map[string]any{}, "expanded_devices": map[string]any{},
		},
	}, capacity: native.StorageCapacity{TotalBytes: 100 * gib, FreeBytes: 10 * gib}}
	service := New(store, config.Defaults(), Options{Guest: guest})
	service.admissionMu.Lock()
	reclaimed, reclaimErr := service.reclaimCachedCapacity(context.Background(), 1, "", 0)
	service.admissionMu.Unlock()
	if reclaimErr != nil {
		t.Fatal(reclaimErr)
	}
	if reclaimed || guest.deleteCalls != 0 {
		t.Fatalf("ineligible cache reclaimed=%v delete_calls=%d", reclaimed, guest.deleteCalls)
	}
}

func TestReclaimCachedCapacityUsesOldestCacheFirst(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const gib = int64(1024 * 1024 * 1024)
	oldest := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	newer := oldest.Add(time.Hour)
	for _, task := range []state.Task{
		{TaskID: "z-old-cache", SessionID: "z-old-session", Generation: 1, State: stateCached, InstanceName: "agent-z-old-cache", CreatedAt: nowString(oldest), UpdatedAt: nowString(oldest), InactiveSince: stringPointer(nowString(oldest)), ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`},
		{TaskID: "a-new-cache", SessionID: "a-new-session", Generation: 1, State: stateCached, InstanceName: "agent-a-new-cache", CreatedAt: nowString(newer), UpdatedAt: nowString(newer), InactiveSince: stringPointer(nowString(newer)), ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`},
	} {
		if err := store.InsertTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	guest := &capacityGuest{startupGuest: &startupGuest{
		status: "Stopped",
		config: map[string]any{
			"config": map[string]any{
				"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "z-old-cache",
				"user.agent-vm.session": "z-old-session", "user.agent-vm.generation": "1",
			},
			"devices": map[string]any{}, "expanded_devices": map[string]any{},
		},
	}, capacity: native.StorageCapacity{TotalBytes: 100 * gib, FreeBytes: 10 * gib}}
	service := New(store, config.Defaults(), Options{Guest: guest})
	guest.capacityFunc = func() native.StorageCapacity {
		if guest.deleteCalls > 0 {
			return native.StorageCapacity{TotalBytes: 100 * gib, FreeBytes: 60 * gib}
		}
		return guest.capacity
	}
	service.admissionMu.Lock()
	reclaimed, reclaimErr := service.reclaimCachedCapacity(context.Background(), 1, "", 0)
	service.admissionMu.Unlock()
	if reclaimErr != nil || !reclaimed {
		t.Fatalf("oldest cache reclaim = %v, err=%v", reclaimed, reclaimErr)
	}
	if task, err := store.GetTask(context.Background(), map[string]any{"task_id": "z-old-cache"}); err != nil || task != nil {
		t.Fatalf("oldest cache task = %#v err=%v", task, err)
	}
	if task, err := store.GetTask(context.Background(), map[string]any{"task_id": "a-new-cache"}); err != nil || task == nil {
		t.Fatalf("newer cache task = %#v err=%v", task, err)
	}
}

func TestReclaimCachedCapacityJournalsSessionScopedCleanup(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const gib = int64(1024 * 1024 * 1024)
	session := "reclaim-journal-session"
	generation := int64(1)
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "reclaim-journal-task", SessionID: session, Generation: generation, State: stateCached,
		InstanceName: "agent-reclaim-journal", CreatedAt: nowString(time.Now()), UpdatedAt: nowString(time.Now()),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	guest := &capacityGuest{startupGuest: &startupGuest{
		status:    "Stopped",
		deleteErr: errors.New("delete interrupted"),
		config: map[string]any{
			"config": map[string]any{
				"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "reclaim-journal-task",
				"user.agent-vm.session": session, "user.agent-vm.generation": "1",
			},
			"devices": map[string]any{}, "expanded_devices": map[string]any{},
		},
	}, capacity: native.StorageCapacity{TotalBytes: 100 * gib, FreeBytes: 10 * gib}}
	service := New(store, config.Defaults(), Options{Guest: guest})
	service.admissionMu.Lock()
	_, reclaimErr := service.reclaimCachedCapacity(context.Background(), 1, "", 0)
	service.admissionMu.Unlock()
	if reclaimErr == nil {
		t.Fatal("reclaim unexpectedly succeeded")
	}
	pending, err := store.PendingOperations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Action != "destroy" || pending[0].SessionID == nil || *pending[0].SessionID != session {
		t.Fatalf("pending reclaim cleanup = %#v", pending)
	}
	request, ok := pending[0].Response["_request"].(map[string]any)
	if !ok || request["session_id"] != session || request["operation_id"] != pending[0].OperationID {
		t.Fatalf("reclaim cleanup request = %#v", pending[0].Response["_request"])
	}
	task, err := store.GetTask(context.Background(), map[string]any{"task_id": "reclaim-journal-task"})
	if err != nil || task == nil || task.State != stateDeleting {
		t.Fatalf("reclaim task = %#v err=%v", task, err)
	}
}

func TestAdminMetricsDefaultAdmissionReflectsDrainAndStructuredUnknowns(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	service := New(store, cfg, Options{})
	service.MarkReady()
	if _, err := service.drain(ctx, true, protocol.Request{"session_id": "test-drain"}); err != nil {
		t.Fatal(err)
	}
	metrics, err := service.adminMetrics(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defaultAdmission := metrics["default_admission"].(map[string]any)
	if defaultAdmission["ready"] != false {
		t.Fatalf("default admission = %#v", defaultAdmission)
	}
	reasons := defaultAdmission["reasons"].([]any)
	if len(reasons) == 0 || reasons[0].(map[string]any)["code"] != "draining" {
		t.Fatalf("default admission reasons = %#v", reasons)
	}
	if metrics["admission_ready"] != false {
		t.Fatalf("admission_ready = %#v", metrics["admission_ready"])
	}
	if warningCounts, ok := metrics["warning_counts"].(map[string]int64); !ok || len(warningCounts) != 0 {
		t.Fatalf("transient warning counts = %#v", metrics["warning_counts"])
	}
	if warnings, ok := metrics["warnings"].([]any); !ok || len(warnings) == 0 {
		t.Fatalf("transient warnings = %#v", metrics["warnings"])
	}
	if metrics["reclaim_required"] != nil {
		t.Fatalf("unmeasured reclaim_required = %#v", metrics["reclaim_required"])
	}
	if _, ok := metrics["disk_admission"].(map[string]any); !ok {
		t.Fatalf("disk_admission = %#v", metrics["disk_admission"])
	}
	if _, ok := metrics["ct_memory"].(map[string]any); !ok {
		t.Fatalf("ct_memory = %#v", metrics["ct_memory"])
	}
	if _, ok := metrics["instances"].(map[string]any); !ok {
		t.Fatalf("instances = %#v", metrics["instances"])
	}
	doctor, ok := metrics["doctor"].(map[string]any)
	if !ok || doctor["ready"] != false {
		t.Fatalf("doctor = %#v", metrics["doctor"])
	}
}

func TestAdminMetricsPreservesPhysicalCapacityAndAdmissionCost(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	guest := &capacityGuest{
		startupGuest: &startupGuest{},
		capacity:     native.StorageCapacity{TotalBytes: 100 * 1024 * 1024 * 1024, FreeBytes: 90 * 1024 * 1024 * 1024},
	}
	service := New(store, cfg, Options{Guest: guest, Resident: true})
	service.MarkReady()
	metrics, err := service.adminMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	capacity, ok := metrics["capacity"].(map[string]any)
	if !ok || capacity["verified"] != true || capacity["free_bytes"] != int64(90*1024*1024*1024) {
		t.Fatalf("capacity = %#v", metrics["capacity"])
	}
	admission := metrics["default_admission"].(map[string]any)
	if admission["request_cost_bytes"] != cfg.CreateCostBytes || admission["ready"] != false {
		t.Fatalf("default admission = %#v", admission)
	}
	reasons := admission["reasons"].([]any)
	if len(reasons) == 0 || reasons[0].(map[string]any)["code"] != "resource_unverified" {
		t.Fatalf("default admission reasons = %#v", reasons)
	}
	disk, ok := metrics["disk_admission"].(map[string]any)
	if !ok || disk["verified"] != true {
		t.Fatalf("disk admission = %#v", metrics["disk_admission"])
	}
}

func TestAdminMetricsPublishesVerifiedMemoryObservation(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	current := int64(3 * 1024 * 1024 * 1024)
	service := New(store, config.Defaults(), Options{
		Memory: staticMemoryObserver{observation: observe.MemoryObservation{
			LimitBytes: 4 * 1024 * 1024 * 1024, AvailableBytes: 512 * 1024 * 1024,
			CurrentBytes: &current, ThresholdBytes: 1024 * 1024 * 1024,
			OOMEvents: map[string]int64{"oom": 2},
		}},
	})
	metrics, err := service.adminMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	memory := metrics["ct_memory"].(map[string]any)
	if memory["capacity_bytes"] != int64(4*1024*1024*1024) || memory["current_bytes"] != &current || memory["available_bytes"] != int64(512*1024*1024) {
		t.Fatalf("ct memory = %#v", memory)
	}
	if memory["pressure"] != true || memory["oom_events"].(map[string]int64)["oom"] != 2 {
		t.Fatalf("ct memory pressure = %#v", memory)
	}
}

func TestAdminMetricsFailsAdmissionReadinessWhenResidentMemoryIsUnverified(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	guest := &capacityGuest{
		startupGuest: &startupGuest{},
		capacity:     native.StorageCapacity{TotalBytes: 100 * 1024 * 1024 * 1024, FreeBytes: 90 * 1024 * 1024 * 1024},
	}
	service := New(store, config.Defaults(), Options{
		Guest:    guest,
		CPU:      staticCPUObserver{observation: observe.CPUObservation{Capacity: 8}},
		Resident: true,
	})
	service.MarkReady()
	metrics, err := service.adminMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if metrics["admission_ready"] != false || metrics["default_admission"].(map[string]any)["ready"] != false {
		t.Fatalf("unverified resident memory readiness = %#v", metrics)
	}
	found := false
	for _, raw := range metrics["default_admission"].(map[string]any)["reasons"].([]any) {
		if reason, ok := raw.(map[string]any); ok && reason["code"] == "resource_unverified" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unverified resident memory reasons = %#v", metrics["default_admission"])
	}
}

func TestAdmissionFailsClosedWhenConfiguredMemoryObservationFails(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, config.Defaults(), Options{
		Memory:   staticMemoryObserver{err: errors.New("memory probe unavailable")},
		Resident: true,
	})
	err = service.checkAdmission(context.Background(), map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}, "", true)
	control := protocol.AsError(err)
	if control == nil || control.Code != "resource_unverified" {
		t.Fatalf("memory observation admission error = %#v", err)
	}
}

func TestAdminMetricsDefaultAdmissionMatchesPhysicalRejection(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const gib = int64(1024 * 1024 * 1024)
	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.DiskLowBytes = 200 * gib
	cfg.DiskTargetBytes = 300 * gib
	cfg.CreateCostBytes = 4 * gib
	guest := &capacityGuest{startupGuest: &startupGuest{}, capacity: native.StorageCapacity{TotalBytes: 1024 * gib, FreeBytes: 203 * gib}}
	service := New(store, cfg, Options{Guest: guest, CPU: staticCPUObserver{observation: observe.CPUObservation{Capacity: 8}}, Resident: true})
	service.MarkReady()
	requested := map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}
	if control := protocol.AsError(service.checkAdmission(context.Background(), requested, "", true)); control == nil || control.Code != "resource_insufficient" {
		t.Fatalf("physical rejection = %#v", control)
	}
	metrics, err := service.adminMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	admission := metrics["default_admission"].(map[string]any)
	if admission["ready"] != false {
		t.Fatalf("physical default admission = %#v", admission)
	}
	reasons := admission["reasons"].([]any)
	found := false
	for _, raw := range reasons {
		if reason, ok := raw.(map[string]any); ok && reason["code"] == "disk_reclaim_required" {
			found = true
		}
	}
	if !found {
		t.Fatalf("physical default admission reasons = %#v", reasons)
	}
}

func TestAdmissionReclaimsCachedCapacityBeforeRejectingPhysicalSpace(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const gib = int64(1024 * 1024 * 1024)
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "reclaimable-cache", SessionID: "reclaimable-cache-session", Generation: 1, State: stateCached,
		InstanceName: "agent-reclaimable-cache", CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	guest := &capacityGuest{
		startupGuest: &startupGuest{
			status: "Stopped",
			config: map[string]any{
				"config":  map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "reclaimable-cache", "user.agent-vm.session": "reclaimable-cache-session", "user.agent-vm.generation": "1"},
				"devices": map[string]any{}, "expanded_devices": map[string]any{},
			},
		},
		capacity: native.StorageCapacity{TotalBytes: 100 * gib, FreeBytes: 25 * gib},
	}
	cfg := config.Defaults()
	cfg.DiskLowBytes = 20 * gib
	cfg.DiskTargetBytes = 30 * gib
	cfg.CreateCostBytes = 10 * gib
	service := New(store, cfg, Options{Guest: guest, Clock: func() time.Time { return now }})
	guest.capacityFunc = func() native.StorageCapacity {
		if guest.deleteCalls > 0 {
			return native.StorageCapacity{TotalBytes: 100 * gib, FreeBytes: 60 * gib}
		}
		return guest.capacity
	}
	if err := service.checkAdmission(context.Background(), map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}, "", true); err != nil {
		t.Fatal(err)
	}
	if guest.deleteCalls != 1 {
		t.Fatalf("cached delete calls = %d", guest.deleteCalls)
	}
	if task, err := store.GetTask(context.Background(), map[string]any{"task_id": "reclaimable-cache"}); err != nil || task != nil {
		t.Fatalf("reclaimed task = %#v err=%v", task, err)
	}
}

func TestReclaimCachedCapacityReachesTargetWatermark(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const gib = int64(1024 * 1024 * 1024)
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "target-cache", SessionID: "target-cache-session", Generation: 1, State: stateCached,
		InstanceName: "agent-target-cache", CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	guest := &capacityGuest{
		startupGuest: &startupGuest{
			status: "Stopped",
			config: map[string]any{
				"config":  map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "target-cache", "user.agent-vm.session": "target-cache-session", "user.agent-vm.generation": "1"},
				"devices": map[string]any{}, "expanded_devices": map[string]any{},
			},
		},
		capacity: native.StorageCapacity{TotalBytes: 100 * gib, FreeBytes: 10 * gib},
	}
	cfg := config.Defaults()
	cfg.DiskLowBytes = 20 * gib
	cfg.DiskTargetBytes = 30 * gib
	guest.capacityFunc = func() native.StorageCapacity {
		if guest.deleteCalls > 0 {
			return native.StorageCapacity{TotalBytes: 100 * gib, FreeBytes: 25 * gib}
		}
		return guest.capacity
	}
	service := New(store, cfg, Options{Guest: guest, Clock: func() time.Time { return now }})
	service.admissionMu.Lock()
	reclaimed, reclaimErr := service.reclaimCachedCapacity(context.Background(), 1, "", 0)
	service.admissionMu.Unlock()
	if reclaimErr != nil {
		t.Fatal(reclaimErr)
	}
	if !reclaimed {
		t.Fatalf("reclaim did not report low-watermark admission after candidate exhaustion")
	}
	if guest.deleteCalls != 1 {
		t.Fatalf("cached delete calls = %d", guest.deleteCalls)
	}
	if guest.capacityCalls < 2 {
		t.Fatalf("reclaim stopped before checking the target watermark: capacity calls = %d", guest.capacityCalls)
	}
}

func TestReclaimCachedCapacitySkipsBusySession(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "busy-cache", SessionID: "busy-cache-session", Generation: 1, State: stateCached,
		InstanceName: "agent-busy-cache", CreatedAt: nowString(time.Now()), UpdatedAt: nowString(time.Now()),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	unlockSession, locked := service.trySessionLock("busy-cache-session")
	if !locked {
		t.Fatal("failed to acquire busy session lock")
	}
	defer unlockSession()

	service.admissionMu.Lock()
	reclaimed, reclaimErr := service.reclaimCachedCapacity(context.Background(), 1, "", 0)
	service.admissionMu.Unlock()
	if reclaimErr != nil {
		t.Fatal(reclaimErr)
	}
	if reclaimed {
		t.Fatal("busy cached session was reclaimed")
	}
}

func TestAdmissionDoesNotReclaimCachedCapacityWhenStartupPressureRejects(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const gib = int64(1024 * 1024 * 1024)
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "pressure-cache", SessionID: "pressure-cache-session", Generation: 1, State: stateCached,
		InstanceName: "agent-pressure-cache", CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	guest := &capacityGuest{
		startupGuest: &startupGuest{
			status: "Stopped",
			config: map[string]any{
				"config": map[string]any{
					"user.agent-vm.managed":    "native-v1",
					"user.agent-vm.task":       "pressure-cache",
					"user.agent-vm.session":    "pressure-cache-session",
					"user.agent-vm.generation": "1",
				},
				"devices": map[string]any{}, "expanded_devices": map[string]any{},
			},
		},
		capacity: native.StorageCapacity{TotalBytes: 100 * gib, FreeBytes: 25 * gib},
	}
	guest.capacityFunc = func() native.StorageCapacity {
		if guest.deleteCalls > 0 {
			return native.StorageCapacity{TotalBytes: 100 * gib, FreeBytes: 60 * gib}
		}
		return guest.capacity
	}
	cfg := config.Defaults()
	cfg.DiskLowBytes = 20 * gib
	cfg.DiskTargetBytes = 30 * gib
	cfg.CreateCostBytes = 10 * gib
	cfg.CPUStartupPressureFullAvg10Percent = 20
	service := New(store, cfg, Options{
		Guest: guest,
		CPU:   staticCPUObserver{observation: observe.CPUObservation{Capacity: 1, Full: observe.PSIValues{Avg10: 50}}},
		Clock: func() time.Time { return now },
	})
	control := protocol.AsError(service.checkAdmission(context.Background(), map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}, "", true))
	if control == nil || control.Code != "resource_pressure" {
		t.Fatalf("startup pressure rejection = %#v", control)
	}
	if guest.deleteCalls != 0 {
		t.Fatalf("cached delete calls = %d", guest.deleteCalls)
	}
	if task, err := store.GetTask(context.Background(), map[string]any{"task_id": "pressure-cache"}); err != nil || task == nil {
		t.Fatalf("pressure cache task = %#v err=%v", task, err)
	}
}

func TestInconsistentAdminMetricsCannotReportReady(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	calls := 0
	guest := &capacityGuest{
		startupGuest: &startupGuest{},
		capacity:     native.StorageCapacity{TotalBytes: 100 * 1024 * 1024 * 1024, FreeBytes: 90 * 1024 * 1024 * 1024},
		probeHook: func() {
			calls++
			if err := store.SetMeta(context.Background(), "probe_epoch", fmt.Sprintf("%d", calls)); err != nil {
				t.Fatalf("probe epoch = %v", err)
			}
		},
	}
	service := New(store, config.Defaults(), Options{Guest: guest})
	service.MarkReady()
	metrics, err := service.adminMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("native probe hook was not called")
	}
	if metrics["observation_consistent"] != false {
		t.Fatalf("observation consistency = %#v", metrics["observation_consistent"])
	}
	if metrics["admission_ready"] != false {
		t.Fatalf("admission ready = %#v", metrics["admission_ready"])
	}
	defaultAdmission := metrics["default_admission"].(map[string]any)
	if defaultAdmission["ready"] != false {
		t.Fatalf("default admission = %#v", defaultAdmission)
	}
	doctor := metrics["doctor"].(map[string]any)
	if doctor["ready"] != false {
		t.Fatalf("doctor = %#v", doctor)
	}
}

func TestFailedSnapshotRefreshInvalidatesPreviousSource(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshots := observe.NewSnapshotStore(filepath.Join(t.TempDir(), "metrics"))
	service := New(store, config.Defaults(), Options{Snapshots: snapshots})
	if err := snapshots.Publish("capacity", map[string]any{"up": true, "resources": map[string]any{"allocated": map[string]any{"cpu": int64(2)}}}); err != nil {
		t.Fatal(err)
	}
	if err := service.publishSnapshotFailure("admin-metrics", protocol.NewError("resource_unverified", "capacity unavailable", nil)); err != nil {
		t.Fatal(err)
	}
	capacity, err := snapshots.Read("capacity")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(capacity), `"up":false`) {
		t.Fatalf("failed capacity snapshot = %s", capacity)
	}
	capacityMetrics, err := os.ReadFile(filepath.Join(snapshots.Directory, "capacity.prom"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(capacityMetrics), "agent_vm_capacity_up 0") || strings.Contains(string(capacityMetrics), "agent_vm_admission_cpu_allocated_vcpu") {
		t.Fatalf("failed capacity metrics = %s", capacityMetrics)
	}
	if err := snapshots.Publish("inventory", map[string]any{"up": true, "tasks": []any{map[string]any{"task_id": "stale"}}}); err != nil {
		t.Fatal(err)
	}
	if err := service.publishSnapshotFailure("admin-status", protocol.NewError("sqlite_unavailable", "inventory unavailable", nil)); err != nil {
		t.Fatal(err)
	}
	inventory, err := snapshots.Read("inventory")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(inventory), `"tasks"`) || strings.Contains(string(inventory), `"task_states"`) {
		t.Fatalf("failed inventory snapshot retained unverifiable rows = %s", inventory)
	}
	inventoryMetrics, err := os.ReadFile(filepath.Join(snapshots.Directory, "inventory.prom"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(inventoryMetrics), "agent_vm_inventory_up 0") || strings.Contains(string(inventoryMetrics), "task_info") {
		t.Fatalf("failed inventory metrics = %s", inventoryMetrics)
	}
}

func TestAdminMetricsMatchesAdmissionForUnknownState(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.InstanceLimit = 0
	task := state.Task{TaskID: "unknown-state-task", SessionID: "unknown-state-session", State: "mystery", ResourcesJSON: `{}`, ResourceReservationJSON: `{}`}
	if err := store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	service := New(store, cfg, Options{Guest: &capacityGuest{startupGuest: &startupGuest{}, capacity: native.StorageCapacity{TotalBytes: 100 * 1024 * 1024 * 1024, FreeBytes: 90 * 1024 * 1024 * 1024}}, CPU: staticCPUObserver{observation: observe.CPUObservation{Capacity: 8}}})
	service.MarkReady()
	metrics, err := service.adminMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	admission := metrics["default_admission"].(map[string]any)
	if admission["ready"] != false {
		t.Fatalf("unknown state default admission = %#v", admission)
	}
	if _, ok := metrics["instances"].(map[string]any)["verified"].(bool); !ok || metrics["instances"].(map[string]any)["verified"] != false {
		t.Fatalf("unknown state instance verification = %#v", metrics["instances"])
	}
	instances := metrics["instances"].(map[string]any)["admission"].(map[string]any)
	for _, key := range []string{"allocated", "reserved", "allocated_and_reserved", "remaining"} {
		if instances[key] != nil {
			t.Fatalf("unknown state instance %s = %#v", key, instances[key])
		}
	}
	resources := metrics["resources"].(map[string]any)
	for _, key := range []string{"allocated", "reserved", "allocated_and_reserved"} {
		value, ok := resources[key].(map[string]int64)
		if !ok || value != nil {
			t.Fatalf("unknown state resource %s = %#v", key, resources[key])
		}
	}
	if remaining, ok := resources["remaining"].(map[string]any); !ok || len(remaining) != 0 {
		t.Fatalf("unknown state resource remaining = %#v", resources["remaining"])
	}
	startupPressure := metrics["ct_cpu"].(map[string]any)["startup_pressure"].(map[string]any)
	if startupPressure["verified"] != false || startupPressure["startup_blocked"] != nil {
		t.Fatalf("unknown state startup pressure = %#v", startupPressure)
	}
	if control := protocol.AsError(service.checkAdmission(context.Background(), map[string]int64{"cpu": 2, "memory_mib": 4096, "disk_gib": 20}, "", true)); control == nil || control.Code != "resource_unverified" {
		t.Fatalf("unknown state admission error = %#v", control)
	}
}

func TestAdminMetricsDoesNotDoubleCountResizeReservation(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.CPUBudget = 6
	cfg.CPUOvercommitRatio = json.Number("1")
	cfg.MemoryBudgetMiB = 12288
	cfg.MemoryOvercommitRatio = json.Number("1")
	cfg.DiskBudgetGiB = 50
	task := state.Task{TaskID: "resize-metrics-task", SessionID: "resize-metrics-session", State: stateActive, ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{"cpu":4,"memory_mib":8192,"disk_gib":30}`}
	if err := store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	service := New(store, cfg, Options{Guest: &capacityGuest{startupGuest: &startupGuest{}, capacity: native.StorageCapacity{TotalBytes: 100 * 1024 * 1024 * 1024, FreeBytes: 90 * 1024 * 1024 * 1024}}})
	service.MarkReady()
	metrics, err := service.adminMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	admission := metrics["default_admission"].(map[string]any)
	if admission["ready"] != true {
		t.Fatalf("resize default admission = %#v", admission)
	}
	resources := metrics["resources"].(map[string]any)["allocated_and_reserved"].(map[string]int64)
	if resources["cpu"] != 4 || resources["memory_mib"] != 8192 || resources["disk_gib"] != 30 {
		t.Fatalf("resize allocated_and_reserved = %#v", resources)
	}
}

func TestAdminMetricsSumsSeparateAllocatedAndReservedTasks(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.CPUBudget = 6
	cfg.CPUOvercommitRatio = json.Number("1")
	cfg.MemoryBudgetMiB = 12288
	cfg.MemoryOvercommitRatio = json.Number("1")
	cfg.DiskBudgetGiB = 50
	active := state.Task{TaskID: "allocated-task", SessionID: "allocated-session", State: stateActive, InstanceName: "allocated-instance", ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`}
	preparing := state.Task{TaskID: "reserved-task", SessionID: "reserved-session", State: statePreparing, InstanceName: "reserved-instance", ResourcesJSON: `{"cpu":3,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`}
	for _, task := range []state.Task{active, preparing} {
		if err := store.InsertTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	service := New(store, cfg, Options{})
	service.MarkReady()
	metrics, err := service.adminMetrics(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resources := metrics["resources"].(map[string]any)["allocated_and_reserved"].(map[string]int64)
	if resources["cpu"] != 5 || resources["memory_mib"] != 8192 || resources["disk_gib"] != 40 {
		t.Fatalf("separate task allocated_and_reserved = %#v", resources)
	}
	if metrics["default_admission"].(map[string]any)["ready"] != false {
		t.Fatalf("separate task default admission = %#v", metrics["default_admission"])
	}
}

func TestStatusReportsCachedNativeStateAndGuestAvailability(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	session := "cached-status"
	task := state.Task{
		TaskID: "cached-status-task", SessionID: session, Generation: 2, State: stateCached,
		InstanceName: "cached-status-instance", CreatedAt: nowString(time.Now()), UpdatedAt: nowString(time.Now()),
		ImageFingerprint: strings.Repeat("a", 64), ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: "{}",
	}
	if err := store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	guest := &perInstanceGuest{
		startupGuest: startupGuest{status: "Stopped"},
		configs: map[string]map[string]any{
			task.InstanceName: {
				"config": map[string]any{
					"user.agent-vm.managed": "native-v1", "user.agent-vm.task": task.TaskID,
					"user.agent-vm.session": task.SessionID, "user.agent-vm.generation": "2",
				}, "devices": map[string]any{}, "expanded_devices": map[string]any{},
			},
		},
	}
	service := New(store, config.Defaults(), Options{Guest: guest})
	result, err := service.status(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	if result["native_status"] != "Stopped" || result["guest_available"] != false {
		t.Fatalf("cached status = %#v", result)
	}
	admin, err := service.adminStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	items := admin["tasks"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["native_status"] != "Stopped" {
		t.Fatalf("admin status = %#v", admin)
	}
}

func TestStatusKeepsGuestAvailabilityUnknownWhenNativeStatusIsUnverified(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	task := state.Task{TaskID: "unverified-status-task", SessionID: "unverified-status-session", Generation: 1, State: stateCached, InstanceName: "unverified-status-instance", ResourcesJSON: `{}`, ResourceReservationJSON: `{}`}
	if err := store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	guest := &perInstanceGuest{
		startupGuest: startupGuest{statusErr: errors.New("status transport failed")},
		configs:      map[string]map[string]any{task.InstanceName: {"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": task.TaskID, "user.agent-vm.session": task.SessionID, "user.agent-vm.generation": "1"}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}},
	}
	service := New(store, config.Defaults(), Options{Guest: guest})
	result, err := service.status(ctx, task.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if result["native_status"] != "unverified" || result["guest_available"] != nil {
		t.Fatalf("unverified status = %#v", result)
	}
	if result["resources"] != nil || result["resource_reservation"] != nil {
		t.Fatalf("unverified durable resources were fabricated = %#v", result)
	}
	resourceState, ok := result["resource_state"].(map[string]any)
	if !ok || resourceState["verified"] != false {
		t.Fatalf("unverified durable resource state = %#v", result)
	}
	admin, err := service.adminStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	adminTask := admin["tasks"].([]any)[0].(map[string]any)
	if adminTask["resources"] != nil || adminTask["resource_reservation"] != nil || adminTask["resource_state"] == nil {
		t.Fatalf("admin status fabricated durable resources = %#v", adminTask)
	}
}

func TestDoctorIncludesIssueListForHealthContract(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	service := New(store, cfg, Options{})
	response, err := service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version,
		"action":   "doctor",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := response["result"].(map[string]any)
	issues, ok := result["issues"].([]any)
	if !ok || len(issues) != 0 {
		t.Fatalf("doctor issues = %#v", result["issues"])
	}
}

func TestAdmissionAndLeaseTransitionsRemainDurable(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	cfg := config.Defaults()
	cfg.StateDB = store.Path()
	cfg.MemoryBudgetMiB = 4096
	cfg.MemoryOvercommitRatio = json.Number("1")
	service := New(store, cfg, Options{Clock: func() time.Time { return now }})
	ctx := context.Background()
	_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "first", "generation": int64(0), "operation_id": "acquire-first"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "second", "generation": int64(0), "operation_id": "acquire-second"})
	if control := protocol.AsError(err); control.Code != "resource_insufficient" {
		t.Fatalf("second acquire error = %#v", control)
	}
	if task, getErr := store.GetTask(ctx, map[string]any{"session_id": "second"}); getErr != nil || task != nil {
		t.Fatalf("rejected acquire left durable task: task=%#v err=%v", task, getErr)
	}
	first, err := store.GetTask(ctx, map[string]any{"session_id": "first"})
	if err != nil || first == nil {
		t.Fatalf("first task = %#v err=%v", first, err)
	}
	if err := store.UpdateTask(ctx, first.TaskID, map[string]any{"lease_expires_at": nowString(now.Add(-time.Second))}); err != nil {
		t.Fatal(err)
	}
	_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "renew", "session_id": "first", "generation": first.Generation, "operation_id": "renew-expired"})
	if control := protocol.AsError(err); control.Code != "lease_expired" {
		t.Fatalf("expired renew error = %#v", control)
	}
	result, err := service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "timer-expire", "task_id": first.TaskID, "generation": first.Generation})
	if err != nil {
		t.Fatal(err)
	}
	if result["result"].(map[string]any)["state"] != stateCached {
		t.Fatalf("expired cleanup result = %#v", result)
	}
}

func TestConcurrentOperationRetryDoesNotDuplicateLifecycleMutation(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, config.Defaults(), Options{})
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "concurrent", "generation": int64(0), "operation_id": "same-operation"}
	responses := make(chan map[string]any, 2)
	errors := make(chan error, 2)
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			response, dispatchErr := service.Dispatch(context.Background(), request)
			responses <- response
			errors <- dispatchErr
		}()
	}
	group.Wait()
	close(responses)
	close(errors)
	successes, replays := 0, 0
	for response := range responses {
		if response == nil {
			continue
		}
		switch response["outcome"] {
		case "success":
			successes++
		case "replayed":
			replays++
		}
	}
	for dispatchErr := range errors {
		if dispatchErr != nil {
			t.Fatalf("concurrent dispatch error = %v", dispatchErr)
		}
	}
	if successes != 1 || replays != 1 {
		t.Fatalf("successes=%d replays=%d", successes, replays)
	}
}

func TestActiveOperationReferenceCountSurvivesConcurrentReplay(t *testing.T) {
	service := New(nil, config.Defaults(), Options{})
	finishFirst := service.markOperationActive("same-operation")
	finishSecond := service.markOperationActive("same-operation")
	finishFirst()
	if !service.operationActive("same-operation") {
		t.Fatal("operation was cleared while another request still owned it")
	}
	finishSecond()
	if service.operationActive("same-operation") {
		t.Fatal("operation remained active after all owners finished")
	}
}

func TestDestroyedSessionCannotReuseOldGeneration(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "generation-reuse.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.PortStart, cfg.PortEnd = 22000, 22010
	service := New(store, cfg, Options{})
	ctx := context.Background()
	acquire := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "generation-reuse", "generation": int64(0), "operation_id": "acquire-old"}
	response, err := service.Dispatch(ctx, acquire)
	if err != nil {
		t.Fatal(err)
	}
	generation := response["result"].(map[string]any)["generation"]
	if generation != int64(1) {
		t.Fatalf("first generation = %#v", generation)
	}
	if _, err := service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "destroy", "session_id": "generation-reuse", "generation": int64(1), "operation_id": "destroy-old"}); err != nil {
		t.Fatal(err)
	}
	response, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "generation-reuse", "generation": int64(0), "operation_id": "acquire-new"})
	if err != nil {
		t.Fatal(err)
	}
	if response["result"].(map[string]any)["generation"] != int64(2) {
		t.Fatalf("reacquired generation = %#v", response)
	}
	_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "renew", "session_id": "generation-reuse", "generation": int64(1), "operation_id": "stale-renew"})
	if control := protocol.AsError(err); control.Code != "generation_mismatch" {
		t.Fatalf("stale generation error = %#v", err)
	}
}

func TestPortNameConflictDoesNotOverwriteExistingMapping(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "port-conflict.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.PortStart, cfg.PortEnd = 22000, 22010
	service := New(store, cfg, Options{})
	ctx := context.Background()
	if _, err := service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "port-session", "generation": int64(0), "operation_id": "port-acquire"}); err != nil {
		t.Fatal(err)
	}
	base := protocol.Request{"protocol": protocol.Version, "action": "port-add", "session_id": "port-session", "generation": int64(1), "name": "web", "vm_port": int64(8080), "operation_id": "port-add-one"}
	if _, err := service.Dispatch(ctx, base); err != nil {
		t.Fatal(err)
	}
	conflict := protocol.Request{"protocol": protocol.Version, "action": "port-add", "session_id": "port-session", "generation": int64(1), "name": "web", "vm_port": int64(9090), "operation_id": "port-add-two"}
	if _, err := service.Dispatch(ctx, conflict); protocol.AsError(err).Code != "operation_conflict" {
		t.Fatalf("port conflict error = %#v", err)
	}
	task, err := store.GetTask(ctx, map[string]any{"session_id": "port-session"})
	if err != nil || task == nil {
		t.Fatalf("port task = %#v err=%v", task, err)
	}
	ports, err := store.ListPorts(ctx, task.TaskID)
	if err != nil || len(ports) != 1 || ports[0].VMPort != 8080 {
		t.Fatalf("existing port was overwritten: ports=%#v err=%v", ports, err)
	}
}

func TestDrainStatusIncludesStateCapabilities(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, config.Defaults(), Options{})
	response, err := service.Dispatch(context.Background(), protocol.Request{"protocol": protocol.Version, "action": "drain-status"})
	if err != nil {
		t.Fatal(err)
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("drain status result = %#v", response["result"])
	}
	capabilities, ok := result["state_capabilities"].([]string)
	if !ok {
		t.Fatalf("state capabilities = %#v", result["state_capabilities"])
	}
	expected := []string{
		"legacy-agent-vm-core@1",
		"managed-image-catalog@1",
		"management-operation-audit@1",
		"session-image-binding@1",
	}
	if !reflect.DeepEqual(capabilities, expected) {
		t.Fatalf("state capabilities = %#v, want %#v", capabilities, expected)
	}
}

func TestDirectReleaseCannotCrossReleaseActivationDrain(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "release-fence.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	lease := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if err := store.InsertTask(ctx, state.Task{TaskID: "direct-release-task", SessionID: "direct-release-session", Generation: 1, State: "active", InstanceName: "agent-vm-direct-release", LeaseExpiresAt: &lease, CreatedAt: "2026-10-28T00:00:00Z", UpdatedAt: "2026-10-28T00:00:00Z", ImageFingerprint: strings.Repeat("a", 64), ResourcesJSON: "{}", ResourceReservationJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMeta(ctx, "draining", "1"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMeta(ctx, "drain_owner", "release-tx-direct"); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "release", "session_id": "direct-release-session", "generation": int64(1), "operation_id": "direct-release-operation"})
	if err == nil || protocol.AsError(err).Code != "draining" {
		t.Fatalf("direct release error = %v", err)
	}
	task, err := store.GetTask(ctx, map[string]any{"task_id": "direct-release-task"})
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.State != "active" {
		t.Fatalf("direct release changed task = %#v", task)
	}
}

func TestTimerExpireCannotCrossReleaseActivationDrain(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "timer-expire-fence.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	lease := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	if err := store.InsertTask(ctx, state.Task{TaskID: "timer-expire-task", SessionID: "timer-expire-session", Generation: 1, State: "active", InstanceName: "agent-vm-timer-expire", LeaseExpiresAt: &lease, CreatedAt: "2026-10-28T00:00:00Z", UpdatedAt: "2026-10-28T00:00:00Z", ImageFingerprint: strings.Repeat("a", 64), ResourcesJSON: "{}", ResourceReservationJSON: "{}"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMeta(ctx, "draining", "1"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMeta(ctx, "drain_owner", "release-tx-expire"); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "timer-expire", "task_id": "timer-expire-task", "generation": int64(1)})
	if err == nil || protocol.AsError(err).Code != "draining" {
		t.Fatalf("timer expire error = %v", err)
	}
	task, err := store.GetTask(ctx, map[string]any{"task_id": "timer-expire-task"})
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.State != stateActive {
		t.Fatalf("timer expire changed task = %#v", task)
	}
}

func TestTimerReconcileContinuesReleaseCleanupDuringDrain(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "timer-drain.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, config.Defaults(), Options{})
	if _, err := service.drain(context.Background(), true, protocol.Request{"session_id": "release-owner"}); err != nil {
		t.Fatal(err)
	}
	response, err := service.Dispatch(context.Background(), protocol.Request{"protocol": protocol.Version, "action": "timer-reconcile", "apply": true})
	if err != nil {
		t.Fatal(err)
	}
	if response["outcome"] != "success" {
		t.Fatalf("timer reconcile = %#v", response)
	}
	if _, err := service.Dispatch(context.Background(), protocol.Request{"protocol": protocol.Version, "action": "admin-metrics"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Dispatch(context.Background(), protocol.Request{"protocol": protocol.Version, "action": "timer-reconcile", "apply": true}); protocol.AsError(err).Code != "draining" {
		t.Fatalf("sealed timer reconcile error = %v", err)
	}
}

func TestTimerOperationsDoNotWakeSchedulerOrReplayAcrossServices(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "timer-operation-identity.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	wakeCount := 0
	service := New(store, config.Defaults(), Options{Wake: func() { wakeCount++ }})
	first, err := service.Dispatch(context.Background(), protocol.Request{"protocol": protocol.Version, "action": "timer-reconcile", "apply": true})
	if err != nil {
		t.Fatal(err)
	}
	secondService := New(store, config.Defaults(), Options{})
	second, err := secondService.Dispatch(context.Background(), protocol.Request{"protocol": protocol.Version, "action": "timer-reconcile", "apply": true})
	if err != nil {
		t.Fatal(err)
	}
	firstID, firstOK := first["operation_id"].(string)
	secondID, secondOK := second["operation_id"].(string)
	if !firstOK || !secondOK || firstID == "" || secondID == "" || firstID == secondID {
		t.Fatalf("timer operation IDs = %q and %q", firstID, secondID)
	}
	if wakeCount != 0 {
		t.Fatalf("timer reconcile woke scheduler %d times", wakeCount)
	}
	for _, operationID := range []string{firstID, secondID} {
		operation, getErr := store.GetOperation(context.Background(), operationID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if operation == nil || operation.Response["outcome"] != "success" {
			t.Fatalf("timer operation %q = %#v", operationID, operation)
		}
	}
}

func TestStartupReconcileRetriesTimerBeforeNativeStart(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "timer-prestart-recovery.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	request := map[string]any{"protocol": protocol.Version, "action": "timer-reconcile", "apply": true, "generation": int64(0), "operation_id": "timer-prestart-recovery"}
	generation := int64(0)
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(ctx, "timer-prestart-recovery", "timer-reconcile", nil, &generation, map[string]any{
		"outcome": "pending", "_request": request, "_request_digest": digest,
		"_recovery_attempts": int64(1),
		"worker":             map[string]any{"state": "starting", "prepared_at": "2026-10-28T00:00:00+00:00"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	if err := service.StartupReconcile(ctx); err != nil {
		t.Fatal(err)
	}
	operation, err := store.GetOperation(ctx, "timer-prestart-recovery")
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "success" {
		t.Fatalf("recovered timer operation = %#v", operation)
	}
}

func TestIncompatibleCapabilityIsRejectedBeforeJournalClaim(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	actions := make(map[string]protocol.ActionCompatibility, len(protocol.Actions))
	for action := range protocol.Actions {
		actions[action] = protocol.ActionCompatibility{
			Request:        protocol.Schema{Schema: 1, SemanticRevision: 1},
			Response:       protocol.Schema{Schema: 1, SemanticRevision: 1},
			RequestWindow:  protocol.CompatibilityWindow{SchemaMin: 1, SchemaMax: 1, SemanticMin: 1, SemanticMax: 1},
			ResponseWindow: protocol.CompatibilityWindow{SchemaMin: 1, SchemaMax: 1, SemanticMin: 1, SemanticMax: 1},
		}
	}
	service := New(store, config.Defaults(), Options{Release: &protocol.ReleaseMetadata{ReleaseID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SourceRevision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Protocol: protocol.Version, Transport: "go-request", Actions: actions}})
	request := protocol.Request{
		"protocol": protocol.Version, "action": "acquire", "session_id": "incompatible", "generation": int64(0), "operation_id": "incompatible-operation",
		"compatibility": map[string]any{
			"release_id": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "protocol": protocol.Version, "action": "acquire",
			"request":           map[string]any{"schema": int64(2), "semantic_revision": int64(1)},
			"accepted_response": map[string]any{"schema_min": int64(1), "schema_max": int64(1), "semantic_min": int64(1), "semantic_max": int64(1)},
		},
	}
	_, err = service.Dispatch(context.Background(), request)
	if control := protocol.AsError(err); control.Code != "request_schema_incompatible" {
		t.Fatalf("dispatch error = %#v", control)
	}
	operation, err := store.GetOperation(context.Background(), "incompatible-operation")
	if err != nil {
		t.Fatal(err)
	}
	if operation != nil {
		t.Fatalf("incompatible request claimed operation: %#v", operation)
	}
}

func TestCompatibleReleaseWarningAndMetadataSurviveReplay(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	actions := make(map[string]protocol.ActionCompatibility, len(protocol.Actions))
	for action := range protocol.Actions {
		actions[action] = protocol.ActionCompatibility{
			Request: protocol.Schema{Schema: 1, SemanticRevision: 1}, Response: protocol.Schema{Schema: 1, SemanticRevision: 1},
			RequestWindow: protocol.CompatibilityWindow{SchemaMin: 1, SchemaMax: 1, SemanticMin: 1, SemanticMax: 1}, ResponseWindow: protocol.CompatibilityWindow{SchemaMin: 1, SchemaMax: 1, SemanticMin: 1, SemanticMax: 1},
		}
	}
	releaseID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	service := New(store, config.Defaults(), Options{Release: &protocol.ReleaseMetadata{ReleaseID: releaseID, SourceRevision: releaseID, Protocol: protocol.Version, Transport: "go-request", ManifestPresent: true, ArtifactsVerified: true, Actions: actions}})
	request := protocol.Request{
		"protocol": protocol.Version, "action": "drain-enter", "generation": int64(0), "operation_id": "warning-operation",
		"compatibility": map[string]any{
			"release_id": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "protocol": protocol.Version, "action": "drain-enter",
			"request":           map[string]any{"schema": int64(1), "semantic_revision": int64(1)},
			"accepted_response": map[string]any{"schema_min": int64(1), "schema_max": int64(1), "semantic_min": int64(1), "semantic_max": int64(1)},
		},
	}
	response, err := service.Dispatch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response["release"].(map[string]any)["release_id"] != releaseID || response["warnings"].([]any)[0].(map[string]any)["code"] != "compatible_release" {
		t.Fatalf("response metadata = %#v", response)
	}
	replayed, err := service.Dispatch(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if replayed["outcome"] != "replayed" || replayed["release"].(map[string]any)["release_id"] != releaseID || replayed["warnings"].([]any)[0].(map[string]any)["code"] != "compatible_release" {
		t.Fatalf("replayed response = %#v", replayed)
	}
}

func TestResidentServiceRefusesAdmissionBeforeStartupReconcile(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, config.Defaults(), Options{Resident: true})
	_, err = service.Dispatch(context.Background(), protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "before-ready", "generation": int64(0), "operation_id": "before-ready"})
	if control := protocol.AsError(err); control.Code != "startup_reconcile_required" {
		t.Fatalf("not-ready error = %#v", control)
	}
	if service.Ready() {
		t.Fatal("service became ready before startup reconciliation")
	}
}

func TestStartupReconcileRecoversOneSavedPendingOperation(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	request := protocol.Request{
		"protocol":     protocol.Version,
		"action":       "drain-enter",
		"generation":   int64(0),
		"operation_id": "recover-drain",
	}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	generation := int64(0)
	if err := store.RecordOperation(context.Background(), "recover-drain", "drain-enter", nil, &generation, map[string]any{
		"outcome":         "pending",
		"_request_digest": digest,
		"_request":        map[string]any(request),
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Resident: true})
	if err := service.StartupReconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	operation, err := store.GetOperation(context.Background(), "recover-drain")
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "success" {
		t.Fatalf("recovered operation = %#v", operation)
	}
}

func TestStartupReconcileRetriesFinishedAdminMaintenanceWorker(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "maintenance-recovery.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	request := protocol.Request{
		"protocol":     protocol.Version,
		"action":       "admin-reconcile",
		"apply":        true,
		"generation":   int64(0),
		"operation_id": "recover-admin-reconcile",
	}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	generation := int64(0)
	if err := store.RecordOperation(context.Background(), "recover-admin-reconcile", "admin-reconcile", nil, &generation, map[string]any{
		"outcome":         "pending",
		"_request_digest": digest,
		"_request":        map[string]any(request),
		"worker":          map[string]any{"state": "finished"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Resident: true})
	if err := service.StartupReconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	operation, err := store.GetOperation(context.Background(), "recover-admin-reconcile")
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "success" {
		t.Fatalf("recovered maintenance operation = %#v", operation)
	}
}

func TestWorkerFinishedKeepsFailedWorkerReconciliable(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "failed-worker.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	operationID := "failed-worker"
	if err := store.RecordOperation(context.Background(), operationID, "release", stringPointer("failed-worker-session"), nil, map[string]any{
		"outcome": "pending",
		"worker":  map[string]any{"state": "starting"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	identity := native.WorkerIdentity{PID: 123, StartTime: "start", ProcessGroup: 123}
	if err := service.WorkerFinished(context.Background(), operationID, identity, errors.New("native cleanup remained uncertain")); err != nil {
		t.Fatal(err)
	}
	later := native.WorkerIdentity{PID: 456, StartTime: "later", ProcessGroup: 456}
	if err := service.WorkerStartAllowed(context.Background(), operationID); !errors.Is(err, errUnresolvedWorker) {
		t.Fatalf("later worker gate error = %v, want unresolved-worker error", err)
	}
	if err := service.WorkerStarted(context.Background(), operationID, later); !errors.Is(err, errUnresolvedWorker) {
		t.Fatalf("later worker start error = %v, want unresolved-worker error", err)
	}
	if err := service.WorkerFinished(context.Background(), operationID, later, nil); !errors.Is(err, errUnresolvedWorker) {
		t.Fatalf("later worker finish error = %v, want unresolved-worker error", err)
	}
	operation, err := store.GetOperation(context.Background(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	worker, ok := operation.Response["worker"].(map[string]any)
	pid, validPID := protocol.Integer(worker["pid"])
	if !ok || worker["state"] != "running" || !validPID || pid != 123 {
		t.Fatalf("failed worker state = %#v, want running", operation.Response)
	}
	if worker["error"] != "native cleanup remained uncertain" {
		t.Fatalf("failed worker error = %#v", worker["error"])
	}
}

func TestWorkerStartGateRecordsAdmissionAndRejectsRetry(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "worker-admission.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	operationID := "worker-admission"
	if err := store.RecordOperation(context.Background(), operationID, "release", stringPointer("worker-session"), nil, map[string]any{
		"outcome": "pending",
		"worker":  map[string]any{"state": "starting"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	if err := service.WorkerStartAllowed(context.Background(), operationID); err != nil {
		t.Fatalf("first worker admission = %v", err)
	}
	operation, err := store.GetOperation(context.Background(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	worker := operation.Response["worker"].(map[string]any)
	if !boolValue(worker["start_attempted"]) {
		t.Fatalf("worker admission marker = %#v, want true", worker)
	}
	if err := service.WorkerStartAllowed(context.Background(), operationID); !errors.Is(err, errUnresolvedWorker) {
		t.Fatalf("second worker admission = %v, want unresolved-worker error", err)
	}
}

func TestWorkerStartGateIgnoresOnlyUnattemptedRecoveryCoordinator(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		t.Run(fmt.Sprintf("attempted=%t", attempted), func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "worker-recovery-coordinator.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			generation := int64(0)
			coordinatorWorker := map[string]any{"state": "starting"}
			if attempted {
				coordinatorWorker["start_attempted"] = true
			}
			if err := store.RecordOperation(context.Background(), "recovery-coordinator", "admin-reconcile", nil, &generation, map[string]any{
				"outcome": "pending", "worker": coordinatorWorker,
			}); err != nil {
				t.Fatal(err)
			}
			if err := store.RecordOperation(context.Background(), "recovered-operation", "admin-reconcile", nil, &generation, map[string]any{
				"outcome": "pending", "worker": map[string]any{"state": "exited"},
			}); err != nil {
				t.Fatal(err)
			}
			service := New(store, config.Defaults(), Options{})
			ctx := native.WithWorkerRecovery(context.Background(), "recovery-coordinator")
			err = service.WorkerStartAllowed(ctx, "recovered-operation")
			if attempted {
				if !errors.Is(err, errUnresolvedWorker) {
					t.Fatalf("attempted coordinator gate error = %v, want unresolved-worker error", err)
				}
			} else if err != nil {
				t.Fatalf("unattempted coordinator gate error = %v, want nil", err)
			}
		})
	}
}

func TestWorkerStartGateRearmsCompletedWorker(t *testing.T) {
	for _, stateValue := range []string{"finished", "exited"} {
		t.Run(stateValue, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "worker-rearm.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			operationID := "worker-rearm-" + stateValue
			if err := store.RecordOperation(context.Background(), operationID, "release", stringPointer("worker-rearm-session"), nil, map[string]any{
				"outcome": "pending",
				"worker":  map[string]any{"state": stateValue},
			}); err != nil {
				t.Fatal(err)
			}
			service := New(store, config.Defaults(), Options{})
			if err := service.WorkerStartAllowed(context.Background(), operationID); err != nil {
				t.Fatalf("worker admission from %s = %v", stateValue, err)
			}
			operation, err := store.GetOperation(context.Background(), operationID)
			if err != nil {
				t.Fatal(err)
			}
			worker := operation.Response["worker"].(map[string]any)
			if worker["state"] != "starting" || !boolValue(worker["start_attempted"]) {
				t.Fatalf("worker admission from %s = %#v, want durable starting marker", stateValue, worker)
			}
			if err := service.WorkerStartAllowed(context.Background(), operationID); !errors.Is(err, errUnresolvedWorker) {
				t.Fatalf("worker retry from %s = %v, want unresolved-worker error", stateValue, err)
			}
		})
	}
}

func TestWorkerStartFailedClearsAdmissionBarrier(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "worker-start-failed.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	operationID := "worker-start-failed"
	if err := store.RecordOperation(context.Background(), operationID, "release", stringPointer("worker-start-failed-session"), nil, map[string]any{
		"outcome": "pending",
		"worker":  map[string]any{"state": "starting", "start_attempted": true},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	if err := service.WorkerStartFailed(context.Background(), operationID, errors.New("exec unavailable")); err != nil {
		t.Fatal(err)
	}
	operation, err := store.GetOperation(context.Background(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	worker := operation.Response["worker"].(map[string]any)
	if worker["state"] != "exited" || worker["error"] != "exec unavailable" {
		t.Fatalf("failed worker state = %#v, want exited with cause", worker)
	}
	if err := service.WorkerStartAllowed(context.Background(), operationID); err != nil {
		t.Fatalf("worker retry after confirmed start failure = %v", err)
	}
}

func TestWorkerFinishedCannotReplaceUnresolvedIdentity(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "worker-identity.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	operationID := "worker-identity"
	if err := store.RecordOperation(context.Background(), operationID, "release", stringPointer("worker-identity-session"), nil, map[string]any{
		"outcome": "pending",
		"worker": map[string]any{
			"pid":           int64(123),
			"start_time":    "old",
			"process_group": int64(123),
			"state":         "running",
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	newIdentity := native.WorkerIdentity{PID: 456, StartTime: "new", ProcessGroup: 456}
	if err := service.WorkerFinished(context.Background(), operationID, newIdentity, nil); !errors.Is(err, errUnresolvedWorker) {
		t.Fatalf("stale worker completion = %v, want unresolved-worker error", err)
	}
}

func TestDestroyMissingInstanceDoesNotFinalizeUnresolvedWorker(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "destroy-unresolved-worker.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "destroy-unresolved-task", SessionID: "destroy-unresolved-session", Generation: 1, State: stateActive,
		InstanceName: "agent-destroy-unresolved", LeaseExpiresAt: stringPointer(now.Add(time.Hour).Format(time.RFC3339Nano)),
		CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	generation := int64(1)
	if err := store.RecordOperation(context.Background(), "destroy-unresolved-operation", "release", stringPointer("destroy-unresolved-session"), &generation, map[string]any{
		"outcome": "pending",
		"worker": map[string]any{
			"pid":           int64(123),
			"start_time":    "old",
			"process_group": int64(123),
			"state":         "running",
			"error":         "native cleanup remained uncertain",
		},
	}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{configErr: missingInstanceError()}
	service := New(store, config.Defaults(), Options{Guest: guest, Clock: func() time.Time { return now }})
	_, err = service.releaseUnlocked(context.Background(), protocol.Request{
		"session_id": "destroy-unresolved-session", "generation": int64(1), "operation_id": "destroy-unresolved-operation",
	}, true)
	if control := protocol.AsError(err); control.Code != "cleanup_incomplete" || !strings.Contains(control.Message, "worker ownership") {
		t.Fatalf("destroy error = %#v, want cleanup_incomplete worker ownership error", control)
	}
	task, err := store.GetTask(context.Background(), map[string]any{"session_id": "destroy-unresolved-session"})
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.State != stateDeleting {
		t.Fatalf("destroy task = %#v, want retained deleting task", task)
	}
}

func TestDestroyPreservesPendingAcquireWithAttemptedWorker(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "destroy-pending-worker.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	session := "destroy-pending-worker-session"
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "destroy-pending-worker-task", SessionID: session, Generation: 1, State: stateActive,
		InstanceName: "agent-destroy-pending-worker", LeaseExpiresAt: stringPointer(now.Add(time.Hour).Format(time.RFC3339Nano)),
		CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	generation := int64(1)
	if err := store.RecordOperation(context.Background(), "destroy-pending-acquire", "acquire", stringPointer(session), &generation, map[string]any{
		"outcome":         "pending",
		"_request_digest": "digest",
		"worker":          map[string]any{"state": "starting", "start_attempted": true},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Clock: func() time.Time { return now }})
	_, err = service.releaseUnlocked(context.Background(), protocol.Request{
		"session_id": session, "generation": int64(1), "operation_id": "destroy-pending-operation",
	}, true)
	control := protocol.AsError(err)
	if control.Code != "cleanup_incomplete" || !strings.Contains(control.Message, "pending acquire operation") {
		t.Fatalf("destroy error = %#v, want unresolved pending acquire", control)
	}
	operation, err := store.GetOperation(context.Background(), "destroy-pending-acquire")
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "pending" {
		t.Fatalf("pending acquire was finalized: %#v", operation)
	}
	task, err := store.GetTask(context.Background(), map[string]any{"session_id": session})
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.State != stateActive {
		t.Fatalf("task state after blocked destroy = %#v, want active", task)
	}
}

func TestReconcilePersistsVerifiedExitedWorker(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "exited-worker.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	operationID := "exited-worker"
	if err := store.RecordOperation(context.Background(), operationID, "admin-reconcile", nil, nil, map[string]any{
		"outcome": "pending",
		"worker": map[string]any{
			"pid":           int64(999999999),
			"start_time":    "missing",
			"process_group": int64(999999999),
			"state":         "running",
			"error":         "native cleanup remained uncertain",
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	pending, err := store.PendingOperations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.reconcilePendingWorkers(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	operation, err := store.GetOperation(context.Background(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	worker, ok := operation.Response["worker"].(map[string]any)
	if !ok || worker["state"] != "exited" {
		t.Fatalf("reconciled worker = %#v, want persisted exited state", operation.Response)
	}
}

func TestRecoverPendingRetriesVerifiedExitedMaintenanceWorker(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "exited-maintenance.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	request := protocol.Request{
		"protocol":     protocol.Version,
		"action":       "admin-reconcile",
		"apply":        true,
		"generation":   int64(0),
		"operation_id": "recover-exited-maintenance",
	}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	generation := int64(0)
	if err := store.RecordOperation(context.Background(), "recover-exited-maintenance", "admin-reconcile", nil, &generation, map[string]any{
		"outcome":         "pending",
		"_request_digest": digest,
		"_request":        map[string]any(request),
		"worker":          map[string]any{"state": "exited", "error": "native command failed"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	pending, err := store.PendingOperations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.recoverPending(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	operation, err := store.GetOperation(context.Background(), "recover-exited-maintenance")
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "success" {
		t.Fatalf("recovered exited maintenance operation = %#v", operation)
	}
}

func TestReleaseCancelsPendingAcquireForCachedTask(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "cached-acquire-recovery.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "cached-task", SessionID: "cached-session", Generation: 8, State: stateCached,
		InstanceName: "agent-cached", CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	request := protocol.Request{
		"protocol":     protocol.Version,
		"action":       "acquire",
		"session_id":   "cached-session",
		"generation":   int64(8),
		"operation_id": "pending-cached-acquire",
	}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	generation := int64(8)
	if err := store.RecordOperation(context.Background(), "pending-cached-acquire", "acquire", stringPointer("cached-session"), &generation, map[string]any{
		"outcome":         "pending",
		"_request_digest": digest,
		"_request":        map[string]any(request),
		"worker":          map[string]any{"state": "starting"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Resident: true, Clock: func() time.Time { return now }})
	if _, err := service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version, "action": "release", "session_id": "cached-session",
		"generation": int64(8), "operation_id": "release-cached-acquire",
	}); err != nil {
		t.Fatal(err)
	}
	operation, err := store.GetOperation(context.Background(), "pending-cached-acquire")
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "error" {
		t.Fatalf("cancelled acquire = %#v", operation)
	}
	control, ok := operation.Response["error"].(map[string]any)
	if !ok || control["code"] != "lease_released" {
		t.Fatalf("cancelled acquire error = %#v", operation.Response["error"])
	}
}

func TestReconcileDoesNotKillAnActivePendingWorker(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	operationID := "active-worker"
	if err := store.RecordOperation(context.Background(), operationID, "release", stringPointer("active-session"), nil, map[string]any{
		"outcome": "pending",
		"worker":  map[string]any{"pid": int64(999999), "start_time": "1", "state": "running"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	finish := service.markOperationActive(operationID)
	defer finish()
	pending, err := store.PendingOperations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.reconcilePendingWorkers(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	operation, err := store.GetOperation(context.Background(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	worker := operation.Response["worker"].(map[string]any)
	if worker["state"] != "running" {
		t.Fatalf("active worker was reconciled: %#v", operation.Response)
	}
}

func TestReconcileRejectsPendingWorkerWithoutProcessGroup(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "missing-process-group.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	operationID := "missing-process-group"
	if err := store.RecordOperation(context.Background(), operationID, "release", stringPointer("missing-group-session"), nil, map[string]any{
		"outcome": "pending",
		"worker":  map[string]any{"pid": int64(999999), "start_time": "1", "state": "running"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	pending, err := store.PendingOperations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.reconcilePendingWorkers(context.Background(), pending); err == nil || protocol.AsError(err).Code != "state_unverified" {
		t.Fatalf("reconcile error = %v, want state_unverified", err)
	}
	operation, err := store.GetOperation(context.Background(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	worker := operation.Response["worker"].(map[string]any)
	if worker["state"] != "running" {
		t.Fatalf("unverified worker was finalized: %#v", operation.Response)
	}
}

func TestStartupReconcileLeavesUnsafePendingAcquireBlocked(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "unsafe", "generation": int64(0), "operation_id": "unsafe-acquire"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	generation := int64(0)
	if err := store.RecordOperation(context.Background(), "unsafe-acquire", "acquire", stringPointer("unsafe"), &generation, map[string]any{
		"outcome":         "pending",
		"_request_digest": digest,
		"_request":        map[string]any(request),
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Resident: true})
	err = service.StartupReconcile(context.Background())
	if control := protocol.AsError(err); control.Code != "startup_reconcile_required" {
		t.Fatalf("startup error = %#v", control)
	}
	operation, err := store.GetOperation(context.Background(), "unsafe-acquire")
	if err != nil {
		t.Fatal(err)
	}
	if operation.Response["outcome"] != "pending" || operation.Response["_recovery_attempts"] != nil {
		t.Fatalf("unsafe acquire was recovered: %#v", operation.Response)
	}
}

func TestStartupReconcileFinalizesOrphanedAcquireWhenNativeIsMissing(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "orphaned-acquire.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session := "orphaned-acquire-session"
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": int64(0), "operation_id": "orphaned-acquire"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	generation := int64(0)
	if err := store.RecordOperation(context.Background(), "orphaned-acquire", "acquire", stringPointer(session), &generation, map[string]any{
		"outcome": "pending", "_request_digest": digest, "_request": map[string]any(request), "worker": map[string]any{"state": "exited"},
	}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{configErr: missingInstanceError()}
	service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
	if err := service.StartupReconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	operation, err := store.GetOperation(context.Background(), "orphaned-acquire")
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "error" {
		t.Fatalf("orphaned acquire = %#v", operation)
	}
	control, ok := operation.Response["error"].(map[string]any)
	if !ok || control["code"] != "acquire_partial" {
		t.Fatalf("orphaned acquire error = %#v", operation.Response["error"])
	}
	detail, ok := control["detail"].(map[string]any)
	if !ok || detail["native_init_recovered_absent"] != true || detail["outcome_verified"] != true {
		t.Fatalf("orphaned acquire detail = %#v", control["detail"])
	}
	if operation.Response["_request_digest"] != digest {
		t.Fatalf("orphaned acquire digest = %#v, want %q", operation.Response["_request_digest"], digest)
	}
}

func TestStartupReconcileLeavesOrphanedAcquireWithNativeInstanceBlocked(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "orphaned-acquire-present.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session := "orphaned-acquire-present-session"
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": int64(0), "operation_id": "orphaned-acquire-present"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	generation := int64(0)
	if err := store.RecordOperation(context.Background(), "orphaned-acquire-present", "acquire", stringPointer(session), &generation, map[string]any{
		"outcome": "pending", "_request_digest": digest, "_request": map[string]any(request),
	}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{config: map[string]any{"config": map[string]any{}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
	service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
	if control := protocol.AsError(service.StartupReconcile(context.Background())); control.Code != "startup_reconcile_required" {
		t.Fatalf("startup error = %#v", control)
	}
	operation, err := store.GetOperation(context.Background(), "orphaned-acquire-present")
	if err != nil {
		t.Fatal(err)
	}
	if operation.Response["outcome"] != "pending" {
		t.Fatalf("orphaned acquire with native instance was finalized: %#v", operation.Response)
	}
}

func TestStartupReconcileCancelsAcquireForCachedStoppedTask(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "cached-pending-acquire.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	session := "cached-pending-acquire-session"
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "cached-pending-task", SessionID: session, Generation: 2, State: stateCached,
		InstanceName: "agent-cached-pending", CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": int64(1), "operation_id": "cached-pending-acquire"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	generation := int64(1)
	if err := store.RecordOperation(context.Background(), "cached-pending-acquire", "acquire", stringPointer(session), &generation, map[string]any{
		"outcome": "pending", "_request_digest": digest, "_request": map[string]any(request), "worker": map[string]any{"state": "exited"},
	}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{
		status: "Stopped",
		config: map[string]any{
			"config":  map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "cached-pending-task", "user.agent-vm.session": session, "user.agent-vm.generation": "2"},
			"devices": map[string]any{}, "expanded_devices": map[string]any{},
		},
	}
	service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
	if err := service.StartupReconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	operation, err := store.GetOperation(context.Background(), "cached-pending-acquire")
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "error" {
		t.Fatalf("cached pending acquire = %#v", operation)
	}
	control, ok := operation.Response["error"].(map[string]any)
	if !ok || control["code"] != "lease_released" {
		t.Fatalf("cached pending acquire error = %#v", operation.Response["error"])
	}
}

func TestStartupReconcileVerifiesDurableActiveGuest(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "native-state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "active-task", SessionID: "active-session", Generation: 1, State: stateActive,
		InstanceName: "agent-active", LeaseExpiresAt: stringPointer(now.Add(time.Hour).Format(time.RFC3339Nano)),
		CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{
		status: "Running",
		config: map[string]any{
			"config":           map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "active-task", "user.agent-vm.session": "active-session", "user.agent-vm.generation": "1"},
			"devices":          map[string]any{},
			"expanded_devices": map[string]any{},
		},
	}
	service := New(store, config.Defaults(), Options{Guest: guest, Resident: true, Clock: func() time.Time { return now }})
	if err := service.StartupReconcile(context.Background()); err != nil {
		t.Fatalf("startup reconcile = %v", err)
	}
	guest.status = "Stopped"
	service = New(store, config.Defaults(), Options{Guest: guest, Resident: true, Clock: func() time.Time { return now }})
	if control := protocol.AsError(service.StartupReconcile(context.Background())); control.Code != "startup_reconcile_required" {
		t.Fatalf("stopped active guest error = %#v", control)
	}
}

func TestDestroyFinalizesWhenIncusConfirmsTheInstanceIsMissing(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "destroy-state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "destroy-task", SessionID: "destroy-session", Generation: 1, State: stateActive,
		InstanceName: "agent-destroy", LeaseExpiresAt: stringPointer(now.Add(time.Hour).Format(time.RFC3339Nano)),
		CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{configErr: missingInstanceError()}
	service := New(store, config.Defaults(), Options{Guest: guest, Clock: func() time.Time { return now }})
	response, err := service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version, "action": "destroy", "session_id": "destroy-session", "generation": int64(1), "operation_id": "destroy-missing",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := response["result"].(map[string]any)
	if result["state"] != stateAbsent {
		t.Fatalf("destroy response = %#v", response)
	}
	task, err := store.GetTask(context.Background(), map[string]any{"session_id": "destroy-session"})
	if err != nil {
		t.Fatal(err)
	}
	if task != nil {
		t.Fatalf("destroy left task = %#v", task)
	}
}

func TestDestroySkipsStopForStoppedGuest(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "stopped-destroy.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "stopped-destroy-task", SessionID: "stopped-destroy-session", Generation: 1, State: stateCached,
		InstanceName: "agent-stopped-destroy", CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{
		status:  "Stopped",
		stopErr: errors.New("stop must not be called for a stopped guest"),
		config: map[string]any{
			"config":           map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "stopped-destroy-task", "user.agent-vm.session": "stopped-destroy-session", "user.agent-vm.generation": "1"},
			"devices":          map[string]any{},
			"expanded_devices": map[string]any{},
		},
	}
	service := New(store, config.Defaults(), Options{Guest: guest, Clock: func() time.Time { return now }})
	response, err := service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version, "action": "destroy", "session_id": "stopped-destroy-session", "generation": int64(1), "operation_id": "destroy-stopped",
	})
	if err != nil {
		t.Fatal(err)
	}
	if response["result"].(map[string]any)["state"] != stateAbsent {
		t.Fatalf("destroy response = %#v", response)
	}
}

func TestReconcileFinalizesPendingDestroyAfterCleanup(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "pending-destroy.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	session := "pending-destroy-session"
	generation := int64(1)
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "pending-destroy-task", SessionID: session, Generation: generation, State: stateDeleting,
		InstanceName: "agent-pending-destroy", CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: `{"cpu":0,"memory_mib":0,"disk_gib":0}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{
		"protocol": protocol.Version, "action": "destroy", "session_id": session,
		"generation": generation, "operation_id": "destroy-pending",
	}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(context.Background(), "destroy-pending", "destroy", stringPointer(session), &generation, map[string]any{
		"outcome": "pending", "_request": request, "_request_digest": digest,
	}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{
		status: "Stopped",
		config: map[string]any{
			"config":           map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "pending-destroy-task", "user.agent-vm.session": session, "user.agent-vm.generation": "1"},
			"devices":          map[string]any{},
			"expanded_devices": map[string]any{},
		},
	}
	service := New(store, config.Defaults(), Options{Guest: guest, Clock: func() time.Time { return now }})
	if _, err := service.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if task, err := store.GetTask(context.Background(), map[string]any{"session_id": session}); err != nil {
		t.Fatal(err)
	} else if task != nil {
		t.Fatalf("reconciled task = %#v", task)
	}
	operation, err := store.GetOperation(context.Background(), "destroy-pending")
	if err != nil {
		t.Fatal(err)
	}
	if operation.Response["outcome"] != "success" {
		t.Fatalf("reconciled operation = %#v", operation.Response)
	}
}

func TestReconcileFinalizesPendingDestroyAfterAbsentCommit(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "pending-absent-destroy.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	session := "pending-absent-destroy-session"
	generation := int64(1)
	if err := store.InsertTask(ctx, state.Task{
		TaskID: "pending-absent-destroy-task", SessionID: session, Generation: generation, State: stateAbsent,
		InstanceName: "agent-pending-absent-destroy", CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: `{"cpu":0,"memory_mib":0,"disk_gib":0}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{
		"protocol": protocol.Version, "action": "destroy", "session_id": session,
		"generation": generation, "operation_id": "destroy-absent-pending",
	}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(ctx, "destroy-absent-pending", "destroy", stringPointer(session), &generation, map[string]any{
		"outcome": "pending", "_request": request, "_request_digest": digest, "worker": map[string]any{"state": "exited", "start_attempted": false},
	}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{configErr: &native.CommandError{Result: native.Result{Code: 1, Stderr: []byte("Error: Instance not found")}, Cause: exec.Command("false").Run()}}
	service := New(store, config.Defaults(), Options{Guest: guest, Clock: func() time.Time { return now }})
	if _, err := service.reconcile(ctx, true); err != nil {
		t.Fatal(err)
	}
	if task, err := store.GetTask(ctx, map[string]any{"session_id": session}); err != nil {
		t.Fatal(err)
	} else if task != nil {
		t.Fatalf("reconciled task = %#v", task)
	}
	operation, err := store.GetOperation(ctx, "destroy-absent-pending")
	if err != nil {
		t.Fatal(err)
	}
	if operation.Response["outcome"] != "success" {
		t.Fatalf("reconciled operation = %#v", operation.Response)
	}
}

func TestReconcileFinalizesPendingDestroyWithoutTaskRow(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "pending-no-task-destroy.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	session := "pending-no-task-destroy-session"
	generation := int64(1)
	if allocated, err := store.NextGeneration(ctx, session); err != nil || allocated != generation {
		t.Fatalf("allocated generation = %d, error = %v", allocated, err)
	}
	request := map[string]any{
		"protocol": protocol.Version, "action": "destroy", "session_id": session,
		"generation": generation, "operation_id": "destroy-no-task-pending",
	}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(ctx, "destroy-no-task-pending", "destroy", stringPointer(session), &generation, map[string]any{
		"outcome": "pending", "_request": request, "_request_digest": digest, "worker": map[string]any{"state": "exited", "start_attempted": false},
	}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{configErr: &native.CommandError{Result: native.Result{Code: 1, Stderr: []byte("Error: Instance not found")}, Cause: exec.Command("false").Run()}}
	service := New(store, config.Defaults(), Options{Guest: guest})
	if _, err := service.reconcile(ctx, true); err != nil {
		t.Fatal(err)
	}
	operation, err := store.GetOperation(ctx, "destroy-no-task-pending")
	if err != nil {
		t.Fatal(err)
	}
	if operation.Response["outcome"] != "success" {
		t.Fatalf("reconciled operation = %#v", operation.Response)
	}
}

func TestPendingAbsentDestroyRemainsPendingWithoutSafeEvidence(t *testing.T) {
	for _, mode := range []string{"no-native", "no-worker", "starting", "finished-no-identity", "partial-identity", "other-worker", "active", "session-locked", "generation", "missing-tombstone", "stale-tombstone"} {
		t.Run(mode, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			session, generation, operationID := "absent-destroy-"+mode, int64(1), "destroy-"+mode
			now := nowString(time.Now())
			task := state.Task{TaskID: "task-" + mode, SessionID: session, Generation: generation, State: stateAbsent, InstanceName: "instance-" + mode, CreatedAt: now, UpdatedAt: now, ResourcesJSON: `{"cpu":0,"memory_mib":0,"disk_gib":0}`, ResourceReservationJSON: `{}`}
			if mode == "generation" {
				task.Generation++
			}
			if mode == "stale-tombstone" {
				for range 2 {
					if _, err := store.NextGeneration(ctx, session); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode != "missing-tombstone" && mode != "stale-tombstone" {
				if err := store.InsertTask(ctx, task); err != nil {
					t.Fatal(err)
				}
			}
			request := protocol.Request{"protocol": protocol.Version, "action": "destroy", "session_id": session, "generation": generation, "operation_id": operationID}
			digest, err := protocol.Digest(request)
			if err != nil {
				t.Fatal(err)
			}
			response := map[string]any{"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest, "worker": map[string]any{"state": "exited", "start_attempted": false}}
			if mode == "no-worker" {
				delete(response, "worker")
			}
			if mode == "starting" {
				response["worker"] = map[string]any{"state": "starting"}
			}
			if mode == "finished-no-identity" {
				response["worker"] = map[string]any{"state": "finished"}
			}
			if mode == "partial-identity" {
				response["worker"] = map[string]any{"state": "exited", "start_attempted": false, "pid": "invalid"}
			}
			if err := store.RecordOperation(ctx, operationID, "destroy", &session, &generation, response); err != nil {
				t.Fatal(err)
			}
			if mode == "other-worker" {
				if err := store.RecordOperation(ctx, "other-acquire", "acquire", &session, &generation, map[string]any{"outcome": "pending", "worker": map[string]any{"state": "finished"}}); err != nil {
					t.Fatal(err)
				}
			}
			var guest native.Guest = &startupGuest{configErr: missingInstanceError()}
			if mode == "no-native" {
				guest = nil
			}
			service := New(store, config.Defaults(), Options{Guest: guest})
			if mode == "active" {
				done := service.markOperationActive(operationID)
				defer done()
			}
			if mode == "session-locked" {
				unlock := service.lockAction("session", protocol.Request{"session_id": session})
				defer unlock()
			}
			pending, err := store.PendingOperations(ctx)
			if err != nil {
				t.Fatal(err)
			}
			err = service.reconcilePendingAbsentDestroy(ctx, pending)
			if mode != "session-locked" && err == nil {
				t.Fatal("unsafe destroy was accepted")
			}
			operation, err := store.GetOperation(ctx, operationID)
			if err != nil || operation == nil || operation.Response["outcome"] != "pending" {
				t.Fatalf("operation = %#v, err = %v", operation, err)
			}
			current, err := store.GetTask(ctx, map[string]any{"session_id": session})
			noTask := mode == "missing-tombstone" || mode == "stale-tombstone"
			if err != nil || (noTask && current != nil) || (!noTask && (current == nil || current.State != stateAbsent || current.Generation != task.Generation)) {
				t.Fatalf("task = %#v, err = %v", current, err)
			}
		})
	}
}

func TestStartupPressureRefusesBeforeDurableAcquire(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.ImageFingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	service := New(store, cfg, Options{CPU: staticCPUObserver{observation: observe.CPUObservation{
		Capacity: 1,
		Full:     observe.PSIValues{Avg10: 20},
		Some:     observe.PSIValues{Avg10: 20},
	}}})
	_, err = service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version, "action": "acquire", "session_id": "pressure", "generation": int64(0), "operation_id": "acquire-pressure",
	})
	if control := protocol.AsError(err); control.Code != "resource_pressure" {
		t.Fatalf("pressure error = %#v", control)
	}
	if task, getErr := store.GetTask(context.Background(), map[string]any{"session_id": "pressure"}); getErr != nil || task != nil {
		t.Fatalf("pressure acquire left task: task=%#v err=%v", task, getErr)
	}
}

// A command can create the instance and still return an uncertain result.
func TestAcquireInitUncertainResultCanReconcileOwnedInstance(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.VMIPStart = "10.0.0.10"
	cfg.VMIPEnd = "10.0.0.20"
	guest := &startupGuest{initErr: errors.New("inspect native process tree: read /proc/123/stat: no such process"), status: "Stopped", config: map[string]any{"config": map[string]any{}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
	service := New(store, cfg, Options{Guest: guest})
	_, err = service.Dispatch(context.Background(), protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "init-uncertain", "generation": int64(0), "operation_id": "init-uncertain-acquire"})
	if protocol.AsError(err).Code != "native_failed" {
		t.Fatalf("acquire error = %v", err)
	}
	if err := service.recordWorkerState(context.Background(), "init-uncertain-acquire", map[string]any{"state": "exited"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.reconcile(context.Background(), true); err != nil {
		t.Fatalf("reconcile after uncertain init = %v", err)
	}
	task, err := store.GetTask(context.Background(), map[string]any{"session_id": "init-uncertain"})
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.State != stateCached || task.ResourceReservationJSON != "{}" {
		t.Fatalf("reconciled task = %#v", task)
	}
}

func TestLifecycleImageUnpackFailureIsCountedForUnknownOutcome(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "lifecycle-image-unpack-audit.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.VMIPStart = "10.0.0.10"
	cfg.VMIPEnd = "10.0.0.20"
	guest := &startupGuest{initErr: &native.CommandError{
		Cause:  errors.New("exit status 1"),
		Result: native.Result{Code: 1, Stderr: []byte("Error: failed to unpack image rootfs")},
	}}
	service := New(store, cfg, Options{Guest: guest})
	_, err = service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version, "action": "acquire", "session_id": "unpack-audit", "generation": int64(0), "operation_id": "unpack-audit-acquire",
	})
	if protocol.AsError(err).Code != "native_failed" {
		t.Fatalf("lifecycle image unpack error = %#v", err)
	}
	summary, err := store.AuditSummary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if summary.ImageUnpackFailures["native-init|image_unpack_failed"] != 1 {
		t.Fatalf("lifecycle image unpack summary = %#v", summary.ImageUnpackFailures)
	}
}

func TestAcquireFailedBeforeNativeCreationReclaimsReservation(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.VMIPStart = "10.0.0.10"
	cfg.VMIPEnd = "10.0.0.20"
	guest := &startupGuest{initErr: errors.New("init failed"), configErr: missingInstanceError()}
	service := New(store, cfg, Options{Guest: guest})
	_, err = service.Dispatch(context.Background(), protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "init-absent", "generation": int64(0), "operation_id": "init-absent-acquire"})
	if protocol.AsError(err).Code != "native_failed" {
		t.Fatalf("acquire error = %v", err)
	}
	// The fake does not run a native worker; model a worker verified exited.
	if err := service.recordWorkerState(context.Background(), "init-absent-acquire", map[string]any{"state": "exited"}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.reconcile(context.Background(), true); err != nil {
		t.Fatalf("reconcile after absent init = %v", err)
	}
	task, err := store.GetTask(context.Background(), map[string]any{"session_id": "init-absent"})
	if err != nil {
		t.Fatal(err)
	}
	if task != nil {
		t.Fatalf("missing instance retained task and reservation: %#v", task)
	}
}

func TestFailedInitCleanupRefusesMismatchedOwnership(t *testing.T) {
	for _, field := range []string{"user.agent-vm.task", "user.agent-vm.session", "user.agent-vm.generation", "user.agent-vm.managed"} {
		t.Run(field, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			cfg := config.Defaults()
			cfg.VMIPStart = "10.0.0.10"
			cfg.VMIPEnd = "10.0.0.20"
			guest := &startupGuest{initErr: errors.New("uncertain init"), status: "Stopped", config: map[string]any{"config": map[string]any{}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
			service := New(store, cfg, Options{Guest: guest})
			ctx := context.Background()
			_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "mismatch", "generation": int64(0), "operation_id": "mismatch-acquire"})
			if protocol.AsError(err).Code != "native_failed" {
				t.Fatalf("acquire: %v", err)
			}
			if err := service.recordWorkerState(ctx, "mismatch-acquire", map[string]any{"state": "exited"}); err != nil {
				t.Fatal(err)
			}
			guest.config["config"].(map[string]any)[field] = "other"
			if _, err := service.reconcile(ctx, true); protocol.AsError(err).Code != "cleanup_incomplete" {
				t.Fatalf("reconcile error = %v", err)
			}
			task, err := store.GetTask(ctx, map[string]any{"session_id": "mismatch"})
			if err != nil || task == nil || task.ResourceReservationJSON == "{}" {
				t.Fatalf("task = %#v, error = %v", task, err)
			}
		})
	}
}

func TestFailedInitMissingInstanceKeepsUnresolvedWorkerReservation(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := config.Defaults()
	cfg.VMIPStart = "10.0.0.10"
	cfg.VMIPEnd = "10.0.0.20"
	guest := &startupGuest{initErr: errors.New("uncertain init"), configErr: missingInstanceError()}
	service := New(store, cfg, Options{Guest: guest})
	ctx := context.Background()
	_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "unresolved", "generation": int64(0), "operation_id": "unresolved-acquire"})
	if protocol.AsError(err).Code != "native_failed" {
		t.Fatalf("acquire: %v", err)
	}
	if err := service.recordWorkerState(ctx, "unresolved-acquire", map[string]any{"state": "starting", "start_attempted": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.reconcile(ctx, true); err == nil {
		t.Fatal("unresolved worker allowed cleanup")
	}
	task, err := store.GetTask(ctx, map[string]any{"session_id": "unresolved"})
	if err != nil || task == nil || task.ResourceReservationJSON == "{}" {
		t.Fatalf("task = %#v, error = %v", task, err)
	}
}

func TestReconcileRejectsLegacyFailedFinishedWorkerWithoutIdentity(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session := "legacy-worker"
	if err := store.RecordOperation(context.Background(), "legacy-worker-op", "acquire", &session, nil, map[string]any{"outcome": "pending", "worker": map[string]any{"state": "finished", "error": "inspect native process tree: read /proc/123/stat: no such process"}}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	pending, err := store.PendingOperations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.reconcilePendingWorkers(context.Background(), pending); err == nil || protocol.AsError(err).Code != "state_unverified" {
		t.Fatalf("reconcile error = %v, want unverified legacy identity", err)
	}
}

func TestStartupDoesNotFinalizeActiveOrphanedAcquire(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session := "active-orphan"
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": int64(0), "operation_id": "active-orphan-acquire"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	generation := int64(0)
	if err := store.RecordOperation(context.Background(), "active-orphan-acquire", "acquire", &session, &generation, map[string]any{"outcome": "pending", "_request_digest": digest, "_request": map[string]any(request), "worker": map[string]any{"state": "starting"}}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{configErr: missingInstanceError()}
	service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
	finish := service.markOperationActive("active-orphan-acquire")
	defer finish()
	if err := service.StartupReconcile(context.Background()); err == nil {
		t.Fatal("startup admitted while acquire active")
	}
	op, err := store.GetOperation(context.Background(), "active-orphan-acquire")
	if err != nil {
		t.Fatal(err)
	}
	if op.Response["outcome"] != "pending" {
		t.Fatalf("active operation finalized: %#v", op.Response)
	}
}

func TestReconcileVerifiesLegacyFailedFinishedWorker(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session := "legacy-worker-verified"
	operationID := "legacy-worker-verified-op"
	if err := store.RecordOperation(context.Background(), operationID, "acquire", &session, nil, map[string]any{"outcome": "pending", "worker": map[string]any{"state": "finished", "pid": int64(1 << 30), "process_group": int64(1 << 30), "start_time": "1", "error": "inspect native process tree"}}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	pending, err := store.PendingOperations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := service.reconcilePendingWorkers(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	operation, err := store.GetOperation(context.Background(), operationID)
	if err != nil {
		t.Fatal(err)
	}
	if worker := operation.Response["worker"].(map[string]any); worker["state"] != "exited" {
		t.Fatalf("worker = %#v, want verified exited", worker)
	}
}

func TestReleaseDoesNotTouchOwnedGuestWithUnresolvedAcquireWorker(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	session := "pending-owned"
	generation := int64(1)
	if err := store.InsertTask(context.Background(), state.Task{TaskID: "pending-owned-task", SessionID: session, Generation: generation, State: stateFailed, InstanceName: "pending-owned-instance", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(context.Background(), "pending-owned-acquire", "acquire", &session, &generation, map[string]any{"outcome": "pending", "worker": map[string]any{"state": "starting", "start_attempted": true}}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{status: "Stopped", config: map[string]any{"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "pending-owned-task", "user.agent-vm.session": session, "user.agent-vm.generation": "1"}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
	service := New(store, config.Defaults(), Options{Guest: guest})
	_, err = service.releaseUnlocked(context.Background(), protocol.Request{"session_id": session, "generation": generation}, false)
	if protocol.AsError(err).Code != "cleanup_incomplete" {
		t.Fatalf("release error = %v", err)
	}
	task, err := store.GetTask(context.Background(), map[string]any{"session_id": session})
	if err != nil || task == nil || task.ResourceReservationJSON == "{}" || guest.statusCalls != 0 {
		t.Fatalf("task = %#v, status calls = %d, error = %v", task, guest.statusCalls, err)
	}
}

func TestWorkerRecoveryRereadsJournalAfterSessionLock(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session := "stale-worker"
	operationID := "stale-worker-op"
	generation := int64(1)
	if err := store.RecordOperation(context.Background(), operationID, "acquire", &session, &generation, map[string]any{"outcome": "pending", "worker": map[string]any{"state": "running"}}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingOperations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(context.Background(), operationID, "acquire", &session, &generation, map[string]any{"outcome": "success"}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	if err := service.reconcilePendingWorkers(context.Background(), pending); err != nil {
		t.Fatalf("stale pending snapshot triggered worker recovery: %v", err)
	}
	op, err := store.GetOperation(context.Background(), operationID)
	if err != nil || op.Response["outcome"] != "success" {
		t.Fatalf("operation = %#v, error = %v", op, err)
	}
}

func TestStartupKeepsStaleAcquirePendingForCachedGeneration(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	session := "stale-cached"
	generation := int64(0)
	if err := store.InsertTask(context.Background(), state.Task{TaskID: "stale-cached-task", SessionID: session, Generation: 3, State: stateCached, InstanceName: "stale-cached-instance", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{}`, ResourceReservationJSON: `{}`}); err != nil {
		t.Fatal(err)
	}
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": generation, "operation_id": "stale-cached-acquire"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(context.Background(), "stale-cached-acquire", "acquire", &session, &generation, map[string]any{"outcome": "pending", "_request_digest": digest, "_request": map[string]any(request), "worker": map[string]any{"state": "exited"}}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{status: "Stopped", config: map[string]any{"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "stale-cached-task", "user.agent-vm.session": session, "user.agent-vm.generation": "3"}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
	service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
	if err := service.StartupReconcile(context.Background()); err == nil {
		t.Fatal("stale acquire was silently finalized")
	}
	op, err := store.GetOperation(context.Background(), "stale-cached-acquire")
	if err != nil || op.Response["outcome"] != "pending" {
		t.Fatalf("operation = %#v, error = %v", op, err)
	}
}

func TestStartupRecoversOwnedFailedInitWithVerifiedLegacyWorker(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	session := "startup-init"
	generation := int64(0)
	lastError := `{"phase":"native-init","cause":"inspect native process tree: read /proc/123/stat: no such process"}`
	if err := store.InsertTask(context.Background(), state.Task{TaskID: "startup-init-task", SessionID: session, Generation: 1, State: stateClosing, InstanceName: "startup-init-instance", CreatedAt: nowString(now), UpdatedAt: nowString(now), LastError: &lastError, ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`}); err != nil {
		t.Fatal(err)
	}
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": generation, "operation_id": "startup-init-acquire"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(context.Background(), "startup-init-acquire", "acquire", &session, &generation, map[string]any{"outcome": "pending", "_request_digest": digest, "_request": map[string]any(request), "worker": map[string]any{"state": "finished", "pid": int64(1 << 30), "process_group": int64(1 << 30), "start_time": "1", "error": "inspect native process tree"}}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{status: "Stopped", config: map[string]any{"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "startup-init-task", "user.agent-vm.session": session, "user.agent-vm.generation": "1"}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
	service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
	if err := service.StartupReconcile(context.Background()); err != nil {
		t.Fatalf("startup = %v", err)
	}
	task, err := store.GetTask(context.Background(), map[string]any{"session_id": session})
	if err != nil || task == nil || task.State != stateCached || task.ResourceReservationJSON != "{}" {
		t.Fatalf("task = %#v, error = %v", task, err)
	}
	op, err := store.GetOperation(context.Background(), "startup-init-acquire")
	if err != nil || op.Response["outcome"] != "error" {
		t.Fatalf("operation = %#v, error = %v", op, err)
	}
}

func TestAcquireBindsJournalToRecreatedTaskGeneration(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	session := "recreated-session"
	for i := 0; i < 2; i++ {
		if _, err := store.NextGeneration(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.Defaults()
	cfg.VMIPStart = "10.0.0.10"
	cfg.VMIPEnd = "10.0.0.20"
	guest := &startupGuest{initErr: errors.New("uncertain init"), status: "Stopped", config: map[string]any{"config": map[string]any{}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
	service := New(store, cfg, Options{Guest: guest})
	_, err = service.Dispatch(ctx, protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": int64(0), "operation_id": "recreated-acquire"})
	if protocol.AsError(err).Code != "native_failed" {
		t.Fatalf("acquire = %v", err)
	}
	task, err := store.GetTask(ctx, map[string]any{"session_id": session})
	if err != nil || task == nil || task.Generation != 3 {
		t.Fatalf("task = %#v, error = %v", task, err)
	}
	operation, err := store.GetOperation(ctx, "recreated-acquire")
	if err != nil {
		t.Fatal(err)
	}
	generation, ok := protocol.Integer(operation.Response["_task_generation"])
	if !ok || generation != task.Generation || operation.Response["_task_id"] != task.TaskID {
		t.Fatalf("operation missing task binding: %#v", operation.Response)
	}
	if err := service.recordWorkerState(ctx, "recreated-acquire", map[string]any{"state": "exited"}); err != nil {
		t.Fatal(err)
	}
	if err := service.StartupReconcile(ctx); err != nil {
		t.Fatalf("startup recovery = %v", err)
	}
	operation, err = store.GetOperation(ctx, "recreated-acquire")
	if err != nil || operation.Response["outcome"] != "error" {
		t.Fatalf("recovered operation = %#v, error = %v", operation, err)
	}
}

func TestStartupKeepsReservationForPreviousGeneration(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	previous := int64(1)
	session := "previous-generation"
	lastError := `{"phase":"native-ownership"}`
	if err := store.InsertTask(context.Background(), state.Task{TaskID: "previous-task", SessionID: session, Generation: 2, PreviousGeneration: &previous, State: stateClosing, InstanceName: "previous-instance", LastError: &lastError, CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{status: "Stopped", config: map[string]any{"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "previous-task", "user.agent-vm.session": session, "user.agent-vm.generation": "1"}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
	service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
	if err := service.StartupReconcile(context.Background()); err == nil {
		t.Fatal("startup accepted a stale native generation")
	}
	task, err := store.GetTask(context.Background(), map[string]any{"session_id": session})
	if err != nil || task == nil || task.State != stateClosing || task.ResourceReservationJSON == "{}" {
		t.Fatalf("task = %#v, error = %v", task, err)
	}
}

func TestWorkerRecoverySkipsBusySession(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session := "busy-session"
	generation := int64(0)
	if err := store.RecordOperation(context.Background(), "busy-op", "acquire", &session, &generation, map[string]any{"outcome": "pending", "worker": map[string]any{"state": "starting", "start_attempted": true}}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingOperations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{})
	unlock := service.lockAction("session", protocol.Request{"session_id": session})
	defer unlock()
	if err := service.reconcilePendingWorkers(context.Background(), pending); err != nil {
		t.Fatalf("busy session should be skipped: %v", err)
	}
	op, err := store.GetOperation(context.Background(), "busy-op")
	if err != nil || op.Response["outcome"] != "pending" {
		t.Fatalf("operation = %#v, error = %v", op, err)
	}
}

func TestReconcileClosingTaskHonorsPendingDestroy(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	session := "closing-destroy"
	generation := int64(1)
	if err := store.InsertTask(context.Background(), state.Task{TaskID: "closing-destroy-task", SessionID: session, Generation: generation, State: stateClosing, InstanceName: "closing-destroy-instance", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMeta(context.Background(), "draining", "1"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMeta(context.Background(), "drain_owner", "release-recovery-test"); err != nil {
		t.Fatal(err)
	}
	request := protocol.Request{"protocol": protocol.Version, "action": "destroy", "session_id": session, "generation": generation, "operation_id": "closing-destroy-op"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(context.Background(), "closing-destroy-op", "destroy", &session, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest, "worker": map[string]any{"state": "finished"}}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{status: "Stopped", config: map[string]any{"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "closing-destroy-task", "user.agent-vm.session": session, "user.agent-vm.generation": "1"}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
	service := New(store, config.Defaults(), Options{Guest: guest})
	if _, err := service.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(context.Background(), map[string]any{"session_id": session})
	if err != nil || task != nil || guest.deleteCalls != 1 {
		t.Fatalf("destroy should remove task and instance: %#v, calls=%d, %v", task, guest.deleteCalls, err)
	}
	op, err := store.GetOperation(context.Background(), "closing-destroy-op")
	if err != nil || op.Response["outcome"] != "success" {
		t.Fatalf("destroy operation: %#v, %v", op, err)
	}
}

func TestStartupReconcilesClosingDuringReleaseDrain(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := nowString(time.Now())
	session := "closing-release-drain"
	task := state.Task{TaskID: "closing-task", SessionID: session, Generation: 1, State: stateClosing, InstanceName: "closing-instance", CreatedAt: now, UpdatedAt: now, ResourcesJSON: `{}`, ResourceReservationJSON: `{}`}
	if err := store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMeta(ctx, "draining", "1"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMeta(ctx, "drain_owner", "release-recovery-test"); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{status: "Stopped", config: map[string]any{"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": task.TaskID, "user.agent-vm.session": session, "user.agent-vm.generation": "1"}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
	service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
	if err := service.StartupReconcile(ctx); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetTask(ctx, map[string]any{"session_id": session})
	if err != nil || current == nil || current.State != stateCached || current.LeaseExpiresAt != nil {
		t.Fatalf("task=%#v, error=%v", current, err)
	}
	if guest.deleteCalls != 0 {
		t.Fatal("release cleanup deleted the instance")
	}
}

func TestReconcileClosingRefusesUnverifiedDestroy(t *testing.T) {
	for _, mode := range []string{"bad-digest", "worker-running", "wrong-request", "incompatible"} {
		t.Run(mode, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			now := time.Now()
			session := "unsafe-destroy"
			generation := int64(1)
			if err := store.InsertTask(context.Background(), state.Task{TaskID: "unsafe-task", SessionID: session, Generation: generation, State: stateClosing, InstanceName: "unsafe-instance", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{"cpu":2}`, ResourceReservationJSON: `{"cpu":2}`}); err != nil {
				t.Fatal(err)
			}
			request := protocol.Request{"protocol": protocol.Version, "action": "destroy", "session_id": session, "generation": generation, "operation_id": "unsafe-op"}
			digest, err := protocol.Digest(request)
			if err != nil {
				t.Fatal(err)
			}
			worker := map[string]any{"state": "finished"}
			if mode == "bad-digest" {
				digest = "invalid"
			}
			if mode == "worker-running" {
				worker["state"] = "starting"
				worker["start_attempted"] = true
			}
			if mode == "wrong-request" {
				request["session_id"] = "other"
				digest, err = protocol.Digest(request)
				if err != nil {
					t.Fatal(err)
				}
			}
			var release *protocol.ReleaseMetadata
			if mode == "incompatible" {
				request["compatibility"] = map[string]any{
					"release_id": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "protocol": protocol.Version, "action": "destroy",
					"request":           map[string]any{"schema": int64(2), "semantic_revision": int64(1)},
					"accepted_response": map[string]any{"schema_min": int64(1), "schema_max": int64(1), "semantic_min": int64(1), "semantic_max": int64(1)},
				}
				digest, err = protocol.Digest(request)
				if err != nil {
					t.Fatal(err)
				}
				release = &protocol.ReleaseMetadata{ReleaseID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Protocol: protocol.Version, Actions: map[string]protocol.ActionCompatibility{
					"destroy": {Request: protocol.Schema{Schema: 1, SemanticRevision: 1}, Response: protocol.Schema{Schema: 1, SemanticRevision: 1}, RequestWindow: protocol.CompatibilityWindow{SchemaMin: 1, SchemaMax: 1, SemanticMin: 1, SemanticMax: 1}},
				}}
			}
			if err := store.RecordOperation(context.Background(), "unsafe-op", "destroy", &session, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest, "worker": worker}); err != nil {
				t.Fatal(err)
			}
			guest := &startupGuest{status: "Stopped", config: map[string]any{"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": "unsafe-task", "user.agent-vm.session": session, "user.agent-vm.generation": "1"}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
			service := New(store, config.Defaults(), Options{Guest: guest, Release: release})
			if _, err := service.reconcile(context.Background(), true); err == nil {
				t.Fatal("unverified destroy was accepted")
			}
			task, err := store.GetTask(context.Background(), map[string]any{"session_id": session})
			if err != nil || task == nil || task.State != stateClosing || task.ResourceReservationJSON == "{}" || guest.statusCalls != 0 {
				t.Fatalf("task=%#v, calls=%d, error=%v", task, guest.statusCalls, err)
			}
		})
	}
}

func TestReconcileClosingDestroyRejectsUnmarkedInstance(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	session := "unmarked-destroy"
	generation := int64(1)
	if err := store.InsertTask(context.Background(), state.Task{TaskID: "unmarked-task", SessionID: session, Generation: generation, State: stateClosing, InstanceName: "unmarked-instance", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{"cpu":2}`, ResourceReservationJSON: `{"cpu":2}`}); err != nil {
		t.Fatal(err)
	}
	request := protocol.Request{"protocol": protocol.Version, "action": "destroy", "session_id": session, "generation": generation, "operation_id": "unmarked-op"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(context.Background(), "unmarked-op", "destroy", &session, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest, "worker": map[string]any{"state": "finished"}}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{status: "Stopped", config: map[string]any{"config": map[string]any{}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
	service := New(store, config.Defaults(), Options{Guest: guest})
	if _, err := service.reconcile(context.Background(), true); protocol.AsError(err).Code != "cleanup_incomplete" {
		t.Fatalf("reconcile error=%v", err)
	}
	task, err := store.GetTask(context.Background(), map[string]any{"session_id": session})
	if err != nil || task == nil || task.State != stateDeleting || task.ResourceReservationJSON == "{}" || guest.deleteCalls != 0 {
		t.Fatalf("task=%#v, delete calls=%d, error=%v", task, guest.deleteCalls, err)
	}
	op, err := store.GetOperation(context.Background(), "unmarked-op")
	if err != nil || op.Response["outcome"] != "pending" {
		t.Fatalf("operation=%#v, error=%v", op, err)
	}
}

func TestNotReadyAcquireIsRecordedAsDefinitiveError(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service := New(store, config.Defaults(), Options{Resident: true})
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": "not-ready-session", "generation": int64(0), "operation_id": "not-ready-acquire"}
	_, err = service.Dispatch(context.Background(), request)
	if protocol.AsError(err).Code != "startup_reconcile_required" {
		t.Fatalf("acquire error = %v", err)
	}
	op, err := store.GetOperation(context.Background(), "not-ready-acquire")
	if err != nil || op == nil || op.Response["outcome"] != "error" {
		t.Fatalf("rejected acquire left pending: %#v, %v", op, err)
	}
	pending, err := store.PendingOperations(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending = %#v, error = %v", pending, err)
	}
}

func TestStartupRecoversExplicitlyUnattemptedAcquire(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session := "pre-start-session"
	generation := int64(0)
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": generation, "operation_id": "pre-start-acquire"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(context.Background(), "pre-start-acquire", "acquire", &session, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest, "worker": map[string]any{"state": "starting", "start_attempted": false}}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Guest: &startupGuest{configErr: missingInstanceError()}, Resident: true})
	if err := service.StartupReconcile(context.Background()); err != nil {
		t.Fatalf("startup: %v", err)
	}
	op, err := store.GetOperation(context.Background(), "pre-start-acquire")
	if err != nil || op == nil || op.Response["outcome"] != "error" {
		t.Fatalf("operation=%#v, error=%v", op, err)
	}
}

func TestWorkerRecoveryDoesNotAssumeLegacyOrContradictoryStartWasSafe(t *testing.T) {
	for _, tc := range []struct {
		name   string
		worker map[string]any
		active bool
	}{
		{"legacy", map[string]any{"state": "starting"}, false},
		{"identity-present", map[string]any{"state": "starting", "start_attempted": false, "pid": int64(12)}, false},
		{"active", map[string]any{"state": "starting", "start_attempted": false}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			session := "start-" + tc.name
			generation := int64(0)
			if err := store.RecordOperation(context.Background(), tc.name, "acquire", &session, &generation, map[string]any{"outcome": "pending", "worker": tc.worker}); err != nil {
				t.Fatal(err)
			}
			service := New(store, config.Defaults(), Options{})
			if tc.active {
				done := service.markOperationActive(tc.name)
				defer done()
			}
			pending, err := store.PendingOperations(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := service.reconcilePendingWorkers(context.Background(), pending); err != nil {
				t.Fatal(err)
			}
			op, err := store.GetOperation(context.Background(), tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if worker := op.Response["worker"].(map[string]any); worker["state"] != "starting" {
				t.Fatalf("unexpected worker recovery: %#v", worker)
			}
		})
	}
}

func TestStartupReconcilesIndependentCachedAcquireDespiteUnownedTask(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now()
	ctx := context.Background()
	closing := state.Task{TaskID: "blocked-task", SessionID: "blocked-session", Generation: 1, State: stateClosing, InstanceName: "blocked-instance", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{"cpu":2}`, ResourceReservationJSON: `{"cpu":2}`}
	cached := state.Task{TaskID: "independent-task", SessionID: "independent-session", Generation: 1, State: stateCached, InstanceName: "independent-instance", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{}`, ResourceReservationJSON: `{}`}
	for _, task := range []state.Task{closing, cached} {
		if err := store.InsertTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": cached.SessionID, "generation": int64(0), "operation_id": "independent-acquire"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	generation := int64(0)
	if err := store.RecordOperation(ctx, "independent-acquire", "acquire", &cached.SessionID, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest, "worker": map[string]any{"state": "exited"}}); err != nil {
		t.Fatal(err)
	}
	guest := &perInstanceGuest{startupGuest: startupGuest{status: "Stopped"}, configs: map[string]map[string]any{
		"blocked-instance":     {"config": map[string]any{}, "devices": map[string]any{}, "expanded_devices": map[string]any{}},
		"independent-instance": {"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": cached.TaskID, "user.agent-vm.session": cached.SessionID, "user.agent-vm.generation": "1"}, "devices": map[string]any{}, "expanded_devices": map[string]any{}},
	}}
	service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
	if err := service.StartupReconcile(ctx); err == nil {
		t.Fatal("unowned task should keep service unready")
	}
	op, err := store.GetOperation(ctx, "independent-acquire")
	if err != nil || op.Response["outcome"] != "error" {
		t.Fatalf("independent operation=%#v, error=%v", op, err)
	}
	task, err := store.GetTask(ctx, map[string]any{"session_id": closing.SessionID})
	if err != nil || task == nil || task.State != stateClosing {
		t.Fatalf("unowned task=%#v, error=%v", task, err)
	}
}

type perInstanceGuest struct {
	startupGuest
	configs map[string]map[string]any
}

func (guest *perInstanceGuest) Config(_ context.Context, name string) (map[string]any, error) {
	v, ok := guest.configs[name]
	if !ok {
		return nil, errors.New("unknown instance")
	}
	return v, nil
}

func TestOrphanAcquireRefusesUnverifiedWorkerOrRequest(t *testing.T) {
	for _, mode := range []string{"missing-worker", "bad-digest", "wrong-session"} {
		t.Run(mode, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			session := "orphan-" + mode
			generation := int64(0)
			request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": generation, "operation_id": mode}
			digest, err := protocol.Digest(request)
			if err != nil {
				t.Fatal(err)
			}
			worker := any(map[string]any{"state": "exited"})
			if mode == "missing-worker" {
				worker = nil
			}
			if mode == "bad-digest" {
				digest = "invalid"
			}
			if mode == "wrong-session" {
				request["session_id"] = "other"
				digest, err = protocol.Digest(request)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := store.RecordOperation(ctx, mode, "acquire", &session, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest, "worker": worker}); err != nil {
				t.Fatal(err)
			}
			service := New(store, config.Defaults(), Options{Guest: &startupGuest{configErr: missingInstanceError()}, Resident: true})
			if err := service.StartupReconcile(ctx); err == nil {
				t.Fatal("unverified operation was finalized")
			}
			op, err := store.GetOperation(ctx, mode)
			if err != nil || op.Response["outcome"] != "pending" {
				t.Fatalf("operation=%#v, error=%v", op, err)
			}
		})
	}
}

func TestOrphanRecoveryContinuesAfterStaleGeneration(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now()
	configs := map[string]map[string]any{}
	for _, item := range []struct {
		session    string
		taskGen    int64
		requestGen int64
	}{{"stale", 5, 1}, {"valid", 2, 1}} {
		task := state.Task{TaskID: item.session + "-task", SessionID: item.session, Generation: item.taskGen, State: stateCached, InstanceName: item.session + "-instance", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{}`, ResourceReservationJSON: `{}`}
		if err := store.InsertTask(ctx, task); err != nil {
			t.Fatal(err)
		}
		req := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": item.session, "generation": item.requestGen, "operation_id": item.session + "-op"}
		digest, err := protocol.Digest(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordOperation(ctx, item.session+"-op", "acquire", &item.session, &item.requestGen, map[string]any{"outcome": "pending", "_request": map[string]any(req), "_request_digest": digest, "worker": map[string]any{"state": "exited"}}); err != nil {
			t.Fatal(err)
		}
		configs[task.InstanceName] = map[string]any{"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": task.TaskID, "user.agent-vm.session": task.SessionID, "user.agent-vm.generation": fmt.Sprint(task.Generation)}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}
	}
	guest := &perInstanceGuest{startupGuest: startupGuest{status: "Stopped"}, configs: configs}
	service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
	if err := service.StartupReconcile(ctx); err == nil {
		t.Fatal("stale generation must keep admission blocked")
	}
	for _, check := range []struct{ id, want string }{{"stale-op", "pending"}, {"valid-op", "error"}} {
		op, err := store.GetOperation(ctx, check.id)
		if err != nil || op.Response["outcome"] != check.want {
			t.Fatalf("operation=%#v, error=%v", op, err)
		}
	}
}

func TestReconcileClosingDestroyFinalizesVerifiedMissingInstance(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now()
	session := "closing-missing"
	generation := int64(1)
	if err := store.InsertTask(ctx, state.Task{TaskID: "closing-missing-task", SessionID: session, Generation: generation, State: stateClosing, InstanceName: "closing-missing-instance", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{"cpu":2}`, ResourceReservationJSON: `{"cpu":2}`}); err != nil {
		t.Fatal(err)
	}
	req := protocol.Request{"protocol": protocol.Version, "action": "destroy", "session_id": session, "generation": generation, "operation_id": "closing-missing-op"}
	digest, err := protocol.Digest(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(ctx, "closing-missing-op", "destroy", &session, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(req), "_request_digest": digest, "worker": map[string]any{"state": "exited"}}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Guest: &startupGuest{configErr: missingInstanceError()}})
	if _, err := service.reconcile(ctx, true); err != nil {
		t.Fatalf("reconcile=%v", err)
	}
	task, err := store.GetTask(ctx, map[string]any{"session_id": session})
	if err != nil || task != nil {
		t.Fatalf("task=%#v, error=%v", task, err)
	}
	op, err := store.GetOperation(ctx, "closing-missing-op")
	if err != nil || op.Response["outcome"] != "success" {
		t.Fatalf("op=%#v, error=%v", op, err)
	}
}

func TestStartupRecoversLegacyPreTransitionCachedAcquires(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now()
	session := "legacy-pretransition"
	generation := int64(14)
	task := state.Task{TaskID: "legacy-pretransition-task", SessionID: session, Generation: generation, State: stateCached, InstanceName: "legacy-pretransition-instance", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{}`, ResourceReservationJSON: `{}`}
	if err := store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"legacy-pretransition-one", "legacy-pretransition-two"} {
		req := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": generation, "operation_id": id}
		digest, err := protocol.Digest(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordOperation(ctx, id, "acquire", &session, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(req), "_request_digest": digest, "worker": map[string]any{"state": "starting"}}); err != nil {
			t.Fatal(err)
		}
	}
	guest := &startupGuest{status: "Stopped", config: map[string]any{"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": task.TaskID, "user.agent-vm.session": session, "user.agent-vm.generation": "14"}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
	service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
	if err := service.StartupReconcile(ctx); err != nil {
		t.Fatalf("startup: %v", err)
	}
	for _, id := range []string{"legacy-pretransition-one", "legacy-pretransition-two"} {
		op, err := store.GetOperation(ctx, id)
		if err != nil || op.Response["outcome"] != "error" {
			t.Fatalf("op=%#v,error=%v", op, err)
		}
	}
}

func TestLegacyPreTransitionAcquiresLeaveOlderGenerationPending(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	now := time.Now()
	session := "legacy-pretransition"
	generation := int64(14)
	task := state.Task{TaskID: "legacy-pretransition-task", SessionID: session, Generation: generation, State: stateCached, InstanceName: "legacy-pretransition-instance", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{}`, ResourceReservationJSON: `{}`}
	if err := store.InsertTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"legacy-pretransition-one", "legacy-pretransition-two"} {
		req := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": generation, "operation_id": id}
		digest, err := protocol.Digest(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordOperation(ctx, id, "acquire", &session, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(req), "_request_digest": digest, "worker": map[string]any{"state": "starting"}}); err != nil {
			t.Fatal(err)
		}
	}
	olderGeneration := generation - 2
	olderRequest := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": olderGeneration, "operation_id": "older-pending"}
	olderDigest, err := protocol.Digest(olderRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(ctx, "older-pending", "acquire", &session, &olderGeneration, map[string]any{"outcome": "pending", "_request": map[string]any(olderRequest), "_request_digest": olderDigest, "worker": map[string]any{"state": "finished"}}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{status: "Stopped", config: map[string]any{"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": task.TaskID, "user.agent-vm.session": session, "user.agent-vm.generation": "14"}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
	service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
	if err := service.StartupReconcile(ctx); err == nil {
		t.Fatal("older uncertain acquire must keep admission blocked")
	}
	older, err := store.GetOperation(ctx, "older-pending")
	if err != nil || older.Response["outcome"] != "pending" {
		t.Fatalf("older operation = %#v, error = %v", older, err)
	}
	for _, id := range []string{"legacy-pretransition-one", "legacy-pretransition-two"} {
		op, err := store.GetOperation(ctx, id)
		if err != nil || op.Response["outcome"] != "error" {
			t.Fatalf("op=%#v,error=%v", op, err)
		}
	}
}

func TestLegacyCachedStartRecoveryRefusesUnsafeEvidence(t *testing.T) {
	for _, mode := range []string{"generation", "ownership", "reservation", "other-worker", "active"} {
		t.Run(mode, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			now := time.Now()
			session := "legacy-unsafe-" + mode
			generation := int64(2)
			taskGen := generation
			if mode == "generation" {
				taskGen++
			}
			task := state.Task{TaskID: "legacy-unsafe-task", SessionID: session, Generation: taskGen, State: stateCached, InstanceName: "legacy-unsafe-instance", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{}`, ResourceReservationJSON: `{}`}
			if mode == "reservation" {
				task.ResourceReservationJSON = `{"cpu":2}`
			}
			if err := store.InsertTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			req := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": generation, "operation_id": "legacy-unsafe-op"}
			digest, err := protocol.Digest(req)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.RecordOperation(ctx, "legacy-unsafe-op", "acquire", &session, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(req), "_request_digest": digest, "worker": map[string]any{"state": "starting"}}); err != nil {
				t.Fatal(err)
			}
			if mode == "other-worker" {
				if err := store.RecordOperation(ctx, "other-op", "restart", &session, &generation, map[string]any{"outcome": "pending", "worker": map[string]any{"state": "starting", "start_attempted": true}}); err != nil {
					t.Fatal(err)
				}
			}
			nativeTask := task.TaskID
			if mode == "ownership" {
				nativeTask = "other"
			}
			guest := &startupGuest{status: "Stopped", config: map[string]any{"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": nativeTask, "user.agent-vm.session": session, "user.agent-vm.generation": fmt.Sprint(taskGen)}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}
			service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
			if mode == "active" {
				finish := service.markOperationActive("legacy-unsafe-op")
				defer finish()
			}
			if err := service.StartupReconcile(ctx); err == nil {
				t.Fatal("unsafe legacy acquire was finalized")
			}
			op, err := store.GetOperation(ctx, "legacy-unsafe-op")
			if err != nil || op.Response["outcome"] != "pending" {
				t.Fatalf("op=%#v,error=%v", op, err)
			}
		})
	}
}

func TestStartupRetriesPreviouslyBlockedMaintenanceAfterTasksClear(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	generation := int64(0)
	req := protocol.Request{"protocol": protocol.Version, "action": "admin-reconcile", "apply": true, "generation": generation, "operation_id": "previously-blocked-maintenance"}
	digest, err := protocol.Digest(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(ctx, "previously-blocked-maintenance", "admin-reconcile", nil, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(req), "_request_digest": digest, "_recovery_attempts": int64(2), "worker": map[string]any{"state": "finished", "pid": int64(1 << 30), "start_time": "1", "process_group": int64(1 << 30)}}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Resident: true, Guest: &startupGuest{}})
	if err := service.StartupReconcile(ctx); err != nil {
		t.Fatalf("startup=%v", err)
	}
	op, err := store.GetOperation(ctx, "previously-blocked-maintenance")
	if err != nil || op.Response["outcome"] != "success" {
		t.Fatalf("operation=%#v,error=%v", op, err)
	}
}

func TestStartupRetriesMultipleBlockedMaintenanceOperationsOnce(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	generation := int64(0)
	for _, id := range []string{"maintenance-a", "maintenance-b", "maintenance-c"} {
		req := protocol.Request{"protocol": protocol.Version, "action": "admin-reconcile", "apply": true, "generation": generation, "operation_id": id}
		digest, err := protocol.Digest(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordOperation(ctx, id, "admin-reconcile", nil, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(req), "_request_digest": digest, "_recovery_attempts": int64(2), "worker": map[string]any{"state": "finished", "pid": int64(1 << 30), "start_time": "1", "process_group": int64(1 << 30)}}); err != nil {
			t.Fatal(err)
		}
	}
	service := New(store, config.Defaults(), Options{Guest: &startupGuest{}, Resident: true})
	if err := service.StartupReconcile(ctx); err != nil {
		t.Fatalf("startup=%v", err)
	}
	for _, id := range []string{"maintenance-a", "maintenance-b", "maintenance-c"} {
		op, err := store.GetOperation(ctx, id)
		if err != nil || op.Response["outcome"] != "success" {
			t.Fatalf("operation=%#v,error=%v", op, err)
		}
	}
}

func TestMaintenanceRetryRequiresVerifiedEvidence(t *testing.T) {
	for _, mode := range []string{"transitional-task", "invalid-digest", "missing-worker-identity", "already-retried"} {
		t.Run(mode, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			generation := int64(0)
			req := protocol.Request{"protocol": protocol.Version, "action": "admin-reconcile", "apply": true, "generation": generation, "operation_id": "blocked-maintenance"}
			digest, err := protocol.Digest(req)
			if err != nil {
				t.Fatal(err)
			}
			worker := map[string]any{"state": "finished", "pid": int64(1 << 30), "start_time": "1", "process_group": int64(1 << 30)}
			response := map[string]any{"outcome": "pending", "_request": map[string]any(req), "_request_digest": digest, "_recovery_attempts": int64(2), "worker": worker}
			if mode == "invalid-digest" {
				response["_request_digest"] = "invalid"
			}
			if mode == "missing-worker-identity" {
				delete(worker, "process_group")
			}
			if mode == "already-retried" {
				response["_recovery_after_clear_attempted"] = true
			}
			if err := store.RecordOperation(ctx, "blocked-maintenance", "admin-reconcile", nil, &generation, response); err != nil {
				t.Fatal(err)
			}
			if mode == "transitional-task" {
				now := time.Now()
				if err := store.InsertTask(ctx, state.Task{TaskID: "blocked-task", SessionID: "blocked-session", Generation: 1, State: stateClosing, InstanceName: "blocked-instance", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{}`, ResourceReservationJSON: `{}`}); err != nil {
					t.Fatal(err)
				}
			}
			service := New(store, config.Defaults(), Options{Guest: &startupGuest{config: map[string]any{"config": map[string]any{}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}, Resident: true})
			if err := service.StartupReconcile(ctx); err == nil {
				t.Fatal("unverified maintenance was finalized")
			}
			op, err := store.GetOperation(ctx, "blocked-maintenance")
			if err != nil || op.Response["outcome"] != "pending" {
				t.Fatalf("op=%#v,error=%v", op, err)
			}
		})
	}
}

func TestStartupRecoversLegacyNeverAllocatedAcquire(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	session := "legacy-never-allocated"
	generation := int64(0)
	req := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": generation, "operation_id": "legacy-never-allocated-op"}
	digest, err := protocol.Digest(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(ctx, "legacy-never-allocated-op", "acquire", &session, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(req), "_request_digest": digest, "worker": map[string]any{"state": "starting"}}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Guest: &startupGuest{configErr: missingInstanceError()}, Resident: true})
	if err := service.StartupReconcile(ctx); err != nil {
		t.Fatalf("startup=%v", err)
	}
	op, err := store.GetOperation(ctx, "legacy-never-allocated-op")
	if err != nil || op.Response["outcome"] != "error" {
		t.Fatalf("operation=%#v, error=%v", op, err)
	}
}

func TestLegacyNeverAllocatedRecoveryRequiresTombstoneAndNativeAbsence(t *testing.T) {
	for _, mode := range []string{"tombstone", "native-present", "native-error", "other-pending"} {
		t.Run(mode, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			session := "never-allocated-" + mode
			generation := int64(0)
			if mode == "tombstone" {
				if _, err := store.NextGeneration(ctx, session); err != nil {
					t.Fatal(err)
				}
			}
			req := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": session, "generation": generation, "operation_id": "never-allocated-op"}
			digest, err := protocol.Digest(req)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.RecordOperation(ctx, "never-allocated-op", "acquire", &session, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(req), "_request_digest": digest, "worker": map[string]any{"state": "starting"}}); err != nil {
				t.Fatal(err)
			}
			if mode == "other-pending" {
				if err := store.RecordOperation(ctx, "other-op", "restart", &session, &generation, map[string]any{"outcome": "pending", "worker": map[string]any{"state": "starting"}}); err != nil {
					t.Fatal(err)
				}
			}
			guest := &startupGuest{configErr: missingInstanceError()}
			if mode == "native-present" {
				guest.configErr = nil
				guest.config = map[string]any{}
			}
			if mode == "native-error" {
				guest.configErr = errors.New("transport failed")
			}
			service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
			if err := service.StartupReconcile(ctx); err == nil {
				t.Fatal("unsafe pending operation finalized")
			}
			op, err := store.GetOperation(ctx, "never-allocated-op")
			if err != nil || op.Response["outcome"] != "pending" {
				t.Fatalf("op=%#v,error=%v", op, err)
			}
		})
	}
}

type quiescentStartupGuest struct {
	*startupGuest
	quiescenceErr error
}

func (guest *quiescentStartupGuest) VerifyRecoveryQuiescence(context.Context) error {
	return guest.quiescenceErr
}

func TestSupersededOperationsRetainUnknownHistoricalOutcomes(t *testing.T) {
	for _, action := range []string{"acquire", "resize", "restart"} {
		t.Run(action, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			session, generation := "superseded-"+action, int64(1)
			taskGeneration := generation
			if action == "acquire" {
				taskGeneration = 3
			}
			now := nowString(time.Now())
			task := state.Task{TaskID: "superseded-task", SessionID: session, Generation: taskGeneration, State: stateCached, InstanceName: "superseded-instance", CreatedAt: now, UpdatedAt: now, ResourcesJSON: `{}`, ResourceReservationJSON: `{}`}
			if err := store.InsertTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			req := protocol.Request{"protocol": protocol.Version, "action": action, "session_id": session, "generation": generation, "operation_id": "original"}
			digest, err := protocol.Digest(req)
			if err != nil {
				t.Fatal(err)
			}
			worker := map[string]any{"state": "finished", "pid": int64(1 << 30), "process_group": int64(1 << 30), "start_time": "1", "finished_at": nowString(time.Now())}
			if err := store.RecordOperation(ctx, "original", action, &session, &generation, map[string]any{"outcome": "pending", "_request": map[string]any(req), "_request_digest": digest, "worker": worker}); err != nil {
				t.Fatal(err)
			}
			laterAction, laterGeneration := "release", taskGeneration
			result := map[string]any{"task_id": task.TaskID, "session_id": session, "generation": taskGeneration, "state": stateCached, "effective_lease": false}
			if action == "acquire" {
				laterAction, laterGeneration = "acquire", int64(2)
				result["state"], result["effective_lease"] = stateActive, true
			}
			laterReq := protocol.Request{"protocol": protocol.Version, "action": laterAction, "session_id": session, "generation": laterGeneration, "operation_id": "later"}
			later := protocol.SuccessWithRelease(laterReq, result, nil, nil)
			laterDigest, err := protocol.Digest(laterReq)
			if err != nil {
				t.Fatal(err)
			}
			later["_request_digest"] = laterDigest
			if err := store.RecordOperation(ctx, "later", laterAction, &session, &laterGeneration, later); err != nil {
				t.Fatal(err)
			}
			guest := &quiescentStartupGuest{startupGuest: &startupGuest{status: "Stopped", config: map[string]any{"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": task.TaskID, "user.agent-vm.session": session, "user.agent-vm.generation": fmt.Sprint(taskGeneration)}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}}
			service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
			if err := service.StartupReconcile(ctx); err != nil {
				t.Fatalf("startup: %v", err)
			}
			original, err := store.GetOperation(ctx, "original")
			if err != nil || original == nil || original.Response["outcome"] != "error" || original.Response["_request"] == nil || original.Response["worker"] == nil {
				t.Fatalf("original = %#v, error = %v", original, err)
			}
			_, err = service.replayRecord(req, original)
			if control := protocol.AsError(err); control.Code != "operation_outcome_unknown" {
				t.Fatalf("replay = %#v", control)
			}
			detail, _ := protocol.AsError(err).Detail.(map[string]any)
			if detail["outcome_verified"] != false || detail["lease_state_verified"] != true {
				t.Fatalf("detail = %#v", detail)
			}
			metrics, err := service.adminMetrics(ctx)
			if err != nil {
				t.Fatal(err)
			}
			unknown, ok := metrics["historical_unknown_operations"].([]any)
			if !ok || len(unknown) != 1 || unknown[0].(map[string]any)["operation_id"] != "original" {
				t.Fatalf("historical unknown operations = %#v", metrics["historical_unknown_operations"])
			}
		})
	}
}

func TestSupersededOperationsRequireQuiescenceAndLaterEvidence(t *testing.T) {
	for _, mode := range []string{"native-busy", "worker-missing", "no-witness", "wrong-session", "wrong-task", "missing-lease", "unverified-cache", "other-pending", "active", "late-worker", "bad-witness-digest", "bound-task-mismatch"} {
		t.Run(mode, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			ctx := context.Background()
			session, generation := "superseded-negative-"+mode, int64(1)
			now := nowString(time.Now())
			task := state.Task{TaskID: "task", SessionID: session, Generation: generation, State: stateCached, InstanceName: "instance", CreatedAt: now, UpdatedAt: now, ResourcesJSON: `{}`, ResourceReservationJSON: `{}`}
			if err := store.InsertTask(ctx, task); err != nil {
				t.Fatal(err)
			}
			req := protocol.Request{"protocol": protocol.Version, "action": "restart", "session_id": session, "generation": generation, "operation_id": "original"}
			digest, err := protocol.Digest(req)
			if err != nil {
				t.Fatal(err)
			}
			worker := map[string]any{"state": "exited", "pid": int64(1 << 30), "process_group": int64(1 << 30), "start_time": "1", "finished_at": nowString(time.Now())}
			if mode == "worker-missing" {
				delete(worker, "process_group")
			}
			if mode == "late-worker" {
				worker["finished_at"] = nowString(time.Now().Add(time.Hour))
			}
			originalResponse := map[string]any{"outcome": "pending", "_request": map[string]any(req), "_request_digest": digest, "worker": worker}
			if mode == "bound-task-mismatch" {
				originalResponse["_task_id"] = "different"
			}
			if err := store.RecordOperation(ctx, "original", "restart", &session, &generation, originalResponse); err != nil {
				t.Fatal(err)
			}
			if mode != "no-witness" {
				laterReq := protocol.Request{"protocol": protocol.Version, "action": "release", "session_id": session, "generation": generation, "operation_id": "later"}
				result := map[string]any{"task_id": task.TaskID, "session_id": session, "generation": generation, "state": stateCached, "effective_lease": false}
				if mode == "wrong-session" {
					result["session_id"] = "different"
				}
				if mode == "wrong-task" {
					result["task_id"] = "different"
				}
				if mode == "missing-lease" {
					delete(result, "effective_lease")
				}
				later := protocol.SuccessWithRelease(laterReq, result, nil, nil)
				laterDigest, err := protocol.Digest(laterReq)
				if err != nil {
					t.Fatal(err)
				}
				later["_request_digest"] = laterDigest
				if mode == "bad-witness-digest" {
					later["_request_digest"] = "invalid"
				}
				if err := store.RecordOperation(ctx, "later", "release", &session, &generation, later); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "other-pending" {
				if err := store.RecordOperation(ctx, "other", "destroy", &session, &generation, map[string]any{"outcome": "pending"}); err != nil {
					t.Fatal(err)
				}
			}
			nativeTask := task.TaskID
			if mode == "unverified-cache" {
				nativeTask = "different"
			}
			guest := &quiescentStartupGuest{startupGuest: &startupGuest{status: "Stopped", config: map[string]any{"config": map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": nativeTask, "user.agent-vm.session": session, "user.agent-vm.generation": "1"}, "devices": map[string]any{}, "expanded_devices": map[string]any{}}}}
			if mode == "native-busy" {
				guest.quiescenceErr = errors.New("native work remains")
			}
			service := New(store, config.Defaults(), Options{Guest: guest, Resident: true})
			if mode == "active" {
				finish := service.markOperationActive("original")
				defer finish()
			}
			pending, err := store.PendingOperations(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.reconcileSupersededOperations(ctx, pending); (err != nil) != (mode == "native-busy") {
				t.Fatalf("reconciliation error = %v", err)
			}
			original, err := store.GetOperation(ctx, "original")
			if err != nil || original == nil || original.Response["outcome"] != "pending" {
				t.Fatalf("original = %#v, error = %v", original, err)
			}
		})
	}
}
