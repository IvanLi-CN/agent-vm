package deployd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

func TestFinalizeMigrationIsIdempotentAfterCompletionMarkerCommit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("migration writer verification requires Linux /proc")
	}
	root := t.TempDir()
	legacyState := filepath.Join(root, "state.sqlite3")
	recovery := filepath.Join(root, "migration")
	for _, path := range []string{legacyState, legacyState + "-wal", legacyState + "-shm"} {
		if err := os.WriteFile(path, []byte("legacy"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runtime := NewHostRuntime(Config{Migration: MigrationConfig{
		LegacyStateDB:      legacyState,
		RecoveryDirectory:  recovery,
		WriterCheckSeconds: 1,
	}})
	result := state.SQLiteMigrationResult{
		SourceSHA256: strings.Repeat("a", 64),
		SourceSize:   1,
		StateEpoch:   "epoch-1",
		TargetSHA256: strings.Repeat("b", 64),
	}
	if err := runtime.FinalizeMigration(context.Background(), "migration-1", true, result); err != nil {
		t.Fatalf("first migration finalization failed: %v", err)
	}
	if _, err := os.Stat(legacyState); !os.IsNotExist(err) {
		t.Fatalf("legacy state still exists after finalization, err=%v", err)
	}
	if _, err := os.Stat(runtime.migrationMarkerPath()); err != nil {
		t.Fatalf("migration completion marker is missing: %v", err)
	}
	if err := runtime.FinalizeMigration(context.Background(), "migration-1", true, result); err != nil {
		t.Fatalf("recovery finalization was not idempotent: %v", err)
	}
}
