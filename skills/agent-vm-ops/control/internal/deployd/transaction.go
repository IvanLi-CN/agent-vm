package deployd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	MetricFamilies    map[string]string `json:"-"`
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
	Error      string    `json:"error,omitempty"`
}

type Runtime interface {
	Observe(context.Context) (RuntimeSnapshot, error)
	Stage(context.Context, string, string) error
	Verify(context.Context, string, string) error
	GrantController(context.Context, string, controllerrole.Lease) error
	RevokeController(context.Context, string) error
	EnableTraffic(context.Context, string) error
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

	mu   sync.Mutex
	term uint64
}

func NewManager(database *store.Store, runtime Runtime, leaseDuration time.Duration, clock func() time.Time) *Manager {
	if clock == nil {
		clock = time.Now
	}
	if leaseDuration <= 0 {
		leaseDuration = 15 * time.Second
	}
	term := uint64(clock().UnixNano())
	if term == 0 {
		term = 1
	}
	return &Manager{Store: database, Runtime: runtime, Clock: clock, LeaseDuration: leaseDuration, term: term}
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
	updated, err := manager.transition(ctx, transaction.ID, PhaseStage, PhaseStandbyValidation, "staged", map[string]any{"target_slot": targetSlot})
	if err != nil {
		return nil, err
	}
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
	if !ok || !target.ProcessUp || !target.ProcessReady {
		return nil, manager.failure(ctx, id, PhaseStandbyValidation, errors.New("candidate slot is not process-ready"))
	}
	updated, err := manager.transition(ctx, id, PhaseStandbyValidation, PhaseControllerHandoff, "verified", map[string]any{"candidate_ready": true})
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
	if observation.ActiveSlot != transaction.PreviousSlot {
		return nil, manager.failure(ctx, id, transaction.Phase, errors.New("active slot changed outside deployd"))
	}
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		if err := manager.Runtime.RevokeController(ctx, transaction.PreviousSlot); err != nil {
			return nil, manager.failure(ctx, id, PhaseControllerHandoff, err)
		}
	}
	lease := manager.newLease(transaction.TargetSlot, transaction.Generation)
	if err := manager.Runtime.GrantController(ctx, transaction.TargetSlot, lease); err != nil {
		return nil, manager.failure(ctx, id, PhaseControllerHandoff, err)
	}
	observation, err = manager.observe(ctx, id)
	if err != nil {
		return nil, manager.rollbackOrFence(ctx, *transaction, err)
	}
	target, ok := observation.Slots[transaction.TargetSlot]
	if !ok || !target.ProcessUp || !target.ProcessReady || !target.ControllerOwned {
		return nil, manager.rollbackOrFence(ctx, *transaction, errors.New("candidate controller lease was not observed"))
	}
	if err := manager.waitForReady(ctx, id, transaction.TargetSlot); err != nil {
		return nil, manager.rollbackOrFence(ctx, *transaction, err)
	}
	if _, err := manager.transition(ctx, id, PhaseControllerHandoff, PhaseTrafficEnable, "controller_granted", map[string]any{"lease_id": lease.LeaseID, "term": lease.Term}); err != nil {
		return nil, err
	}
	if err := manager.Runtime.EnableTraffic(ctx, transaction.TargetSlot); err != nil {
		return nil, manager.rollbackOrFence(ctx, *transaction, err)
	}
	if _, err := manager.transition(ctx, id, PhaseTrafficEnable, PhaseActiveSwitch, "traffic_enabled", nil); err != nil {
		return nil, err
	}
	if err := manager.Runtime.SwitchActive(ctx, transaction.TargetSlot); err != nil {
		return nil, manager.rollbackOrFence(ctx, *transaction, err)
	}
	observation, err = manager.observe(ctx, id)
	if err != nil {
		return nil, manager.rollbackOrFence(ctx, *transaction, err)
	}
	target, ok = observation.Slots[transaction.TargetSlot]
	if !ok || observation.ActiveSlot != transaction.TargetSlot || !target.ProcessUp || !target.ProcessReady || !target.Ready || !target.ControllerOwned {
		return nil, manager.rollbackOrFence(ctx, *transaction, errors.New("active switch was not verified"))
	}
	if _, err := manager.transition(ctx, id, PhaseActiveSwitch, PhaseDrainOld, "active_switched", map[string]any{"active_slot": transaction.TargetSlot}); err != nil {
		return nil, err
	}
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		if err := manager.Runtime.Drain(ctx, transaction.PreviousSlot); err != nil {
			return nil, manager.rollbackOrFence(ctx, *transaction, err)
		}
	}
	if _, err := manager.transition(ctx, id, PhaseDrainOld, PhaseQuiescence, "drain_started", nil); err != nil {
		return nil, err
	}
	if err := manager.Runtime.Quiesce(ctx, transaction.PreviousSlot); err != nil {
		return nil, manager.rollbackOrFence(ctx, *transaction, err)
	}
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		if err := manager.Runtime.Stop(ctx, transaction.PreviousSlot); err != nil {
			return nil, manager.rollbackOrFence(ctx, *transaction, err)
		}
	}
	observation, err = manager.observe(ctx, id)
	if err != nil {
		return nil, manager.rollbackOrFence(ctx, *transaction, err)
	}
	target, ok = observation.Slots[transaction.TargetSlot]
	if !ok || observation.ActiveSlot != transaction.TargetSlot || !target.ProcessUp || !target.ProcessReady || !target.Ready || !target.ControllerOwned {
		return nil, manager.rollbackOrFence(ctx, *transaction, errors.New("final active slot was not verified"))
	}
	updated, err := manager.transition(ctx, id, PhaseQuiescence, PhaseFinalize, "finalized", map[string]any{"quiescent": true})
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
		if err := manager.Runtime.RevokeController(ctx, otherSlot(transaction.TargetSlot)); err != nil {
			return nil, fmt.Errorf("fence the non-selected slot before recovery finalize: %w", err)
		}
		lease := manager.newLease(transaction.TargetSlot, transaction.Generation)
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
	observation, err := manager.observe(ctx, "")
	if err != nil {
		return err
	}
	transactions, err := manager.Store.ListActive(ctx)
	if err != nil {
		return err
	}
	for _, transaction := range transactions {
		if transaction.RecoveryRequired || transaction.State == "needs_recovery" {
			return manager.fenceSlots(ctx, errors.New("deployment recovery is required"))
		}
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
	if err := manager.Runtime.RevokeController(ctx, other); err != nil {
		return fmt.Errorf("fence inactive controller: %w", err)
	}
	generation := int64(1)
	for _, transaction := range transactions {
		if transaction.TargetSlot == observation.ActiveSlot || transaction.PreviousSlot == observation.ActiveSlot {
			generation = transaction.Generation
			break
		}
	}
	lease := manager.newLease(observation.ActiveSlot, generation)
	if err := manager.Runtime.GrantController(ctx, observation.ActiveSlot, lease); err != nil {
		return fmt.Errorf("grant active controller: %w", err)
	}
	return nil
}

