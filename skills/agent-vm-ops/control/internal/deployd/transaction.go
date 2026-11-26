package deployd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

const (
	PhaseStage             = "stage"
	PhaseStandbyValidation = "standby_validation"
	PhaseControllerHandoff = "controller_handoff"
	PhaseTrafficEnable     = "traffic_enable"
	PhaseActiveSwitch      = "active_switch"
	PhaseDrainOld          = "drain_old"
	PhaseQuiescence        = "quiescence"
	PhaseRollback          = "rollback"
	PhaseFinalize          = "finalize"
)

var ErrUncertain = errors.New("deployment outcome is uncertain")

type TransactionError struct {
	ID    string
	Phase string
	Cause error
}

func (err *TransactionError) Error() string {
	if err == nil || err.Cause == nil {
		return "deployment transaction failed"
	}
	return err.Cause.Error()
}

func (err *TransactionError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

type UncertainError struct{ Cause error }

func (err *UncertainError) Error() string {
	if err == nil || err.Cause == nil {
		return ErrUncertain.Error()
	}
	return ErrUncertain.Error() + ": " + err.Cause.Error()
}

func (err *UncertainError) Unwrap() error { return err.Cause }

func (err *UncertainError) Is(target error) bool { return target == ErrUncertain }

type DeploymentCompatibilityError struct{ Cause error }

func (err *DeploymentCompatibilityError) Error() string {
	if err == nil || err.Cause == nil {
		return "deployment contract is incompatible"
	}
	return err.Cause.Error()
}

func (err *DeploymentCompatibilityError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

type SlotObservation struct {
	Slot                  string                `json:"slot"`
	ProcessUp             bool                  `json:"process_up"`
	ProcessReady          bool                  `json:"process_ready"`
	Ready                 bool                  `json:"ready"`
	LeaseRenewalSupported bool                  `json:"controller_lease_renewal"`
	ControllerLease       *controllerrole.Lease `json:"controller_lease,omitempty"`
	ControllerOwned       bool                  `json:"controller_owned"`
	TrafficAccepting      bool                  `json:"traffic_accepting"`
	Draining              bool                  `json:"draining"`
	CountersKnown         bool                  `json:"counters_known"`
	InFlightRequests      int64                 `json:"in_flight_requests"`
	NativeWorkers         int64                 `json:"native_workers"`
	PendingOperations     int64                 `json:"pending_operations"`
	ObservedUnix          int64                 `json:"observed_at_seconds"`
	StateAuthority        string                `json:"state_authority,omitempty"`
	StateEpoch            string                `json:"state_epoch,omitempty"`
	ReleaseID             string                `json:"release_id,omitempty"`
	SourceRevision        string                `json:"source_revision,omitempty"`
	StateCapabilities     []string              `json:"state_capabilities,omitempty"`
	MetricFamilies        map[string]string     `json:"metric_families,omitempty"`
}

type RuntimeSnapshot struct {
	ObservedAt             time.Time                    `json:"observed_at"`
	FreshUntil             time.Time                    `json:"fresh_until"`
	ActiveSlot             string                       `json:"active_slot"`
	ControllerSlot         string                       `json:"controller_slot"`
	Consistent             bool                         `json:"consistent"`
	ControllerRecoveryOnly bool                         `json:"controller_recovery_only"`
	Slots                  map[string]SlotObservation   `json:"slots"`
	Sources                map[string]SourceObservation `json:"sources,omitempty"`
}

type SourceObservation struct {
	Up         bool      `json:"up"`
	ObservedAt time.Time `json:"observed_at"`
	FreshUntil time.Time `json:"fresh_until"`
	Error      string    `json:"error,omitempty"`
}

type Runtime interface {
	Observe(context.Context) (RuntimeSnapshot, error)
	Stage(context.Context, string, string) error
	Verify(context.Context, string, string) error
	VerifySlotIdentity(context.Context, string, string, string, []string) error
	GrantController(context.Context, string, controllerrole.Lease) error
	RevokeController(context.Context, string, uint64) error
	FenceController(context.Context, string) error
	EnableTraffic(context.Context, string) error
	DisableTraffic(context.Context, string) error
	SwitchActive(context.Context, string) error
	Drain(context.Context, string) error
	ExitDrain(context.Context, string) error
	Quiesce(context.Context, string) error
	Stop(context.Context, string) error
	Rollback(context.Context, string, string, controllerrole.Lease, string) error
}

type RuntimePreflight interface {
	Preflight(context.Context, string, string) error
}

type Status struct {
	Observed     RuntimeSnapshot     `json:"observation"`
	Transactions []store.Transaction `json:"transactions"`
}

type Manager struct {
	Store         *store.Store
	Runtime       Runtime
	Migration     MigrationRuntime
	Clock         func() time.Time
	LeaseDuration time.Duration

	mu            sync.Mutex
	startupFenced bool
}

func NewManager(database *store.Store, runtime Runtime, leaseDuration time.Duration, clock func() time.Time) *Manager {
	return NewManagerWithMigration(database, runtime, nil, leaseDuration, clock)
}

func NewManagerWithMigration(database *store.Store, runtime Runtime, migration MigrationRuntime, leaseDuration time.Duration, clock func() time.Time) *Manager {
	if clock == nil {
		clock = time.Now
	}
	if leaseDuration <= 0 {
		leaseDuration = 15 * time.Second
	}
	return &Manager{Store: database, Runtime: runtime, Migration: migration, Clock: clock, LeaseDuration: leaseDuration}
}

func (manager *Manager) Status(ctx context.Context) (Status, error) {
	// Read-only probes must not hold the serialized role writer while waiting on business state.
	observation, err := manager.Runtime.Observe(ctx)
	if err != nil {
		return Status{}, err
	}
	transactions, err := manager.Store.ListActive(ctx)
	if err != nil {
		return Status{}, err
	}
	return Status{Observed: observation, Transactions: transactions}, nil
}

func (manager *Manager) Transaction(ctx context.Context, id string) (map[string]any, error) {
	transaction, err := manager.Store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if transaction == nil {
		return nil, fmt.Errorf("transaction %s was not found", id)
	}
	observation, err := manager.Store.Observation(ctx, id)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"transaction": transaction, "observation": observation}
	// A non-final state always has a recorded cause; without it the operator
	// sees only the phase and has to read the journal for the actual failure.
	if transaction.State != "finalized" {
		detail, err := manager.failureEvidence(ctx, id)
		if err != nil {
			return nil, err
		}
		if detail != nil {
			result["failure"] = detail
		}
	}
	return result, nil
}

func (manager *Manager) Plan(ctx context.Context, targetSlot, bundle string) (map[string]any, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if err := validSlot(targetSlot); err != nil {
		return nil, err
	}
	observation, err := manager.observe(ctx, "")
	if err != nil {
		return nil, err
	}
	if err := manager.ensureNoActiveTransaction(ctx); err != nil {
		return nil, err
	}
	if err := validateStageObservation(observation, targetSlot, manager.Clock().UTC()); err != nil {
		return nil, err
	}
	return map[string]any{
		"target_slot":   targetSlot,
		"previous_slot": observation.ActiveSlot,
		"bundle":        bundle,
		"phases":        []string{PhaseStage, PhaseStandbyValidation, PhaseControllerHandoff, PhaseTrafficEnable, PhaseActiveSwitch, PhaseDrainOld, PhaseQuiescence, PhaseFinalize},
		"observation":   observation,
	}, nil
}

func (manager *Manager) Stage(ctx context.Context, targetSlot, bundle string) (*store.Transaction, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if err := validSlot(targetSlot); err != nil {
		return nil, err
	}
	if bundle == "" {
		return nil, errors.New("bundle is required")
	}
	observation, err := manager.observe(ctx, "")
	if err != nil {
		return nil, err
	}
	if err := manager.ensureNoActiveTransaction(ctx); err != nil {
		return nil, err
	}
	if err := validateStageObservation(observation, targetSlot, manager.Clock().UTC()); err != nil {
		return nil, err
	}
	if preflight, ok := manager.Runtime.(RuntimePreflight); ok {
		if err := preflight.Preflight(ctx, targetSlot, bundle); err != nil {
			return nil, err
		}
	}
	createdEvidence := map[string]any{"active_slot": observation.ActiveSlot}
	backupBeforeStage := manager.usesRecoveryPoints() && observation.ActiveSlot != "none"
	if backupBeforeStage {
		createdEvidence["backup_before_stage"] = true
	}
	if observation.ActiveSlot != "none" {
		previous, ok := observation.Slots[observation.ActiveSlot]
		if !ok || previous.ReleaseID == "" || previous.SourceRevision == "" || previous.StateEpoch == "" || previous.StateAuthority == "" {
			return nil, errors.New("active slot identity is incomplete for rollback")
		}
		createdEvidence["previous_release_id"] = previous.ReleaseID
		createdEvidence["previous_source_revision"] = previous.SourceRevision
		createdEvidence["previous_state_epoch"] = previous.StateEpoch
		createdEvidence["previous_state_authority"] = previous.StateAuthority
		createdEvidence["previous_state_capabilities"] = append([]string(nil), previous.StateCapabilities...)
	}
	now := manager.Clock().UTC()
	transaction := store.Transaction{ID: newID("tx"), TargetSlot: targetSlot, PreviousSlot: observation.ActiveSlot, Bundle: bundle, Phase: PhaseStage, State: "active", Generation: now.UnixNano(), CreatedAt: now, UpdatedAt: now}
	if err := manager.Store.Create(ctx, transaction, createdEvidence); err != nil {
		return nil, err
	}
	if err := manager.prepareRecoveryPoint(ctx, transaction, "stage", observation.Slots[observation.ActiveSlot]); err != nil {
		// This failure precedes anything that can change either slot's roles.
		return nil, manager.failure(context.WithoutCancel(ctx), transaction.ID, PhaseStage, err)
	}
	if backupBeforeStage {
		if _, err := manager.Store.Transition(ctx, transaction.ID, PhaseStage, PhaseStage, "active", "stage_effect_started", "deployd", nil, manager.Clock().UTC()); err != nil {
			return nil, err
		}
	}
	if err := manager.withControllerRenewal(ctx, observation.Slots[observation.ActiveSlot], func(stageContext context.Context) error {
		return manager.Runtime.Stage(stageContext, targetSlot, bundle)
	}); err != nil {
		if errors.Is(err, ErrUncertain) {
			return nil, manager.effectFailure(ctx, transaction.ID, PhaseStage, err)
		}
		return nil, manager.failure(ctx, transaction.ID, PhaseStage, err)
	}
	if err := manager.transitionAfterEffect(ctx, transaction.ID, PhaseStage, PhaseStandbyValidation, "staged", map[string]any{"target_slot": targetSlot}); err != nil {
		return nil, err
	}
	updated, err := manager.Store.Get(ctx, transaction.ID)
	return updated, nil
}

func (manager *Manager) Verify(ctx context.Context, id string) (*store.Transaction, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	transaction, err := manager.Store.Get(ctx, id)
	if err != nil || transaction == nil {
		return transaction, errOrNotFound(err, id, transaction)
	}
	if transaction.Phase != PhaseStandbyValidation {
		return nil, fmt.Errorf("transaction %s is in phase %s", id, transaction.Phase)
	}
	before, err := manager.observe(ctx, "")
	if err != nil {
		return nil, err
	}
	if err := manager.withControllerRenewal(ctx, before.Slots[transaction.PreviousSlot], func(verifyContext context.Context) error {
		return manager.Runtime.Verify(verifyContext, transaction.TargetSlot, transaction.Bundle)
	}); err != nil {
		return nil, manager.failure(ctx, id, PhaseStandbyValidation, err)
	}
	observation, err := manager.observe(ctx, id)
	if err != nil {
		return nil, manager.failure(ctx, id, PhaseStandbyValidation, err)
	}
	target, ok := observation.Slots[transaction.TargetSlot]
	if !validObservation(observation, manager.Clock().UTC()) || !ok || !target.ProcessUp || !target.ProcessReady || target.ControllerOwned || target.TrafficAccepting || target.ReleaseID == "" || target.SourceRevision == "" || target.StateEpoch == "" {
		return nil, manager.failure(ctx, id, PhaseStandbyValidation, errors.New("candidate slot is not process-ready"))
	}
	previous, previousOK := observation.Slots[transaction.PreviousSlot]
	_, _, stagedAuthority, _, stagedErr := manager.verifiedPreviousIdentity(ctx, id)
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" && (stagedErr != nil || !previousOK || previous.StateAuthority != stagedAuthority || target.StateAuthority != stagedAuthority) {
		return nil, manager.failure(ctx, id, PhaseStandbyValidation, errors.New("candidate state contract is incompatible with the active slot"))
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseStandbyValidation, PhaseControllerHandoff, "verified", map[string]any{"candidate_ready": true, "release_id": target.ReleaseID, "source_revision": target.SourceRevision, "state_epoch": target.StateEpoch, "state_authority": target.StateAuthority, "state_capabilities": append([]string(nil), target.StateCapabilities...)}); err != nil {
		return nil, err
	}
	updated, err := manager.Store.Get(ctx, id)
	return updated, err
}

