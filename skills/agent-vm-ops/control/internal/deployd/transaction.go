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
	"sync"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
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

type UncertainError struct{ Cause error }

func (err *UncertainError) Error() string {
	if err == nil || err.Cause == nil {
		return ErrUncertain.Error()
	}
	return ErrUncertain.Error() + ": " + err.Cause.Error()
}

func (err *UncertainError) Unwrap() error { return err.Cause }

func (err *UncertainError) Is(target error) bool { return target == ErrUncertain }

type SlotObservation struct {
	Slot              string            `json:"slot"`
	ProcessUp         bool              `json:"process_up"`
	ProcessReady      bool              `json:"process_ready"`
	Ready             bool              `json:"ready"`
	ControllerOwned   bool              `json:"controller_owned"`
	TrafficAccepting  bool              `json:"traffic_accepting"`
	InFlightRequests  int64             `json:"in_flight_requests"`
	NativeWorkers     int64             `json:"native_workers"`
	PendingOperations int64             `json:"pending_operations"`
	ObservedUnix      int64             `json:"observed_at_seconds"`
	StateEpoch        string            `json:"state_epoch,omitempty"`
	ReleaseID         string            `json:"release_id,omitempty"`
	SourceRevision    string            `json:"source_revision,omitempty"`
	StateCapabilities []string          `json:"state_capabilities,omitempty"`
	MetricFamilies    map[string]string `json:"metric_families,omitempty"`
}

type RuntimeSnapshot struct {
	ObservedAt     time.Time                    `json:"observed_at"`
	FreshUntil     time.Time                    `json:"fresh_until"`
	ActiveSlot     string                       `json:"active_slot"`
	ControllerSlot string                       `json:"controller_slot"`
	Consistent     bool                         `json:"consistent"`
	Slots          map[string]SlotObservation   `json:"slots"`
	Sources        map[string]SourceObservation `json:"sources,omitempty"`
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
	GrantController(context.Context, string, controllerrole.Lease) error
	RevokeController(context.Context, string, uint64) error
	EnableTraffic(context.Context, string) error
	DisableTraffic(context.Context, string) error
	SwitchActive(context.Context, string) error
	Drain(context.Context, string) error
	Quiesce(context.Context, string) error
	Stop(context.Context, string) error
	Rollback(context.Context, string, string, controllerrole.Lease) error
}

type Status struct {
	Observed     RuntimeSnapshot     `json:"observation"`
	Transactions []store.Transaction `json:"transactions"`
}

type Manager struct {
	Store         *store.Store
	Runtime       Runtime
	Clock         func() time.Time
	LeaseDuration time.Duration

	mu            sync.Mutex
	startupFenced bool
}

func NewManager(database *store.Store, runtime Runtime, leaseDuration time.Duration, clock func() time.Time) *Manager {
	if clock == nil {
		clock = time.Now
	}
	if leaseDuration <= 0 {
		leaseDuration = 15 * time.Second
	}
	return &Manager{Store: database, Runtime: runtime, Clock: clock, LeaseDuration: leaseDuration}
}

