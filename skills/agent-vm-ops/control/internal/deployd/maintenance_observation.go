package deployd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
)

type controllerMaintenanceKey struct{}
type controllerMaintenancePolicy struct{ Slot string }

// CurrentOwnedControllerLease is a live role-channel check. In-memory process
// pins are deliberately required; persisted observations cannot resume renewal.
func (runtime *HostRuntime) CurrentOwnedControllerLease(ctx context.Context) (controllerrole.Lease, bool, error) {
	slot, err := runtime.activeSlot()
	if err != nil || (slot != "blue" && slot != "green") {
		return controllerrole.Lease{}, false, nil
	}
	runtime.mu.Lock()
	pinned, present := runtime.stagedProcess[slot]
	runtime.mu.Unlock()
	if !present {
		return controllerrole.Lease{}, false, nil
	}
	body, status, err := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodGet, "/internal/v1/health", nil)
	if err != nil {
		return controllerrole.Lease{}, false, err
	}
	process, processOK := processIdentityFromHealth(body)
	if (status != http.StatusOK && status != http.StatusServiceUnavailable) || !healthEnvelopeValid(body) || body["process_ready"] != true || !processOK || process != pinned {
		return controllerrole.Lease{}, false, errors.New("pinned controller process changed")
	}
	if body["controller_owned"] != true || body["controller_lease_renewal"] != true {
		return controllerrole.Lease{}, false, nil
	}
	var lease controllerrole.Lease
	encoded, _ := json.Marshal(body["lease"])
	if json.Unmarshal(encoded, &lease) != nil || lease.Validate(time.Now(), slot) != nil || lease.OwnerPID != os.Getpid() || lease.OwnerStartTime != controllerrole.ProcessStartToken(os.Getpid()) {
		return controllerrole.Lease{}, false, errors.New("live controller authority is not owned")
	}
	if err := runtime.systemdProcessMatches(ctx, slot, process); err != nil {
		return controllerrole.Lease{}, false, err
	}
	other := "blue"
	if slot == "blue" {
		other = "green"
	}
	if !runtime.systemdSlotStopped(ctx, other) {
		otherBody, otherStatus, otherErr := runtime.slotRequest(ctx, runtime.socket(other), http.MethodGet, "/internal/v1/health", nil)
		otherProcess, otherOK := processIdentityFromHealth(otherBody)
		if otherErr != nil || (otherStatus != http.StatusOK && otherStatus != http.StatusServiceUnavailable) || !healthEnvelopeValid(otherBody) || !otherOK || otherBody["controller_owned"] != false || runtime.systemdProcessMatches(ctx, other, otherProcess) != nil {
			return controllerrole.Lease{}, false, errors.New("inactive slot authority is not verified")
		}
	}
	end, err := runtime.activeSlot()
	if err != nil || end != slot {
		return controllerrole.Lease{}, false, errors.New("active slot changed during authority check")
	}
	return lease, true, nil
}

func sameLeaseAuthority(a, b controllerrole.Lease) bool { a.Expiry = b.Expiry; return a == b }

// observeRuntime preserves only a live, process-pinned existing authority while
// slow business probes run. Read-only status calls never enter this path.
func (manager *Manager) observeRuntime(ctx context.Context) (RuntimeSnapshot, error) {
	// The enclosing readiness/transaction heartbeat already owns renewal.
	// A second heartbeat would race its exact expiry proof and fence the owner.
	if ctx.Value(transactionRenewalKey{}) == true {
		return manager.Runtime.Observe(ctx)
	}
	probe, ok := manager.Runtime.(interface {
		CurrentOwnedControllerLease(context.Context) (controllerrole.Lease, bool, error)
	})
	policy, permitted := ctx.Value(controllerMaintenanceKey{}).(controllerMaintenancePolicy)
	if !ok || !permitted {
		return manager.Runtime.Observe(ctx)
	}
	lease, owned, err := probe.CurrentOwnedControllerLease(ctx)
	if err != nil {
		return RuntimeSnapshot{}, err
	}
	if !owned {
		return manager.Runtime.Observe(ctx)
	}
	if policy.Slot != "" && policy.Slot != lease.Slot {
		return RuntimeSnapshot{}, errors.New("transaction cannot retain this controller")
	}
	renewalCtx, stop := manager.beginTransactionRenewal(ctx, lease)
	snapshot, observeErr := manager.Runtime.Observe(renewalCtx)
	renewalErr := stop()
	if err := errors.Join(observeErr, renewalErr); err != nil {
		return RuntimeSnapshot{}, err
	}
	current, stillOwned, err := probe.CurrentOwnedControllerLease(ctx)
	if err != nil || !stillOwned || !sameLeaseAuthority(current, lease) {
		return RuntimeSnapshot{}, errors.New("controller authority changed during observation")
	}
	target := snapshot.Slots[lease.Slot]
	if target.ControllerLease == nil || !sameLeaseAuthority(*target.ControllerLease, current) || !target.ControllerOwned {
		return RuntimeSnapshot{}, errors.New("observed controller authority changed")
	}
	// The end check revalidates only process/role/routing evidence. Business
	// source timestamps and failures remain unchanged and still gate traffic.
	now := manager.Clock().UTC()
	for _, name := range []string{"active_link", "systemd", "slot_contracts"} {
		source := snapshot.Sources[name]
		if source.Up {
			window := source.FreshUntil.Sub(source.ObservedAt)
			source.ObservedAt = now
			source.FreshUntil = now.Add(window)
			snapshot.Sources[name] = source
		}
	}
	snapshot.FreshUntil = composedFreshUntil(snapshot.Sources, now)
	target.ControllerLease = &current
	snapshot.Slots[lease.Slot] = target
	return snapshot, nil
}