func (manager *Manager) Activate(ctx context.Context, id string) (*store.Transaction, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	transaction, err := manager.Store.Get(ctx, id)
	if err != nil || transaction == nil {
		return transaction, errOrNotFound(err, id, transaction)
	}
	if transaction.Phase != PhaseControllerHandoff {
		return nil, fmt.Errorf("transaction %s is in phase %s", id, transaction.Phase)
	}
	verifiedReleaseID, verifiedSourceRevision, verifiedAuthority, _, identityErr := manager.verifiedCandidateIdentity(ctx, id)
	if identityErr != nil {
		return nil, manager.failure(ctx, id, transaction.Phase, identityErr)
	}
	if err := manager.Runtime.Verify(ctx, transaction.TargetSlot, transaction.Bundle); err != nil {
		return nil, manager.failure(ctx, id, transaction.Phase, err)
	}
	observation, err := manager.observe(ctx, id)
	if err != nil {
		return nil, manager.failure(ctx, id, transaction.Phase, err)
	}
	if !validObservation(observation, manager.Clock().UTC()) || observation.ActiveSlot != transaction.PreviousSlot {
		return nil, manager.failure(ctx, id, transaction.Phase, errors.New("active slot changed outside deployd"))
	}
	target, targetOK := observation.Slots[transaction.TargetSlot]
	if !targetOK || !target.ProcessUp || !target.ProcessReady || target.ControllerOwned || target.TrafficAccepting || target.ReleaseID != verifiedReleaseID || target.SourceRevision != verifiedSourceRevision || target.StateEpoch == "" || target.StateAuthority != verifiedAuthority {
		return nil, manager.failure(ctx, id, transaction.Phase, errors.New("candidate identity or readiness changed after verification"))
	}
	var ok bool
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		term, termErr := manager.nextFenceTerm(ctx)
		if termErr != nil {
			return nil, manager.failure(ctx, id, PhaseControllerHandoff, fmt.Errorf("allocate controller fencing term: %w", termErr))
		}
		if err := manager.Runtime.RevokeController(ctx, transaction.PreviousSlot, term); err != nil {
			return nil, manager.effectFailure(ctx, id, PhaseControllerHandoff, err)
		}
	}
	lease, err := manager.newLease(ctx, transaction.TargetSlot, transaction.Generation)
	if err != nil {
		return nil, manager.failure(ctx, id, PhaseControllerHandoff, fmt.Errorf("allocate candidate controller lease: %w", err))
	}
	if err := manager.Runtime.GrantController(ctx, transaction.TargetSlot, lease); err != nil {
		return nil, manager.effectFailure(ctx, id, PhaseControllerHandoff, err)
	}
	parentContext := ctx
	stopRenewal := func() error { return nil }
	if _, supported := manager.Runtime.(interface {
		RenewTransactionLease(context.Context, controllerrole.Lease) (controllerrole.Lease, error)
	}); supported {
		ctx, stopRenewal = manager.beginTransactionRenewal(ctx, lease)
	}
	defer stopRenewal()
	failEffect := func(phase string, cause error) error {
		_ = stopRenewal()
		return manager.effectFailure(parentContext, id, phase, cause)
	}
	failRollback := func(cause error) error {
		_ = stopRenewal()
		return manager.rollbackOrFence(parentContext, *transaction, cause)
	}
	observation, err = manager.observe(ctx, id)
	if err != nil {
		return nil, failRollback(err)
	}
	target, ok = observation.Slots[transaction.TargetSlot]
	if !ok || !target.ProcessUp || !target.ProcessReady || !target.ControllerOwned || target.ReleaseID != verifiedReleaseID || target.SourceRevision != verifiedSourceRevision || target.StateEpoch == "" || target.StateAuthority != verifiedAuthority {
		return nil, failRollback(errors.New("candidate controller lease was not observed"))
	}
	if err := manager.waitForReady(ctx, id, transaction.TargetSlot); err != nil {
		return nil, failRollback(err)
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseControllerHandoff, PhaseTrafficEnable, "controller_granted", map[string]any{"lease_id": lease.LeaseID, "term": lease.Term}); err != nil {
		return nil, err
	}
	if err := manager.Runtime.EnableTraffic(ctx, transaction.TargetSlot); err != nil {
		return nil, failEffect(PhaseTrafficEnable, err)
	}
	observation, err = manager.observe(ctx, id)
	if err != nil {
		return nil, failEffect(PhaseTrafficEnable, err)
	}
	target, ok = observation.Slots[transaction.TargetSlot]
	if !validObservation(observation, manager.Clock().UTC()) || !ok || !target.ControllerOwned || !target.TrafficAccepting || target.ReleaseID != verifiedReleaseID || target.SourceRevision != verifiedSourceRevision || target.StateEpoch == "" || target.StateAuthority != verifiedAuthority {
		return nil, failEffect(PhaseTrafficEnable, errors.New("candidate traffic role was not observed"))
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseTrafficEnable, PhaseActiveSwitch, "traffic_enabled", nil); err != nil {
		return nil, err
	}
	if err := manager.Runtime.SwitchActive(ctx, transaction.TargetSlot); err != nil {
		return nil, failRollback(err)
	}
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		if err := manager.Runtime.DisableTraffic(ctx, transaction.PreviousSlot); err != nil {
			return nil, failEffect(PhaseActiveSwitch, err)
		}
	}
	observation, err = manager.observe(ctx, id)
	if err != nil {
		return nil, failRollback(err)
	}
	target, ok = observation.Slots[transaction.TargetSlot]
	if !validObservation(observation, manager.Clock().UTC()) || !ok || observation.ActiveSlot != transaction.TargetSlot || !target.ProcessUp || !target.ProcessReady || !target.Ready || !target.ControllerOwned || !target.TrafficAccepting || target.ReleaseID != verifiedReleaseID || target.SourceRevision != verifiedSourceRevision || target.StateEpoch == "" || target.StateAuthority != verifiedAuthority {
		return nil, failRollback(errors.New("active switch was not verified"))
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseActiveSwitch, PhaseDrainOld, "active_switched", map[string]any{"active_slot": transaction.TargetSlot}); err != nil {
		return nil, err
	}
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		if err := manager.Runtime.Drain(ctx, transaction.PreviousSlot); err != nil {
			return nil, failEffect(PhaseDrainOld, err)
		}
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseDrainOld, PhaseQuiescence, "drain_started", nil); err != nil {
		return nil, err
	}
	if err := manager.Runtime.Quiesce(ctx, transaction.PreviousSlot); err != nil {
		return nil, failEffect(PhaseQuiescence, err)
	}
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		observation, err = manager.observe(ctx, id)
		previous, previousOK := observation.Slots[transaction.PreviousSlot]
		if err != nil || !quiescenceObservationValid(observation, manager.Clock().UTC()) || !previousOK || !previous.CountersKnown || previous.TrafficAccepting || previous.InFlightRequests != 0 || previous.NativeWorkers != 0 || previous.PendingOperations != 0 {
			cause := quiescenceVerificationError(observation, *transaction, previousOK, manager.Clock().UTC())
			if err != nil {
				cause = fmt.Errorf("observe quiescence counters: %w", err)
			}
			return nil, failEffect(PhaseQuiescence, cause)
		}
	}
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		if err := manager.Runtime.Stop(ctx, transaction.PreviousSlot); err != nil {
			return nil, failEffect(PhaseQuiescence, err)
		}
	}
	if err := manager.Runtime.ExitDrain(ctx, transaction.TargetSlot); err != nil {
		return nil, failEffect(PhaseQuiescence, fmt.Errorf("exit shared admission drain on active slot: %w", err))
	}
	observation, err = manager.observe(ctx, id)
	if err != nil {
		return nil, failRollback(err)
	}
	target, ok = observation.Slots[transaction.TargetSlot]
	old, oldOK := observation.Slots[transaction.PreviousSlot]
	previousSlotRequired := transaction.PreviousSlot != "" && transaction.PreviousSlot != "none"
	oldNotStopped := previousSlotRequired && (!oldOK || !old.CountersKnown || old.ProcessUp || old.ProcessReady || old.Ready || old.ControllerOwned || old.TrafficAccepting || old.InFlightRequests != 0 || old.NativeWorkers != 0 || old.PendingOperations != 0)
	if !validObservation(observation, manager.Clock().UTC()) || !ok || observation.ActiveSlot != transaction.TargetSlot || !target.ProcessUp || !target.ProcessReady || !target.Ready || !target.ControllerOwned || !target.TrafficAccepting || target.ReleaseID != verifiedReleaseID || target.SourceRevision != verifiedSourceRevision || target.StateEpoch == "" || target.StateAuthority != verifiedAuthority || oldNotStopped {
		return nil, failRollback(errors.New("final active slot was not verified"))
	}
	if err := stopRenewal(); err != nil {
		return nil, failEffect(PhaseQuiescence, err)
	}
	ctx = parentContext
	if err := manager.transitionAfterEffect(ctx, id, PhaseQuiescence, PhaseFinalize, "finalized", map[string]any{"quiescent": true}); err != nil {
		return nil, err
	}
	manager.startupFenced = false
	updated, err := manager.Store.Get(ctx, id)
	return updated, err
}

// Quiescence only needs fresh source evidence for the previous slot. During
// active handoff the global controller/active snapshot may be transiently
// inconsistent; finalization still requires validObservation.
func quiescenceObservationValid(observation RuntimeSnapshot, now time.Time) bool {
	if observation.ObservedAt.IsZero() || observation.FreshUntil.IsZero() || !now.Before(observation.FreshUntil) {
		return false
	}
	for _, source := range observation.Sources {
		if !source.Up || source.ObservedAt.IsZero() || !now.Before(source.FreshUntil) {
			return false
		}
	}
	return true
}

func quiescenceVerificationError(observation RuntimeSnapshot, transaction store.Transaction, previousOK bool, now time.Time) error {
	previous := observation.Slots[transaction.PreviousSlot]
	return fmt.Errorf("quiescence counters were not verified before stopping the previous slot: active=%s consistent=%t valid=%t previous_present=%t previous={traffic_accepting:%t counters_known:%t workers:%d pending:%d in_flight:%d}",
		observation.ActiveSlot,
		observation.Consistent,
		validObservation(observation, now),
		previousOK,
		previous.TrafficAccepting,
		previous.CountersKnown,
		previous.NativeWorkers,
		previous.PendingOperations,
		previous.InFlightRequests,
	)
}

func (manager *Manager) Rollback(ctx context.Context, id string) (*store.Transaction, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	transaction, err := manager.Store.Get(ctx, id)
	if err != nil || transaction == nil {
		return transaction, errOrNotFound(err, id, transaction)
	}
	// Rollback is a destructive deployment operation. Observe the runtime
	// without a transaction identity first, so an incomplete inventory cannot
	// reach slot verification or a native rollback effect.
	observation, err := manager.completeRuntimeObservation(ctx)
	if err != nil {
		return nil, err
	}
	if transaction.State == "finalized" && manager.usesRecoveryPoints() {
		if err := manager.ensureNoActiveTransaction(ctx); err != nil {
			return nil, err
		}
		if !validObservation(observation, manager.Clock().UTC()) || observation.ActiveSlot != transaction.TargetSlot {
			return nil, errors.New("rollback requires the currently active deployment")
		}
		latest, err := manager.Store.LatestRollbackTarget(ctx)
		if err != nil {
			return nil, err
		}
		if latest != id {
			return nil, errors.New("rollback requires the latest deployment transaction")
		}
		if err := manager.prepareRecoveryPoint(ctx, *transaction, "rollback", observation.Slots[observation.ActiveSlot]); err != nil {
			return nil, err
		}
	}
	if err := manager.rollback(ctx, *transaction); err != nil {
		return nil, err
	}
	return manager.Store.Get(ctx, id)
}

func (manager *Manager) completeRuntimeObservation(ctx context.Context) (RuntimeSnapshot, error) {
	observation, err := manager.observe(ctx, "")
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	if observation.ControllerRecoveryOnly {
		return RuntimeSnapshot{}, errors.New("rollback requires a complete runtime observation")
	}
	if !observation.Consistent {
		return RuntimeSnapshot{}, manager.fenceSlots(ctx, errors.New("rollback runtime observation is inconsistent"))
	}
	return observation, nil
}

