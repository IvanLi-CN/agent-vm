package install

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

func TestAccountJournalRetainsRollbackHistoryAndRefusesUnresolvedDeployment(t *testing.T) {
	dir := t.TempDir()
	source, dest := filepath.Join(dir, "old.sqlite3"), filepath.Join(dir, "new.sqlite3")
	old, err := store.Open(source, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	ctx := t.Context()
	tx := store.Transaction{ID: "update-1", TargetSlot: "green", PreviousSlot: "blue", Bundle: "candidate.tar", Generation: 42, Phase: "rollback", RollbackUsed: true}
	if err := old.Create(ctx, tx, nil); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := old.ConsumeRollbackStart(ctx, tx.ID); err != nil {
			t.Fatal(err)
		}
	}
	copyJournal := func() error {
		return copyAccountJournal(ctx, source, dest, os.Geteuid(), os.Getegid())
	}
	if err := copyJournal(); err == nil {
		t.Fatal("an active rollback was copied")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("rejected transfer published a destination: %v", err)
	}
	if _, err := old.Transition(ctx, tx.ID, "rollback", "rollback", "rolled_back", "completed", "deployd", nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// Leave the writer open so the last transaction and retry evidence may
	// still be in the WAL, as after an interrupted daemon shutdown.
	if err := copyJournal(); err != nil {
		t.Fatal(err)
	}
	if err := copyJournal(); err == nil {
		t.Fatal("a second transfer replaced the installed journal")
	}
	copied, err := store.Open(dest, time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer copied.Close()
	loaded, err := copied.Get(ctx, tx.ID)
	if err != nil || loaded == nil || loaded.PreviousSlot != "blue" || loaded.Generation != 42 || !loaded.RollbackUsed || loaded.State != "rolled_back" {
		t.Fatalf("transferred transaction: %+v, %v", loaded, err)
	}
	event, err := copied.LatestEvent(ctx, tx.ID, "rollback", "rollback_start_attempt")
	if err != nil || event == nil || event.Evidence["attempt"] != float64(3) {
		t.Fatalf("transferred retry evidence: %+v, %v", event, err)
	}
}
