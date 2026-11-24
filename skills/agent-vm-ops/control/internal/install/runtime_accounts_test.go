package install

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestMigrationObserverKeepsServiceRestrictions(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("../../../../..", "deploy/assets/systemd/agent-vm-observe@.service"))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := migrationObserverUnit(source)
	if err != nil {
		t.Fatal(err)
	}
	recreated := bytes.Replace(prepared, []byte("ExecStart="+migrationDeployBinary), []byte("ExecStart=/opt/agent-vm-native/bin/agent-vm-deploy"), 1)
	if bytes.Equal(source, prepared) || !bytes.Equal(source, recreated) {
		t.Fatal("migration observer must change only its executable path")
	}
	if _, err := migrationObserverUnit(prepared); err == nil {
		t.Fatal("unexpected original command was accepted")
	}
}

func TestHandoffHelpersKeepServiceRestrictions(t *testing.T) {
	for _, tc := range []struct{ unit, action string }{
		{"agent-vm-deploy-host@.service", "host-action"},
		{"agent-vm-slot-relay@.service", "slot-relay"},
		{"agent-vm-request@.service", "request-relay"},
	} {
		t.Run(tc.action, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join("../../../../..", "deploy/assets/systemd", tc.unit))
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := migrationHelperUnit(source, tc.action)
			if err != nil {
				t.Fatal(err)
			}
			recreated := bytes.Replace(prepared, []byte("ExecStart="+migrationDeployBinary), []byte("ExecStart=/opt/agent-vm-native/bin/agent-vm-deploy"), 1)
			if bytes.Equal(source, prepared) || !bytes.Equal(source, recreated) {
				t.Fatal("migration helper must change only its executable path")
			}
		})
	}
}

func TestMigratedSlotProbesUseVerifiedMaintenanceBinary(t *testing.T) {
	steady, migration, err := slotAccountDropIns(filepath.Join("../../../../..", "deploy"))
	if err != nil {
		t.Fatal(err)
	}
	for _, content := range [][]byte{steady, migration} {
		for _, command := range []string{
			"ExecStartPre=" + migrationDeployBinary + " wait-authority",
			"ExecStartPost=\nExecStartPost=" + migrationDeployBinary + " wait-process",
		} {
			if !bytes.Contains(content, []byte(command)) {
				t.Errorf("missing required slot probe %q", command)
			}
		}
		if bytes.Contains(content, []byte("/opt/agent-vm-native/bin/agent-vm-deploy")) {
			t.Fatal("migrated slot probe still invokes the root-only legacy binary")
		}
	}
}

func TestDeployBinaryVersionReadsSelfReport(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent-vm-deploy")
	// Real Go binaries embed the version literal; build a Go string table copy.
	contents := []byte("\x7fELF\x00agent-vm-deploy/1.12.0\x00other\x00")
	if err := os.WriteFile(path, contents, 0o755); err != nil {
		t.Fatal(err)
	}
	version, err := DeployBinaryVersion(path)
	if err != nil || version != "agent-vm-deploy/1.12.0" {
		t.Fatalf("version = %q, err = %v", version, err)
	}
	if _, err := DeployBinaryVersion(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing binary was accepted")
	}
}