func (manager *Manager) Recover(ctx context.Context, id, action string) (*store.Transaction, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	transaction, err := manager.Store.Get(ctx, id)
	if err != nil || transaction == nil {
		return transaction, errOrNotFound(err, id, transaction)
	}
	if action == "retry-stage" {
		return manager.retryStage(ctx, *transaction)
	}
	if !transaction.RecoveryRequired && transaction.State != "needs_recovery" {
		return nil, errors.New("transaction does not require recovery")
	}
	if transaction.TargetSlot == "state" {
		if action != "retry-migration" {
			return nil, errors.New("state migration recovery action must be retry-migration")
		}
		if manager.Migration == nil {
			return nil, errors.New("migration runtime is unavailable")
		}
		if err := manager.Store.ConsumeMigrationRetry(ctx, transaction.ID, manager.Clock().UTC()); err != nil {
			return nil, err
		}
		if err := manager.Migration.FenceLegacy(ctx); err != nil {
			return nil, err
		}
		if err := manager.Migration.StartStateAuthority(ctx); err != nil {
			return nil, err
		}
		expectedEvidence, evidencePresent, err := manager.migrationSourceEvidence(ctx, transaction.ID)
		if err != nil {
			return nil, manager.migrationFailure(ctx, transaction.ID, transaction.Phase, err, true)
		}
		allowResnapshot := (transaction.Phase == PhaseMigrationStop || transaction.Phase == PhaseMigrationSourceCheck) && !evidencePresent
		result, err := manager.Migration.RetryMigration(ctx, transaction.ID, expectedEvidence, allowResnapshot)
		if err != nil {
			if markErr := manager.Store.FailMigrationRetry(ctx, transaction.ID, manager.Clock().UTC()); markErr != nil {
				return nil, &UncertainError{Cause: fmt.Errorf("migration retry failed and retry state could not be finalized: %v; %w", err, markErr)}
			}
			return nil, &UncertainError{Cause: err}
		}
		updated, err := manager.Store.Transition(ctx, transaction.ID, transaction.Phase, PhaseMigrationFinalize, stateForPhase(PhaseMigrationFinalize), "recovery_import_verified", "deployd", map[string]any{
			"source_sha256": result.SourceSHA256,
			"source_size":   result.SourceSize,
			"state_epoch":   result.StateEpoch,
			"target_sha256": result.TargetSHA256,
		}, manager.Clock().UTC())
		if err != nil {
			return nil, manager.migrationFailure(ctx, transaction.ID, PhaseMigrationFinalize, err, true)
		}
		if err := manager.Migration.FinalizeMigration(ctx, transaction.ID, true, result); err != nil {
			return nil, manager.migrationFailure(ctx, transaction.ID, PhaseMigrationFinalize, err, true)
		}
		if verifier, ok := manager.Migration.(migrationTargetVerifier); ok {
			if err := verifier.VerifyMigrationTarget(ctx, transaction.ID); err != nil {
				return nil, manager.migrationFailure(ctx, transaction.ID, PhaseMigrationFinalize, err, true)
			}
		}
		updated, err = manager.Store.Transition(ctx, transaction.ID, PhaseMigrationFinalize, PhaseFinalize, stateForPhase(PhaseFinalize), "migration_recovered", "deployd", map[string]any{"business_authority": "rqlite"}, manager.Clock().UTC())
		if err != nil {
			return nil, manager.migrationFailure(ctx, transaction.ID, PhaseMigrationFinalize, err, true)
		}
		return updated, nil
	}
	if action == "resume-bootstrap" {
		return manager.resumeBootstrap(ctx, *transaction)
	}
	switch action {
	case "rollback":
		if transaction.RollbackUsed && transaction.Phase != PhaseRollback {
			return nil, errors.New("transaction rollback is already used")
		}
		if _, err := manager.completeRuntimeObservation(ctx); err != nil {
			return nil, err
		}
		if err := manager.rollback(ctx, *transaction); err != nil {
			return nil, err
		}
		return manager.Store.Get(ctx, id)
	case "finalize":
		observation, observeErr := manager.observe(ctx, id)
		verifiedReleaseID, verifiedSourceRevision, verifiedAuthority, verifiedCapabilities, identityErr := manager.verifiedCandidateIdentity(ctx, id)
		if observeErr != nil || identityErr != nil || !recoveryTargetVerified(observation, *transaction, manager.Clock().UTC(), false, false, false, verifiedReleaseID, verifiedSourceRevision, verifiedAuthority, verifiedCapabilities) {
			return nil, errors.New("recovery evidence is not sufficient to finalize")
		}
		previous, previousOK := observation.Slots[transaction.PreviousSlot]
		if previousOK && previous.ProcessUp {
			if err := manager.Runtime.DisableTraffic(ctx, transaction.PreviousSlot); err != nil {
				return nil, manager.effectFailure(ctx, id, transaction.Phase, fmt.Errorf("disable previous traffic before recovery finalize: %w", err))
			}
			term, termErr := manager.nextFenceTerm(ctx)
			if termErr != nil {
				return nil, manager.effectFailure(ctx, id, transaction.Phase, fmt.Errorf("allocate recovery fencing term: %w", termErr))
			}
			if err := manager.Runtime.RevokeController(ctx, transaction.PreviousSlot, term); err != nil {
				return nil, manager.effectFailure(ctx, id, transaction.Phase, fmt.Errorf("fence the non-selected slot before recovery finalize: %w", err))
			}
			if err := manager.Runtime.Drain(ctx, transaction.PreviousSlot); err != nil {
				return nil, manager.effectFailure(ctx, id, transaction.Phase, fmt.Errorf("drain the previous slot before recovery finalize: %w", err))
			}
			if err := manager.Runtime.Quiesce(ctx, transaction.PreviousSlot); err != nil {
				return nil, manager.effectFailure(ctx, id, transaction.Phase, fmt.Errorf("quiesce the previous slot before recovery finalize: %w", err))
			}
			if err := manager.Runtime.Stop(ctx, transaction.PreviousSlot); err != nil {
				return nil, manager.effectFailure(ctx, id, transaction.Phase, fmt.Errorf("stop the previous slot before recovery finalize: %w", err))
			}
		}
		observation, observeErr = manager.observe(ctx, id)
		if observeErr != nil || !recoveryTargetVerified(observation, *transaction, manager.Clock().UTC(), false, false, true, verifiedReleaseID, verifiedSourceRevision, verifiedAuthority, verifiedCapabilities) {
			cause := errors.New("previous slot was not stopped before recovery finalize")
			if observeErr != nil {
				cause = fmt.Errorf("observe recovery stop outcome: %w", observeErr)
			}
			return nil, manager.effectFailure(ctx, id, transaction.Phase, cause)
		}
		if verifier, ok := manager.Runtime.(interface {
			PrepareRecoveryController(context.Context, string, string) error
		}); ok {
			if err := verifier.PrepareRecoveryController(ctx, transaction.TargetSlot, transaction.Bundle); err != nil {
				return nil, manager.effectFailure(ctx, id, transaction.Phase, err)
			}
		}
		lease, err := manager.newLease(ctx, transaction.TargetSlot, transaction.Generation)
		if err != nil {
			return nil, fmt.Errorf("allocate recovery controller lease: %w", err)
		}
		if err := manager.Runtime.GrantController(ctx, transaction.TargetSlot, lease); err != nil {
			return nil, manager.effectFailure(ctx, id, transaction.Phase, fmt.Errorf("regrant the selected controller before recovery finalize: %w", err))
		}
		parentContext := ctx
		ctx, stopRenewal := manager.beginTransactionRenewal(ctx, lease)
		defer stopRenewal()
		fail := func(cause error) (*store.Transaction, error) {
			_ = stopRenewal()
			return nil, manager.effectFailure(parentContext, id, transaction.Phase, cause)
		}
		if err := manager.waitForReady(ctx, id, transaction.TargetSlot); err != nil {
			return fail(fmt.Errorf("recovery finalize readiness was not verified: %w", err))
		}
		if err := manager.Runtime.EnableTraffic(ctx, transaction.TargetSlot); err != nil {
			return fail(fmt.Errorf("recovery finalize traffic role was not restored: %w", err))
		}
		observation, observeErr = manager.observe(ctx, id)
		if observeErr != nil || !recoveryTargetVerified(observation, *transaction, manager.Clock().UTC(), true, true, true, verifiedReleaseID, verifiedSourceRevision, verifiedAuthority, verifiedCapabilities) {
			return fail(errors.New("recovery finalize controller evidence was not verified"))
		}
		if err := stopRenewal(); err != nil {
			return fail(err)
		}
		ctx = parentContext
		updated, err := manager.transition(ctx, id, transaction.Phase, PhaseFinalize, "recovered_finalize", map[string]any{"verified": true, "active_slot": transaction.TargetSlot, "lease_id": lease.LeaseID})
		if err != nil {
			return fail(fmt.Errorf("recovery finalize persistence failed: %w", err))
		}
		manager.startupFenced = false
		return updated, nil
	default:
		return nil, errors.New("recovery action must be retry-stage, rollback or finalize")
	}
}

func (manager *Manager) retryStage(ctx context.Context, transaction store.Transaction) (*store.Transaction, error) {
	if validSlot(transaction.TargetSlot) != nil || validSlot(transaction.PreviousSlot) != nil || transaction.TargetSlot == transaction.PreviousSlot {
		return nil, errors.New("stage recovery requires distinct fixed slots")
	}
	if transaction.Phase != PhaseStage && transaction.Phase != PhaseStandbyValidation && transaction.Phase != PhaseControllerHandoff {
		return nil, errors.New("stage recovery is limited to stage, standby_validation, or controller_handoff")
	}
	created, err := manager.Store.LatestEventByTransition(ctx, transaction.ID, "created")
	if err != nil || created == nil {
		return nil, errors.New("original stage identity is unavailable")
	}
	if created.Evidence["backup_before_stage"] != true || created.Evidence["active_slot"] != transaction.PreviousSlot {
		return nil, errors.New("stage recovery requires the original backup-gated transaction")
	}
	started, err := manager.Store.LatestEventByTransition(ctx, transaction.ID, "stage_effect_started")
	if err != nil || started == nil {
		return nil, errors.New("stage recovery requires a persisted stage effect intent")
	}
	prepared, err := manager.Store.LatestEvent(ctx, transaction.ID, PhaseStage, "retry_stage_prepared")
	if err != nil {
		return nil, err
	}
	if !transaction.RecoveryRequired && transaction.State != "needs_recovery" &&
		(transaction.State != "active" || prepared == nil || (transaction.Phase != PhaseStandbyValidation && transaction.Phase != PhaseControllerHandoff)) {
		return nil, errors.New("stage recovery does not match an active retained retry intent")
	}
	previousRelease, previousSource, previousAuthority, _, err := previousIdentityFromEvent(created)
	if err != nil {
		return nil, err
	}
	validator, ok := manager.Runtime.(recoveryPointValidator)
	if !ok || !manager.usesRecoveryPoints() {
		return nil, errors.New("stage recovery requires an existing verified rqlite recovery point")
	}
	point, err := validator.ValidateRecoveryPoint(ctx, transaction.ID, "stage", previousAuthority)
	if err != nil {
		return nil, fmt.Errorf("validate original stage recovery point: %w", err)
	}
	preflight, ok := manager.Runtime.(RuntimePreflight)
	if !ok {
		return nil, errors.New("stage recovery requires formal bundle preflight")
	}
	if err := preflight.Preflight(ctx, transaction.TargetSlot, transaction.Bundle); err != nil {
		return nil, err
	}
	observation, err := manager.observe(ctx, transaction.ID)
	if err != nil {
		return nil, err
	}
	if !validObservation(observation, manager.Clock().UTC()) || observation.ActiveSlot != transaction.PreviousSlot || observation.ControllerSlot != "none" {
		return nil, errors.New("stage recovery requires a fresh consistent observation of the original active slot with no controller")
	}
	for _, slot := range []string{"blue", "green"} {
		current := observation.Slots[slot]
		if current.ControllerOwned || current.TrafficAccepting {
			return nil, errors.New("stage recovery requires both slots fenced from controller and traffic")
		}
	}
	previous, previousOK := observation.Slots[transaction.PreviousSlot]
	target, targetOK := observation.Slots[transaction.TargetSlot]
	if !previousOK || !previous.ProcessUp || !previous.ProcessReady || previous.TrafficAccepting ||
		!previous.CountersKnown || previous.InFlightRequests != 0 || previous.NativeWorkers != 0 || previous.PendingOperations != 0 ||
		previous.ReleaseID != previousRelease || previous.SourceRevision != previousSource || previous.StateAuthority != previousAuthority ||
		previous.StateEpoch == "" || !targetOK ||
		target.ControllerOwned || target.TrafficAccepting || target.InFlightRequests != 0 || target.NativeWorkers != 0 || target.PendingOperations != 0 ||
		(target.ProcessUp && (!target.ProcessReady || !target.CountersKnown || target.InFlightRequests != 0 || target.NativeWorkers != 0 || target.PendingOperations != 0)) {
		return nil, errors.New("stage recovery incumbent or target slot is not safely fenced and quiescent")
	}
	if prepared == nil {
		if transaction.Phase != PhaseStage || transaction.State != "needs_recovery" {
			return nil, errors.New("stage recovery retry identity is missing")
		}
	}
	if prepared == nil {
		evidence := map[string]any{
			"previous_release_id": previous.ReleaseID, "previous_source_revision": previous.SourceRevision,
			"previous_state_epoch": previous.StateEpoch, "previous_state_authority": previous.StateAuthority,
			"previous_state_capabilities": append([]string(nil), previous.StateCapabilities...),
			"recovery_point_id":           point.ID, "recovery_point_sha256": point.SHA256, "recovery_point_boundary": point.Boundary,
		}
		if _, err := manager.Store.Transition(ctx, transaction.ID, PhaseStage, PhaseStage, "needs_recovery", "retry_stage_prepared", "deployd", evidence, manager.Clock().UTC()); err != nil {
			return nil, err
		}
	} else if !stageRetryCheckpointMatches(prepared.Evidence, previous, point) {
		return nil, errors.New("stage recovery checkpoint no longer matches the active slot or original recovery point")
	}
	if transaction.Phase == PhaseStage {
		if err := manager.Runtime.Drain(ctx, transaction.PreviousSlot); err != nil {
			return nil, fmt.Errorf("establish original release drain for stage recovery: %w", err)
		}
		observation, err = manager.observe(ctx, transaction.ID)
		if err != nil {
			return nil, manager.effectFailure(ctx, transaction.ID, transaction.Phase, err)
		}
		currentPrevious, previousOK := observation.Slots[transaction.PreviousSlot]
		currentTarget, targetOK := observation.Slots[transaction.TargetSlot]
		if !validObservation(observation, manager.Clock().UTC()) || observation.ActiveSlot != transaction.PreviousSlot || observation.ControllerSlot != "none" ||
			!previousOK || !currentPrevious.ProcessUp || !currentPrevious.ProcessReady || !currentPrevious.Draining || currentPrevious.TrafficAccepting || !currentPrevious.CountersKnown ||
			currentPrevious.InFlightRequests != 0 || currentPrevious.NativeWorkers != 0 || currentPrevious.PendingOperations != 0 || currentPrevious.ReleaseID != previous.ReleaseID ||
			currentPrevious.SourceRevision != previous.SourceRevision || currentPrevious.StateAuthority != previous.StateAuthority || currentPrevious.StateEpoch == "" ||
			!targetOK || currentTarget.ControllerOwned || currentTarget.TrafficAccepting ||
			(currentTarget.ProcessUp && (!currentTarget.ProcessReady || !currentTarget.CountersKnown || currentTarget.InFlightRequests != 0 || currentTarget.NativeWorkers != 0 || currentTarget.PendingOperations != 0)) {
			return nil, manager.effectFailure(ctx, transaction.ID, transaction.Phase, errors.New("stage recovery drain or slot quiescence was not verified"))
		}
		drainEvidence := map[string]any{
			"previous_release_id": currentPrevious.ReleaseID, "previous_source_revision": currentPrevious.SourceRevision,
			"drained_state_epoch": currentPrevious.StateEpoch, "previous_state_authority": currentPrevious.StateAuthority,
			"previous_state_capabilities": append([]string(nil), currentPrevious.StateCapabilities...),
			"recovery_point_id":           point.ID, "recovery_point_sha256": point.SHA256, "recovery_point_boundary": point.Boundary,
		}
		if _, err := manager.Store.Transition(ctx, transaction.ID, PhaseStage, PhaseStage, transaction.State, "retry_stage_drained", "deployd", drainEvidence, manager.Clock().UTC()); err != nil {
			return nil, err
		}
		previous = currentPrevious
		target = currentTarget
		if !target.ProcessUp {
			if err := manager.Runtime.Stage(ctx, transaction.TargetSlot, transaction.Bundle); err != nil {
				return nil, manager.effectFailure(ctx, transaction.ID, PhaseStage, fmt.Errorf("retry stage failed: %w", err))
			}
		}
	} else {
		if !target.ProcessUp || !target.ProcessReady || !target.CountersKnown || target.InFlightRequests != 0 || target.NativeWorkers != 0 || target.PendingOperations != 0 {
			return nil, errors.New("recovery candidate is not process-ready and quiescent after its stage phase")
		}
	}
	if transaction.Phase == PhaseStage {
		if err := manager.transitionAfterEffect(ctx, transaction.ID, PhaseStage, PhaseStandbyValidation, "stage_retried", map[string]any{"target_slot": transaction.TargetSlot, "reused_recovery_point": point.ID}); err != nil {
			return nil, err
		}
		transaction.Phase = PhaseStandbyValidation
	}
	if transaction.Phase == PhaseStandbyValidation {
		return manager.verifyRetryStageCandidate(ctx, transaction, previous, point)
	}
	return manager.reverifyRetryStageCandidate(ctx, transaction, previous, point)
}

