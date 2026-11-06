package deployd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
)

func TestSupportedBundleManifestFormats(t *testing.T) {
	for _, format := range []string{"2", "3", "4"} {
		if !supportedBundleManifestFormat(json.Number(format)) {
			t.Fatalf("manifest format %s was rejected", format)
		}
	}
	if supportedBundleManifestFormat(json.Number("1")) {
		t.Fatal("unsupported manifest format was accepted")
	}
	if supportedBundleManifestFormat(json.Number("not-a-number")) {
		t.Fatal("malformed manifest format was accepted")
	}
}

func TestSlotSocketLinkAllowsProxyTraversalAndRejectsPrivateStage(t *testing.T) {
	root, err := os.MkdirTemp(t.TempDir(), "stage-")
	if err != nil {
		t.Fatal(err)
	}
	runtime := &HostRuntime{Config: Defaults()}
	if err := runtime.ensureSlotSocketLink(root, "blue"); err != nil {
		t.Fatal(err)
	}
	if err := runtime.verifySlotSocketLink(root, "blue"); err != nil {
		t.Fatalf("proxy cannot traverse published slot: %v", err)
	}
	for _, mode := range []os.FileMode{0o700, 0o770, 0o755} {
		if err := os.Chmod(root, mode); err != nil {
			t.Fatal(err)
		}
		if err := runtime.verifySlotSocketLink(root, "blue"); err == nil {
			t.Fatalf("unsafe or inaccessible slot mode %o accepted", mode)
		}
	}
}

func TestAdminMetricsValidationUsesTopLevelObservationEpoch(t *testing.T) {
	result := map[string]any{
		"up":                            true,
		"observed_at":                   "2026-11-03T00:00:00Z",
		"observation_consistent":        true,
		"observation_epoch":             "epoch-1",
		"pending_operations":            []any{},
		"historical_unknown_operations": []any{},
		"in_flight_operations":          float64(0),
		"doctor":                        map[string]any{"ready": true, "issues": []any{}},
	}
	if err := validateSourceResult("admin-metrics", result); err != nil {
		t.Fatalf("admin-metrics response rejected: %v", err)
	}
	delete(result, "observation_epoch")
	if err := validateSourceResult("admin-metrics", result); err == nil {
		t.Fatal("admin-metrics response without observation epoch was accepted")
	}
}

func TestAdminMetricsValidationRejectsInconsistentObservation(t *testing.T) {
	result := map[string]any{
		"up":                            true,
		"observed_at":                   "2026-11-03T00:00:00Z",
		"observation_consistent":        false,
		"observation_epoch":             "epoch-1",
		"pending_operations":            []any{},
		"historical_unknown_operations": []any{},
		"in_flight_operations":          float64(0),
		"doctor":                        map[string]any{"ready": true, "issues": []any{}},
	}
	if err := validateSourceResult("admin-metrics", result); err == nil {
		t.Fatal("inconsistent admin-metrics response was accepted")
	}
}

func TestAdminStatusValidationRetainsPartialObservation(t *testing.T) {
	result := map[string]any{
		"up": true, "observed_at": "2026-11-03T00:00:00Z", "partial": false,
		"unknown": float64(0), "probed": float64(1), "verified": float64(1), "timed_out": float64(0),
		"observation_consistent": true, "tasks": []any{},
	}
	if err := validateSourceResult("admin-status", result); err != nil {
		t.Fatalf("complete admin-status response rejected: %v", err)
	}
	result["up"] = false
	result["partial"] = true
	result["unknown"] = float64(1)
	if err := validateSourceResult("admin-status", result); err != nil {
		t.Fatalf("partial admin-status response rejected: %v", err)
	}
	result["partial"] = false
	if err := validateSourceResult("admin-status", result); err == nil {
		t.Fatal("admin-status up=false without partial observation was accepted")
	}
}

func TestInventoryObservationConsistencyRejectsPartialSnapshots(t *testing.T) {
	if !inventoryObservationConsistent(map[string]any{"partial": false, "observation_consistent": true}) {
		t.Fatal("complete inventory observation was rejected")
	}
	for index, value := range []map[string]any{
		{"partial": true, "observation_consistent": true},
		{"partial": false, "observation_consistent": false},
		{"partial": false},
	} {
		if inventoryObservationConsistent(value) {
			t.Fatalf("invalid inventory observation %d was accepted: %#v", index, value)
		}
	}
}

