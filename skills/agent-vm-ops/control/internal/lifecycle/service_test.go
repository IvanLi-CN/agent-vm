package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
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

type staticCPUObserver struct {
	observation observe.CPUObservation
	err         error
}

type startupGuest struct {
	status      string
	statuses    []map[string]any
	statusCalls int
	config      map[string]any
	configErr   error
	configWait  bool
	initErr     error
	configHook  func()
	stopErr     error
	deviceSets  []string
}

func (guest *startupGuest) Init(context.Context, string, string) error              { return guest.initErr }
func (guest *startupGuest) ConfigSet(context.Context, string, string, string) error { return nil }

func (guest *startupGuest) DeviceSet(_ context.Context, _ string, device, key, value string) error {
	guest.deviceSets = append(guest.deviceSets, device+":"+key+"="+value)
	return nil
}
func (guest *startupGuest) DeviceAdd(context.Context, string, string, string, map[string]string) error {
	return nil
}
func (guest *startupGuest) DeviceRemove(context.Context, string, string) error { return nil }
func (guest *startupGuest) Start(context.Context, string) error                { return nil }
func (guest *startupGuest) Stop(context.Context, string) error                 { return guest.stopErr }
func (guest *startupGuest) Delete(context.Context, string) error               { return nil }
func (guest *startupGuest) Status(context.Context, string) (map[string]any, error) {
	if len(guest.statuses) > 0 {
		index := guest.statusCalls
		if index >= len(guest.statuses) {
			index = len(guest.statuses) - 1
		}
		guest.statusCalls++
		return guest.statuses[index], nil
	}
	return map[string]any{"status": guest.status}, nil
}
func (guest *startupGuest) Config(ctx context.Context, _ string) (map[string]any, error) {
	if guest.configWait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if guest.configHook != nil {
		hook := guest.configHook
		guest.configHook = nil
		hook()
	}
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
		"worker":          map[string]any{"state": "finished", "error": "native command failed"},
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
	if worker["state"] != "exited" || worker["pre_spawn"] != true || worker["error"] != "exec unavailable" {
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
	guest := &startupGuest{configErr: &native.CommandError{Result: native.Result{Stderr: []byte("instance not found")}, Cause: errors.New("incus query failed")}}
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
		"outcome": "pending", "_request_digest": digest, "_request": map[string]any(request),
	}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{configErr: &native.CommandError{Result: native.Result{Stderr: []byte("instance not found")}, Cause: errors.New("incus query failed")}}
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
		"outcome": "pending", "_request_digest": digest, "_request": map[string]any(request),
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
	vmIP := "10.250.19.28"
	sshHostPort := int64(22001)
	previousGeneration := int64(0)
	if err := store.InsertTask(context.Background(), state.Task{
		TaskID: "destroy-task", SessionID: "destroy-session", Generation: 1, State: stateActive,
		InstanceName: "agent-destroy", VMIP: &vmIP, SSHHostPort: &sshHostPort, PreviousGeneration: &previousGeneration,
		LeaseExpiresAt: stringPointer(now.Add(time.Hour).Format(time.RFC3339Nano)),
		CreatedAt:      nowString(now), UpdatedAt: nowString(now), ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ResourceReservationJSON: `{}`,
	}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{configErr: &native.CommandError{Result: native.Result{Stderr: []byte("instance not found")}, Cause: errors.New("incus query failed")}}
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
	digest, err := protocol.Digest(protocol.Request(request))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(context.Background(), "destroy-pending", "destroy", stringPointer(session), &generation, map[string]any{
		"outcome": "pending", "_request": request, "_request_digest": digest,
		"worker": map[string]any{"pid": int64(1234), "start_time": "1", "process_group": int64(1234), "state": "finished", "finished_at": "2026-10-27T00:00:00Z"},
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

func TestReconcileDoesNotFinalizeForeignPendingCleanupRequest(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "foreign-pending-destroy.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	cleanupSession := "cleanup-owner"
	foreignSession := "foreign-owner"
	generation := int64(1)
	for _, task := range []state.Task{
		{TaskID: "cleanup-owner-task", SessionID: cleanupSession, Generation: generation, State: stateDeleting, InstanceName: "agent-cleanup-owner", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: mustJSON(zeroResources()), ResourceReservationJSON: "{}"},
		{TaskID: "foreign-owner-task", SessionID: foreignSession, Generation: generation, State: stateAbsent, InstanceName: "agent-foreign-owner", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: mustJSON(zeroResources()), ResourceReservationJSON: "{}"},
	} {
		if err := store.InsertTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	request := protocol.Request{"protocol": protocol.Version, "action": "destroy", "session_id": foreignSession, "generation": generation, "operation_id": "foreign-pending-destroy"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(context.Background(), request["operation_id"].(string), "destroy", stringPointer(cleanupSession), &generation, map[string]any{
		"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest,
		"worker": map[string]any{"pid": int64(1234), "start_time": "1", "process_group": int64(1234), "state": "finished", "finished_at": "2026-10-27T00:00:00Z"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Clock: func() time.Time { return now }})
	if _, err := service.reconcile(context.Background(), true); err == nil {
		t.Fatal("foreign cleanup request was finalized")
	}
	for _, session := range []string{cleanupSession, foreignSession} {
		task, err := store.GetTask(context.Background(), map[string]any{"session_id": session})
		if err != nil {
			t.Fatal(err)
		}
		if task == nil {
			t.Fatalf("session %s was deleted", session)
		}
	}
	operation, err := store.GetOperation(context.Background(), request["operation_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "pending" {
		t.Fatalf("foreign cleanup journal = %#v", operation)
	}
}

func nativeInitRecoveryTask(now time.Time) state.Task {
	previousGeneration := int64(0)
	sshPort := int64(22022)
	lease := now.Add(time.Hour).Format(time.RFC3339Nano)
	inactive := now.Add(-time.Minute).Format(time.RFC3339Nano)
	cleanup := now.Add(time.Hour).Format(time.RFC3339Nano)
	lastError := mustJSON(map[string]any{
		"phase": "native-init", "cause": "incus init failed", "outcome_verified": false,
	})
	return state.Task{
		TaskID: "native-init-recovery-task", SessionID: "native-init-recovery-session", Generation: 1,
		State: stateFailed, InstanceName: "agent-native-init-recovery", VMIP: stringPointer("10.250.19.28"),
		SSHHostPort: &sshPort, LeaseExpiresAt: &lease, InactiveSince: &inactive, CleanupAfter: &cleanup,
		CreatedAt: nowString(now), UpdatedAt: nowString(now), LastError: &lastError,
		ResourcesJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`, ReservationBytes: 1234,
		PreviousGeneration: &previousGeneration, ResourceReservationJSON: `{"cpu":2,"memory_mib":4096,"disk_gib":20}`,
	}
}

func insertNativeInitRecoveryTask(t *testing.T, store *state.Store, now time.Time) state.Task {
	t.Helper()
	task := nativeInitRecoveryTask(now)
	if err := store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	return task
}

func recordPendingLifecycleOperation(t *testing.T, store *state.Store, action, session string, generation int64, operationID string, includeIdentity bool) protocol.Request {
	t.Helper()
	request := protocol.Request{
		"protocol": protocol.Version, "action": action, "session_id": session,
		"generation": generation, "operation_id": operationID,
	}
	response := map[string]any{"outcome": "pending"}
	if includeIdentity {
		digest, err := protocol.Digest(request)
		if err != nil {
			t.Fatal(err)
		}
		response["_request"] = map[string]any(request)
		response["_request_digest"] = digest
		response["worker"] = map[string]any{
			"pid": 1234, "start_time": "1", "process_group": 1234,
			"state": "finished", "finished_at": "2026-10-27T00:00:00Z",
		}
	}
	if err := store.RecordOperation(context.Background(), operationID, action, stringPointer(session), &generation, response); err != nil {
		t.Fatal(err)
	}
	return request
}

func nativeInitMissingGuest() *startupGuest {
	return &startupGuest{configErr: &native.CommandError{
		Result: native.Result{Stderr: []byte("instance not found")}, Cause: errors.New("incus query failed"),
	}}
}

func intPointer(value int64) *int64 { return &value }

func TestReconcileRecoversVerifiedNativeInitAbsence(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "native-init-recovery.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	task := insertNativeInitRecoveryTask(t, store, now)
	acquire := recordPendingLifecycleOperation(t, store, "acquire", task.SessionID, 0, "native-init-acquire", true)
	release := recordPendingLifecycleOperation(t, store, "release", task.SessionID, task.Generation, "native-init-release", true)
	destroy := recordPendingLifecycleOperation(t, store, "destroy", task.SessionID, task.Generation, "native-init-destroy", true)
	service := New(store, config.Defaults(), Options{Guest: nativeInitMissingGuest(), Clock: func() time.Time { return now }})

	result, err := service.reconcile(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if result["dry_run"] != false {
		t.Fatalf("reconcile result = %#v", result)
	}
	actions, ok := result["actions"].([]any)
	if !ok || len(actions) != 1 {
		t.Fatalf("reconcile actions = %#v", result["actions"])
	}
	if action, ok := actions[0].(map[string]any); !ok || action["action"] != "close-incomplete-or-expired" {
		t.Fatalf("reconcile compatibility action = %#v", actions[0])
	}
	recovered, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if recovered != nil {
		t.Fatalf("recovered task = %#v", recovered)
	}
	if ports, err := store.ListPorts(context.Background(), task.TaskID); err != nil || len(ports) != 0 {
		t.Fatalf("recovered ports = %#v, err=%v", ports, err)
	}

	for _, operationID := range []string{"native-init-acquire", "native-init-release", "native-init-destroy"} {
		operation, err := store.GetOperation(context.Background(), operationID)
		if err != nil {
			t.Fatal(err)
		}
		if operation == nil || operation.Response["outcome"] == "pending" {
			t.Fatalf("operation %s remained pending: %#v", operationID, operation)
		}
		if operation.Response["_request_digest"] == nil {
			t.Fatalf("operation %s lost request digest: %#v", operationID, operation.Response)
		}
	}
	acquireOperation, err := store.GetOperation(context.Background(), acquire["operation_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	acquireError, ok := acquireOperation.Response["error"].(map[string]any)
	if !ok || acquireError["code"] != "acquire_partial" {
		t.Fatalf("acquire recovery = %#v", acquireOperation.Response)
	}
	for _, request := range []protocol.Request{release, destroy} {
		operation, err := store.GetOperation(context.Background(), request["operation_id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if operation.Response["outcome"] != "success" || operation.Response["result"].(map[string]any)["state"] != stateAbsent {
			t.Fatalf("cleanup recovery = %#v", operation.Response)
		}
	}

	if _, err := service.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	again, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Fatalf("idempotent recovery task = %#v", again)
	}
}

func TestAcquireFailurePathReconcilesVerifiedPreSpawnNativeAbsence(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "native-init-acquire-failure.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	missing := &native.CommandError{Result: native.Result{Stderr: []byte("instance not found")}, Cause: errors.New("incus init failed before process spawn")}
	guest := &startupGuest{initErr: missing, configErr: missing}
	cfg := config.Defaults()
	cfg.VMIPStart = "10.250.19.20"
	cfg.VMIPEnd = "10.250.19.30"
	service := New(store, cfg, Options{Guest: guest, Clock: func() time.Time { return now }})
	request := protocol.Request{
		"protocol": protocol.Version, "action": "acquire", "session_id": "native-init-acquire-failure",
		"generation": int64(0), "operation_id": "native-init-acquire-failure",
	}
	if _, dispatchErr := service.Dispatch(context.Background(), request); protocol.AsError(dispatchErr).Code != "native_failed" {
		t.Fatalf("acquire failure = %#v", dispatchErr)
	}
	operation, err := store.GetOperation(context.Background(), request["operation_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "pending" {
		t.Fatalf("acquire operation after native failure = %#v", operation)
	}
	if err := service.WorkerStartFailed(context.Background(), request["operation_id"].(string), missing); err != nil {
		t.Fatal(err)
	}
	if _, err := service.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	task, err := store.GetTask(context.Background(), map[string]any{"session_id": request["session_id"]})
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.State != stateAbsent || task.ReservationBytes != 0 || task.VMIP != nil {
		t.Fatalf("acquire failure recovery task = %#v", task)
	}
	operation, err = store.GetOperation(context.Background(), request["operation_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "error" {
		t.Fatalf("acquire failure recovery operation = %#v", operation)
	}
}

func TestDestroyReplayDoesNotDeleteForConflictingOrStaleIdentity(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "destroy-replay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	zero := mustJSON(zeroResources())
	for _, task := range []state.Task{
		{TaskID: "replay-target-task", SessionID: "replay-target", Generation: 1, State: stateAbsent, InstanceName: "agent-replay-target", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: zero, ResourceReservationJSON: "{}"},
		{TaskID: "stale-target-task", SessionID: "stale-target", Generation: 2, State: stateAbsent, InstanceName: "agent-stale-target", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: zero, ResourceReservationJSON: "{}"},
	} {
		if err := store.InsertTask(context.Background(), task); err != nil {
			t.Fatal(err)
		}
	}
	conflictingOriginal := protocol.Request{"protocol": protocol.Version, "action": "destroy", "session_id": "different-session", "generation": int64(1), "operation_id": "shared-destroy"}
	conflictingDigest, err := protocol.Digest(conflictingOriginal)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(context.Background(), "shared-destroy", "destroy", stringPointer("different-session"), intPointer(1), map[string]any{"outcome": "success", "_request_digest": conflictingDigest}); err != nil {
		t.Fatal(err)
	}
	staleOriginal := protocol.Request{"protocol": protocol.Version, "action": "destroy", "session_id": "stale-target", "generation": int64(1), "operation_id": "stale-destroy"}
	staleDigest, err := protocol.Digest(staleOriginal)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(context.Background(), "stale-destroy", "destroy", stringPointer("stale-target"), intPointer(1), map[string]any{"outcome": "success", "_request_digest": staleDigest}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Clock: func() time.Time { return now }})
	if _, err := service.Dispatch(context.Background(), protocol.Request{"protocol": protocol.Version, "action": "destroy", "session_id": "replay-target", "generation": int64(1), "operation_id": "shared-destroy"}); err == nil || protocol.AsError(err).Code != "operation_conflict" {
		t.Fatalf("conflicting replay error = %#v", err)
	}
	if task, err := store.GetTask(context.Background(), map[string]any{"session_id": "replay-target"}); err != nil || task == nil {
		t.Fatalf("conflicting replay deleted target: task=%#v err=%v", task, err)
	}
	replayed, err := service.Dispatch(context.Background(), staleOriginal)
	if err != nil {
		t.Fatal(err)
	}
	if replayed["outcome"] != "replayed" {
		t.Fatalf("stale replay = %#v", replayed)
	}
	staleTask, err := store.GetTask(context.Background(), map[string]any{"session_id": "stale-target"})
	if err != nil {
		t.Fatal(err)
	}
	if staleTask == nil || staleTask.Generation != 2 {
		t.Fatalf("stale replay changed newer task = %#v", staleTask)
	}
}

func TestReconcileFinalizesPendingAbsentDestroyAfterRestart(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "absent-destroy-reconcile.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	task := state.Task{TaskID: "absent-destroy-task", SessionID: "absent-destroy-session", Generation: 4, State: stateAbsent, InstanceName: "agent-absent-destroy", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: mustJSON(zeroResources()), ResourceReservationJSON: "{}"}
	if err := store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	request := recordPendingLifecycleOperation(t, store, "destroy", task.SessionID, task.Generation, "absent-destroy-operation", true)
	guest := nativeInitMissingGuest()
	service := New(store, config.Defaults(), Options{Guest: guest, Clock: func() time.Time { return now }})
	if _, err := service.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	remaining, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if remaining != nil {
		t.Fatalf("reconciled absent task remains = %#v", remaining)
	}
	operation, err := store.GetOperation(context.Background(), request["operation_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "success" {
		t.Fatalf("destroy journal was not finalized = %#v", operation)
	}
}

func TestReconcileConsumesPreparedAbsentDestroyWithoutNativeWorker(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "prepared-absent-destroy.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	task := state.Task{TaskID: "prepared-absent-destroy-task", SessionID: "prepared-absent-destroy-session", Generation: 4, State: stateAbsent, InstanceName: "agent-prepared-absent-destroy", CreatedAt: nowString(now), UpdatedAt: nowString(now), ResourcesJSON: mustJSON(zeroResources()), ResourceReservationJSON: "{}"}
	if err := store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	request := protocol.Request{"protocol": protocol.Version, "action": "destroy", "session_id": task.SessionID, "generation": task.Generation, "operation_id": "prepared-absent-destroy"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(context.Background(), request["operation_id"].(string), "destroy", stringPointer(task.SessionID), &task.Generation, map[string]any{
		"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest,
		"worker": map[string]any{"state": "starting", "prepared_at": "2026-10-27T00:00:00Z"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Guest: nativeInitMissingGuest(), Clock: func() time.Time { return now }})
	if _, err := service.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	remaining, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if remaining != nil {
		t.Fatalf("prepared absent destroy left task = %#v", remaining)
	}
	operation, err := store.GetOperation(context.Background(), request["operation_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "success" {
		t.Fatalf("prepared absent destroy journal = %#v", operation)
	}
}

func TestNativeInitRecoveryBlocksJournalSessionMismatch(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "native-init-mismatch.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	task := insertNativeInitRecoveryTask(t, store, now)
	request := protocol.Request{"protocol": protocol.Version, "action": "acquire", "session_id": task.SessionID, "generation": int64(0), "operation_id": "native-init-mismatch"}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOperation(context.Background(), request["operation_id"].(string), "acquire", stringPointer("foreign-session"), intPointer(0), map[string]any{
		"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest,
		"worker": map[string]any{"pid": 1234, "start_time": "1", "process_group": 1234, "state": "finished", "finished_at": "2026-10-27T00:00:00Z"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Guest: nativeInitMissingGuest(), Clock: func() time.Time { return now }})
	if _, err := service.reconcile(context.Background(), true); err == nil || protocol.AsError(err).Code != "operation_unverified" {
		t.Fatalf("mismatched journal recovery error = %#v", err)
	}
	current, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.State != stateFailed || current.ReservationBytes != task.ReservationBytes {
		t.Fatalf("mismatched journal changed task = %#v", current)
	}
	pending, err := store.PendingOperations(context.Background())
	if err != nil || len(pending) != 1 {
		t.Fatalf("mismatched journal pending = %#v err=%v", pending, err)
	}
}

func TestNativeInitRecoveryRequiresVerifiedExitedWorkers(t *testing.T) {
	cases := []struct {
		name   string
		worker map[string]any
	}{
		{name: "missing", worker: nil},
		{name: "starting", worker: map[string]any{"state": "starting"}},
		{name: "running", worker: map[string]any{"pid": int64(1234), "start_time": "1", "process_group": int64(1234), "state": "running"}},
		{name: "orphaned", worker: map[string]any{"pid": int64(1234), "start_time": "1", "process_group": int64(1234), "state": "orphaned", "observed_at": "2026-10-27T00:00:00Z"}},
		{name: "missing-process-group", worker: map[string]any{"pid": int64(1234), "start_time": "1", "state": "finished", "finished_at": "2026-10-27T00:00:00Z"}},
		{name: "zero-process-group", worker: map[string]any{"pid": int64(1234), "start_time": "1", "process_group": int64(0), "state": "finished", "finished_at": "2026-10-27T00:00:00Z"}},
		{name: "timed-out", worker: map[string]any{"pid": int64(1234), "start_time": "1", "process_group": int64(1234), "state": "finished", "finished_at": "2026-10-27T00:00:00Z", "timed_out": true}},
		{name: "timed-out-string", worker: map[string]any{"pid": int64(1234), "start_time": "1", "process_group": int64(1234), "state": "finished", "finished_at": "2026-10-27T00:00:00Z", "timed_out": "true"}},
		{name: "timed-out-number", worker: map[string]any{"pid": int64(1234), "start_time": "1", "process_group": int64(1234), "state": "finished", "finished_at": "2026-10-27T00:00:00Z", "timed_out": int64(1)}},
		{name: "malformed", worker: map[string]any{"pid": int64(1234), "start_time": "1", "process_group": int64(1234), "state": "finished"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "native-init-worker.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
			task := insertNativeInitRecoveryTask(t, store, now)
			recordPendingLifecycleOperation(t, store, "acquire", task.SessionID, 0, "native-init-worker-"+testCase.name, true)
			if err := store.UpdateOperationResponse(context.Background(), "native-init-worker-"+testCase.name, func(response map[string]any) error {
				if testCase.worker == nil {
					delete(response, "worker")
				} else {
					response["worker"] = testCase.worker
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			service := New(store, config.Defaults(), Options{Guest: nativeInitMissingGuest(), Clock: func() time.Time { return now }})
			recovered, err := service.reconcileVerifiedNativeInitAbsence(context.Background(), task, "")
			if !recovered || err == nil {
				t.Fatalf("worker state unexpectedly recovered: recovered=%v err=%v", recovered, err)
			}
			current, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			if current == nil || current.State != stateFailed || current.ReservationBytes != task.ReservationBytes {
				t.Fatalf("blocked task changed = %#v", current)
			}
			pending, err := store.PendingOperations(context.Background())
			if err != nil || len(pending) != 1 {
				t.Fatalf("blocked operations = %#v, err=%v", pending, err)
			}
		})
	}
}

func TestNativeInitRecoveryAcceptsVerifiedPreSpawnFailure(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "native-init-pre-spawn.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	task := insertNativeInitRecoveryTask(t, store, now)
	request := recordPendingLifecycleOperation(t, store, "acquire", task.SessionID, 0, "native-init-pre-spawn", true)
	if err := store.UpdateOperationResponse(context.Background(), request["operation_id"].(string), func(response map[string]any) error {
		response["worker"] = map[string]any{
			"state": "exited", "start_attempted": true, "pre_spawn": true,
			"finished_at": "2026-10-27T00:00:00Z",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Guest: nativeInitMissingGuest(), Clock: func() time.Time { return now }})
	if _, err := service.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil || recovered.State != stateAbsent || recovered.ReservationBytes != 0 {
		t.Fatalf("pre-spawn recovery task = %#v", recovered)
	}
	operation, err := store.GetOperation(context.Background(), request["operation_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "error" {
		t.Fatalf("pre-spawn operation = %#v", operation)
	}
}

func TestNativeInitRecoveryBoundsNativeAbsenceQuery(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "native-init-timeout.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	task := insertNativeInitRecoveryTask(t, store, now)
	recordPendingLifecycleOperation(t, store, "acquire", task.SessionID, 0, "native-init-timeout", true)
	cfg := config.Defaults()
	cfg.WorkerTimeoutSeconds = 1
	service := New(store, cfg, Options{Guest: &startupGuest{configWait: true}, Clock: func() time.Time { return now }})
	started := time.Now()
	recovered, err := service.reconcileVerifiedNativeInitAbsence(context.Background(), task, "")
	if !recovered || err == nil {
		t.Fatalf("timed-out native query = recovered=%v err=%v", recovered, err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("native absence query exceeded bounded timeout: %s", elapsed)
	}
	current, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.State != stateFailed || current.ReservationBytes != task.ReservationBytes {
		t.Fatalf("timed-out native query changed task = %#v", current)
	}
}

func TestAdminReconcileExcludesItsOwnPendingJournalFromNativeInitProof(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "native-init-admin-journal.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	task := insertNativeInitRecoveryTask(t, store, now)
	recordPendingLifecycleOperation(t, store, "acquire", task.SessionID, 0, "native-init-admin-acquire", true)
	service := New(store, config.Defaults(), Options{Guest: nativeInitMissingGuest(), Clock: func() time.Time { return now }})
	response, err := service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version, "action": "admin-reconcile", "session_id": task.SessionID,
		"generation": int64(0), "operation_id": "native-init-admin-reconcile", "apply": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response["outcome"] != "success" {
		t.Fatalf("admin reconcile response = %#v", response)
	}
	recovered, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil || recovered.State != stateAbsent {
		t.Fatalf("admin reconcile task = %#v", recovered)
	}
	if pending, err := store.PendingOperations(context.Background()); err != nil || len(pending) != 0 {
		t.Fatalf("admin reconcile journals = %#v, err=%v", pending, err)
	}
}

func TestReconcileRetriesInterruptedAdminReconcileBeforeNativeInitProof(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "native-init-admin-retry.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	task := insertNativeInitRecoveryTask(t, store, now)
	recordPendingLifecycleOperation(t, store, "acquire", task.SessionID, 0, "native-init-admin-retry-acquire", true)
	request := protocol.Request{
		"protocol": protocol.Version, "action": "admin-reconcile", "session_id": task.SessionID,
		"generation": int64(0), "operation_id": "native-init-admin-retry", "apply": true,
	}
	digest, err := protocol.Digest(request)
	if err != nil {
		t.Fatal(err)
	}
	generation := int64(0)
	if err := store.RecordOperation(context.Background(), request["operation_id"].(string), "admin-reconcile", stringPointer(task.SessionID), &generation, map[string]any{
		"outcome": "pending", "_request": map[string]any(request), "_request_digest": digest,
		"_recovery_attempts": int64(1), "worker": map[string]any{"state": "finished"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Guest: nativeInitMissingGuest(), Clock: func() time.Time { return now }})
	if _, err := service.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil || recovered.State != stateAbsent {
		t.Fatalf("reconciled task = %#v", recovered)
	}
	operation, err := store.GetOperation(context.Background(), request["operation_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] != "success" {
		t.Fatalf("reconciled maintenance operation = %#v", operation)
	}
	if pending, err := store.PendingOperations(context.Background()); err != nil || len(pending) != 0 {
		t.Fatalf("pending journals after retry = %#v, err=%v", pending, err)
	}
}

func TestReconcileLeavesUncertainNativeInitAbsenceBlocked(t *testing.T) {
	cases := []struct {
		name        string
		configure   func(*state.Store, *startupGuest, state.Task) error
		operation   func(*testing.T, *state.Store, state.Task)
		portRemain  bool
		noOperation bool
	}{
		{name: "present-foreign", configure: func(_ *state.Store, guest *startupGuest, task state.Task) error {
			guest.configErr = nil
			guest.config = map[string]any{
				"config":  map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": task.TaskID, "user.agent-vm.session": task.SessionID, "user.agent-vm.generation": "999"},
				"devices": map[string]any{}, "expanded_devices": map[string]any{},
			}
			return nil
		}},
		{name: "present-owned", configure: func(_ *state.Store, guest *startupGuest, task state.Task) error {
			guest.configErr = nil
			guest.config = map[string]any{
				"config":  map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": task.TaskID, "user.agent-vm.session": task.SessionID, "user.agent-vm.generation": "1"},
				"devices": map[string]any{}, "expanded_devices": map[string]any{},
			}
			return nil
		}},
		{name: "unavailable", configure: func(_ *state.Store, guest *startupGuest, _ state.Task) error {
			guest.configErr = errors.New("incus unavailable")
			return nil
		}},
		{name: "malformed", configure: func(_ *state.Store, guest *startupGuest, _ state.Task) error {
			guest.configErr = nil
			guest.config = map[string]any{"config": map[string]any{}}
			return nil
		}},
		{name: "ports-remain", configure: func(store *state.Store, _ *startupGuest, task state.Task) error {
			return store.UpsertPort(context.Background(), state.Port{TaskID: task.TaskID, Generation: task.Generation, PortName: "ssh", Protocol: "tcp", HostPort: 22022, VMPort: 22, DeviceName: "proxy-ssh-tcp", State: "preparing", CreatedAt: task.CreatedAt})
		}, portRemain: true},
		{name: "missing-request", configure: func(_ *state.Store, _ *startupGuest, _ state.Task) error { return nil }, operation: func(t *testing.T, store *state.Store, task state.Task) {
			recordPendingLifecycleOperation(t, store, "acquire", task.SessionID, 0, "native-init-missing-request", false)
		}},
		{name: "unknown-pending-action", configure: func(_ *state.Store, _ *startupGuest, _ state.Task) error { return nil }, operation: func(t *testing.T, store *state.Store, task state.Task) {
			recordPendingLifecycleOperation(t, store, "restart", task.SessionID, task.Generation, "native-init-unknown-action", true)
		}},
		{name: "missing-journal", configure: func(_ *state.Store, _ *startupGuest, _ state.Task) error { return nil }, noOperation: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			store, err := state.OpenForTest(filepath.Join(t.TempDir(), "native-init-blocked.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
			task := insertNativeInitRecoveryTask(t, store, now)
			guest := nativeInitMissingGuest()
			if testCase.configure != nil {
				if err := testCase.configure(store, guest, task); err != nil {
					t.Fatal(err)
				}
			}
			if testCase.operation != nil {
				testCase.operation(t, store, task)
			} else if !testCase.noOperation {
				recordPendingLifecycleOperation(t, store, "acquire", task.SessionID, 0, "native-init-blocked", true)
			}
			service := New(store, config.Defaults(), Options{Guest: guest, Clock: func() time.Time { return now }})
			if _, err := service.reconcile(context.Background(), true); err == nil {
				t.Fatal("reconcile unexpectedly recovered uncertain native-init state")
			}
			current, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			if current == nil || current.State != stateFailed || current.Generation != task.Generation || current.ReservationBytes != task.ReservationBytes {
				t.Fatalf("blocked task changed = %#v", current)
			}
			pending, err := store.PendingOperations(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			expectedPending := 1
			if testCase.noOperation {
				expectedPending = 0
			}
			if len(pending) != expectedPending {
				t.Fatalf("pending operations = %#v", pending)
			}
			if testCase.portRemain {
				ports, err := store.ListPorts(context.Background(), task.TaskID)
				if err != nil || len(ports) != 1 {
					t.Fatalf("blocked ports = %#v, err=%v", ports, err)
				}
			}
		})
	}
}

func TestNativeInitRecoveryRequiresJournalRequestGeneration(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "native-init-missing-generation.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	task := insertNativeInitRecoveryTask(t, store, now)
	request := map[string]any{
		"protocol": protocol.Version, "action": "acquire", "session_id": task.SessionID,
		"operation_id": "native-init-missing-generation", "generation": int64(0),
	}
	digest, err := protocol.Digest(protocol.Request(request))
	if err != nil {
		t.Fatal(err)
	}
	delete(request, "generation")
	generation := int64(0)
	if err := store.RecordOperation(context.Background(), "native-init-missing-generation", "acquire", stringPointer(task.SessionID), &generation, map[string]any{
		"outcome": "pending", "_request": request, "_request_digest": digest,
		"worker": map[string]any{"pid": int64(1234), "start_time": "1", "process_group": int64(1234), "state": "finished", "finished_at": "2026-10-27T00:00:00Z"},
	}); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Guest: nativeInitMissingGuest(), Clock: func() time.Time { return now }})
	if recovered, err := service.reconcileVerifiedNativeInitAbsence(context.Background(), task, ""); !recovered || err == nil {
		t.Fatalf("missing generation unexpectedly recovered: recovered=%v err=%v", recovered, err)
	}
	if pending, err := store.PendingOperations(context.Background()); err != nil || len(pending) != 1 {
		t.Fatalf("missing generation journals = %#v, err=%v", pending, err)
	}
}

func TestReconcileBindsGenerationZeroAcquireAfterTombstone(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "native-init-generation-tombstone.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	task := insertNativeInitRecoveryTask(t, store, now)
	lastError := mustJSON(map[string]any{"phase": "native-init", "operation_id": "native-init-tombstone-acquire", "cause": "incus init failed", "outcome_verified": false})
	if err := store.UpdateTask(context.Background(), task.TaskID, map[string]any{"generation": int64(3), "previous_generation": nil, "last_error": lastError}); err != nil {
		t.Fatal(err)
	}
	task.Generation = 3
	request := recordPendingLifecycleOperation(t, store, "acquire", task.SessionID, 0, "native-init-tombstone-acquire", true)
	service := New(store, config.Defaults(), Options{Guest: nativeInitMissingGuest(), Clock: func() time.Time { return now }})
	if _, err := service.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if recovered == nil || recovered.State != stateAbsent || recovered.Generation != 3 {
		t.Fatalf("recovered task = %#v", recovered)
	}
	operation, err := store.GetOperation(context.Background(), request["operation_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if operation == nil || operation.Response["outcome"] == "pending" {
		t.Fatalf("generation-zero operation remained pending: %#v", operation)
	}
}

func TestAcquireReusesVerifiedAbsentTaskWithNextGeneration(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "absent-task-reuse.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	recoveryMarker := mustJSON(map[string]any{"phase": "native-init", "native_init_recovered_absent": true, "outcome_verified": true})
	task := state.Task{
		TaskID: "absent-reuse-task", SessionID: "absent-reuse-session", Generation: 3, State: stateAbsent,
		InstanceName: "agent-absent-reuse", CreatedAt: nowString(now), UpdatedAt: nowString(now), LastError: &recoveryMarker,
		ResourcesJSON: mustJSON(zeroResources()), ResourceReservationJSON: "{}",
	}
	if err := store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Clock: func() time.Time { return now }})
	response, err := service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version, "action": "acquire", "session_id": task.SessionID, "generation": int64(0), "operation_id": "absent-reuse-acquire",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := response["result"].(map[string]any)
	if result["state"] != stateActive || result["generation"] != int64(4) {
		t.Fatalf("reused absent task = %#v", result)
	}
	if _, err := service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version, "action": "destroy", "session_id": task.SessionID, "generation": int64(4), "operation_id": "absent-reuse-destroy",
	}); err != nil {
		t.Fatal(err)
	}
	remaining, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if remaining != nil {
		t.Fatalf("destroy left reused task = %#v", remaining)
	}
}

func TestAcquireReusesVerifiedAbsentTaskWithCurrentGeneration(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "absent-task-current-generation.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	recoveryMarker := mustJSON(map[string]any{"phase": "native-init", "native_init_recovered_absent": true, "outcome_verified": true})
	task := state.Task{
		TaskID: "absent-current-task", SessionID: "absent-current-session", Generation: 7, State: stateAbsent,
		InstanceName: "agent-absent-current", CreatedAt: nowString(now), UpdatedAt: nowString(now), LastError: &recoveryMarker,
		ResourcesJSON: mustJSON(zeroResources()), ResourceReservationJSON: "{}",
	}
	if err := store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Clock: func() time.Time { return now }})
	response, err := service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version, "action": "acquire", "session_id": task.SessionID, "generation": task.Generation, "operation_id": "absent-current-acquire",
	})
	if err != nil {
		t.Fatal(err)
	}
	result := response["result"].(map[string]any)
	if result["state"] != stateActive || result["generation"] != int64(8) {
		t.Fatalf("current-generation absent reuse = %#v", result)
	}
}

func TestAcquireLeavesAbsentTaskWithResidualCleanupMetadataBlocked(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "absent-task-residual.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	inactive := nowString(now)
	task := state.Task{
		TaskID: "absent-residual-task", SessionID: "absent-residual-session", Generation: 1, State: stateAbsent,
		InstanceName: "agent-absent-residual", InactiveSince: &inactive, CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: mustJSON(zeroResources()), ResourceReservationJSON: "{}",
	}
	if err := store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	service := New(store, config.Defaults(), Options{Guest: nativeInitMissingGuest(), Clock: func() time.Time { return now }})
	_, dispatchErr := service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version, "action": "acquire", "session_id": task.SessionID,
		"generation": int64(0), "operation_id": "absent-residual-acquire",
	})
	if dispatchErr == nil || protocol.AsError(dispatchErr).Code != "task_transition" {
		t.Fatalf("residual acquire error = %#v", dispatchErr)
	}
	current, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.State != stateAbsent || current.InactiveSince == nil {
		t.Fatalf("residual absent task changed = %#v", current)
	}
}

func TestDestroyLeavesAbsentTaskWithOtherPendingJournalBlocked(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "absent-task-pending.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	task := state.Task{
		TaskID: "absent-pending-task", SessionID: "absent-pending-session", Generation: 1, State: stateAbsent,
		InstanceName: "agent-absent-pending", CreatedAt: nowString(now), UpdatedAt: nowString(now),
		ResourcesJSON: mustJSON(zeroResources()), ResourceReservationJSON: "{}",
	}
	if err := store.InsertTask(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	recordPendingLifecycleOperation(t, store, "release", task.SessionID, task.Generation, "absent-pending-release", true)
	service := New(store, config.Defaults(), Options{Guest: nativeInitMissingGuest(), Clock: func() time.Time { return now }})
	_, dispatchErr := service.Dispatch(context.Background(), protocol.Request{
		"protocol": protocol.Version, "action": "destroy", "session_id": task.SessionID,
		"generation": task.Generation, "operation_id": "absent-pending-destroy",
	})
	if dispatchErr == nil || protocol.AsError(dispatchErr).Code != "task_transition" {
		t.Fatalf("pending destroy error = %#v", dispatchErr)
	}
	current, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.State != stateAbsent {
		t.Fatalf("pending absent task changed = %#v", current)
	}
	pending, err := store.PendingOperations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].OperationID != "absent-pending-release" || pending[1].OperationID != "absent-pending-destroy" {
		t.Fatalf("pending journals = %#v", pending)
	}
}

func TestReconcileLeavesNativeInitRecoveryBlockedOnGenerationRace(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "native-init-generation-race.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	task := insertNativeInitRecoveryTask(t, store, now)
	recordPendingLifecycleOperation(t, store, "acquire", task.SessionID, 0, "native-init-generation-race", true)
	guest := nativeInitMissingGuest()
	guest.configHook = func() {
		if err := store.UpdateTask(context.Background(), task.TaskID, map[string]any{"generation": int64(2)}); err != nil {
			t.Fatal(err)
		}
	}
	service := New(store, config.Defaults(), Options{Guest: guest, Clock: func() time.Time { return now }})
	if _, err := service.reconcile(context.Background(), true); err == nil {
		t.Fatal("generation race unexpectedly recovered")
	}
	current, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.Generation != 2 || current.State != stateFailed {
		t.Fatalf("generation race task = %#v", current)
	}
	if pending, err := store.PendingOperations(context.Background()); err != nil || len(pending) != 1 {
		t.Fatalf("generation race journals = %#v, err=%v", pending, err)
	}
}

func TestReconcileKeepsOrdinaryFailedCleanupBehavior(t *testing.T) {
	store, err := state.OpenForTest(filepath.Join(t.TempDir(), "ordinary-failed-cleanup.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	task := insertNativeInitRecoveryTask(t, store, now)
	lastError := mustJSON(map[string]any{"phase": "native-config", "outcome_verified": false})
	if err := store.UpdateTask(context.Background(), task.TaskID, map[string]any{"last_error": lastError}); err != nil {
		t.Fatal(err)
	}
	guest := &startupGuest{
		status: "Stopped",
		config: map[string]any{
			"config":  map[string]any{"user.agent-vm.managed": "native-v1", "user.agent-vm.task": task.TaskID, "user.agent-vm.session": task.SessionID, "user.agent-vm.generation": "1"},
			"devices": map[string]any{}, "expanded_devices": map[string]any{},
		},
	}
	service := New(store, config.Defaults(), Options{Guest: guest, Clock: func() time.Time { return now }})
	if _, err := service.reconcile(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetTask(context.Background(), map[string]any{"session_id": task.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if current == nil || current.State != stateCached {
		t.Fatalf("ordinary failed cleanup = %#v", current)
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