func (manager *Manager) verifyRetryStageCandidate(ctx context.Context, transaction store.Transaction, previous SlotObservation, point recoveryPoint) (*store.Transaction, error) {
	if err := manager.verifyRetryStageBundle(ctx, transaction.TargetSlot, transaction.Bundle); err != nil {
		return nil, manager.effectFailure(ctx, transaction.ID, PhaseStandbyValidation, fmt.Errorf("retry candidate verification failed: %w", err))
	}
	observation, err := manager.observe(ctx, transaction.ID)
	if err != nil {
		return nil, manager.effectFailure(ctx, transaction.ID, PhaseStandbyValidation, err)
	}
	target, targetOK := observation.Slots[transaction.TargetSlot]
	currentPrevious, previousOK := observation.Slots[transaction.PreviousSlot]
	if !validObservation(observation, manager.Clock().UTC()) || observation.ActiveSlot != transaction.PreviousSlot || observation.ControllerSlot != "none" ||
		!targetOK || !target.ProcessUp || !target.ProcessReady || target.ControllerOwned || target.TrafficAccepting || target.ReleaseID == "" ||
		target.SourceRevision == "" || target.StateEpoch == "" || target.StateAuthority != previous.StateAuthority ||
		!previousOK || !currentPrevious.ProcessUp || !currentPrevious.ProcessReady || currentPrevious.ControllerOwned || currentPrevious.TrafficAccepting || !currentPrevious.CountersKnown ||
		currentPrevious.InFlightRequests != 0 || currentPrevious.NativeWorkers != 0 || currentPrevious.PendingOperations != 0 || currentPrevious.ReleaseID != previous.ReleaseID ||
		currentPrevious.SourceRevision != previous.SourceRevision || currentPrevious.StateEpoch == "" || currentPrevious.StateAuthority != previous.StateAuthority {
		return nil, manager.effectFailure(ctx, transaction.ID, PhaseStandbyValidation, errors.New("retry candidate or incumbent identity changed during stage recovery"))
	}
	if err := manager.transitionAfterEffect(ctx, transaction.ID, PhaseStandbyValidation, PhaseControllerHandoff, "verified", map[string]any{
		"candidate_ready": true, "release_id": target.ReleaseID, "source_revision": target.SourceRevision,
		"state_epoch": target.StateEpoch, "state_authority": target.StateAuthority,
		"state_capabilities": append([]string(nil), target.StateCapabilities...), "recovery_point_id": point.ID,
	}); err != nil {
		return nil, err
	}
	return manager.Store.Get(ctx, transaction.ID)
}

func (manager *Manager) reverifyRetryStageCandidate(ctx context.Context, transaction store.Transaction, previous SlotObservation, point recoveryPoint) (*store.Transaction, error) {
	releaseID, sourceRevision, authority, _, err := manager.verifiedCandidateIdentity(ctx, transaction.ID)
	if err != nil {
		return nil, err
	}
	if err := manager.verifyRetryStageBundle(ctx, transaction.TargetSlot, transaction.Bundle); err != nil {
		return nil, manager.effectFailure(ctx, transaction.ID, transaction.Phase, fmt.Errorf("retry candidate verification failed: %w", err))
	}
	observation, err := manager.observe(ctx, transaction.ID)
	if err != nil {
		return nil, manager.effectFailure(ctx, transaction.ID, transaction.Phase, err)
	}
	target, targetOK := observation.Slots[transaction.TargetSlot]
	currentPrevious, previousOK := observation.Slots[transaction.PreviousSlot]
	if !validObservation(observation, manager.Clock().UTC()) || observation.ActiveSlot != transaction.PreviousSlot || observation.ControllerSlot != "none" ||
		!targetOK || !target.ProcessUp || !target.ProcessReady || target.ControllerOwned || target.TrafficAccepting || target.ReleaseID != releaseID ||
		target.SourceRevision != sourceRevision || target.StateAuthority != authority ||
		!previousOK || !currentPrevious.ProcessUp || !currentPrevious.ProcessReady || currentPrevious.ControllerOwned || currentPrevious.TrafficAccepting || !currentPrevious.CountersKnown ||
		currentPrevious.InFlightRequests != 0 || currentPrevious.NativeWorkers != 0 || currentPrevious.PendingOperations != 0 || currentPrevious.ReleaseID != previous.ReleaseID ||
		currentPrevious.SourceRevision != previous.SourceRevision || currentPrevious.StateEpoch == "" || currentPrevious.StateAuthority != previous.StateAuthority {
		return nil, manager.effectFailure(ctx, transaction.ID, transaction.Phase, errors.New("recovered stage identity is no longer verified"))
	}
	current, err := manager.Store.Get(ctx, transaction.ID)
	if err != nil || current == nil {
		return nil, errOrNotFound(err, transaction.ID, current)
	}
	if current.State == "needs_recovery" || current.RecoveryRequired {
		if err := manager.transitionAfterEffect(ctx, transaction.ID, transaction.Phase, transaction.Phase, "stage_retry_reverified", map[string]any{
			"release_id": releaseID, "source_revision": sourceRevision, "state_authority": authority,
			"recovery_point_id": point.ID,
		}); err != nil {
			return nil, err
		}
		return manager.Store.Get(ctx, transaction.ID)
	}
	return current, nil
}

func (manager *Manager) verifyRetryStageBundle(ctx context.Context, slot, bundle string) error {
	if verifier, ok := manager.Runtime.(interface {
		PrepareRecoveryController(context.Context, string, string) error
	}); ok {
		return verifier.PrepareRecoveryController(ctx, slot, bundle)
	}
	return manager.Runtime.Verify(ctx, slot, bundle)
}

func stageRetryCheckpointMatches(evidence map[string]any, previous SlotObservation, point recoveryPoint) bool {
	releaseID, releaseOK := evidence["previous_release_id"].(string)
	sourceRevision, sourceOK := evidence["previous_source_revision"].(string)
	stateEpoch, epochOK := evidence["previous_state_epoch"].(string)
	authority, authorityOK := evidence["previous_state_authority"].(string)
	_, capabilitiesOK := evidenceStrings(evidence["previous_state_capabilities"])
	pointID, pointIDOK := evidence["recovery_point_id"].(string)
	pointDigest, pointDigestOK := evidence["recovery_point_sha256"].(string)
	pointBoundary, pointBoundaryOK := evidence["recovery_point_boundary"].(string)
	return releaseOK && sourceOK && epochOK && stateEpoch != "" && authorityOK && capabilitiesOK && pointIDOK && pointDigestOK && pointBoundaryOK &&
		releaseID == previous.ReleaseID && sourceRevision == previous.SourceRevision && authority == previous.StateAuthority &&
		pointID == point.ID && pointDigest == point.SHA256 && pointBoundary == point.Boundary
}