func TestValidObservedAtRejectsFutureTimestamp(t *testing.T) {
	now := time.Date(2026, time.November, 3, 0, 0, 0, 0, time.UTC)
	if _, ok := validObservedAt("2026-11-03T00:00:01Z", now); ok {
		t.Fatal("future observation timestamp was accepted")
	}
	if _, ok := validObservedAt("2026-11-03T00:00:00Z", now); !ok {
		t.Fatal("current observation timestamp was rejected")
	}
}

func TestValidObservationRejectsMissingSourceFreshness(t *testing.T) {
	now := time.Now().UTC()
	sources := map[string]SourceObservation{}
	for _, name := range []string{"active_link", "systemd", "slot_contracts", "native_observation", "inventory_observation"} {
		sources[name] = SourceObservation{Up: true, ObservedAt: now, FreshUntil: now.Add(time.Minute)}
	}
	sources["systemd"] = SourceObservation{Up: true, ObservedAt: now}
	observation := RuntimeSnapshot{
		ObservedAt: now,
		FreshUntil: now.Add(time.Minute),
		ActiveSlot: "blue",
		Consistent: true,
		Sources:    sources,
	}
	if validObservation(observation, now) {
		t.Fatal("observation with missing source freshness was accepted")
	}
}

func TestCompareReleaseTreesRejectsStagedTampering(t *testing.T) {
	expectedRoot := t.TempDir()
	actualRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(expectedRoot, "release-manifest.json"), []byte("manifest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actualRoot, "release-manifest.json"), []byte("manifest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compareReleaseTrees(expectedRoot, actualRoot); err != nil {
		t.Fatalf("identical release trees rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(actualRoot, "release-manifest.json"), []byte("tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compareReleaseTrees(expectedRoot, actualRoot); err == nil {
		t.Fatal("tampered release tree was accepted")
	}
	if err := os.WriteFile(filepath.Join(actualRoot, "release-manifest.json"), []byte("manifest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(actualRoot, "unexpected"), []byte("extra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compareReleaseTrees(expectedRoot, actualRoot); err == nil {
		t.Fatal("release tree with an undeclared member was accepted")
	}
}

func TestParseSystemdProperties(t *testing.T) {
	properties := parseSystemdProperties("ActiveState=active\nMainPID=42\nExecStart={ path=/var/lib/agent-vm-native/slots/blue/bin/agent-vm-control ; argv[]=/var/lib/agent-vm-native/slots/blue/bin/agent-vm-control serve }\n")
	if properties["ActiveState"] != "active" || properties["MainPID"] != "42" {
		t.Fatalf("parsed systemd properties = %#v", properties)
	}
	if properties["ExecStart"] == "" {
		t.Fatal("ExecStart property was not retained")
	}
}

func TestProcessIdentityRequiresStartToken(t *testing.T) {
	if _, ok := processIdentityFromHealth(map[string]any{"process_id": float64(42), "process_start_time": ""}); ok {
		t.Fatal("empty process start token was accepted")
	}
	if sameProcess(processIdentity{PID: 42, StartTime: ""}, processIdentity{PID: 42, StartTime: ""}) {
		t.Fatal("empty process start token matched another process")
	}
}

func TestStoppedSlotCountersRequireDeploydStopRequest(t *testing.T) {
	evidence := quiescenceEvidence{Process: processIdentity{PID: 42, StartTime: "start-1"}}
	if quiescenceEvidenceProvesStopped(evidence) {
		t.Fatal("quiescence evidence without a deployd stop request proved stopped counters")
	}
	evidence.StopRequested = true
	if !quiescenceEvidenceProvesStopped(evidence) {
		t.Fatal("verified deployd stop request did not prove stopped counters")
	}
	evidence.Process = processIdentity{}
	if quiescenceEvidenceProvesStopped(evidence) {
		t.Fatal("quiescence evidence without process identity proved stopped counters")
	}
}

func TestProcessStartTokenMatchesExactProcessInstance(t *testing.T) {
	token := controllerrole.ProcessStartToken(os.Getpid())
	if token == "" {
		t.Skip("process start tokens are unavailable on this platform")
	}
	if !processStartTokenMatches(processIdentity{PID: int64(os.Getpid()), StartTime: token}) {
		t.Fatal("current process start token did not match")
	}
	if processStartTokenMatches(processIdentity{PID: int64(os.Getpid()), StartTime: "stale"}) {
		t.Fatal("stale process start token matched")
	}
}

func TestActiveLinkStableRequiresSameSuccessfulRead(t *testing.T) {
	if !activeLinkStable("blue", nil, "blue", nil) {
		t.Fatal("unchanged active link was not considered stable")
	}
	if activeLinkStable("blue", nil, "green", nil) {
		t.Fatal("active link change was considered stable")
	}
	if activeLinkStable("blue", nil, "blue", os.ErrNotExist) {
		t.Fatal("failed final active link read was considered stable")
	}
}

func TestSystemdExecStartMatchesExactSlotCommand(t *testing.T) {
	expected := "/var/lib/agent-vm-native/slots/blue/bin/agent-vm-control serve --config /etc/agent-vm-native/slots/blue/config.json --manifest /var/lib/agent-vm-native/slots/blue/release-manifest.json"
	valid := "{ path=/var/lib/agent-vm-native/slots/blue/bin/agent-vm-control ; argv[]=" + expected + " ; ignore_errors=no }"
	if !systemdExecStartMatches(valid, expected) {
		t.Fatal("exact systemd command was rejected")
	}
	unsafe := "{ path=/var/lib/agent-vm-native/slots/blue/bin/agent-vm-control ; argv[]=" + expected + " --unsafe ; ignore_errors=no }"
	if systemdExecStartMatches(unsafe, expected) {
		t.Fatal("systemd command with extra arguments was accepted")
	}
}

func TestSystemdSlotStoppedRequiresLoadedDeadUnit(t *testing.T) {
	properties := map[string]string{
		"LoadState":    "loaded",
		"ActiveState":  "inactive",
		"SubState":     "dead",
		"MainPID":      "0",
		"FragmentPath": "/etc/systemd/system/agent-vm-native@.service",
	}
	if !systemdSlotStoppedProperties(properties) {
		t.Fatal("loaded inactive unit was not recognized as stopped")
	}
	properties["SubState"] = "failed"
	if systemdSlotStoppedProperties(properties) {
		t.Fatal("failed unit was treated as an expected stopped slot")
	}
}

func TestSlotMetricsPreserveIndependentLanes(t *testing.T) {
	runtime := NewHostRuntime(Config{})
	runtime.rememberMetrics("blue", "capacity", map[string]string{"capacity_metric": "1"})
	runtime.rememberMetrics("blue", "inventory", map[string]string{"inventory_metric": "2"})
	runtime.rememberMetrics("blue", "health", map[string]string{"health_metric": "3"})
	runtime.invalidateMetrics("blue", "health")
	metrics := runtime.slotMetrics("blue")
	if metrics["capacity_metric"] != "1" || metrics["inventory_metric"] != "2" {
		t.Fatalf("independent slot metrics were cleared: %#v", metrics)
	}
	if _, ok := metrics["health_metric"]; ok {
		t.Fatalf("failed health lane remained visible: %#v", metrics)
	}
}

func TestActiveSlotRequiresLink(t *testing.T) {
	runtime := NewHostRuntime(Config{ActivePath: filepath.Join(t.TempDir(), "active")})
	if _, err := runtime.activeSlot(); err == nil {
		t.Fatal("missing active link was treated as a valid slot")
	}
}

func TestCoherentPartialInventoryIsNotDeploymentReady(t *testing.T) {
	inventory := map[string]any{"partial": true, "observation_consistent": true}
	if !inventoryObservationCoherent(inventory) || inventoryObservationConsistent(inventory) {
		t.Fatal("coherent partial inventory lost its recovery-only distinction")
	}
	inventory["observation_consistent"] = false
	if inventoryObservationCoherent(inventory) {
		t.Fatal("inconsistent inventory was accepted for recovery")
	}
	delete(inventory, "partial")
	if inventoryObservationCoherent(inventory) {
		t.Fatal("missing completeness was accepted for recovery")
	}
}

func TestSlotRequestPreservesControllerLeaseIdentity(t *testing.T) {
	directory, err := os.MkdirTemp("", "avm-uds-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	socket := filepath.Join(directory, "app.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	lease := controllerrole.Lease{Slot: "blue", Term: 9007199254740993, Generation: 1793891163387293090, LeaseID: "test-lease", Nonce: "test-nonce", Expiry: time.Now().Add(time.Minute)}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"lease": lease})
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	runtime := &HostRuntime{}
	body, status, err := runtime.slotRequest(context.Background(), socket, http.MethodGet, "/internal/v1/health", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("health status=%d err=%v", status, err)
	}
	encoded, err := json.Marshal(body["lease"])
	if err != nil {
		t.Fatal(err)
	}
	var observed controllerrole.Lease
	if err := json.Unmarshal(encoded, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Term != lease.Term || observed.Generation != lease.Generation {
		t.Fatalf("lease identity lost precision: term=%d generation=%d", observed.Term, observed.Generation)
	}
}

func TestTransactionRenewalRequiresLiveMatchingProcessAndLease(t *testing.T) {
	for _, scenario := range []string{"not-ready-live", "green-not-ready-live", "expired", "changed-lease", "changed-process"} {
		t.Run(scenario, func(t *testing.T) {
			dir, err := os.MkdirTemp("", "avm-bootstrap-uds-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(dir)
			socket := filepath.Join(dir, "app.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			lease := controllerrole.Lease{Slot: "blue", Term: 3, Generation: 1, LeaseID: "original", Nonce: "original-nonce", Expiry: time.Now().Add(5 * time.Second)}
			if scenario == "green-not-ready-live" {
				lease.Slot = "green"
			}
			if scenario == "expired" {
				lease.Expiry = time.Now().Add(-time.Second)
			}
			observed := lease
			if scenario == "changed-lease" {
				observed.Nonce = "different"
			}
			process := processIdentity{PID: 123, StartTime: "456"}
			if scenario == "changed-process" {
				process.StartTime = "457"
			}
			grants := 0
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.WriteHeader(http.StatusServiceUnavailable)
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "process_ready": true, "controller_owned": true, "ready": false, "lease": observed, "process_id": process.PID, "process_start_time": process.StartTime})
				} else {
					grants++
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "valid": true})
				}
			}))
			server.Listener.Close()
			server.Listener = listener
			server.Start()
			defer server.Close()
			cfg := Defaults()
			cfg.BlueSocket = socket
			cfg.GreenSocket = socket
			runtime := NewHostRuntime(cfg)
			runtime.stagedProcess[lease.Slot] = processIdentity{PID: 123, StartTime: "456"}
			renewed, err := runtime.RenewTransactionLease(context.Background(), lease)
			if scenario == "not-ready-live" || scenario == "green-not-ready-live" {
				if err != nil || grants != 1 || !renewed.Expiry.After(lease.Expiry) {
					t.Fatalf("valid startup renewal: %+v %v grants=%d", renewed, err, grants)
				}
				renewed.Expiry = lease.Expiry
				if renewed != lease {
					t.Fatal("renewal changed identity")
				}
			} else if err == nil || grants != 0 {
				t.Fatal("invalid authority renewed")
			}
		})
	}
}

