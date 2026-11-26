package deployd

import (
	"os"
	"path/filepath"
	"testing"
)

// Historical backups parked in the recovery directory are never restored by a
// transaction. Charging them against the budget starved deployments on hosts
// that accumulated them (119 MiB of dead weight on one live host).
func TestScopedRecoveryBytesIgnoresHistoricalBackups(t *testing.T) {
	root := t.TempDir()
	write := func(relative string, size int) {
		path := filepath.Join(root, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// In scope: a recovery point and a migration snapshot.
	write("rqlite-recovery/point-x/snapshot.sqlite3", 100)
	write("migration-owner.sqlite3", 50)
	// Out of scope: parked binaries, quarantined stages, input copies.
	write("bridge-1.9.5/bridge.ts", 1000)
	write("quarantine-stage/inputs.json", 1000)
	write("pre-deployd-upgrade/adapter", 1000)

	total, err := scopedRecoveryMaterialBytes(root)
	if err != nil {
		t.Fatal(err)
	}
	if total != 150 {
		t.Fatalf("scoped bytes = %d, want 150", total)
	}
}