// CompleteForward finishes a fenced deployment transaction with a verified
// candidate when the previous release can no longer start against the current
// state authority. It installs caller-supplied, preflight-verified bytes only
// into the original target slot, proves the incumbent is stopped and idle,
// grants the sole controller role, enables traffic and switches the active
// link. It never restores, replaces or deletes business state, and it refuses
// while any slot still holds a role.
func (manager *Manager) CompleteForward(ctx context.Context, id, bundle string) (*store.Transaction, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	transaction, err := manager.Store.Get(ctx, id)
	if err != nil || transaction == nil {
		return transaction, errOrNotFound(err, id, transaction)
	}
	if transaction.State != "needs_recovery" || !transaction.RecoveryRequired {
		return nil, errors.New("complete-forward requires a transaction in needs_recovery")
	}
	switch transaction.Phase {
	case PhaseStage, PhaseStandbyValidation, PhaseControllerHandoff, PhaseTrafficEnable, PhaseRollback:
	default:
		return nil, fmt.Errorf("complete-forward is not allowed in phase %s", transaction.Phase)
	}
	if validSlot(transaction.TargetSlot) != nil || validSlot(transaction.PreviousSlot) != nil || transaction.TargetSlot == transaction.PreviousSlot {
		return nil, errors.New("complete-forward requires distinct fixed slots")
	}
	if bundle == "" || !strings.HasPrefix(bundle, "/") {
		return nil, errors.New("complete-forward requires an absolute verified bundle")
	}
	created, err := manager.Store.LatestEventByTransition(ctx, id, "created")
	if err != nil || created == nil {
		return nil, errors.New("original deployment identity is unavailable")
	}
	if created.Evidence["active_slot"] != transaction.PreviousSlot {
		return nil, errors.New("original active slot does not match the transaction")
	}
	previousRelease, previousSource, previousAuthority, previousCapabilities, err := previousIdentityFromEvent(created)
	if err != nil {
		return nil, err
	}
	preflight, ok := manager.Runtime.(RuntimePreflight)
	if !ok {
		return nil, errors.New("complete-forward requires formal bundle preflight")
	}
	if err := preflight.Preflight(ctx, transaction.TargetSlot, bundle); err != nil {
		return nil, err
	}
	observation, err := manager.observe(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := forwardRecoveryObservation(observation, *transaction, manager.Clock().UTC()); err != nil {
		return nil, err
	}
	if _, err := manager.transition(ctx, id, transaction.Phase, PhaseControllerHandoff, "forward_recovery_started", map[string]any{
		"previous_slot": transaction.PreviousSlot, "previous_release_id": previousRelease,
		"previous_source_revision": previousSource, "previous_state_authority": previousAuthority,
		"previous_state_capabilities": append([]string(nil), previousCapabilities...),
		"previous_slot_stopped":       true,
	}); err != nil {
		return nil, err
	}
	if err := manager.Runtime.Stage(ctx, transaction.TargetSlot, bundle); err != nil {
		return nil, manager.effectFailure(ctx, id, PhaseControllerHandoff, fmt.Errorf("forward stage failed: %w", err))
	}
	if err := manager.Runtime.Verify(ctx, transaction.TargetSlot, bundle); err != nil {
		return nil, manager.effectFailure(ctx, id, PhaseControllerHandoff, fmt.Errorf("forward candidate verification failed: %w", err))
	}
	observation, err = manager.observe(ctx, id)
	if err != nil {
		return nil, manager.effectFailure(ctx, id, PhaseControllerHandoff, err)
	}
	target, targetOK := observation.Slots[transaction.TargetSlot]
	if !targetOK || !target.ProcessUp || !target.ProcessReady || target.ControllerOwned || target.TrafficAccepting || target.ReleaseID == "" || target.SourceRevision == "" || target.StateEpoch == "" || target.StateAuthority != previousAuthority {
		return nil, manager.effectFailure(ctx, id, PhaseControllerHandoff, errors.New("forward candidate identity or state compatibility was not verified"))
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseControllerHandoff, PhaseControllerHandoff, "forward_candidate_verified", map[string]any{
		"release_id": target.ReleaseID, "source_revision": target.SourceRevision,
		"state_epoch": target.StateEpoch, "state_authority": target.StateAuthority,
		"state_capabilities": append([]string(nil), target.StateCapabilities...),
	}); err != nil {
		return nil, err
	}
	lease, err := manager.newLease(ctx, transaction.TargetSlot, transaction.Generation)
	if err != nil {
		return nil, fmt.Errorf("allocate forward recovery controller lease: %w", err)
	}
	if err := manager.Runtime.GrantController(ctx, transaction.TargetSlot, lease); err != nil {
		return nil, manager.effectFailure(ctx, id, PhaseControllerHandoff, fmt.Errorf("grant the forward recovery controller: %w", err))
	}
	parentContext := ctx
	stopRenewal := func() error { return nil }
	if _, supported := manager.Runtime.(interface {
		RenewTransactionLease(context.Context, controllerrole.Lease) (controllerrole.Lease, error)
	}); supported {
		ctx, stopRenewal = manager.beginTransactionRenewal(ctx, lease)
	}
	defer stopRenewal()
	fail := func(cause error) (*store.Transaction, error) {
		_ = stopRenewal()
		return nil, manager.effectFailure(parentContext, id, PhaseControllerHandoff, cause)
	}
	if err := manager.waitForReady(ctx, id, transaction.TargetSlot); err != nil {
		return fail(fmt.Errorf("forward recovery readiness was not verified: %w", err))
	}
	if err := manager.Runtime.EnableTraffic(ctx, transaction.TargetSlot); err != nil {
		return fail(fmt.Errorf("forward recovery traffic role was not enabled: %w", err))
	}
	if err := manager.Runtime.SwitchActive(ctx, transaction.TargetSlot); err != nil {
		return fail(err)
	}
	observation, err = manager.observe(ctx, id)
	if err != nil {
		return fail(err)
	}
	target = observation.Slots[transaction.TargetSlot]
	previous, previousOK := observation.Slots[transaction.PreviousSlot]
	previousStopped := previousOK && !previous.ProcessUp && !previous.ProcessReady && !previous.ControllerOwned && !previous.TrafficAccepting && previous.InFlightRequests == 0 && previous.NativeWorkers == 0 && previous.PendingOperations == 0
	if !validObservation(observation, manager.Clock().UTC()) || observation.ActiveSlot != transaction.TargetSlot || observation.ControllerSlot != transaction.TargetSlot ||
		!target.ProcessUp || !target.ProcessReady || !target.Ready || !target.ControllerOwned || !target.TrafficAccepting ||
		target.ReleaseID == "" || target.SourceRevision == "" || target.StateEpoch == "" || target.StateAuthority != previousAuthority ||
		!previousStopped {
		return fail(errors.New("forward recovery outcome was not verified"))
	}
	if err := stopRenewal(); err != nil {
		return fail(err)
	}
	ctx = parentContext
	if err := manager.transitionAfterEffect(ctx, id, PhaseControllerHandoff, PhaseFinalize, "forward_recovered", map[string]any{
		"active_slot": transaction.TargetSlot, "previous_slot_stopped": true, "lease_id": lease.LeaseID,
	}); err != nil {
		return nil, err
	}
	manager.startupFenced = false
	return manager.Store.Get(ctx, id)
}

// forwardRecoveryObservation accepts the fenced state this recovery exists for:
// the active slot is already stopped, so slot health contracts and business
// readiness cannot be observed. Structural authority (active link and systemd
// slot identities) must still be fresh, and no slot may hold a role.
func forwardRecoveryObservation(observation RuntimeSnapshot, transaction store.Transaction, now time.Time) error {
	// The composed freshness window collapses to the stale source, and a stopped
	// active slot necessarily reports no slot health contract. Judge the sources
	// this recovery depends on instead of the composed window.
	if observation.ObservedAt.IsZero() || observation.ObservedAt.After(now.Add(2*time.Second)) {
		return errors.New("complete-forward requires a current observation")
	}
	for _, name := range []string{"active_link", "systemd"} {
		source, ok := observation.Sources[name]
		if !ok || !source.Up || source.ObservedAt.IsZero() || source.FreshUntil.IsZero() || !source.FreshUntil.After(now) ||
			source.ObservedAt.After(now) || source.ObservedAt.Before(observation.ObservedAt.Add(-2*time.Second)) {
			return fmt.Errorf("complete-forward requires a fresh %s observation", name)
		}
	}
	if observation.ActiveSlot != transaction.PreviousSlot || observation.ControllerSlot != "none" {
		return errors.New("complete-forward requires the original active slot with no controller")
	}
	for _, slot := range []string{"blue", "green"} {
		current := observation.Slots[slot]
		if current.ControllerOwned || current.TrafficAccepting {
			return errors.New("complete-forward requires both slots fenced from controller and traffic")
		}
	}
	previous, previousOK := observation.Slots[transaction.PreviousSlot]
	if !previousOK || previous.ProcessUp || previous.ProcessReady || previous.InFlightRequests != 0 || previous.NativeWorkers != 0 || previous.PendingOperations != 0 {
		return errors.New("complete-forward requires the previous slot stopped and idle")
	}
	target, targetOK := observation.Slots[transaction.TargetSlot]
	if !targetOK {
		return errors.New("complete-forward requires an observed target slot")
	}
	if target.ProcessUp && (!target.ProcessReady || !target.CountersKnown || target.InFlightRequests != 0 || target.NativeWorkers != 0 || target.PendingOperations != 0) {
		return errors.New("complete-forward target slot is not quiescent")
	}
	return nil
}

func (manager *Manager) migrationSourceEvidence(ctx context.Context, id string) (state.SQLiteMigrationEvidence, bool, error) {
	event, err := manager.Store.LatestEvent(ctx, id, PhaseMigrationImport, "source_verified")
	if err != nil {
		return state.SQLiteMigrationEvidence{}, false, err
	}
	if event == nil {
		return state.SQLiteMigrationEvidence{}, false, nil
	}
	sourceSHA256, sourceOK := event.Evidence["source_sha256"].(string)
	stateEpoch, epochOK := event.Evidence["state_epoch"].(string)
	sourceSize, sizeOK := integerValue(event.Evidence["source_size"])
	fenceID, fenceOK := event.Evidence["fence_id"].(string)
	if !sourceOK || sourceSHA256 == "" || !epochOK || stateEpoch == "" || !sizeOK || sourceSize < 0 || !fenceOK || fenceID == "" {
		return state.SQLiteMigrationEvidence{}, true, errors.New("migration source evidence is incomplete")
	}
	return state.SQLiteMigrationEvidence{
		FenceID:      fenceID,
		SourceSHA256: sourceSHA256,
		SourceSize:   sourceSize,
		StateEpoch:   stateEpoch,
		VerifiedAt:   event.CreatedAt,
	}, true, nil
}

func (manager *Manager) RecoverOnStart(ctx context.Context) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	transactions, err := manager.Store.ListActive(ctx)
	if err != nil {
		manager.startupFenced = true
		return err
	}
	var firstErr error
	for _, transaction := range transactions {
		if transaction.Phase == PhaseStage {
			created, startErr := manager.Store.LatestEventByTransition(ctx, transaction.ID, "created")
			started, effectErr := manager.Store.LatestEventByTransition(ctx, transaction.ID, "stage_effect_started")
			if startErr == nil && effectErr == nil && created != nil && created.Evidence["backup_before_stage"] == true && started == nil {
				if _, err := manager.Store.Transition(ctx, transaction.ID, PhaseStage, PhaseStage, "failed", "backup_interrupted", "deployd", nil, manager.Clock().UTC()); err != nil && firstErr == nil {
					firstErr = err
				}
				continue
			}
		}
		if transaction.TargetSlot == "state" && manager.Migration != nil {
			if err := manager.recoverMigrationOnStart(ctx, transaction); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		_, observeErr := manager.observe(ctx, transaction.ID)
		cause := &UncertainError{Cause: errors.New("deployd restarted while deployment transaction was active")}
		if observeErr != nil {
			cause.Cause = fmt.Errorf("deployment transaction was active after deployd restart: %w", observeErr)
		}
		if err := manager.failure(ctx, transaction.ID, transaction.Phase, cause); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if firstErr != nil {
		manager.startupFenced = true
	}
	return firstErr
}

func (manager *Manager) Cleanup(ctx context.Context) error {
	_, err := manager.Store.Cleanup(ctx, manager.Clock().UTC())
	return err
}

func (manager *Manager) ensureNoActiveTransaction(ctx context.Context) error {
	transactions, err := manager.Store.ListActive(ctx)
	if err != nil {
		return err
	}
	if len(transactions) != 0 {
		return fmt.Errorf("deployment transaction %s is already active", transactions[0].ID)
	}
	return nil
}

func (manager *Manager) waitForReady(ctx context.Context, transactionID, slot string) error {
	deadline := time.NewTimer(60 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		observation, err := manager.observe(ctx, transactionID)
		if err != nil {
			return err
		}
		candidate, ok := observation.Slots[slot]
		if !ok || !candidate.ProcessUp || !candidate.ProcessReady || !candidate.ControllerOwned {
			return errors.New("candidate controller lease or process contract was lost")
		}
		if candidate.Ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("candidate slot did not become ready after controller handoff")
		case <-ticker.C:
		}
	}
}

// MaintainController renews the active slot lease after fresh authority
// evidence. Business consistency separately gates traffic. Persisted transactions never authorize a role
// after a Deployd restart; an active recovery transaction fences both slots.
func (manager *Manager) MaintainController(ctx context.Context) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.startupFenced {
		return manager.fenceSlots(ctx, errors.New("deployd startup recovery is unresolved"))
	}
	waiting, policyErr := manager.Store.ListActive(ctx)
	if policyErr != nil {
		return manager.fenceSlots(ctx, errors.New("deployment transaction state could not be read before renewal"))
	}
	policy := controllerMaintenancePolicy{}
	if len(waiting) > 0 {
		if len(waiting) != 1 {
			return manager.fenceSlots(ctx, errors.New("multiple active transactions cannot retain controller"))
		}
		transaction := waiting[0]
		if transaction.State != "active" || transaction.RecoveryRequired || (transaction.Phase != PhaseStandbyValidation && transaction.Phase != PhaseControllerHandoff) || transaction.PreviousSlot == "" || transaction.PreviousSlot == "none" || transaction.TargetSlot == transaction.PreviousSlot {
			return manager.fenceSlots(ctx, errors.New("deployment transaction cannot retain controller before observation"))
		}
		policy.Slot = transaction.PreviousSlot
	}
	ctx = context.WithValue(ctx, controllerMaintenanceKey{}, policy)
	observation, err := manager.observe(ctx, "")
	if err != nil {
		return manager.fenceSlots(ctx, errors.New("slot observation could not be persisted; controller roles were fenced"))
	}
	transactions, err := manager.Store.ListActive(ctx)
	if err != nil {
		return manager.fenceSlots(ctx, errors.New("deployment transaction state could not be read; controller roles were fenced"))
	}
	if len(transactions) != 0 {
		safeWaiting := len(transactions) == 1
		for _, transaction := range transactions {
			phaseSafe := transaction.Phase == PhaseStandbyValidation || transaction.Phase == PhaseControllerHandoff
			safeWaiting = safeWaiting && transaction.State == "active" && !transaction.RecoveryRequired && phaseSafe && transaction.PreviousSlot == observation.ActiveSlot && transaction.TargetSlot != observation.ActiveSlot
		}
		active := observation.Slots[observation.ActiveSlot]
		lease := active.ControllerLease
		if !safeWaiting || !active.ControllerOwned || lease == nil || lease.Validate(manager.Clock(), observation.ActiveSlot) != nil || lease.OwnerPID != os.Getpid() || lease.OwnerStartTime != controllerrole.ProcessStartToken(os.Getpid()) {
			return manager.fenceSlots(ctx, errors.New("deployment transaction cannot retain active controller authority"))
		}
	}
	// Normal lifecycle commits can advance the epoch between the separate
	// metrics/inventory/doctor reads. Re-observe before revoking an otherwise
	// live controller; business samples never authorize a new role identity.
	var retryLease *controllerrole.Lease
	for attempt := 1; attempt < 3 && manager.retryableControllerObservation(observation); attempt++ {
		if retryLease == nil {
			original := *observation.Slots[observation.ActiveSlot].ControllerLease
			retryLease = &original
		}
		observation, err = manager.observe(ctx, "")
		if current := observation.Slots[observation.ActiveSlot].ControllerLease; current != nil && sameLeaseAuthority(*current, *retryLease) && !current.Expiry.Before(retryLease.Expiry) {
			updated := *current
			retryLease = &updated
		}
		if err != nil {
			return manager.fenceSlots(ctx, errors.New("slot observation retry could not be persisted; controller roles were fenced"))
		}
	}
	controllerObservation := observation
	controllerObservation.Consistent = observation.Consistent || observation.ControllerRecoveryOnly
	if !validObservation(controllerObservation, manager.Clock().UTC()) && !validControllerRecoveryObservation(observation, manager.Clock().UTC()) {
		// Business snapshot consistency controls traffic, while fresh process,
		// singleton and active-link evidence independently verifies the existing
		// writer. Preserve its exact live identity so in-flight effects and lease
		// cleanup do not lose authority merely because the business epoch moves.
		if retryLease != nil && manager.retryableControllerObservation(observation) {
			current := observation.Slots[observation.ActiveSlot].ControllerLease
			if current != nil && *current == *retryLease && retryLease.Validate(manager.Clock(), observation.ActiveSlot) == nil {
				for _, slot := range []string{"blue", "green"} {
					if err := manager.Runtime.DisableTraffic(ctx, slot); err != nil {
						return manager.fenceSlots(ctx, errors.New("disable traffic during inconsistent business observation failed"))
					}
				}
				lease, err := manager.renewOrAllocateLease(ctx, observation.Slots[observation.ActiveSlot], retryLease.Generation, retryLease)
				if err != nil {
					return manager.fenceSlots(ctx, errors.New("business observation recovery lost the existing controller lease"))
				}
				if err := manager.Runtime.GrantController(ctx, observation.ActiveSlot, lease); err != nil {
					return manager.fenceSlots(ctx, errors.New("business observation recovery could not renew the existing controller lease"))
				}
				return errors.New("business observation is inconsistent; traffic disabled and verified existing controller lease renewed")
			}
		}
		return manager.fenceSlots(ctx, errors.New("slot observation is inconsistent"))
	}
	target, ok := observation.Slots[observation.ActiveSlot]
	if !ok || !target.ProcessUp || !target.ProcessReady || target.StateEpoch == "" {
		return manager.fenceSlots(ctx, errors.New("active slot is not process-ready"))
	}
	if observation.ControllerRecoveryOnly {
		// Inventory is a deployment-readiness input, not proof that the active
		// service or database is unavailable. Preserve the verified live role and
		// its traffic state until a complete inventory is available again.
		current := target.ControllerLease
		if !target.LeaseRenewalSupported || !target.ControllerOwned || current == nil ||
			current.Validate(manager.Clock(), target.Slot) != nil || current.OwnerPID != os.Getpid() ||
			current.OwnerStartTime == "" || current.OwnerStartTime != controllerrole.ProcessStartToken(os.Getpid()) {
			return manager.fenceSlots(ctx, errors.New("inventory observation requires a valid active controller lease"))
		}
		for slot, value := range observation.Slots {
			if slot != observation.ActiveSlot && (value.ControllerOwned || value.TrafficAccepting) {
				return manager.fenceSlots(ctx, errors.New("inventory observation has an unresolved inactive role"))
			}
		}
		original := *current
		lease, err := manager.renewOrAllocateLease(ctx, target, original.Generation, &original)
		if err != nil {
			return manager.fenceSlots(ctx, errors.New("inventory observation controller lease expired or changed"))
		}
		if err := manager.Runtime.GrantController(ctx, observation.ActiveSlot, lease); err != nil {
			return manager.fenceSlots(ctx, errors.New("inventory observation controller renewal failed"))
		}
		return nil
	}
	if target.Draining {
		if err := manager.Runtime.DisableTraffic(ctx, observation.ActiveSlot); err != nil {
			return manager.fenceSlots(ctx, fmt.Errorf("disable draining active traffic: %w", err))
		}
	}
	other := "blue"
	if observation.ActiveSlot == "blue" {
		other = "green"
	}
	if inactive, inactiveOK := observation.Slots[other]; inactiveOK && inactive.ProcessUp {
		if err := manager.Runtime.DisableTraffic(ctx, other); err != nil {
			return manager.fenceSlots(ctx, fmt.Errorf("disable inactive traffic role: %w", err))
		}
		term, termErr := manager.nextFenceTerm(ctx)
		if termErr != nil {
			return manager.fenceSlots(ctx, errors.New("allocate inactive fencing term failed; controller roles were fenced"))
		}
		if err := manager.Runtime.RevokeController(ctx, other, term); err != nil {
			return manager.fenceSlots(ctx, errors.New("inactive controller fencing failed"))
		}
	}
	generation := int64(1)
	for _, transaction := range transactions {
		if transaction.TargetSlot == observation.ActiveSlot || transaction.PreviousSlot == observation.ActiveSlot {
			generation = transaction.Generation
			break
		}
	}
	lease, err := manager.renewOrAllocateLease(ctx, target, generation, retryLease)
	if err != nil {
		return manager.fenceSlots(ctx, errors.New("allocate active controller lease failed; controller roles were fenced"))
	}
	if preparer, ok := manager.Runtime.(interface {
		PrepareControllerRenewal(context.Context, SlotObservation) error
	}); ok {
		if err := preparer.PrepareControllerRenewal(ctx, target); err != nil {
			return manager.fenceSlots(ctx, errors.New("active controller process identity could not be pinned"))
		}
	}
	if err := manager.Runtime.GrantController(ctx, observation.ActiveSlot, lease); err != nil {
		return manager.fenceSlots(ctx, errors.New("grant active controller failed; controller roles were fenced"))
	}
	if target.Draining {
		// A drained controller must retain its lease so startup reconciliation
		// and explicit maintenance can converge an incomplete inventory. Traffic
		// stays disabled; strict deployment validation still rejects this view.
		return nil
	}
	// A live ready controller only needs its lease extended. Starting another
	// full business observation here needlessly delays the next renewal.
	if current := target.ControllerLease; target.Ready && target.TrafficAccepting && target.ControllerOwned && current != nil && current.Term == lease.Term && current.LeaseID == lease.LeaseID {
		return nil
	}
	readyContext, stopRenewal := manager.beginTransactionRenewal(ctx, lease)
	readyErr := manager.waitForReady(readyContext, "", observation.ActiveSlot)
	if readyErr == nil {
		readyErr = manager.Runtime.EnableTraffic(readyContext, observation.ActiveSlot)
	}
	renewalErr := stopRenewal()
	if readyErr != nil {
		return manager.fenceSlots(ctx, fmt.Errorf("active controller readiness or traffic failed: %w", readyErr))
	}
	if renewalErr != nil {
		return manager.fenceSlots(ctx, fmt.Errorf("active controller renewal failed: %w", renewalErr))
	}
	return nil
}