func TestCapacityHealthRemainsCoherentAcrossLaterCommits(t *testing.T) {
	directory, err := os.MkdirTemp("", "avm-observe-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	socket := filepath.Join(directory, "app.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		if request["action"] != "admin-metrics" {
			t.Errorf("unexpected separate observation: %v", request)
			w.WriteHeader(500)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "protocol": 4, "action": "admin-metrics", "result": map[string]any{
			"up": true, "observed_at": time.Now().UTC().Format(time.RFC3339Nano), "observation_consistent": true, "observation_epoch": "before-later-commit",
			"pending_operations": []any{}, "historical_unknown_operations": []any{}, "in_flight_operations": 0,
			"doctor": map[string]any{"ready": true, "issues": []any{}},
		}})
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	runtime := NewHostRuntime(Config{BlueSocket: socket})
	metrics, health, _, err := runtime.observeCapacityHealth(context.Background(), "blue")
	if err != nil {
		t.Fatal(err)
	}
	if metrics["observation_epoch"] != "before-later-commit" || health["state_epoch"] != metrics["observation_epoch"] || health["ready"] != true {
		t.Fatalf("incoherent capacity/health: metrics=%v health=%v", metrics, health)
	}
}

func TestInventoryRetriesConcurrentMaintenanceCommit(t *testing.T) {
	for _, keepsChanging := range []bool{false, true} {
		t.Run(fmt.Sprint(keepsChanging), func(t *testing.T) {
			directory, err := os.MkdirTemp("", "avm-inventory-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(directory)
			socket := filepath.Join(directory, "app.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				consistent := calls > 1 && !keepsChanging
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "protocol": 4, "action": "admin-status", "result": map[string]any{
					"up": consistent, "observed_at": time.Now().UTC().Format(time.RFC3339Nano), "partial": !consistent, "observation_consistent": consistent,
					"unknown": 0, "probed": 0, "verified": 0, "timed_out": 0, "tasks": []any{},
				}})
			}))
			server.Listener.Close()
			server.Listener = listener
			server.Start()
			defer server.Close()
			runtime := NewHostRuntime(Config{BlueSocket: socket})
			inventory, _, err := runtime.observeSource(context.Background(), "blue", "inventory", "admin-status")
			if err != nil {
				t.Fatal(err)
			}
			wantCalls := 2
			if keepsChanging {
				wantCalls = 3
			}
			if calls != wantCalls || inventoryObservationCoherent(inventory) == keepsChanging {
				t.Fatalf("calls=%d inventory=%v", calls, inventory)
			}
		})
	}
}