func (manager *Manager) Status(ctx context.Context) (Status, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	observation, err := manager.observe(ctx, "")
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
	return map[string]any{"transaction": transaction, "observation": observation}, nil
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
	if observation.ActiveSlot == targetSlot {
		return nil, fmt.Errorf("target slot %s is already active", targetSlot)
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
	if observation.ActiveSlot == targetSlot {
		return nil, fmt.Errorf("target slot %s is already active", targetSlot)
	}
	now := manager.Clock().UTC()
	transaction := store.Transaction{ID: newID("tx"), TargetSlot: targetSlot, PreviousSlot: observation.ActiveSlot, Bundle: bundle, Phase: PhaseStage, State: "active", Generation: now.UnixNano(), CreatedAt: now, UpdatedAt: now}
	if err := manager.Store.Create(ctx, transaction, map[string]any{"active_slot": observation.ActiveSlot}); err != nil {
		return nil, err
	}
	if err := manager.Runtime.Stage(ctx, targetSlot, bundle); err != nil {
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
	if err := manager.Runtime.Verify(ctx, transaction.TargetSlot, transaction.Bundle); err != nil {
		return nil, manager.failure(ctx, id, PhaseStandbyValidation, err)
	}
	observation, err := manager.observe(ctx, id)
	if err != nil {
		return nil, manager.failure(ctx, id, PhaseStandbyValidation, err)
	}
	target, ok := observation.Slots[transaction.TargetSlot]
	if !validObservation(observation, manager.Clock().UTC()) || !ok || !target.ProcessUp || !target.ProcessReady || !target.Ready || target.TrafficAccepting || target.ReleaseID == "" || target.SourceRevision == "" || target.StateEpoch == "" || len(target.StateCapabilities) == 0 {
		return nil, manager.failure(ctx, id, PhaseStandbyValidation, errors.New("candidate slot is not process-ready"))
	}
	previous, previousOK := observation.Slots[transaction.PreviousSlot]
	if previousOK && previous.StateEpoch != "" && previous.StateEpoch != target.StateEpoch {
		return nil, manager.failure(ctx, id, PhaseStandbyValidation, errors.New("candidate state epoch is incompatible with the active slot"))
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseStandbyValidation, PhaseControllerHandoff, "verified", map[string]any{"candidate_ready": true, "release_id": target.ReleaseID, "source_revision": target.SourceRevision, "state_epoch": target.StateEpoch}); err != nil {
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
	observation, err := manager.observe(ctx, id)
	if err != nil {
		return nil, manager.failure(ctx, id, transaction.Phase, err)
	}
	if !validObservation(observation, manager.Clock().UTC()) || observation.ActiveSlot != transaction.PreviousSlot {
		return nil, manager.failure(ctx, id, transaction.Phase, errors.New("active slot changed outside deployd"))
	}
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		term, termErr := manager.nextFenceTerm(ctx)
		if termErr != nil {
			return nil, fmt.Errorf("allocate controller fencing term: %w", termErr)
		}
		if err := manager.Runtime.RevokeController(ctx, transaction.PreviousSlot, term); err != nil {
			return nil, manager.effectFailure(ctx, id, PhaseControllerHandoff, err)
		}
	}
	lease, err := manager.newLease(ctx, transaction.TargetSlot, transaction.Generation)
	if err != nil {
		return nil, fmt.Errorf("allocate candidate controller lease: %w", err)
	}
	if err := manager.Runtime.GrantController(ctx, transaction.TargetSlot, lease); err != nil {
		return nil, manager.effectFailure(ctx, id, PhaseControllerHandoff, err)
	}
	observation, err = manager.observe(ctx, id)
	if err != nil {
		return nil, manager.rollbackOrFence(ctx, *transaction, err)
	}
	target, ok := observation.Slots[transaction.TargetSlot]
	if !ok || !target.ProcessUp || !target.ProcessReady || !target.Ready || !target.ControllerOwned || target.ReleaseID == "" || target.SourceRevision == "" || target.StateEpoch == "" {
		return nil, manager.rollbackOrFence(ctx, *transaction, errors.New("candidate controller lease was not observed"))
	}
	if err := manager.waitForReady(ctx, id, transaction.TargetSlot); err != nil {
		return nil, manager.rollbackOrFence(ctx, *transaction, err)
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseControllerHandoff, PhaseTrafficEnable, "controller_granted", map[string]any{"lease_id": lease.LeaseID, "term": lease.Term}); err != nil {
		return nil, err
	}
	if err := manager.Runtime.EnableTraffic(ctx, transaction.TargetSlot); err != nil {
		return nil, manager.effectFailure(ctx, id, PhaseTrafficEnable, err)
	}
	observation, err = manager.observe(ctx, id)
	if err != nil {
		return nil, manager.effectFailure(ctx, id, PhaseTrafficEnable, err)
	}
	target, ok = observation.Slots[transaction.TargetSlot]
	if !validObservation(observation, manager.Clock().UTC()) || !ok || !target.ControllerOwned || !target.TrafficAccepting {
		return nil, manager.effectFailure(ctx, id, PhaseTrafficEnable, errors.New("candidate traffic role was not observed"))
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseTrafficEnable, PhaseActiveSwitch, "traffic_enabled", nil); err != nil {
		return nil, err
	}
	if err := manager.Runtime.SwitchActive(ctx, transaction.TargetSlot); err != nil {
		return nil, manager.rollbackOrFence(ctx, *transaction, err)
	}
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		if err := manager.Runtime.DisableTraffic(ctx, transaction.PreviousSlot); err != nil {
			return nil, manager.effectFailure(ctx, id, PhaseActiveSwitch, err)
		}
	}
	observation, err = manager.observe(ctx, id)
	if err != nil {
		return nil, manager.rollbackOrFence(ctx, *transaction, err)
	}
	target, ok = observation.Slots[transaction.TargetSlot]
	if !validObservation(observation, manager.Clock().UTC()) || !ok || observation.ActiveSlot != transaction.TargetSlot || !target.ProcessUp || !target.ProcessReady || !target.Ready || !target.ControllerOwned || !target.TrafficAccepting {
		return nil, manager.rollbackOrFence(ctx, *transaction, errors.New("active switch was not verified"))
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseActiveSwitch, PhaseDrainOld, "active_switched", map[string]any{"active_slot": transaction.TargetSlot}); err != nil {
		return nil, err
	}
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		if err := manager.Runtime.Drain(ctx, transaction.PreviousSlot); err != nil {
			return nil, manager.effectFailure(ctx, id, PhaseDrainOld, err)
		}
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseDrainOld, PhaseQuiescence, "drain_started", nil); err != nil {
		return nil, err
	}
	if err := manager.Runtime.Quiesce(ctx, transaction.PreviousSlot); err != nil {
		return nil, manager.effectFailure(ctx, id, PhaseQuiescence, err)
	}
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		if err := manager.Runtime.Stop(ctx, transaction.PreviousSlot); err != nil {
			return nil, manager.effectFailure(ctx, id, PhaseQuiescence, err)
		}
	}
	observation, err = manager.observe(ctx, id)
	if err != nil {
		return nil, manager.rollbackOrFence(ctx, *transaction, err)
	}
	target, ok = observation.Slots[transaction.TargetSlot]
	old, oldOK := observation.Slots[transaction.PreviousSlot]
	if !validObservation(observation, manager.Clock().UTC()) || !ok || observation.ActiveSlot != transaction.TargetSlot || !target.ProcessUp || !target.ProcessReady || !target.Ready || !target.ControllerOwned || !target.TrafficAccepting || (oldOK && (old.ProcessUp || old.TrafficAccepting || old.InFlightRequests != 0 || old.NativeWorkers != 0 || old.PendingOperations != 0)) {
		return nil, manager.rollbackOrFence(ctx, *transaction, errors.New("final active slot was not verified"))
	}
	if err := manager.transitionAfterEffect(ctx, id, PhaseQuiescence, PhaseFinalize, "finalized", map[string]any{"quiescent": true}); err != nil {
		return nil, err
	}
	updated, err := manager.Store.Get(ctx, id)
	return updated, err
}