func (manager *Manager) retryableControllerObservation(observation RuntimeSnapshot) bool {
	now := manager.Clock().UTC()
	if observation.Consistent || observation.ControllerRecoveryOnly || observation.ControllerSlot != observation.ActiveSlot {
		return false
	}
	target, ok := observation.Slots[observation.ActiveSlot]
	if !ok || !target.ProcessUp || !target.ProcessReady || !target.ControllerOwned || !target.LeaseRenewalSupported {
		return false
	}
	lease := target.ControllerLease
	if lease == nil || lease.Validate(now, target.Slot) != nil || lease.OwnerPID != os.Getpid() || lease.OwnerStartTime == "" || lease.OwnerStartTime != controllerrole.ProcessStartToken(os.Getpid()) {
		return false
	}
	for slot, value := range observation.Slots {
		if slot != observation.ActiveSlot && value.ControllerOwned {
			return false
		}
	}
	for _, name := range []string{"active_link", "systemd", "slot_contracts", "native_observation"} {
		source, present := observation.Sources[name]
		if !present || !source.Up || source.ObservedAt.IsZero() || source.ObservedAt.After(now) || source.FreshUntil.IsZero() || !source.FreshUntil.After(now) {
			return false
		}
	}
	return true
}

func (manager *Manager) fenceSlots(ctx context.Context, cause error) error {
	var first error
	for _, slot := range []string{"blue", "green"} {
		if err := manager.Runtime.DisableTraffic(ctx, slot); err != nil && first == nil {
			first = err
		}
	}
	for _, slot := range []string{"blue", "green"} {
		if err := manager.Runtime.FenceController(ctx, slot); err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return fmt.Errorf("%w; controller fencing failed: %v", cause, first)
	}
	return cause
}

func (manager *Manager) observe(ctx context.Context, transactionID string) (RuntimeSnapshot, error) {
	observation, err := manager.observeRuntime(ctx)
	for attempt := 1; transactionID != "" && err == nil && !observation.Consistent && attempt < 3; attempt++ {
		observation, err = manager.observeRuntime(ctx)
	}
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = manager.Clock().UTC()
	}
	if observation.Slots == nil {
		observation.Slots = map[string]SlotObservation{}
	}
	// A running heartbeat owns expiry updates for its exact lease. A second
	// observation-driven grant would race it and invalidate its expiry proof.
	if transactionID != "" && ctx.Value(transactionRenewalKey{}) != true && validObservation(observation, manager.Clock().UTC()) {
		transaction, readErr := manager.Store.Get(ctx, transactionID)
		if readErr != nil {
			return RuntimeSnapshot{}, readErr
		}
		if transaction != nil && transaction.State != "finalized" && observation.ControllerSlot == transaction.TargetSlot {
			target := observation.Slots[transaction.TargetSlot]
			current := target.ControllerLease
			if target.ProcessUp && target.ProcessReady && target.ControllerOwned && target.LeaseRenewalSupported && current != nil &&
				current.Generation == transaction.Generation && current.Validate(manager.Clock(), target.Slot) == nil &&
				current.OwnerPID == os.Getpid() && current.OwnerStartTime != "" && current.OwnerStartTime == controllerrole.ProcessStartToken(os.Getpid()) {
				// Deployment phases can outlast a role TTL. Renew only the exact
				// live candidate authority verified by this fresh observation; an
				// expired/fenced role is never recreated from the transaction row.
				lease, renewErr := manager.renewOrAllocateLease(ctx, target, transaction.Generation, current)
				if renewErr != nil {
					return RuntimeSnapshot{}, renewErr
				}
				if err := manager.Runtime.GrantController(ctx, target.Slot, lease); err != nil {
					return RuntimeSnapshot{}, &UncertainError{Cause: err}
				}
				target.ControllerLease = &lease
				observation.Slots[target.Slot] = target
			}
		}
	}
	payload := map[string]any{"active_slot": observation.ActiveSlot, "controller_slot": observation.ControllerSlot, "consistent": observation.Consistent, "observed_at": observation.ObservedAt, "fresh_until": observation.FreshUntil, "slots": observation.Slots, "sources": observation.Sources}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	value := store.Observation{ObservedAt: observation.ObservedAt, FreshUntil: observation.FreshUntil, Digest: hex.EncodeToString(digest[:]), Payload: payload}
	if err := manager.Store.SaveObservation(ctx, store.LatestObservationID, value); err != nil {
		return RuntimeSnapshot{}, err
	}
	if transactionID != "" {
		if err := manager.Store.SaveObservation(ctx, transactionID, value); err != nil {
			return RuntimeSnapshot{}, err
		}
	}
	return observation, nil
}

func (manager *Manager) transition(ctx context.Context, id, expected, phase, transition string, evidence map[string]any) (*store.Transaction, error) {
	return manager.Store.Transition(ctx, id, expected, phase, stateForPhase(phase), transition, "deployd", evidence, manager.Clock().UTC())
}

