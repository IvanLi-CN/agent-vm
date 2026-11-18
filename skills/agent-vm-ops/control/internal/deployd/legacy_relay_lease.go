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

func (runtime *HostRuntime) CloseLegacyLeaseOwner() {
	runtime.legacyRelayMu.Lock()
	defer runtime.legacyRelayMu.Unlock()
	runtime.closeLegacyLeaseOwnerLocked()
}

func (runtime *HostRuntime) closeLegacyLeaseOwnerLocked() {
	if runtime.legacyRelayConn != nil {
		_ = runtime.legacyRelayConn.Close()
		runtime.legacyRelayConn = nil
	}
	runtime.legacyRelayOwner = legacyLeaseOwner{}
	runtime.legacyRelayGrants = nil
}

func (runtime *HostRuntime) relayHistoricalSlot(ctx context.Context, request slotRelayRequest, timeout time.Duration) (map[string]any, int, error) {
	if request.Path == "/internal/v1/controller-role" && request.Method == http.MethodPost {
		var command controllerrole.Command
		if err := json.Unmarshal(request.Body, &command); err != nil {
			return nil, 0, err
		}
		if command.Action == "grant" {
			return runtime.grantHistoricalSlot(ctx, request, timeout, command)
		}
	}
	result, status, err := requestSlotRelay(ctx, request, timeout)
	if err == nil && request.Path == "/internal/v1/health" {
		runtime.normalizeHistoricalLease(request.Slot, result)
	}
	return result, status, err
}

func (runtime *HostRuntime) grantHistoricalSlot(ctx context.Context, request slotRelayRequest, timeout time.Duration, command controllerrole.Command) (map[string]any, int, error) {
	lease := command.Lease
	if lease.Validate(time.Now(), request.Slot) != nil || lease.OwnerPID != os.Getpid() ||
		lease.OwnerStartTime == "" || lease.OwnerStartTime != controllerrole.ProcessStartToken(os.Getpid()) {
		return nil, 0, errors.New("historical lease must be owned by this deployment process")
	}
	runtime.legacyRelayMu.Lock()
	defer runtime.legacyRelayMu.Unlock()
	owner := runtime.legacyRelayOwner
	if runtime.legacyRelayConn == nil || owner.StartTime != controllerrole.ProcessStartToken(owner.PID) {
		runtime.closeLegacyLeaseOwnerLocked()
		conn, witness, err := connectLegacyLeaseOwner(ctx)
		if err != nil {
			return nil, 0, err
		}
		runtime.legacyRelayConn, runtime.legacyRelayOwner = conn, witness
		runtime.legacyRelayGrants = make(map[string]controllerrole.Lease)
		owner = witness
	}
	command.Lease.OwnerPID, command.Lease.OwnerStartTime = owner.PID, owner.StartTime
	request.Body, _ = json.Marshal(command)
	result, status, err := requestSlotRelay(ctx, request, timeout)
	if err == nil && status == http.StatusOK && responseOK(result) && result["valid"] == true {
		runtime.legacyRelayGrants[request.Slot] = command.Lease
	}
	return result, status, err
}

func (runtime *HostRuntime) normalizeHistoricalLease(slot string, result map[string]any) {
	runtime.legacyRelayMu.Lock()
	defer runtime.legacyRelayMu.Unlock()
	expected, ok := runtime.legacyRelayGrants[slot]
	if !ok || expected.OwnerStartTime != controllerrole.ProcessStartToken(expected.OwnerPID) {
		return
	}
	var reported controllerrole.Lease
	encoded, err := json.Marshal(result["lease"])
	if err != nil || json.Unmarshal(encoded, &reported) != nil || !sameLeaseAuthority(reported, expected) || !reported.Expiry.Equal(expected.Expiry) {
		return
	}
	reported.OwnerPID = os.Getpid()
	reported.OwnerStartTime = controllerrole.ProcessStartToken(os.Getpid())
	result["lease"] = reported
}