func (manager *Manager) Rollback(ctx context.Context, id string) (*store.Transaction, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	transaction, err := manager.Store.Get(ctx, id)
	if err != nil || transaction == nil {
		return transaction, errOrNotFound(err, id, transaction)
	}
	if transaction.RollbackUsed {
		return nil, errors.New("transaction rollback is already used")
	}
	if err := manager.rollback(ctx, *transaction); err != nil {
		return nil, err
	}
	return manager.Store.Get(ctx, id)
}

func (manager *Manager) Recover(ctx context.Context, id, action string) (*store.Transaction, error) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	transaction, err := manager.Store.Get(ctx, id)
	if err != nil || transaction == nil {
		return transaction, errOrNotFound(err, id, transaction)
	}
	if !transaction.RecoveryRequired && transaction.State != "needs_recovery" {
		return nil, errors.New("transaction does not require recovery")
	}
	switch action {
	case "rollback":
		if transaction.RollbackUsed {
			return nil, errors.New("transaction rollback is already used")
		}
		if err := manager.rollback(ctx, *transaction); err != nil {
			return nil, err
		}
		return manager.Store.Get(ctx, id)
	case "finalize":
		observation, observeErr := manager.observe(ctx, id)
		if observeErr != nil || !recoveryTargetVerified(observation, *transaction, manager.Clock().UTC(), false) {
			return nil, errors.New("recovery evidence is not sufficient to finalize")
		}
		term, termErr := manager.nextFenceTerm(ctx)
		if termErr != nil {
			return nil, fmt.Errorf("allocate recovery fencing term: %w", termErr)
		}
		if err := manager.Runtime.RevokeController(ctx, otherSlot(transaction.TargetSlot), term); err != nil {
			return nil, fmt.Errorf("fence the non-selected slot before recovery finalize: %w", err)
		}
		lease, err := manager.newLease(ctx, transaction.TargetSlot, transaction.Generation)
		if err != nil {
			return nil, fmt.Errorf("allocate recovery controller lease: %w", err)
		}
		if err := manager.Runtime.GrantController(ctx, transaction.TargetSlot, lease); err != nil {
			return nil, fmt.Errorf("regrant the selected controller before recovery finalize: %w", err)
		}
		observation, observeErr = manager.observe(ctx, id)
		if observeErr != nil || !recoveryTargetVerified(observation, *transaction, manager.Clock().UTC(), true) {
			failureErr := manager.failure(ctx, id, transaction.Phase, &UncertainError{Cause: errors.New("recovery finalize controller evidence was not verified")})
			fenceErr := manager.fenceSlots(ctx, errors.New("recovery finalize was fenced"))
			return nil, fmt.Errorf("%w; %v", failureErr, fenceErr)
		}
		updated, err := manager.transition(ctx, id, transaction.Phase, PhaseFinalize, "recovered_finalize", map[string]any{"verified": true, "active_slot": transaction.TargetSlot, "lease_id": lease.LeaseID})
		if err != nil {
			fenceErr := manager.fenceSlots(ctx, errors.New("recovery finalize persistence failed; controller roles were fenced"))
			return nil, fmt.Errorf("%w; %v", err, fenceErr)
		}
		manager.startupFenced = false
		return updated, nil
	default:
		return nil, errors.New("recovery action must be rollback or finalize")
	}
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
	deadline := time.NewTimer(15 * time.Second)
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