func (manager *Manager) verifiedCandidateIdentity(ctx context.Context, id string) (string, string, string, []string, error) {
	event, err := manager.Store.LatestEvent(ctx, id, PhaseControllerHandoff, "verified")
	if err != nil {
		return "", "", "", nil, fmt.Errorf("read verified candidate identity: %w", err)
	}
	if event == nil {
		return "", "", "", nil, errors.New("verified candidate identity is missing")
	}
	releaseID, releaseOK := event.Evidence["release_id"].(string)
	sourceRevision, sourceOK := event.Evidence["source_revision"].(string)
	stateAuthority, authorityOK := event.Evidence["state_authority"].(string)
	capabilities, capabilitiesOK := evidenceStrings(event.Evidence["state_capabilities"])
	if !releaseOK || releaseID == "" || !sourceOK || sourceRevision == "" || !authorityOK || stateAuthority == "" || !capabilitiesOK || len(capabilities) == 0 {
		return "", "", "", nil, errors.New("verified candidate identity is incomplete")
	}
	return releaseID, sourceRevision, stateAuthority, capabilities, nil
}

func previousIdentityFromEvent(event *store.PhaseEvent) (string, string, string, []string, error) {
	if event == nil {
		return "", "", "", nil, errors.New("rollback target identity is missing")
	}
	releaseID, releaseOK := event.Evidence["previous_release_id"].(string)
	sourceRevision, sourceOK := event.Evidence["previous_source_revision"].(string)
	stateAuthority, authorityOK := event.Evidence["previous_state_authority"].(string)
	capabilities, capabilitiesOK := evidenceStrings(event.Evidence["previous_state_capabilities"])
	if !releaseOK || releaseID == "" || !sourceOK || sourceRevision == "" || !authorityOK || stateAuthority == "" || !capabilitiesOK || len(capabilities) == 0 {
		return "", "", "", nil, errors.New("rollback target identity is incomplete")
	}
	return releaseID, sourceRevision, stateAuthority, capabilities, nil
}

func (manager *Manager) verifiedPreviousIdentity(ctx context.Context, id string) (string, string, string, []string, error) {
	event, err := manager.Store.LatestEvent(ctx, id, PhaseStage, "retry_stage_prepared")
	if err != nil {
		return "", "", "", nil, fmt.Errorf("read retry-stage checkpoint: %w", err)
	}
	if event == nil {
		event, err = manager.Store.LatestEventByTransition(ctx, id, "created")
	}
	if err != nil {
		return "", "", "", nil, fmt.Errorf("read rollback target identity: %w", err)
	}
	return previousIdentityFromEvent(event)
}

func (manager *Manager) failure(ctx context.Context, id, phase string, cause error) error {
	code := "deployment_failed"
	if errors.Is(cause, errBackupStorageExhausted) {
		code = "backup_storage_exhausted"
	}
	if phase == PhaseRollback || errors.Is(cause, ErrUncertain) {
		code = "state_unverified"
	}
	payload := map[string]any{"message": failureMessage(phase, code), "uncertain": code == "state_unverified"}
	if sanitized := sanitizedFailureCause(cause); sanitized != "" {
		payload["cause"] = sanitized
		payload["cause_type"] = fmt.Sprintf("%T", cause)
	}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	if err := manager.Store.RecordFailure(ctx, id, phase, code, "deployd", hex.EncodeToString(digest[:]), payload, manager.Clock().UTC()); err != nil {
		// Preserve the transaction identity even when durable failure evidence
		// itself cannot be written. The caller must still have a recovery query
		// handle and must fail closed.
		return &TransactionError{ID: id, Phase: phase, Cause: fmt.Errorf("%w; record deployment failure: %v", cause, err)}
	}
	return &TransactionError{ID: id, Phase: phase, Cause: cause}
}

