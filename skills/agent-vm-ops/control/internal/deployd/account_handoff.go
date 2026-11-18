package deployd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"syscall"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
)

const AccountHandoffFile = "/etc/agent-vm-native/deploy-account-handoff.json"

type AccountHandoff struct {
	Lease          controllerrole.Lease `json:"lease"`
	StateAuthority string               `json:"state_authority"`
}

func ReadAccountHandoff() (AccountHandoff, error) {
	var result AccountHandoff
	if !isDeployAccount(os.Geteuid()) {
		return result, errors.New("account handoff startup requires the dedicated deployment user")
	}
	fd, err := syscall.Open(AccountHandoffFile, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return result, errors.New("cannot open account handoff evidence")
	}
	file := os.NewFile(uintptr(fd), AccountHandoffFile)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0640 || info.Size() > 4096 {
		return result, errors.New("account handoff evidence must be a protected root-owned file")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || decoder.Decode(new(any)) != io.EOF {
		return AccountHandoff{}, errors.New("invalid account handoff evidence")
	}
	return result, nil
}

// AdoptAccountHandoff runs after taking the deployment singleton lock and
// opening the copied journal. It changes only the live active slot's lease.
func (manager *Manager) AdoptAccountHandoff(ctx context.Context, previous controllerrole.Lease, authority string) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if validSlot(previous.Slot) != nil || previous.Validate(manager.Clock(), previous.Slot) != nil ||
		previous.OwnerPID <= 0 || previous.OwnerStartTime == "" || previous.OwnerPID == os.Getpid() || authority == "" {
		return errors.New("account handoff requires the prior process lease and state authority")
	}
	active, err := manager.Store.ListActive(ctx)
	if err != nil || len(active) != 0 {
		return errors.New("cannot adopt a controller with an unresolved deployment transaction")
	}
	runtime, ok := manager.Runtime.(interface {
		VerifyAccountHandoff(context.Context, controllerrole.Lease, string) error
	})
	if !ok {
		return errors.New("runtime cannot verify account handoff")
	}
	if err := runtime.VerifyAccountHandoff(ctx, previous, authority); err != nil {
		return err
	}
	next, err := manager.newLease(ctx, previous.Slot, previous.Generation)
	if err != nil {
		return err
	}
	if next.Term <= previous.Term || next.OwnerStartTime == "" {
		return errors.New("copied deployment journal cannot advance the live controller term")
	}
	if err := manager.Runtime.GrantController(ctx, previous.Slot, next); err != nil {
		return err
	}
	return runtime.VerifyAccountHandoff(ctx, next, authority)
}

func (runtime *HostRuntime) VerifyAccountHandoff(ctx context.Context, lease controllerrole.Lease, authority string) error {
	slot := lease.Slot
	if validSlot(slot) != nil {
		return errors.New("invalid account handoff slot")
	}
	selected, err := runtime.activeSlot()
	if err != nil || selected != slot {
		return errors.New("active route changed during account handoff")
	}
	other := "blue"
	if slot == "blue" {
		other = "green"
	}
	if !runtime.systemdSlotStopped(ctx, other) {
		return errors.New("inactive slot is still running during account handoff")
	}
	body, status, err := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodGet, "/internal/v1/health", nil)
	process, processOK := processIdentityFromHealth(body)
	if err != nil || status != http.StatusOK || !healthEnvelopeValid(body) || !processOK ||
		body["ready"] != true || body["process_ready"] != true || body["state_authority"] != authority ||
		body["controller_owned"] != true || body["traffic_accepting"] != true {
		return errors.New("active slot is not serving the expected state authority")
	}
	var current controllerrole.Lease
	encoded, _ := json.Marshal(body["lease"])
	if json.Unmarshal(encoded, &current) != nil || current.Validate(time.Now(), slot) != nil ||
		!sameLeaseAuthority(current, lease) || current.Expiry.Before(lease.Expiry) {
		return errors.New("active slot does not hold the expected handoff lease")
	}
	if err := runtime.systemdProcessMatches(ctx, slot, process); err != nil {
		return err
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if old, pinned := runtime.stagedProcess[slot]; pinned && old != process {
		return errors.New("active slot process changed during account handoff")
	}
	runtime.stagedProcess[slot] = process
	return nil
}

// WitnessAccountHandoff arms the current root maintenance process as a bounded
// witness. The incumbent remains the deployment writer until it exits.
func WitnessAccountHandoff(ctx context.Context, fixedSlot string, expected controllerrole.Lease) (controllerrole.Lease, error) {
	var empty controllerrole.Lease
	if os.Geteuid() != 0 || validSlot(fixedSlot) != nil || expected.Validate(time.Now(), fixedSlot) != nil {
		return empty, errors.New("account handoff requires root and a current lease for a fixed slot")
	}
	account, err := user.Lookup(slotUser)
	if err != nil {
		return empty, err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid <= 0 {
		return empty, errors.New("account handoff requires a dedicated server UID")
	}
	payload, err := json.Marshal(controllerrole.Command{Action: "witness-handoff", Lease: expected})
	if err != nil {
		return empty, err
	}
	runtime := NewHostRuntime(Defaults())
	result, status, err := callSlot(ctx, runtime.socket(fixedSlot), http.MethodPost, "/internal/v1/controller-role", payload, 3*time.Second, uid)
	if err != nil {
		return empty, err
	}
	if status != http.StatusOK || !responseOK(result) || result["valid"] != true {
		return empty, errors.New("active slot did not confirm the account handoff witness")
	}
	encoded, err := json.Marshal(result["lease"])
	var observed controllerrole.Lease
	if err != nil || json.Unmarshal(encoded, &observed) != nil || !sameLeaseAuthority(expected, observed) ||
		observed.Validate(time.Now(), fixedSlot) != nil || observed.Expiry.Before(expected.Expiry) {
		return empty, errors.New("active slot returned a different lease for the account handoff")
	}
	return observed, nil
}