// MaintainController renews the active slot lease only after a fresh,
// consistent observation. Persisted transactions never authorize a role
// after a Deployd restart; an active recovery transaction fences both slots.
func (manager *Manager) MaintainController(ctx context.Context) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.startupFenced {
		return manager.fenceSlots(ctx, errors.New("deployd startup recovery is unresolved"))
	}
	observation, err := manager.observe(ctx, "")
	if err != nil {
		return err
	}
	transactions, err := manager.Store.ListActive(ctx)
	if err != nil {
		return err
	}
	if len(transactions) != 0 {
		return manager.fenceSlots(ctx, errors.New("deployment transaction is active"))
	}
	if !observation.Consistent {
		return manager.fenceSlots(ctx, errors.New("slot observation is inconsistent"))
	}
	target, ok := observation.Slots[observation.ActiveSlot]
	if !ok || !target.ProcessUp || !target.ProcessReady {
		return manager.fenceSlots(ctx, errors.New("active slot is not process-ready"))
	}
	other := "blue"
	if observation.ActiveSlot == "blue" {
		other = "green"
	}
	if inactive, inactiveOK := observation.Slots[other]; inactiveOK && inactive.ProcessUp {
		if err := manager.Runtime.DisableTraffic(ctx, other); err != nil {
			return fmt.Errorf("disable inactive traffic role: %w", err)
		}
		term, termErr := manager.nextFenceTerm(ctx)
		if termErr != nil {
			return fmt.Errorf("allocate inactive fencing term: %w", termErr)
		}
		if err := manager.Runtime.RevokeController(ctx, other, term); err != nil {
			return fmt.Errorf("fence inactive controller: %w", err)
		}
	}
	generation := int64(1)
	for _, transaction := range transactions {
		if transaction.TargetSlot == observation.ActiveSlot || transaction.PreviousSlot == observation.ActiveSlot {
			generation = transaction.Generation
			break
		}
	}
	lease, err := manager.newLease(ctx, observation.ActiveSlot, generation)
	if err != nil {
		return fmt.Errorf("allocate active controller lease: %w", err)
	}
	if err := manager.Runtime.GrantController(ctx, observation.ActiveSlot, lease); err != nil {
		return fmt.Errorf("grant active controller: %w", err)
	}
	if err := manager.Runtime.EnableTraffic(ctx, observation.ActiveSlot); err != nil {
		disableErr := manager.Runtime.DisableTraffic(ctx, observation.ActiveSlot)
		fenceErr := manager.fenceSlots(ctx, errors.New("active traffic role could not be established"))
		if disableErr != nil {
			return fmt.Errorf("enable active traffic: %w; disable after failure: %v; %v", err, disableErr, fenceErr)
		}
		return fmt.Errorf("enable active traffic: %w; %v", err, fenceErr)
	}
	return nil
}

