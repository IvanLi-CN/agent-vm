package deployd

import (
	"context"
	"errors"
	"fmt"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

const (
	PhaseMigrationPrepare     = "migration_prepare"
	PhaseMigrationDrain       = "migration_drain"
	PhaseMigrationQuiescence  = "migration_quiescence"
	PhaseMigrationStop        = "migration_stop"
	PhaseMigrationSourceCheck = "migration_source_check"
	PhaseMigrationImport      = "migration_import"
	PhaseMigrationFinalize    = "migration_finalize"
)

// MigrationSource is produced only after the host runtime has fenced the
// legacy service and verified that no process still has the source database
// open. The backup path is evidence for recovery, not a live data source.
type MigrationSource struct {
	Evidence   state.SQLiteMigrationEvidence `json:"evidence"`
	BackupPath string                        `json:"backup_path"`
}

type MigrationQuiescence struct {
	StateEpoch         string `json:"state_epoch"`
	PendingOperations  int64  `json:"pending_operations"`
	InFlightOperations int64  `json:"in_flight_operations"`
	TransitionTasks    int64  `json:"transition_tasks"`
}

// MigrationRuntime is the host-side boundary for the one-time legacy state
// migration. It is deliberately separate from Runtime: normal blue-green
// transactions cannot call any of these methods.
type MigrationRuntime interface {
	MigrationComplete(context.Context) (bool, error)
	PreflightMigration(context.Context) error
	StartStateAuthority(context.Context) error
	EnterLegacyFence(context.Context, string) error
	WaitLegacyQuiescence(context.Context) (MigrationQuiescence, error)
	StopLegacy(context.Context) error
	VerifyLegacyStopped(context.Context) error
	VerifySQLiteWritersClosed(context.Context) error
	PrepareMigrationSource(context.Context, string) (MigrationSource, error)
	ImportMigration(context.Context, MigrationSource) (state.SQLiteMigrationResult, error)
	RetryMigration(context.Context, string, state.SQLiteMigrationEvidence, bool) (state.SQLiteMigrationResult, error)
	FinalizeMigration(context.Context, string, bool, state.SQLiteMigrationResult) error
	FenceLegacy(context.Context) error
}

type migrationTargetVerifier interface {
	VerifyMigrationTarget(context.Context, string) error
}

type MigrationError struct {
	TransactionID string
	Phase         string
	Cause         error
}

func (err *MigrationError) Error() string {
	if err == nil || err.Cause == nil {
		return "initial state migration failed"
	}
	return fmt.Sprintf("initial state migration %s failed in %s: %v", err.TransactionID, err.Phase, err.Cause)
}

func (err *MigrationError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

type migrationBackupCleaner interface {
	CleanupMigrationBackups(context.Context) error
}

func (manager *Manager) Migrate(ctx context.Context) (*store.Transaction, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.Migration == nil {
		return nil, errors.New("initial state migration is not configured")
	}
	if err := manager.ensureNoActiveTransaction(ctx); err != nil {
		return nil, err
	}
	complete, err := manager.Migration.MigrationComplete(ctx)
	if err != nil {
		return nil, err
	}
	if complete {
		transaction, err := manager.Store.LatestFinalizedMigration(ctx)
		if err != nil {
			return nil, err
		}
		if transaction == nil {
			return nil, errors.New("migration completion marker has no durable transaction")
		}
		if verifier, ok := manager.Migration.(migrationTargetVerifier); ok {
			if err := verifier.VerifyMigrationTarget(ctx, transaction.ID); err != nil {
				return nil, fmt.Errorf("verify completed rqlite migration: %w", err)
			}
		}
		return transaction, nil
	}
	if err := manager.Migration.PreflightMigration(ctx); err != nil {
		return nil, err
	}
	now := manager.Clock().UTC()
	transaction := store.Transaction{
		ID:           newID("migration"),
		TargetSlot:   "state",
		PreviousSlot: "legacy",
		Bundle:       "sqlite-to-rqlite",
		Phase:        PhaseMigrationPrepare,
		State:        "active",
		Generation:   now.UnixNano(),
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if err := manager.Store.Create(ctx, transaction, map[string]any{
		"kind":        "initial_state_migration",
		"source":      "legacy_sqlite",
		"destination": "rqlite",
	}); err != nil {
		return nil, err
	}

	if err := manager.Migration.StartStateAuthority(ctx); err != nil {
		return nil, manager.migrationFailure(ctx, transaction.ID, PhaseMigrationPrepare, err, false)
	}
	if err := manager.transitionMigration(ctx, transaction.ID, PhaseMigrationPrepare, PhaseMigrationDrain, "state_authority_ready", nil); err != nil {
		return nil, err
	}
	if err := manager.Migration.EnterLegacyFence(ctx, transaction.ID); err != nil {
		return nil, manager.migrationFailure(ctx, transaction.ID, PhaseMigrationDrain, err, true)
	}
	if err := manager.transitionMigration(ctx, transaction.ID, PhaseMigrationDrain, PhaseMigrationQuiescence, "legacy_write_fenced", nil); err != nil {
		return nil, err
	}
	quiescence, err := manager.Migration.WaitLegacyQuiescence(ctx)
	if err != nil {
		return nil, manager.migrationFailure(ctx, transaction.ID, PhaseMigrationQuiescence, err, true)
	}
	if err := manager.transitionMigration(ctx, transaction.ID, PhaseMigrationQuiescence, PhaseMigrationStop, "legacy_quiescent", map[string]any{
		"state_epoch":          quiescence.StateEpoch,
		"pending_operations":   quiescence.PendingOperations,
		"in_flight_operations": quiescence.InFlightOperations,
		"transition_tasks":     quiescence.TransitionTasks,
	}); err != nil {
		return nil, err
	}
	if err := manager.Migration.StopLegacy(ctx); err != nil {
		return nil, manager.migrationFailure(ctx, transaction.ID, PhaseMigrationStop, err, true)
	}
	if err := manager.Migration.VerifyLegacyStopped(ctx); err != nil {
		return nil, manager.migrationFailure(ctx, transaction.ID, PhaseMigrationStop, err, true)
	}
	if err := manager.Migration.VerifySQLiteWritersClosed(ctx); err != nil {
		return nil, manager.migrationFailure(ctx, transaction.ID, PhaseMigrationStop, err, true)
	}
	if err := manager.transitionMigration(ctx, transaction.ID, PhaseMigrationStop, PhaseMigrationSourceCheck, "legacy_stopped", nil); err != nil {
		return nil, err
	}
	source, err := manager.Migration.PrepareMigrationSource(ctx, transaction.ID)
	if err != nil {
		return nil, manager.migrationFailure(ctx, transaction.ID, PhaseMigrationSourceCheck, err, true)
	}
	if err := manager.transitionMigration(ctx, transaction.ID, PhaseMigrationSourceCheck, PhaseMigrationImport, "source_verified", map[string]any{
		"fence_id":      source.Evidence.FenceID,
		"source_sha256": source.Evidence.SourceSHA256,
		"source_size":   source.Evidence.SourceSize,
		"state_epoch":   source.Evidence.StateEpoch,
		"backup_path":   source.BackupPath,
	}); err != nil {
		return nil, err
	}
	result, err := manager.Migration.ImportMigration(ctx, source)
	if err != nil {
		return nil, manager.migrationFailure(ctx, transaction.ID, PhaseMigrationImport, err, true)
	}
	if err := manager.transitionMigration(ctx, transaction.ID, PhaseMigrationImport, PhaseMigrationFinalize, "rqlite_import_verified", map[string]any{
		"source_sha256": result.SourceSHA256,
		"source_size":   result.SourceSize,
		"state_epoch":   result.StateEpoch,
		"target_sha256": result.TargetSHA256,
	}); err != nil {
		return nil, err
	}
	if err := manager.Migration.FinalizeMigration(ctx, transaction.ID, true, result); err != nil {
		return nil, manager.migrationFailure(ctx, transaction.ID, PhaseMigrationFinalize, err, true)
	}
	if verifier, ok := manager.Migration.(migrationTargetVerifier); ok {
		if err := verifier.VerifyMigrationTarget(ctx, transaction.ID); err != nil {
			return nil, manager.migrationFailure(ctx, transaction.ID, PhaseMigrationFinalize, err, true)
		}
	}
	if err := manager.transitionMigration(ctx, transaction.ID, PhaseMigrationFinalize, PhaseFinalize, "migration_completed", map[string]any{
		"business_authority": "rqlite",
		"legacy_sqlite":      "fenced_and_retained_for_recovery",
	}); err != nil {
		return nil, err
	}
	return manager.Store.Get(ctx, transaction.ID)
}

func (manager *Manager) transitionMigration(ctx context.Context, id, expected, phase, transition string, evidence map[string]any) error {
	if _, err := manager.Store.Transition(ctx, id, expected, phase, stateForPhase(phase), transition, "deployd", evidence, manager.Clock().UTC()); err != nil {
		return manager.migrationFailure(ctx, id, expected, &UncertainError{Cause: fmt.Errorf("persist migration phase %s: %w", phase, err)}, true)
	}
	return nil
}

func (manager *Manager) migrationFailure(ctx context.Context, id, phase string, cause error, uncertain bool) error {
	if uncertain {
		cause = &UncertainError{Cause: cause}
	}
	if err := manager.Migration.FenceLegacy(ctx); err != nil && uncertain {
		cause = fmt.Errorf("%w; legacy fence failed: %v", cause, err)
	}
	if _, err := manager.Store.Get(ctx, id); err == nil {
		if failureErr := manager.failure(ctx, id, phase, cause); failureErr != nil {
			return &MigrationError{TransactionID: id, Phase: phase, Cause: failureErr}
		}
	}
	return &MigrationError{TransactionID: id, Phase: phase, Cause: cause}
}

func (manager *Manager) recoverMigrationOnStart(ctx context.Context, transaction store.Transaction) error {
	if manager.Migration == nil {
		return errors.New("migration runtime is unavailable after deployd restart")
	}
	cause := &UncertainError{Cause: errors.New("deployd restarted during initial state migration")}
	if err := manager.Migration.FenceLegacy(ctx); err != nil {
		cause.Cause = fmt.Errorf("deployd restarted during initial state migration; legacy fence failed: %w", err)
	}
	return manager.failure(ctx, transaction.ID, transaction.Phase, cause)
}

func (manager *Manager) CleanupMigrationBackups(ctx context.Context) error {
	active, err := manager.Store.ListActive(ctx)
	if err != nil {
		return err
	}
	for _, transaction := range active {
		if transaction.TargetSlot == "state" {
			return nil
		}
	}
	cleaner, ok := manager.Migration.(migrationBackupCleaner)
	if !ok {
		return nil
	}
	return cleaner.CleanupMigrationBackups(ctx)
}
