package deployd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
)

// WitnessAccountHandoff arms the current root maintenance process as a bounded
// witness. The incumbent remains the deployment writer until it exits.
func WitnessAccountHandoff(ctx context.Context, fixedSlot string, expected controllerrole.Lease) error {
	if os.Geteuid() != 0 || validSlot(fixedSlot) != nil || expected.Validate(time.Now(), fixedSlot) != nil {
		return errors.New("account handoff requires root and a current lease for a fixed slot")
	}
	account, err := user.Lookup(slotUser)
	if err != nil {
		return err
	}
	uid, err := strconv.Atoi(account.Uid)
	if err != nil || uid <= 0 {
		return errors.New("account handoff requires a dedicated server UID")
	}
	payload, err := json.Marshal(controllerrole.Command{Action: "witness-handoff", Lease: expected})
	if err != nil {
		return err
	}
	runtime := NewHostRuntime(Defaults())
	result, status, err := callSlot(ctx, runtime.socket(fixedSlot), http.MethodPost, "/internal/v1/controller-role", payload, 3*time.Second, uid)
	if err != nil {
		return err
	}
	if status != http.StatusOK || !responseOK(result) || result["valid"] != true {
		return errors.New("active slot did not confirm the account handoff witness")
	}
	encoded, err := json.Marshal(result["lease"])
	var observed controllerrole.Lease
	if err != nil || json.Unmarshal(encoded, &observed) != nil || !sameLeaseAuthority(expected, observed) || !observed.Expiry.Equal(expected.Expiry) {
		return errors.New("active slot returned a different lease for the account handoff")
	}
	return nil
}