func (manager *Manager) fenceSlots(ctx context.Context, cause error) error {
	var first error
	for _, slot := range []string{"blue", "green"} {
		if err := manager.Runtime.DisableTraffic(ctx, slot); err != nil && first == nil {
			first = err
		}
	}
	fenceTerm, termErr := manager.nextFenceTerm(ctx)
	if termErr != nil && first == nil {
		first = termErr
	}
	for _, slot := range []string{"blue", "green"} {
		if termErr != nil {
			break
		}
		if err := manager.Runtime.RevokeController(ctx, slot, fenceTerm); err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return fmt.Errorf("%w; controller fencing failed: %v", cause, first)
	}
	return cause
}

func (manager *Manager) observe(ctx context.Context, transactionID string) (RuntimeSnapshot, error) {
	observation, err := manager.Runtime.Observe(ctx)
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = manager.Clock().UTC()
	}
	if observation.FreshUntil.IsZero() {
		observation.FreshUntil = observation.ObservedAt.Add(10 * time.Second)
	}
	if observation.Slots == nil {
		observation.Slots = map[string]SlotObservation{}
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

func (manager *Manager) failure(ctx context.Context, id, phase string, cause error) error {
	code := "deployment_failed"
	if phase == PhaseRollback || errors.Is(cause, ErrUncertain) {
		code = "state_unverified"
	}
	payload := map[string]any{"message": cause.Error(), "uncertain": code == "state_unverified"}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	if err := manager.Store.RecordFailure(ctx, id, phase, code, "deployd", hex.EncodeToString(digest[:]), payload, manager.Clock().UTC()); err != nil {
		return fmt.Errorf("%w; record deployment failure: %v", cause, err)
	}
	return cause
}

func (manager *Manager) effectFailure(ctx context.Context, id, phase string, cause error) error {
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
	if current, err := manager.Store.Get(ctx, transaction.ID); err != nil {
		fenceErr := manager.fenceSlots(ctx, errors.New("transaction state could not be read before fencing"))
		return fmt.Errorf("read transaction before failure: %w; %v", err, fenceErr)
	} else if current != nil {
		transaction = *current
	}
	if errors.Is(cause, ErrUncertain) || errors.As(cause, new(*UncertainError)) {
		failureErr := manager.failure(ctx, transaction.ID, transaction.Phase, cause)
		fenceErr := manager.fenceSlots(ctx, errors.New("deployment outcome is uncertain; controller roles were fenced"))
		if fenceErr != nil {
			return fmt.Errorf("%w; %v", failureErr, fenceErr)
		}
		return failureErr
	}
	if transaction.RollbackUsed {
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

func (manager *Manager) rollback(ctx context.Context, transaction store.Transaction) error {
	term, termErr := manager.nextFenceTerm(ctx)
	if termErr != nil {
		return fmt.Errorf("allocate rollback fencing term: %w", termErr)
	}
	lease := controllerrole.Lease{Slot: transaction.TargetSlot, Term: term}
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		lease, termErr = manager.newLease(ctx, transaction.PreviousSlot, transaction.Generation)
		if termErr != nil {
			return fmt.Errorf("allocate rollback controller lease: %w", termErr)
		}
	}
	if _, err := manager.Store.BeginRollback(ctx, transaction.ID, transaction.Phase, PhaseRollback, "rollback_started", "deployd", map[string]any{"bounded": true}, manager.Clock().UTC()); err != nil {
		return err
	}
	if err := manager.Runtime.Rollback(ctx, transaction.TargetSlot, transaction.PreviousSlot, lease); err != nil {
		failureErr := manager.failure(ctx, transaction.ID, PhaseRollback, err)
		fenceErr := manager.fenceSlots(ctx, errors.New("rollback did not reach a verified outcome"))
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
	if !previousOK || !observation.Consistent || observation.ActiveSlot != transaction.PreviousSlot || !previous.ProcessUp || !previous.ProcessReady || !previous.Ready || !previous.ControllerOwned || !previous.TrafficAccepting || previous.NativeWorkers != 0 || previous.PendingOperations != 0 || previous.InFlightRequests != 0 {
		failureErr := manager.failure(ctx, transaction.ID, PhaseRollback, errors.New("rollback outcome was not verified"))
		fenceErr := manager.fenceSlots(ctx, errors.New("rollback verification failed; controller roles were fenced"))
		return fmt.Errorf("%w; %v", failureErr, fenceErr)
	}
	if err := manager.transitionAfterEffect(ctx, transaction.ID, PhaseRollback, PhaseFinalize, "rolled_back", map[string]any{"active_slot": transaction.PreviousSlot}); err != nil {
		return err
	}
	manager.startupFenced = false
	return nil
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

func recoveryTargetVerified(observation RuntimeSnapshot, transaction store.Transaction, now time.Time, requireController bool) bool {
	if !validObservation(observation, now) || observation.ActiveSlot != transaction.TargetSlot {
		return false
	}
	target, ok := observation.Slots[transaction.TargetSlot]
	if !ok || !target.ProcessUp || !target.ProcessReady || !target.Ready || !target.TrafficAccepting || target.StateEpoch == "" || (requireController && !target.ControllerOwned) {
		return false
	}
	old, oldOK := observation.Slots[transaction.PreviousSlot]
	return !oldOK || (!old.TrafficAccepting && old.InFlightRequests == 0 && old.NativeWorkers == 0 && old.PendingOperations == 0)
}

func validObservation(observation RuntimeSnapshot, now time.Time) bool {
	if !observation.Consistent || observation.ObservedAt.IsZero() || observation.FreshUntil.IsZero() || !observation.FreshUntil.After(now) || len(observation.Sources) == 0 {
		return false
	}
	for _, source := range observation.Sources {
		freshUntil := source.FreshUntil
		if freshUntil.IsZero() {
			freshUntil = source.ObservedAt.Add(10 * time.Second)
		}
		if !source.Up || source.ObservedAt.IsZero() || source.ObservedAt.After(now) || !freshUntil.After(now) || source.ObservedAt.Before(observation.ObservedAt.Add(-2*time.Second)) {
			return false
		}
	}
	return true
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
