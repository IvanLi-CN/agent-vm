package install

import (
	"path/filepath"
	"testing"
)

// Dead backups beside the recovery material used to abort a handoff because
// they did not match the private-file assertion. They belong to no
// transaction, so they must not be judged.
func TestRecoveryScopeSkipsHistoricalBackups(t *testing.T) {
	root := "/var/lib/agent-vm-native/migration"
	for _, dir := range []string{"rqlite-recovery"} {
		if !recoveryScopeDir(root, filepath.Join(root, dir)) {
			t.Fatalf("%s should be in scope", dir)
		}
	}
	for _, dir := range []string{"bridge-1.9.5", "quarantine-stage", "agent-vm-deploy-1.9.4"} {
		if recoveryScopeDir(root, filepath.Join(root, dir)) {
			t.Fatalf("%s should be out of scope", dir)
		}
	}
	if !recoveryScopeFile(root, filepath.Join(root, "migration-owner.sqlite3")) {
		t.Fatal("migration snapshot should be in scope")
	}
	if recoveryScopeFile(root, filepath.Join(root, "deployd.json.backup")) {
		t.Fatal("config backup should be out of scope")
	}
}