func TestExitDrainDoesNotWriteWhenAdmissionIsAlreadyOpen(t *testing.T) {
	directory, err := os.MkdirTemp("", "avm-drain-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	socket := filepath.Join(directory, "app.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		if input["action"] != "drain-status" {
			t.Errorf("already-open admission caused mutation: %v", input["action"])
			w.WriteHeader(500)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "protocol": 4, "action": "drain-status", "result": map[string]any{"draining": false}})
	}))
	server.Listener.Close()
	server.Listener = listener
	server.Start()
	defer server.Close()
	runtime := NewHostRuntime(Config{BlueSocket: socket})
	if err := runtime.ExitDrain(context.Background(), "blue"); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackDrainRecoveryRejectsUnprovenIntentAndLiveRoles(t *testing.T) {
	for _, scenario := range []string{"recover", "native-action", "wrong-owner", "wrong-id", "wrong-generation", "in-flight", "controller-live", "traffic-live", "mixed"} {
		t.Run(scenario, func(t *testing.T) {
			directory, err := os.MkdirTemp("", "avm-drain-resume-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(directory)
			socket := filepath.Join(directory, "app.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			row := map[string]any{"action": "drain-exit", "session_id": deploydDrainSession, "operation_id": "deployd-drain-exit-original", "generation": 0}
			switch scenario {
			case "native-action":
				row["action"] = "destroy"
			case "wrong-owner":
				row["session_id"] = "release-other"
			case "wrong-id":
				row["operation_id"] = "foreign"
			case "wrong-generation":
				row["generation"] = 1
			}
			pending := []any{row}
			if scenario == "mixed" {
				pending = append(pending, map[string]any{"action": "destroy"})
			}
			writes := 0
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/internal/v1/deployment-observation" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if r.Method == http.MethodGet {
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "protocol": 4, "process_ready": true, "ready": false, "controller_owned": scenario == "controller-live", "traffic_accepting": scenario == "traffic-live", "process_id": 123, "process_start_time": "1"})
					return
				}
				var request map[string]any
				json.NewDecoder(r.Body).Decode(&request)
				if request["action"] == "admin-metrics" {
					flight := 0
					if scenario == "in-flight" {
						flight = 1
					}
					json.NewEncoder(w).Encode(map[string]any{"ok": true, "protocol": 4, "action": "admin-metrics", "result": map[string]any{"pending_operations": pending, "in_flight_operations": flight}})
					return
				}
				writes++
				if request["operation_id"] != "deployd-drain-exit-original" || request["preserve_traffic"] != true {
					t.Errorf("wrong replay: %v", request)
				}
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "protocol": 4, "action": request["action"], "result": map[string]any{}})
			}))
			server.Listener.Close()
			server.Listener = listener
			server.Start()
			defer server.Close()
			runtime := NewHostRuntime(Config{BlueSocket: socket, GreenSocket: socket})
			err = runtime.resumeRollbackDrainExit(context.Background(), "green")
			if scenario == "recover" {
				if err != nil || writes != 1 {
					t.Fatalf("recovery writes=%d err=%v", writes, err)
				}
			} else if err == nil || writes != 0 {
				t.Fatalf("unsafe resume writes=%d err=%v", writes, err)
			}
		})
	}
}

