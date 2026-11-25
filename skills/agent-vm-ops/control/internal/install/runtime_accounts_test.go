package install

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

func TestInstallMaintenanceBinaryRefreshesEveryOperatorPath(t *testing.T) {
	product := t.TempDir()
	if err := os.MkdirAll(filepath.Join(product, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(product, "bin", "agent-vm-deploy")
	if err := os.WriteFile(source, []byte("agent-vm-deploy/9.9.9"), 0o755); err != nil {
		t.Fatal(err)
	}
	targets := []string{filepath.Join(t.TempDir(), "maintenance", "agent-vm-deploy"), filepath.Join(t.TempDir(), "bin", "agent-vm-deploy")}
	for _, target := range targets {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte("agent-vm-deploy/1.0.0"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	previous := DeployBinaryPaths
	DeployBinaryPaths = targets
	defer func() { DeployBinaryPaths = previous }()
	binary, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	backups, err := installDeployBinary(product, func(_, destination string, _ os.FileMode, _ string) error {
		return os.WriteFile(destination, binary, 0o755)
	})
	if err != nil || len(backups) != len(targets) {
		t.Fatalf("backups = %v, err = %v", backups, err)
	}
	for index, target := range targets {
		if installed, err := os.ReadFile(target); err != nil || string(installed) != "agent-vm-deploy/9.9.9" {
			t.Fatalf("%s was not replaced: %v", target, err)
		}
		if kept, err := os.ReadFile(backups[index]); err != nil || string(kept) != "agent-vm-deploy/1.0.0" {
			t.Fatalf("%s backup = %v", backups[index], err)
		}
	}
}

func TestMigratedDeploydUnitWidensDropInsOnce(t *testing.T) {
	original := []byte("[Service]\nExecStart=/opt/agent-vm-native/bin/agent-vm-deploy serve --config /etc/agent-vm-native/deployd.json\nReadWritePaths=/var/lib/agent-vm-native /run/agent-vm-native\n")
	migrated, err := migratedDeploydUnit(original)
	if err != nil {
		t.Fatal(err)
	}
	text := string(migrated)
	if !strings.Contains(text, "ExecStart="+migrationDeployBinary+" serve") {
		t.Fatalf("executable was not moved to the maintenance copy:\n%s", text)
	}
	for _, dir := range []string{"/etc/systemd/system/agent-vm-native@blue.service.d", "/etc/systemd/system/agent-vm-native@green.service.d"} {
		if !strings.Contains(text, dir) {
			t.Fatalf("%s missing from ReadWritePaths:\n%s", dir, text)
		}
	}
	again, err := migratedDeploydUnit(migrated)
	if err != nil || string(again) != text {
		t.Fatalf("migration is not idempotent: %v\n%s", err, again)
	}
	if _, err := migratedDeploydUnit([]byte("[Service]\nRestart=on-failure\n")); err == nil {
		t.Fatal("unit without a deploy executable was accepted")
	}
}

func TestFreshSlotAccountGrantsRqliteCertificateRead(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("../../../../..", "deploy/assets/systemd/fresh-slot-account.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "SupplementaryGroups=agent-vm-server agent-vm-rqlite") {
		t.Fatalf("slot account drop-in cannot read the rqlite certificate:\n%s", source)
	}
}

func TestMigratedDeploydUnitAddsMissingReadWritePaths(t *testing.T) {
	original := []byte("[Service]\nExecStart=/opt/agent-vm-native/bin/agent-vm-deploy serve --config /etc/agent-vm-native/deployd.json\n")
	migrated, err := migratedDeploydUnit(original)
	if err != nil {
		t.Fatal(err)
	}
	text := string(migrated)
	if strings.Count(text, "ReadWritePaths=") != 1 {
		t.Fatalf("expected exactly one ReadWritePaths line:\n%s", text)
	}
	if !strings.Contains(text, "/etc/systemd/system/agent-vm-native@green.service.d") {
		t.Fatalf("drop-in path missing:\n%s", text)
	}
}
