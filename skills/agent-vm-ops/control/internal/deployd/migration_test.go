package deployd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

type fakeMigrationRuntime struct {
	calls        []string
	failImport   bool
	failRetry    bool
	failFinalize bool
	failTarget   bool
	backup       MigrationSource
	result       state.SQLiteMigrationResult
}

func (runtime *fakeMigrationRuntime) MigrationComplete(context.Context) (bool, error) {
	runtime.calls = append(runtime.calls, "migration-complete")
	return false, nil
}

func (runtime *fakeMigrationRuntime) PreflightMigration(context.Context) error {
	runtime.calls = append(runtime.calls, "preflight")
	return nil
}

func (runtime *fakeMigrationRuntime) StartStateAuthority(context.Context) error {
	runtime.calls = append(runtime.calls, "start-authority")
	return nil
}

func (runtime *fakeMigrationRuntime) GrantLegacyController(context.Context, controllerrole.Lease) error {
	runtime.calls = append(runtime.calls, "grant-legacy-controller")
	return nil
}

func (runtime *fakeMigrationRuntime) EnterLegacyFence(context.Context, string) error {
	runtime.calls = append(runtime.calls, "enter-fence")
	return nil
}

func (runtime *fakeMigrationRuntime) WaitLegacyQuiescence(context.Context) (MigrationQuiescence, error) {
	runtime.calls = append(runtime.calls, "quiesce")
	return MigrationQuiescence{StateEpoch: "epoch-1"}, nil
}

func (runtime *fakeMigrationRuntime) StopLegacy(context.Context) error {
	runtime.calls = append(runtime.calls, "stop-legacy")
	return nil
}

func (runtime *fakeMigrationRuntime) VerifyLegacyStopped(context.Context) error {
	runtime.calls = append(runtime.calls, "verify-stopped")
	return nil
}

func (runtime *fakeMigrationRuntime) VerifySQLiteWritersClosed(context.Context) error {
	runtime.calls = append(runtime.calls, "verify-writers")
	return nil
}

func (runtime *fakeMigrationRuntime) PrepareMigrationSource(context.Context, string) (MigrationSource, error) {
	runtime.calls = append(runtime.calls, "prepare-source")
	if runtime.backup.BackupPath == "" {
		runtime.backup = MigrationSource{Evidence: state.SQLiteMigrationEvidence{FenceID: "migration", SourceSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SourceSize: 1, StateEpoch: "epoch-1", VerifiedAt: time.Now().UTC()}, BackupPath: "/tmp/migration.sqlite3"}
	}
	return runtime.backup, nil
}

func (runtime *fakeMigrationRuntime) ImportMigration(context.Context, MigrationSource) (state.SQLiteMigrationResult, error) {
	runtime.calls = append(runtime.calls, "import")
	if runtime.failImport {
		return state.SQLiteMigrationResult{}, errors.New("import outcome unknown")
	}
	return runtime.result, nil
}

func (runtime *fakeMigrationRuntime) RetryMigration(context.Context, string, state.SQLiteMigrationEvidence, bool) (state.SQLiteMigrationResult, error) {
	runtime.calls = append(runtime.calls, "retry-import")
	if runtime.failRetry {
		return state.SQLiteMigrationResult{}, errors.New("retry outcome unknown")
	}
	return runtime.result, nil
}

func (runtime *fakeMigrationRuntime) FinalizeMigration(context.Context, string, bool, state.SQLiteMigrationResult) error {
	runtime.calls = append(runtime.calls, "finalize-source")
	if runtime.failFinalize {
		return errors.New("finalize outcome unknown")
	}
	return nil
}

func (runtime *fakeMigrationRuntime) VerifyMigrationTarget(context.Context, string) error {
	runtime.calls = append(runtime.calls, "verify-target")
	if runtime.failTarget {
		return errors.New("target verification failed")
	}
	return nil
}

