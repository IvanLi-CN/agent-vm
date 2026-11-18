//go:build linux

package deployd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/controllerrole"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

type accountHandoffRuntime struct {
	fakeRuntime
	checks []controllerrole.Lease
	err    error
}

func (r *accountHandoffRuntime) VerifyAccountHandoff(_ context.Context, lease controllerrole.Lease, authority string) error {
	r.checks = append(r.checks, lease)
	if authority != "rqlite-fixture" {
		return errors.New("unexpected state authority")
	}
	return r.err
}

func TestAdoptAccountHandoffOnlyChangesVerifiedLease(t *testing.T) {
	for _, tc := range []struct {
		name           string
		journalCurrent bool
		verifyError    error
		wantGrant      bool
	}{
		{name: "live handoff", journalCurrent: true, wantGrant: true},
		{name: "stale journal"},
		{name: "active slot changed", journalCurrent: true, verifyError: errors.New("active slot changed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			now := time.Now()
			clock := func() time.Time { return now }
			db, err := store.Open(filepath.Join(t.TempDir(), "deploy.sqlite3"), time.Hour, clock)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if tc.journalCurrent {
				if _, err := db.NextFenceTerm(ctx); err != nil {
					t.Fatal(err)
				}
			}
			previous := controllerrole.Lease{Slot: "blue", Term: 1, LeaseID: "old", Expiry: now.Add(time.Minute),
				Nonce: "old-nonce", Generation: 15, OwnerPID: os.Getpid() + 1, OwnerStartTime: "old-process"}
			runtime := &accountHandoffRuntime{err: tc.verifyError}
			manager := NewManager(db, runtime, time.Minute, clock)
			err = manager.AdoptAccountHandoff(ctx, previous, "rqlite-fixture")
			if !tc.wantGrant {
				if err == nil || len(runtime.sequence) != 0 {
					t.Fatalf("err=%v operations=%v", err, runtime.sequence)
				}
				return
			}
			if err != nil || len(runtime.sequence) != 1 || runtime.sequence[0] != "grant" || len(runtime.checks) != 2 {
				t.Fatalf("err=%v operations=%v checks=%v", err, runtime.sequence, runtime.checks)
			}
			grant := runtime.grantedLeases[0]
			if runtime.checks[0] != previous || runtime.checks[1] != grant || grant.Term != 2 ||
				grant.Slot != previous.Slot || grant.Generation != previous.Generation || grant.OwnerPID != os.Getpid() {
				t.Fatalf("unexpected adopted lease: %+v", grant)
			}
		})
	}
}
