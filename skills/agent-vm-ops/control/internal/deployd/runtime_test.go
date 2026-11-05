package deployd

import (
	"context"
	"encoding/json"
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
