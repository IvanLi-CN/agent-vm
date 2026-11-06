package deployd

import (
	"context"
	"errors"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
	"path/filepath"
	"testing"
	"time"
)

type bootstrapFake struct {
	*fakeRuntime
	prepareErr   error
	preparations int
}

func (r *bootstrapFake) CheckBootstrap(context.Context) error {
	if r.active != "none" {
		return errors.New("already installed")
	}
	return nil
}
func (r *bootstrapFake) PrepareBootstrap(context.Context, string) error {
	r.preparations++
	if r.prepareErr != nil {
		return r.prepareErr
	}
	r.observations["blue"] = SlotObservation{ProcessUp: true, ProcessReady: true, Ready: true}
	return nil
}
func TestBootstrapJournalAndNoReplay(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "uncertain"}[fail], func(t *testing.T) {
			now := time.Unix(100, 0).UTC()
			db, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			runtime := &bootstrapFake{fakeRuntime: &fakeRuntime{active: "none", observations: map[string]SlotObservation{"green": {}}, traffic: map[string]bool{}, readyAfterGrant: true}}
			if fail {
				runtime.prepareErr = errors.New("connection lost after initialization")
			}
			manager := NewManager(db, runtime, time.Minute, func() time.Time { return now })
			id := "install-0123456789abcdef0123456789abcdef"
			tx, err := manager.Bootstrap(context.Background(), id, "/bundle.tar")
			if fail {
				if err == nil {
					t.Fatal("uncertain operation succeeded")
				}
				persisted, e := db.Get(context.Background(), id)
				if e != nil || persisted == nil || !persisted.RecoveryRequired {
					t.Fatalf("missing recovery journal: %+v %v", persisted, e)
				}
			} else if err != nil || tx.State != "finalized" || runtime.active != "blue" {
				t.Fatalf("bootstrap: %+v %v", tx, err)
			}
			_, _ = manager.Bootstrap(context.Background(), id, "/bundle.tar")
			if runtime.preparations != 1 {
				t.Fatal("bootstrap replayed an existing transaction")
			}
		})
	}
}

func (r *bootstrapFake) CheckBootstrapRecovery(ctx context.Context) error {
	return r.CheckBootstrap(ctx)
}
func (r *bootstrapFake) ResumeBootstrap(ctx context.Context, bundle string) error {
	return r.PrepareBootstrap(ctx, bundle)
}
func TestBootstrapExplicitRecoveryRetainsOriginalTransaction(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	db, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runtime := &bootstrapFake{fakeRuntime: &fakeRuntime{active: "none", observations: map[string]SlotObservation{"blue": {}, "green": {}}, traffic: map[string]bool{}, readyAfterGrant: true}, prepareErr: errors.New("initializer result unavailable")}
	manager := NewManager(db, runtime, time.Minute, func() time.Time { return now })
	id := "install-0123456789abcdef0123456789abcdef"
	if _, err := manager.Bootstrap(context.Background(), id, "/bundle.tar"); err == nil {
		t.Fatal("expected failure")
	}
	runtime.prepareErr = nil
	tx, err := manager.Recover(context.Background(), id, "resume-bootstrap")
	if err != nil || tx == nil || tx.ID != id || tx.State != "finalized" {
		t.Fatalf("explicit recovery: %+v %v", tx, err)
	}
}
