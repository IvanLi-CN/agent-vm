package controllerrole

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

var (
	ErrInvalidLease  = errors.New("invalid controller lease")
	ErrLeaseExpired  = errors.New("controller lease expired")
	ErrLeaseMismatch = errors.New("controller lease does not match")
)

// Lease is the short-lived process-held authority for scheduler mutation.
// It is deliberately not persisted by the slot process: a restart must start
// fenced until deployd observes the slot again and grants a fresh lease.
type Lease struct {
	Slot           string    `json:"slot"`
	Term           uint64    `json:"term"`
	LeaseID        string    `json:"lease_id"`
	Expiry         time.Time `json:"expiry"`
	Generation     int64     `json:"generation"`
	Nonce          string    `json:"nonce"`
	OwnerPID       int       `json:"owner_pid,omitempty"`
	OwnerStartTime string    `json:"owner_start_time,omitempty"`
}

type Command struct {
	Action string `json:"action"`
	Lease  Lease  `json:"lease"`
}

func (lease Lease) Validate(now time.Time, slot string) error {
	if lease.Slot == "" || lease.Slot != slot || lease.Term == 0 || lease.LeaseID == "" || lease.Nonce == "" || lease.Generation < 0 {
		return ErrInvalidLease
	}
	if lease.Expiry.IsZero() || !lease.Expiry.After(now) {
		return ErrLeaseExpired
	}
	return nil
}

type Gate struct {
	slot  string
	clock func() time.Time

	mu    sync.RWMutex
	lease Lease
	valid bool
	term  uint64

	changed chan struct{}
}

func New(slot string, clock func() time.Time) *Gate {
	if clock == nil {
		clock = time.Now
	}
	return &Gate{slot: slot, clock: clock, changed: make(chan struct{})}
}

func (gate *Gate) Grant(lease Lease) error {
	now := gate.clock()
	if err := lease.Validate(now, gate.slot); err != nil {
		return err
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if lease.Term <= gate.term {
		return fmt.Errorf("%w: term is not newer", ErrLeaseMismatch)
	}
	gate.lease = lease
	gate.valid = true
	gate.term = lease.Term
	gate.signalLocked()
	return nil
}

func (gate *Gate) Revoke(lease Lease) error {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if lease.Slot != gate.slot || lease.Term == 0 {
		return ErrInvalidLease
	}
	if !gate.valid {
		if lease.Term > gate.term {
			gate.term = lease.Term
		}
		return nil
	}
	if lease.Term < gate.lease.Term || lease.Term == gate.lease.Term && lease.LeaseID != "" && lease.LeaseID != gate.lease.LeaseID {
		return ErrLeaseMismatch
	}
	gate.valid = false
	gate.lease = Lease{}
	if lease.Term > gate.term {
		gate.term = lease.Term
	}
	gate.signalLocked()
	return nil
}

// Fence unconditionally drops the current role while preserving the highest
// accepted term. The deployd hard-fence path uses this when durable fencing
// state is unavailable; future grants still require a newer persisted term.
func (gate *Gate) Fence() {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if !gate.valid {
		return
	}
	gate.valid = false
	gate.lease = Lease{}
	gate.signalLocked()
}

func (gate *Gate) Valid() bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if !gate.valid {
		return false
	}
	if !gate.lease.Expiry.After(gate.clock()) || !ownerAlive(gate.lease.OwnerPID, gate.lease.OwnerStartTime) {
		gate.valid = false
		gate.lease = Lease{}
		gate.signalLocked()
		return false
	}
	return true
}

func ownerAlive(pid int, startTime string) bool {
	if pid <= 0 {
		return true
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if process.Signal(syscall.Signal(0)) != nil {
		return false
	}
	if startTime == "" {
		return true
	}
	return processStartToken(pid) == startTime
}

// ProcessStartToken identifies a process instance, not just its PID. Linux
// uses the kernel start-time field so PID reuse cannot extend an old lease.
func ProcessStartToken(pid int) string { return processStartToken(pid) }

func (gate *Gate) Snapshot() (Lease, bool) {
	if !gate.Valid() {
		return Lease{}, false
	}
	gate.mu.RLock()
	defer gate.mu.RUnlock()
	return gate.lease, gate.valid
}

// Changes returns a channel that is replaced whenever the lease changes. It
// is useful to wake a scheduler without polling more often than its safety
// interval.
func (gate *Gate) Changes() <-chan struct{} {
	gate.mu.RLock()
	defer gate.mu.RUnlock()
	return gate.changed
}

func (gate *Gate) signalLocked() {
	close(gate.changed)
	gate.changed = make(chan struct{})
}
