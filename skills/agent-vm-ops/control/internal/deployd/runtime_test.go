package deployd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAdminMetricsValidationUsesTopLevelObservationEpoch(t *testing.T) {
	result := map[string]any{
		"up":                            true,
		"observed_at":                   "2026-11-03T00:00:00Z",
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

func TestActiveSlotRequiresLink(t *testing.T) {
	runtime := NewHostRuntime(Config{ActivePath: filepath.Join(t.TempDir(), "active")})
	if _, err := runtime.activeSlot(); err == nil {
		t.Fatal("missing active link was treated as a valid slot")
	}
}
