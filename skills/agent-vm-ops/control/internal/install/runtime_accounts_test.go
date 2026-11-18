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
