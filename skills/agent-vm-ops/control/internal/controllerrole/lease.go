package controllerrole

import (
	"errors"
	"fmt"
	"sync"
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
	Slot       string    `json:"slot"`
	Term       uint64    `json:"term"`
	LeaseID    string    `json:"lease_id"`
	Expiry     time.Time `json:"expiry"`
	Generation int64     `json:"generation"`
	Nonce      string    `json:"nonce"`
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
	if gate.valid && gate.lease.LeaseID != lease.LeaseID && lease.Term <= gate.lease.Term {
		return fmt.Errorf("%w: term is not newer", ErrLeaseMismatch)
	}
	gate.lease = lease
	gate.valid = true
	gate.signalLocked()
	return nil
}

func (gate *Gate) Revoke(lease Lease) error {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if !gate.valid {
		return nil
	}
	if lease.LeaseID != "" && lease.LeaseID != gate.lease.LeaseID || lease.Term != 0 && lease.Term != gate.lease.Term {
		return ErrLeaseMismatch
	}
	gate.valid = false
	gate.lease = Lease{}
	gate.signalLocked()
	return nil
}

func (gate *Gate) Valid() bool {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if !gate.valid {
		return false
	}
	if !gate.lease.Expiry.After(gate.clock()) {
		gate.valid = false
		gate.lease = Lease{}
		gate.signalLocked()
		return false
	}
	return true
}

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
