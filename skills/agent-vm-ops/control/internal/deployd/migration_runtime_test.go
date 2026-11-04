package deployd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMigrationCompletionMarkerPrecedesIdempotentLegacyCleanup(t *testing.T) {
	root := t.TempDir()
	legacyState := filepath.Join(root, "state.sqlite3")
	recovery := filepath.Join(root, "migration")
	for _, path := range []string{legacyState, legacyState + "-wal", legacyState + "-shm"} {
		if err := os.WriteFile(path, []byte("legacy"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	host := NewHostRuntime(Config{Migration: MigrationConfig{
		LegacyStateDB:     legacyState,
		RecoveryDirectory: recovery,
	}})
	marker := migrationCompletionMarkerData{
		Format:            2,
		TransactionID:     "migration-1",
		BusinessAuthority: "rqlite",
		SourceSHA256:      strings.Repeat("a", 64),
		SourceSize:        1,
		StateEpoch:        "epoch-1",
		TargetSHA256:      strings.Repeat("b", 64),
		RetentionUntil:    time.Now().UTC().Add(time.Hour),
	}
	if err := host.ensureRecoveryDirectory(); err != nil {
		t.Fatal(err)
	}
	if err := host.writeMigrationMarker(marker); err != nil {
		t.Fatalf("write migration completion marker failed: %v", err)
	}
	if _, err := os.Stat(legacyState); err != nil {
		t.Fatalf("legacy state disappeared before marker cleanup: %v", err)
	}
	if err := removeLegacySQLiteFiles(legacyState); err != nil {
		t.Fatalf("first legacy cleanup failed: %v", err)
	}
	if _, err := os.Stat(legacyState); !os.IsNotExist(err) {
		t.Fatalf("legacy state still exists after cleanup, err=%v", err)
	}
	if _, err := os.Stat(host.migrationMarkerPath()); err != nil {
		t.Fatalf("migration completion marker is missing: %v", err)
	}
	if err := removeLegacySQLiteFiles(legacyState); err != nil {
		t.Fatalf("recovery cleanup was not idempotent: %v", err)
	}
}

func TestSQLiteSourceFootprintIncludesWALAndSHM(t *testing.T) {
	root := t.TempDir()
	statePath := filepath.Join(root, "state.sqlite3")
	for path, content := range map[string]string{
		statePath:          "main",
		statePath + "-wal": "wal",
		statePath + "-shm": "shm",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	footprint, err := sqliteSourceFootprint(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if footprint != int64(len("main")+len("wal")+len("shm")) {
		t.Fatalf("SQLite source footprint = %d", footprint)
	}
}

func TestCleanupMigrationSnapshotTempsRemovesOnlyIncompleteRegularFiles(t *testing.T) {
	directory := t.TempDir()
	temporary := filepath.Join(directory, "migration-1.sqlite3.tmp")
	if err := os.WriteFile(temporary, []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupMigrationSnapshotTemps(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(temporary); !os.IsNotExist(err) {
		t.Fatalf("temporary migration snapshot still exists, err=%v", err)
	}
}
