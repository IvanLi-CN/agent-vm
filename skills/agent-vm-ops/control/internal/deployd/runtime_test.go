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