func TestQuiescenceUsesCoherentLaneAndDoesNotFallbackOnFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			directory, err := os.MkdirTemp("", "avm-qview-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(directory)
			socket := filepath.Join(directory, "app.sock")
			listener, err := net.Listen("unix", socket)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/internal/v1/deployment-observation" {
					t.Error("coherent observation fell back to independent probes")
					w.WriteHeader(500)
					return
				}
				if failure {
					w.WriteHeader(500)
					return
				}
				json.NewEncoder(w).Encode(map[string]any{"ok": true, "protocol": 4, "result": map[string]any{"capacity": map[string]any{"up": true, "observed_at": time.Now().UTC().Format(time.RFC3339Nano), "observation_consistent": true, "observation_epoch": "epoch", "pending_operations": []any{}, "historical_unknown_operations": []any{}, "in_flight_operations": 0, "doctor": map[string]any{"issues": []any{}}}}})
			}))
			server.Listener.Close()
			server.Listener = listener
			server.Start()
			defer server.Close()
			runtime := NewHostRuntime(Config{BlueSocket: socket})
			value, err := runtime.quiescenceMetrics(context.Background(), "blue")
			if failure {
				if err == nil {
					t.Fatal("failed snapshot accepted")
				}
			} else if err != nil || value["observation_epoch"] != "epoch" {
				t.Fatalf("coherent lane=%v err=%v", value, err)
			}
		})
	}
}