func (runtime *fakeMigrationRuntime) FenceLegacy(context.Context) error {
	runtime.calls = append(runtime.calls, "fence-recovery")
	return nil
}

func migrationTestManager(t *testing.T, migration *fakeMigrationRuntime) (*Manager, *store.Store) {
	t.Helper()
	database, err := store.Open(t.TempDir()+"/deployment.sqlite3", time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManagerWithMigration(database, &fakeRuntime{}, migration, time.Minute, time.Now)
	t.Cleanup(func() { _ = database.Close() })
	return manager, database
}

func TestMigrationIsOneDurableDeploydTransaction(t *testing.T) {
	migration := &fakeMigrationRuntime{result: state.SQLiteMigrationResult{SourceSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SourceSize: 1, StateEpoch: "epoch-1"}}
	manager, database := migrationTestManager(t, migration)
	transaction, err := manager.Migrate(context.Background())
	if err != nil {
		t.Fatalf("migration failed: %v", err)
	}
	if transaction == nil || transaction.Phase != PhaseFinalize || transaction.State != "finalized" {
		t.Fatalf("final migration transaction = %#v", transaction)
	}
	for _, expected := range []string{"preflight", "start-authority", "grant-legacy-controller", "enter-fence", "quiesce", "stop-legacy", "verify-stopped", "verify-writers", "prepare-source", "import", "finalize-source"} {
		if !containsString(migration.calls, expected) {
			t.Fatalf("migration call %q missing: %#v", expected, migration.calls)
		}
	}
	stored, err := database.Get(context.Background(), transaction.ID)
	if err != nil || stored == nil || stored.State != "finalized" {
		t.Fatalf("persisted migration transaction = %#v, err=%v", stored, err)
	}
}

func TestMigrationFailureLeavesNeedsRecoveryAndFencesLegacy(t *testing.T) {
	migration := &fakeMigrationRuntime{failImport: true}
	manager, database := migrationTestManager(t, migration)
	transaction, err := manager.Migrate(context.Background())
	if !errors.Is(err, ErrUncertain) || transaction != nil {
		t.Fatalf("migration failure = transaction %#v, err=%v", transaction, err)
	}
	var migrationErr *MigrationError
	if !errors.As(err, &migrationErr) || migrationErr.TransactionID == "" || migrationErr.Phase != PhaseMigrationImport {
		t.Fatalf("migration failure did not retain transaction identity: %v", err)
	}
	active, err := database.ListActive(context.Background())
	if err != nil || len(active) != 1 {
		t.Fatalf("active migration after failure = %#v, err=%v", active, err)
	}
	if active[0].State != "needs_recovery" || !active[0].RecoveryRequired {
		t.Fatalf("migration failure state = %#v", active[0])
	}
	if !containsString(migration.calls, "fence-recovery") {
		t.Fatalf("legacy was not fenced after import failure: %#v", migration.calls)
	}
}

func TestMigrationRecoveryRetriesOnlyAfterExplicitAction(t *testing.T) {
	migration := &fakeMigrationRuntime{failImport: true, result: state.SQLiteMigrationResult{SourceSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SourceSize: 1, StateEpoch: "epoch-1"}}
	manager, database := migrationTestManager(t, migration)
	if _, err := manager.Migrate(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatalf("initial migration error = %v", err)
	}
	active, err := database.ListActive(context.Background())
	if err != nil || len(active) != 1 {
		t.Fatalf("migration transaction = %#v, err=%v", active, err)
	}
	migration.failImport = false
	recovered, err := manager.Recover(context.Background(), active[0].ID, "retry-migration")
	if err != nil {
		t.Fatalf("migration recovery failed: %v", err)
	}
	if recovered == nil || recovered.State != "finalized" || recovered.Phase != PhaseFinalize {
		t.Fatalf("recovered migration transaction = %#v", recovered)
	}
	if !containsString(migration.calls, "retry-import") {
		t.Fatalf("explicit recovery did not retry import: %#v", migration.calls)
	}
}

func TestAutomaticMigrationRetryIsPersistentlyBounded(t *testing.T) {
	migration := &fakeMigrationRuntime{
		failImport: true,
		failRetry:  true,
		result: state.SQLiteMigrationResult{
			SourceSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			SourceSize:   1,
			StateEpoch:   "epoch-1",
		},
	}
	manager, database := migrationTestManager(t, migration)
	if _, err := manager.Migrate(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatalf("initial migration error = %v", err)
	}
	active, err := database.ListActive(context.Background())
	if err != nil || len(active) != 1 {
		t.Fatalf("migration transaction = %#v, err=%v", active, err)
	}
	if _, err := manager.Recover(context.Background(), active[0].ID, "retry-migration"); !errors.Is(err, ErrUncertain) {
		t.Fatalf("failed retry error = %v", err)
	}
	active, err = database.ListActive(context.Background())
	if err != nil || len(active) != 1 || active[0].MigrationRetryCount != 1 || active[0].MigrationRetryState != "exhausted" {
		t.Fatalf("retry consumption was not durable: %#v, err=%v", active, err)
	}
	migration.failRetry = false
	if _, err := manager.Recover(context.Background(), active[0].ID, "retry-migration"); err == nil || !strings.Contains(err.Error(), "already been consumed") {
		t.Fatalf("second automatic retry was accepted: %v", err)
	}
}

func TestMigrationFinalizeFailureLeavesNeedsRecovery(t *testing.T) {
	migration := &fakeMigrationRuntime{failFinalize: true, result: state.SQLiteMigrationResult{SourceSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", SourceSize: 1, StateEpoch: "epoch-1"}}
	manager, database := migrationTestManager(t, migration)
	if _, err := manager.Migrate(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatalf("migration finalize error = %v", err)
	}
	active, err := database.ListActive(context.Background())
	if err != nil || len(active) != 1 {
		t.Fatalf("migration transaction = %#v, err=%v", active, err)
	}
	if active[0].Phase != PhaseMigrationFinalize || active[0].State != "needs_recovery" || !active[0].RecoveryRequired {
		t.Fatalf("finalize failure state = %#v", active[0])
	}
}

func TestMigrationTargetVerificationFailureLeavesNeedsRecovery(t *testing.T) {
	migration := &fakeMigrationRuntime{failTarget: true, result: state.SQLiteMigrationResult{
		SourceSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourceSize:   1,
		StateEpoch:   "epoch-1",
		TargetSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	}}
	manager, database := migrationTestManager(t, migration)
	if _, err := manager.Migrate(context.Background()); !errors.Is(err, ErrUncertain) {
		t.Fatalf("target verification error = %v", err)
	}
	active, err := database.ListActive(context.Background())
	if err != nil || len(active) != 1 {
		t.Fatalf("migration transaction = %#v, err=%v", active, err)
	}
	if active[0].Phase != PhaseMigrationFinalize || active[0].State != "needs_recovery" || !active[0].RecoveryRequired {
		t.Fatalf("target verification failure state = %#v", active[0])
	}
}

func TestRetryMigrationRequiresTheOriginalSourceEvidence(t *testing.T) {
	expected := state.SQLiteMigrationEvidence{SourceSHA256: "a", SourceSize: 10, StateEpoch: "epoch-1"}
	actual := state.SQLiteMigrationEvidence{SourceSHA256: "b", SourceSize: 10, StateEpoch: "epoch-1"}
	if err := verifyMigrationSourceEvidence(expected, actual); err == nil {
		t.Fatal("replacement migration snapshot was accepted")
	}
	actual.SourceSHA256 = expected.SourceSHA256
	actual.StateEpoch = "epoch-2"
	if err := verifyMigrationSourceEvidence(expected, actual); err == nil {
		t.Fatal("migration snapshot with a changed state epoch was accepted")
	}
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