func sanitizedFailureCause(cause error) string {
	if cause == nil {
		return ""
	}
	value := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, cause.Error())
	value = strings.Join(strings.Fields(value), " ")
	fields := strings.Fields(value)
	for index, field := range fields {
		if strings.Contains(field, "/") || strings.Contains(field, `\`) {
			fields[index] = "<private-path>"
		}
	}
	value = strings.Join(fields, " ")
	const maxLength = 512
	if len(value) > maxLength {
		return value[:maxLength] + "..."
	}
	return value
}

func failureMessage(phase, code string) string {
	if code == "state_unverified" {
		return "deployment state could not be verified"
	}
	switch phase {
	case PhaseStage:
		return "deployment stage failed"
	case PhaseStandbyValidation:
		return "deployment verification failed"
	case PhaseControllerHandoff, PhaseTrafficEnable, PhaseActiveSwitch, PhaseDrainOld, PhaseQuiescence:
		return "deployment activation failed"
	default:
		return "deployment operation failed"
	}
}

func (manager *Manager) effectFailure(ctx context.Context, id, phase string, cause error) error {
	// A request timeout cannot cancel recording an uncertain runtime effect or
	// revoking its roles. Cleanup remains bounded and never resumes deployment.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	failureErr := manager.failure(ctx, id, phase, &UncertainError{Cause: cause})
	fenceErr := manager.fenceSlots(ctx, errors.New("deployment runtime effect failed; controller roles were fenced"))
	if fenceErr != nil {
		return fmt.Errorf("%w; %v", failureErr, fenceErr)
	}
	return failureErr
}

func (manager *Manager) transitionAfterEffect(ctx context.Context, id, expected, phase, transition string, evidence map[string]any) error {
	if _, err := manager.transition(ctx, id, expected, phase, transition, evidence); err == nil {
		return nil
	} else {
		return manager.effectFailure(ctx, id, expected, &UncertainError{Cause: fmt.Errorf("persist deployment phase %s: %w", phase, err)})
	}
}

func (manager *Manager) rollbackOrFence(ctx context.Context, transaction store.Transaction, cause error) error {
	if ctx.Err() != nil {
		return manager.effectFailure(ctx, transaction.ID, transaction.Phase, &UncertainError{Cause: cause})
	}

	if current, err := manager.Store.Get(ctx, transaction.ID); err != nil {
		fenceErr := manager.fenceSlots(ctx, errors.New("transaction state could not be read before fencing"))
		return fmt.Errorf("read transaction before failure: %w; %v", err, fenceErr)
	} else if current != nil {
		transaction = *current
	}
	if transaction.PreviousSlot == "" || transaction.PreviousSlot == "none" {
		failureErr := manager.failure(ctx, transaction.ID, transaction.Phase, cause)
		fenceErr := manager.fenceSlots(ctx, errors.New("deployment has no previous active slot; controller roles were fenced"))
		return fmt.Errorf("%w; %v", failureErr, fenceErr)
	}
	if errors.Is(cause, ErrUncertain) || errors.As(cause, new(*UncertainError)) {
		failureErr := manager.failure(ctx, transaction.ID, transaction.Phase, cause)
		fenceErr := manager.fenceSlots(ctx, errors.New("deployment outcome is uncertain; controller roles were fenced"))
		if fenceErr != nil {
			return fmt.Errorf("%w; %v", failureErr, fenceErr)
		}
		return failureErr
	}
	if transaction.RollbackUsed && transaction.Phase != PhaseRollback {
		return manager.failure(ctx, transaction.ID, transaction.Phase, cause)
	}
	if current, err := manager.Store.Get(ctx, transaction.ID); err != nil {
		return err
	} else if current != nil {
		transaction = *current
	}
	if err := manager.rollback(ctx, transaction); err != nil {
		return err
	}
	return cause
}

// recordExhaustedRollback closes a rollback whose replacement never started.
// The incumbent still serves with the only controller lease and the previous
// slot is provably stopped, so nothing is in doubt: the transaction ends as
// `failed`, which does not block later deployments, instead of as
// `needs_recovery` behind a fence and manual recovery. It returns nil once the
// terminal record is durable.
func (manager *Manager) recordExhaustedRollback(ctx context.Context, transaction store.Transaction, cause error, current RuntimeSnapshot) error {
	// PhaseRollback maps to state_unverified, so the plain failure record would
	// re-fence the host. Write the terminal state directly.
	if _, err := manager.Store.Transition(ctx, transaction.ID, PhaseRollback, PhaseRollback, "failed",
		"rollback_start_attempts_exhausted", "deployd", map[string]any{
			"active_slot":      current.ActiveSlot,
			"deployer_service": "deployd",
		}, manager.Clock().UTC()); err != nil {
		return fmt.Errorf("record exhausted rollback: %w", err)
	}
	payload := failurePayload(cause)
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	if err := manager.Store.RecordFailure(ctx, transaction.ID, PhaseRollback, "rollback_previous_unavailable",
		"deployd", hex.EncodeToString(digest[:]), payload, manager.Clock().UTC()); err != nil {
		return fmt.Errorf("record exhausted rollback cause: %w", err)
	}
	return &TransactionError{ID: transaction.ID, Phase: PhaseRollback, Cause: cause}
}

// failurePayload mirrors the message/uncertain shape used by failure().
func failurePayload(cause error) map[string]any {
	payload := map[string]any{"message": "previous version failed to start; current slot remains active", "uncertain": false}
	if sanitized := sanitizedFailureCause(cause); sanitized != "" {
		payload["cause"] = sanitized
		payload["cause_type"] = fmt.Sprintf("%T", cause)
	}
	return payload
}

// isRecordingProblem reports whether a helper result is a bookkeeping failure
// rather than the terminal transaction error it produced.
func isRecordingProblem(err error) bool {
	var transactionErr *TransactionError
	return err != nil && !errors.As(err, &transactionErr)
}

func (manager *Manager) rollback(ctx context.Context, transaction store.Transaction) error {
	if transaction.State == "failed" && transaction.RollbackUsed {
		return errors.New("rollback is already finished")
	}
	if transaction.PreviousSlot == "" || transaction.PreviousSlot == "none" {
		return errors.New("rollback requires a previous active slot")
	}
	previousReleaseID, previousSourceRevision, previousAuthority, previousCapabilities, identityErr := manager.verifiedPreviousIdentity(ctx, transaction.ID)
	if identityErr != nil {
		failureErr := manager.failure(ctx, transaction.ID, transaction.Phase, identityErr)
		fenceErr := manager.fenceSlots(ctx, errors.New("rollback target identity was not verified; controller roles were fenced"))
		return fmt.Errorf("%w; %v", failureErr, fenceErr)
	}
	if err := manager.Runtime.VerifySlotIdentity(ctx, transaction.PreviousSlot, previousReleaseID, previousSourceRevision, previousCapabilities); err != nil {
		failureErr := manager.failure(ctx, transaction.ID, transaction.Phase, err)
		fenceErr := manager.fenceSlots(ctx, errors.New("rollback target was not verified; controller roles were fenced"))
		return fmt.Errorf("%w; %v", failureErr, fenceErr)
	}
	lease, err := manager.newLease(ctx, transaction.PreviousSlot, transaction.Generation)
	if err != nil {
		return fmt.Errorf("allocate rollback controller lease: %w", err)
	}
	resuming := transaction.RollbackUsed && transaction.Phase == PhaseRollback
	if !resuming {
		if _, err := manager.Store.BeginRollback(ctx, transaction.ID, transaction.Phase, PhaseRollback, "rollback_started", "deployd", map[string]any{"bounded": true}, manager.Clock().UTC()); err != nil {
			fenceErr := manager.fenceSlots(ctx, errors.New("rollback state could not be persisted; controller roles were fenced"))
			return fmt.Errorf("persist rollback state: %w; %v", err, fenceErr)
		}
	}
	rollbackCtx := context.WithValue(ctx, rollbackStartKey{}, func() error { return manager.Store.ConsumeRollbackStart(ctx, transaction.ID) })
	rollbackCtx = context.WithValue(rollbackCtx, rollbackReadyKey{}, func(waitCtx context.Context, slot string) error {
		return manager.waitForReady(waitCtx, transaction.ID, slot)
	})
	if preparer, ok := manager.Runtime.(interface {
		PrepareRollback(context.Context, string, string, SlotObservation) error
	}); ok {
		observation, err := manager.observe(ctx, transaction.ID)
		if err != nil {
			return manager.effectFailure(ctx, transaction.ID, PhaseRollback, err)
		}
		active := observation.Slots[observation.ActiveSlot]
		err = manager.withControllerRenewal(rollbackCtx, active, func(preparation context.Context) error {
			return preparer.PrepareRollback(preparation, transaction.PreviousSlot, previousAuthority, active)
		})
		if err != nil {
			if errors.Is(err, errRollbackStartsExhausted) {
				current, observeErr := manager.observe(ctx, transaction.ID)
				incumbent := current.Slots[transaction.TargetSlot]
				standby := current.Slots[transaction.PreviousSlot]
				// A stopped slot has no counters to read, so CountersKnown is false
				// there. Judge on the absence of process and roles; fall back to the
				// runtime verifier when the observation still claims a process.
				stopped := !standby.ProcessUp && !standby.ControllerOwned && !standby.TrafficAccepting
				if verifier, ok := manager.Runtime.(interface {
					verifyFailedRollbackSlotStopped(context.Context, string) bool
				}); !stopped && ok {
					stopped = verifier.verifyFailedRollbackSlotStopped(ctx, transaction.PreviousSlot)
				}
				if observeErr == nil && validObservation(current, manager.Clock().UTC()) &&
					current.ActiveSlot == transaction.TargetSlot && current.ControllerSlot == transaction.TargetSlot &&
					incumbent.Ready && incumbent.ControllerOwned && incumbent.TrafficAccepting &&
					incumbent.StateAuthority == previousAuthority && stopped &&
					!standby.ProcessUp && !standby.ControllerOwned && !standby.TrafficAccepting {
					// The helper records the terminal state and returns the
					// transaction error; anything else is a recording problem.
					if recordingErr := manager.recordExhaustedRollback(ctx, transaction, err, current); !isRecordingProblem(recordingErr) {
						return recordingErr
					} else {
						err = errors.Join(err, recordingErr)
					}
				}
			}
			return manager.effectFailure(ctx, transaction.ID, PhaseRollback, err)
		}
	}
	if err := manager.Runtime.Rollback(rollbackCtx, transaction.TargetSlot, transaction.PreviousSlot, lease, previousAuthority); err != nil {
		failureErr := manager.failure(ctx, transaction.ID, PhaseRollback, err)
		fenceErr := manager.fenceSlots(ctx, errors.New("rollback did not reach a verified outcome"))
		return fmt.Errorf("%w; %v", failureErr, fenceErr)
	}
	if err := manager.waitForReady(ctx, transaction.ID, transaction.PreviousSlot); err != nil {
		failureErr := manager.failure(ctx, transaction.ID, PhaseRollback, &UncertainError{Cause: err})
		fenceErr := manager.fenceSlots(ctx, errors.New("rollback readiness was not verified; controller roles were fenced"))
		return fmt.Errorf("%w; %v", failureErr, fenceErr)
	}
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		if err := manager.Runtime.EnableTraffic(ctx, transaction.PreviousSlot); err != nil {
			failureErr := manager.failure(ctx, transaction.ID, PhaseRollback, err)
			fenceErr := manager.fenceSlots(ctx, errors.New("rollback traffic role could not be restored"))
			return fmt.Errorf("%w; %v", failureErr, fenceErr)
		}
	}
	observation, observeErr := manager.observe(ctx, transaction.ID)
	if observeErr != nil {
		failureErr := manager.failure(ctx, transaction.ID, PhaseRollback, observeErr)
		fenceErr := manager.fenceSlots(ctx, errors.New("rollback observation failed; controller roles were fenced"))
		return fmt.Errorf("%w; %v", failureErr, fenceErr)
	}
	previous, previousOK := observation.Slots[transaction.PreviousSlot]
	target, targetOK := observation.Slots[transaction.TargetSlot]
	targetStopped := targetOK && target.CountersKnown && !target.ProcessUp && !target.ProcessReady && !target.Ready && !target.ControllerOwned && !target.TrafficAccepting && target.NativeWorkers == 0 && target.PendingOperations == 0 && target.InFlightRequests == 0
	if !previousOK || !previous.CountersKnown || !targetStopped || !observation.Consistent || observation.ActiveSlot != transaction.PreviousSlot || !previous.ProcessUp || !previous.ProcessReady || !previous.Ready || !previous.ControllerOwned || !previous.TrafficAccepting || previous.StateEpoch == "" || previous.StateAuthority != previousAuthority || previous.NativeWorkers != 0 || previous.PendingOperations != 0 || previous.InFlightRequests != 0 {
		failureErr := manager.failure(ctx, transaction.ID, PhaseRollback, rollbackVerificationError(observation, transaction, previousOK, targetOK, targetStopped))
		fenceErr := manager.fenceSlots(ctx, errors.New("rollback verification failed; controller roles were fenced"))
		return fmt.Errorf("%w; %v", failureErr, fenceErr)
	}
	if err := manager.transitionAfterEffect(ctx, transaction.ID, PhaseRollback, PhaseFinalize, "rolled_back", map[string]any{"active_slot": transaction.PreviousSlot}); err != nil {
		return err
	}
	manager.startupFenced = false
	return nil
}

func rollbackVerificationError(observation RuntimeSnapshot, transaction store.Transaction, previousOK, targetOK, targetStopped bool) error {
	previous := observation.Slots[transaction.PreviousSlot]
	target := observation.Slots[transaction.TargetSlot]
	return fmt.Errorf("rollback outcome was not verified: active=%s consistent=%t previous_present=%t previous={process_up:%t process_ready:%t ready:%t controller_owned:%t traffic_accepting:%t counters_known:%t state_epoch:%t capabilities:%t workers:%d pending:%d in_flight:%d} target_present=%t target_stopped=%t target={process_up:%t process_ready:%t ready:%t controller_owned:%t traffic_accepting:%t counters_known:%t workers:%d pending:%d in_flight:%d}",
		observation.ActiveSlot,
		observation.Consistent,
		previousOK,
		previous.ProcessUp,
		previous.ProcessReady,
		previous.Ready,
		previous.ControllerOwned,
		previous.TrafficAccepting,
		previous.CountersKnown,
		previous.StateEpoch != "",
		len(previous.StateCapabilities) > 0,
		previous.NativeWorkers,
		previous.PendingOperations,
		previous.InFlightRequests,
		targetOK,
		targetStopped,
		target.ProcessUp,
		target.ProcessReady,
		target.Ready,
		target.ControllerOwned,
		target.TrafficAccepting,
		target.CountersKnown,
		target.NativeWorkers,
		target.PendingOperations,
		target.InFlightRequests,
	)
}

// Only a freshly observed lease held by this exact Deployd process can be
// renewed. Persisted snapshots and a previous process incarnation grant nothing.
func (manager *Manager) renewOrAllocateLease(ctx context.Context, target SlotObservation, generation int64, required *controllerrole.Lease) (controllerrole.Lease, error) {
	now := manager.Clock()
	if current := target.ControllerLease; target.LeaseRenewalSupported && target.ControllerOwned && current != nil &&
		current.Validate(now, target.Slot) == nil && current.OwnerPID == os.Getpid() &&
		current.OwnerStartTime != "" && current.OwnerStartTime == controllerrole.ProcessStartToken(os.Getpid()) {
		if required != nil {
			identity := *required
			identity.Expiry = current.Expiry
			if required.Validate(now, target.Slot) != nil || identity != *current {
				return controllerrole.Lease{}, errors.New("controller lease expired or changed during observation retry")
			}
		}
		renewed := *current
		if expiry := now.Add(manager.LeaseDuration); expiry.After(renewed.Expiry) {
			renewed.Expiry = expiry
		}
		return renewed, nil
	}
	if required != nil {
		return controllerrole.Lease{}, errors.New("controller lease was lost during observation retry")
	}
	return manager.newLease(ctx, target.Slot, generation)
}

func (manager *Manager) newLease(ctx context.Context, slot string, generation int64) (controllerrole.Lease, error) {
	term, err := manager.nextFenceTerm(ctx)
	if err != nil {
		return controllerrole.Lease{}, err
	}
	return controllerrole.Lease{Slot: slot, Term: term, LeaseID: newID("lease"), Expiry: manager.Clock().Add(manager.LeaseDuration), Generation: generation, Nonce: newID("nonce"), OwnerPID: os.Getpid(), OwnerStartTime: controllerrole.ProcessStartToken(os.Getpid())}, nil
}

func (manager *Manager) nextFenceTerm(ctx context.Context) (uint64, error) {
	return manager.Store.NextFenceTerm(ctx)
}

func recoveryTargetVerified(observation RuntimeSnapshot, transaction store.Transaction, now time.Time, requireTraffic, requireController, requirePreviousStopped bool, releaseID, sourceRevision, stateAuthority string, capabilities []string) bool {
	if !validObservation(observation, now) || observation.ActiveSlot != transaction.TargetSlot {
		return false
	}
	target, ok := observation.Slots[transaction.TargetSlot]
	if !ok || !target.ProcessUp || !target.ProcessReady || ((requireTraffic || requireController) && !target.Ready) || target.StateEpoch == "" || target.StateAuthority != stateAuthority || target.ReleaseID != releaseID || target.SourceRevision != sourceRevision || (requireTraffic && !target.TrafficAccepting) || (requireController && !target.ControllerOwned) {
		return false
	}
	old, oldOK := observation.Slots[transaction.PreviousSlot]
	previousSlotRequired := transaction.PreviousSlot != "" && transaction.PreviousSlot != "none"
	if previousSlotRequired && (!oldOK || !old.CountersKnown) {
		return false
	}
	if !previousSlotRequired {
		return true
	}
	if old.TrafficAccepting || old.InFlightRequests != 0 || old.NativeWorkers != 0 || old.PendingOperations != 0 {
		return false
	}
	return !requirePreviousStopped || (!old.ProcessUp && !old.ControllerOwned)
}

func evidenceStrings(value any) ([]string, bool) {
	switch typed := value.(type) {
	case []string:
		return append([]string(nil), typed...), true
	case []any:
		result := make([]string, len(typed))
		for index, item := range typed {
			value, ok := item.(string)
			if !ok || value == "" {
				return nil, false
			}
			result[index] = value
		}
		return result, true
	default:
		return nil, false
	}
}

func validObservation(observation RuntimeSnapshot, now time.Time) bool {
	if !observation.Consistent || observation.ObservedAt.IsZero() || observation.FreshUntil.IsZero() || !observation.FreshUntil.After(now) || len(observation.Sources) == 0 {
		return false
	}
	for _, source := range observation.Sources {
		if !source.Up || source.ObservedAt.IsZero() || source.ObservedAt.After(now) || source.FreshUntil.IsZero() || !source.FreshUntil.After(now) || source.ObservedAt.Before(observation.ObservedAt.Add(-2*time.Second)) {
			return false
		}
	}
	return true
}

func validControllerRecoveryObservation(observation RuntimeSnapshot, now time.Time) bool {
	if !observation.ControllerRecoveryOnly || observation.Consistent || observation.ObservedAt.IsZero() || len(observation.Sources) == 0 {
		return false
	}
	if _, ok := observation.Sources["inventory_observation"]; !ok {
		return false
	}
	for name, source := range observation.Sources {
		if name == "inventory_observation" {
			continue
		}
		if !source.Up || source.ObservedAt.IsZero() || source.ObservedAt.After(now) || source.FreshUntil.IsZero() || !source.FreshUntil.After(now) || source.ObservedAt.Before(observation.ObservedAt.Add(-2*time.Second)) {
			return false
		}
	}
	return true
}

func validateStageObservation(observation RuntimeSnapshot, targetSlot string, now time.Time) error {
	if !validObservation(observation, now) {
		return errors.New("active slot observation is not valid for staging")
	}
	if observation.ActiveSlot == targetSlot {
		return fmt.Errorf("target slot %s is already active", targetSlot)
	}
	if observation.ActiveSlot == "" || observation.ActiveSlot == "none" {
		return errors.New("active slot was not observed")
	}
	active, activeOK := observation.Slots[observation.ActiveSlot]
	target, targetOK := observation.Slots[targetSlot]
	if !activeOK {
		return errors.New("active slot was not observed")
	}
	// Staging only replaces the inactive slot. An operator may repair a drained
	// or controller-less active slot, so require quiescence instead of a healthy
	// role: activation still proves readiness before any traffic effect.
	if active.ProcessUp {
		// Pending operations are allowed: the candidate must reconcile them
		// before activation, so staging may proceed while they are drained.
		if !active.CountersKnown || active.InFlightRequests != 0 || active.NativeWorkers != 0 {
			return errors.New("active slot is not idle for staging")
		}
		serving := active.ControllerOwned && active.ProcessReady && active.Ready && active.TrafficAccepting && !active.Draining
		draining := active.Draining && !active.TrafficAccepting
		fenced := !active.ControllerOwned && !active.TrafficAccepting
		if !serving && !draining && !fenced {
			return errors.New("active slot is neither serving nor idle for staging")
		}
	}
	if !targetOK {
		return fmt.Errorf("target slot %s was not observed", targetSlot)
	}
	if target.ControllerOwned || target.TrafficAccepting {
		return fmt.Errorf("target slot %s still owns controller or traffic admission", targetSlot)
	}
	return nil
}

func otherSlot(slot string) string {
	if slot == "blue" {
		return "green"
	}
	return "blue"
}

func stateForPhase(phase string) string {
	if phase == PhaseFinalize {
		return "finalized"
	}
	if phase == PhaseRollback {
		return "active"
	}
	return "active"
}

func validSlot(value string) error {
	if value != "blue" && value != "green" {
		return fmt.Errorf("slot must be blue or green")
	}
	return nil
}

func errOrNotFound(err error, id string, transaction *store.Transaction) error {
	if err != nil {
		return err
	}
	if transaction == nil {
		return fmt.Errorf("transaction %s was not found", id)
	}
	return nil
}

func newID(prefix string) string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "-" + hex.EncodeToString(value)
}
