package deployd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

type exhaustedRecoveryRuntime struct {
	*fakeRuntime
	requests []string
}

func (*exhaustedRecoveryRuntime) RecoveryAuthority() string { return "authority-1" }

func (runtime *exhaustedRecoveryRuntime) CreateRecoveryPoint(_ context.Context, _, purpose, _ string, _ map[string]bool, _ time.Time) error {
	runtime.requests = append(runtime.requests, purpose)
	return errBackupStorageExhausted
}

func TestRecoveryStorageGatePreservesActiveSlot(t *testing.T) {
	ctx := t.Context()
	now := time.Unix(100, 0).UTC()
	clock := func() time.Time { return now }
	db, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	runtime := &exhaustedRecoveryRuntime{fakeRuntime: &fakeRuntime{
		active: "blue",
		observations: map[string]SlotObservation{
			"blue":  {Slot: "blue", ProcessUp: true, ProcessReady: true, Ready: true, ControllerOwned: true},
			"green": {Slot: "green"},
		},
	}}
	manager := NewManager(db, runtime, time.Minute, clock)
	if _, err := manager.Stage(ctx, "green", "/bundle"); !errors.Is(err, errBackupStorageExhausted) {
		t.Fatalf("stage = %v", err)
	}
	if pending, err := db.ListActive(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("stage left an unfinished deployment: %v (%v)", pending, err)
	}
	old := store.Transaction{ID: "tx-finalized", Phase: PhaseFinalize, State: "finalized", TargetSlot: "blue", PreviousSlot: "green", Bundle: "/bundle", Generation: 1, CreatedAt: now, UpdatedAt: now}
	if err := db.Create(ctx, old, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Rollback(ctx, old.ID); !errors.Is(err, errBackupStorageExhausted) {
		t.Fatalf("rollback = %v", err)
	}
	if observed, _ := runtime.Observe(ctx); observed.ActiveSlot != "blue" || !observed.Slots["blue"].ControllerOwned || !observed.Slots["blue"].TrafficAccepting {
		t.Fatalf("recovery budget rejection affected active roles: %+v", observed)
	}
	if len(runtime.sequence) != 0 || strings.Join(runtime.requests, ",") != "stage,rollback" {
		t.Fatalf("effects = %v, requests = %v", runtime.sequence, runtime.requests)
	}
}

func TestRecoveryBackupRejectsIncompleteStreams(t *testing.T) {
	for _, test := range []struct {
		name, length, trailer string
		limit                 int64
		want                  error
	}{
		{"limit", "", "", 3, errBackupStorageExhausted},
		{"short", "10", "", 20, nil},
		{"trailer", "", "storage failed", 20, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if user, pass, ok := r.BasicAuth(); !ok || user != "agent-vm-backup" || pass != "test" || r.URL.Path != "/db/backup" {
					t.Error("backup used an unexpected endpoint or credential")
				}
				if test.length != "" {
					w.Header().Set("Content-Length", test.length)
				}
				if test.trailer != "" {
					w.Header().Set("Trailer", "X-Stream-Error")
				}
				_, _ = w.Write([]byte("data"))
				if test.trailer != "" {
					w.Header().Set("X-Stream-Error", test.trailer)
				}
			}))
			defer server.Close()
			_, _, err := downloadRecoverySnapshot(t.Context(), server.Client(), server.URL, "test", filepath.Join(t.TempDir(), "snapshot"), test.limit)
			if err == nil || test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("backup error = %v", err)
			}
		})
	}
}

func TestRecoveryPointRetentionAndPendingReconciliation(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name  string
		age   time.Duration
		lease time.Time
		holds map[string]bool
		prune bool
	}{
		{name: "young", age: recoveryRetention - time.Second},
		{name: "leased", age: recoveryRetention, lease: now.Add(time.Hour)},
		{name: "protected transaction", age: recoveryRetention, holds: map[string]bool{"old": true}},
		{name: "eligible at threshold", age: recoveryRetention, prune: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := Defaults()
			config.Migration.RecoveryDirectory = filepath.Join(t.TempDir(), "recovery")
			runtime := NewHostRuntime(config)
			root := runtime.recoveryDirectory()
			authority := strings.Repeat("a", 64)
			for i, operation := range []string{"old", "middle", "latest"} {
				id := "point-" + operation
				dir := filepath.Join(root, id)
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				point := recoveryPoint{ID: id, Operation: operation, Authority: authority, SHA256: strings.Repeat("b", 64), Boundary: "epoch:" + id, Size: 1, Created: now.Add(-test.age + time.Duration(i)*time.Hour)}
				if i == 0 {
					point.LeaseUntil = test.lease
				}
				if err := writeRecoveryRecord(dir, "point.json", point); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "snapshot.sqlite3"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			pending := filepath.Join(root, ".pending-interrupted")
			if err := os.Mkdir(pending, 0o700); err != nil {
				t.Fatal(err)
			}
			err := runtime.CreateRecoveryPoint(t.Context(), "new", "stage", authority, test.holds, now)
			if err == nil || errors.Is(err, errBackupStorageExhausted) == test.prune {
				t.Fatalf("create error = %v", err)
			}
			if _, err := os.Stat(pending); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("pending file survived reconciliation: %v", err)
			}
			points, err := loadRecoveryPoints(root)
			want := 3
			if test.prune {
				want = 2
				if _, err := os.Stat(filepath.Join(root, "pruned-point-old.json")); err != nil {
					t.Fatal(err)
				}
			}
			if err != nil || len(points) != want || points[len(points)-1].Operation != "latest" {
				t.Fatalf("points = %+v, err = %v", points, err)
			}
		})
	}
}

func TestRecoverySnapshotVerifiesIntegrityAndCoverage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "snapshot.sqlite3")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE meta(key TEXT PRIMARY KEY,value TEXT NOT NULL);
		INSERT INTO meta VALUES('deployment_recovery_marker','epoch:point-first');`)
	if err = errors.Join(err, db.Close()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if err := verifyRecoverySnapshot(ctx, path, "epoch:point-first"); err != nil {
		t.Fatal(err)
	}
	if err := verifyRecoverySnapshot(ctx, path, "epoch:point-second"); err == nil {
		t.Fatal("snapshot without expected marker was accepted")
	}
	point := recoveryPoint{ID: ".", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Boundary: "epoch:point-first"}
	if err := validateRegisteredRecoveryPoint(ctx, dir, point); err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateRegisteredRecoveryPoint(ctx, dir, point); err == nil {
		t.Fatal("modified snapshot was accepted")
	}
}