func (manager *Manager) fenceSlots(ctx context.Context, cause error) error {
	var first error
	for _, slot := range []string{"blue", "green"} {
		if err := manager.Runtime.RevokeController(ctx, slot); err != nil && first == nil {
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
	if transactionID != "" {
		payload := map[string]any{"active_slot": observation.ActiveSlot, "consistent": observation.Consistent, "slots": observation.Slots}
		encoded, _ := json.Marshal(payload)
		digest := sha256.Sum256(encoded)
		if err := manager.Store.SaveObservation(ctx, transactionID, store.Observation{ObservedAt: observation.ObservedAt, FreshUntil: observation.FreshUntil, Digest: hex.EncodeToString(digest[:]), Payload: payload}); err != nil {
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

func (manager *Manager) rollbackOrFence(ctx context.Context, transaction store.Transaction, cause error) error {
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
	if err := manager.rollback(ctx, transaction); err != nil {
		return err
	}
	return cause
}

func (manager *Manager) rollback(ctx context.Context, transaction store.Transaction) error {
	if err := manager.Store.SetRollbackUsed(ctx, transaction.ID, manager.Clock().UTC()); err != nil {
		return err
	}
	if _, err := manager.transition(ctx, transaction.ID, transaction.Phase, PhaseRollback, "rollback_started", map[string]any{"bounded": true}); err != nil {
		return err
	}
	lease := controllerrole.Lease{}
	if transaction.PreviousSlot != "" && transaction.PreviousSlot != "none" {
		lease = manager.newLease(transaction.PreviousSlot, transaction.Generation)
	}
	if err := manager.Runtime.Rollback(ctx, transaction.TargetSlot, transaction.PreviousSlot, lease); err != nil {
		failureErr := manager.failure(ctx, transaction.ID, PhaseRollback, err)
		fenceErr := manager.fenceSlots(ctx, errors.New("rollback did not reach a verified outcome"))
		return fmt.Errorf("%w; %v", failureErr, fenceErr)
	}
	observation, observeErr := manager.observe(ctx, transaction.ID)
	if observeErr != nil {
		failureErr := manager.failure(ctx, transaction.ID, PhaseRollback, observeErr)
		fenceErr := manager.fenceSlots(ctx, errors.New("rollback observation failed; controller roles were fenced"))
		return fmt.Errorf("%w; %v", failureErr, fenceErr)
	}
	previous, previousOK := observation.Slots[transaction.PreviousSlot]
	if !previousOK || !observation.Consistent || observation.ActiveSlot != transaction.PreviousSlot || !previous.ProcessUp || !previous.ProcessReady || !previous.Ready || !previous.ControllerOwned || previous.NativeWorkers != 0 || previous.PendingOperations != 0 || previous.InFlightRequests != 0 {
		failureErr := manager.failure(ctx, transaction.ID, PhaseRollback, errors.New("rollback outcome was not verified"))
		fenceErr := manager.fenceSlots(ctx, errors.New("rollback verification failed; controller roles were fenced"))
		return fmt.Errorf("%w; %v", failureErr, fenceErr)
	}
	_, err := manager.transition(ctx, transaction.ID, PhaseRollback, PhaseFinalize, "rolled_back", map[string]any{"active_slot": transaction.PreviousSlot})
	return err
}

func (manager *Manager) newLease(slot string, generation int64) controllerrole.Lease {
	manager.term++
	return controllerrole.Lease{Slot: slot, Term: manager.term, LeaseID: newID("lease"), Expiry: manager.Clock().Add(manager.LeaseDuration), Generation: generation, Nonce: newID("nonce")}
}

func recoveryTargetVerified(observation RuntimeSnapshot, transaction store.Transaction, now time.Time, requireController bool) bool {
	if !observation.Consistent || observation.ActiveSlot != transaction.TargetSlot || observation.FreshUntil.IsZero() || !observation.FreshUntil.After(now) {
		return false
	}
	target, ok := observation.Slots[transaction.TargetSlot]
	return ok && target.ProcessUp && target.ProcessReady && target.Ready && (!requireController || target.ControllerOwned)
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
