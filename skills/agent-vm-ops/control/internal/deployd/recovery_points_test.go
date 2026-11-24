package deployd

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
)

type exhaustedRecoveryRuntime struct {
	*fakeRuntime
	requests []string
}

func TestExportBackupCredential(t *testing.T) {
	const backup = `{"username":"agent-vm-backup","password":"test-backup","perms":["backup","query"]}`
	const control = `{"username":"agent-vm-control","password":"test-control","perms":["execute","query"]}`
	got, err := ExportBackupCredential([]byte("[" + control + "," + backup + "]"))
	if err != nil || string(got) != `{"password":"test-backup"}` {
		t.Fatal("installer did not export exactly the backup credential")
	}
	for name, auth := range map[string]string{
		"missing":         "[" + control + "]",
		"duplicate":       "[" + backup + "," + backup + "]",
		"write allowed":   "[" + strings.Replace(backup, `"query"`, `"query","execute"`, 1) + "]",
		"no query":        "[" + strings.Replace(backup, `,"query"`, "", 1) + "]",
		"shared password": "[" + backup + "," + strings.Replace(control, "test-control", "test-backup", 1) + "]",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ExportBackupCredential([]byte(auth)); err == nil {
				t.Fatal("invalid backup identity was accepted")
			}
		})
	}
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

// seedRecoveryPoints writes one point per operation, oldest first, so budget
// tests do not need the rqlite path of CreateRecoveryPoint.
func seedRecoveryPoints(t *testing.T, runtime *HostRuntime, authority string, age time.Duration, operations ...string) {
	t.Helper()
	root := runtime.recoveryDirectory()
	now := time.Now().UTC()
	for index, operation := range operations {
		id := "point-" + operation
		dir := filepath.Join(root, id)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		point := recoveryPoint{ID: id, Operation: operation, Purpose: "stage", Authority: authority, SHA256: strings.Repeat("b", 64), Boundary: "epoch:" + id, Size: 1, Created: now.Add(-age + time.Duration(index)*time.Hour)}
		if err := writeRecoveryRecord(dir, "point.json", point); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "snapshot.sqlite3"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func budgetRuntime(t *testing.T) (*HostRuntime, string) {
	t.Helper()
	config := Defaults()
	config.Migration.RecoveryDirectory = filepath.Join(t.TempDir(), "recovery")
	config.Migration.RQLiteURL = "https://rqlite.example:4001"
	runtime := NewHostRuntime(config)
	if err := runtime.ensureRecoveryDirectory(); err != nil {
		t.Fatal(err)
	}
	root := runtime.recoveryDirectory()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return runtime, runtime.RecoveryAuthority()
}

func TestRecoveryPointsReportAgeAndReferenceBlockers(t *testing.T) {
	runtime, authority := budgetRuntime(t)
	seedRecoveryPoints(t, runtime, authority, 40*24*time.Hour, "first", "second", "third")
	status, err := runtime.RecoveryPoints()
	if err != nil || len(status) != 3 {
		t.Fatalf("points = %v, err = %v", status, err)
	}
	now := time.Now().UTC()
	for index, point := range status {
		report := recoveryPointReport{ID: point.ID, Created: point.Created}
		report.Eligible = recoveryPointEligible(point, index, len(status), map[string]bool{}, now)
		report.Blockers = recoveryPointBlockers(point, index, len(status), map[string]bool{}, now)
		if report.Eligible != (index == 0) || (index > 0) != slices.Contains(report.Blockers, "newest") {
			t.Fatalf("%s eligibility is wrong: eligible=%v blockers=%v", point.ID, report.Eligible, report.Blockers)
		}
	}
	blocked := recoveryPointBlockers(status[0], 0, len(status), map[string]bool{recoveryPointKey("first", "stage"): true}, now)
	if !slices.Contains(blocked, "referenced") {
		t.Fatalf("a rollback-referenced point is not blocked: %v", blocked)
	}
}

func TestPruneRecoveryPointsKeepsNewestTwo(t *testing.T) {
	runtime, authority := budgetRuntime(t)
	seedRecoveryPoints(t, runtime, authority, 40*24*time.Hour, "first", "second", "third")
	pruned, err := runtime.PruneRecoveryPoints(time.Now().UTC(), map[string]bool{}, 2)
	if err != nil || len(pruned) != 1 || pruned[0].ID != "point-first" {
		t.Fatalf("pruned = %v, err = %v", pruned, err)
	}
	points, err := loadRecoveryPoints(runtime.recoveryDirectory())
	if err != nil || len(points) != 2 || points[1].Operation != "third" {
		t.Fatalf("points = %+v, err = %v", points, err)
	}
	if _, err := os.Stat(filepath.Join(runtime.recoveryDirectory(), "pruned-point-first.json")); err != nil {
		t.Fatalf("prune left no deletion receipt: %v", err)
	}
	again, err := runtime.PruneRecoveryPoints(time.Now().UTC(), map[string]bool{}, 2)
	if err != nil || len(again) != 0 {
		t.Fatalf("second prune = %v, err = %v", again, err)
	}
}

func TestPruneRecoveryPointsStopsAtReferenceAndRetention(t *testing.T) {
	runtime, authority := budgetRuntime(t)
	seedRecoveryPoints(t, runtime, authority, 40*24*time.Hour, "first", "second", "third")
	pruned, err := runtime.PruneRecoveryPoints(time.Now().UTC(), map[string]bool{recoveryPointKey("first", "stage"): true}, 2)
	if err != nil || len(pruned) != 0 {
		t.Fatalf("referenced point was pruned: %v, err = %v", pruned, err)
	}
	young, err := runtime.PruneRecoveryPoints(time.Now().UTC().Add(-recoveryRetention+time.Hour), map[string]bool{}, 2)
	if err != nil || len(young) != 0 {
		t.Fatalf("retained point was pruned: %v, err = %v", young, err)
	}
}

func TestRecoveryPointEndpoints(t *testing.T) {
	runtime, authority := budgetRuntime(t)
	seedRecoveryPoints(t, runtime, authority, 40*24*time.Hour, "first", "second", "third")
	db, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := httptest.NewServer(NewServer(NewManager(db, runtime, time.Minute, time.Now), nil).Handler())
	defer server.Close()
	response, err := http.Get(server.URL + "/v1/recovery-points")
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Value struct {
			Total  int `json:"total"`
			Budget int `json:"budget_units"`
			Points []struct {
				Eligible bool `json:"eligible"`
			} `json:"points"`
		} `json:"value"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, decode = %v", response.StatusCode, err)
	}
	_ = response.Body.Close()
	if len(listed.Value.Points) != 3 || listed.Value.Budget != 3 || !listed.Value.Points[0].Eligible || listed.Value.Points[1].Eligible {
		t.Fatalf("listing = %+v", listed.Value)
	}
	pruned, err := http.Post(server.URL+"/v1/recovery-points/prune", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Value struct {
			Pruned []struct {
				ID string `json:"id"`
			} `json:"pruned"`
		} `json:"value"`
	}
	if err := json.NewDecoder(pruned.Body).Decode(&result); err != nil || len(result.Value.Pruned) != 1 || result.Value.Pruned[0].ID != "point-first" {
		t.Fatalf("prune response = %+v, err = %v", result.Value, err)
	}
	_ = pruned.Body.Close()
}

func TestTransactionExposesRecordedCause(t *testing.T) {
	runtime, _ := budgetRuntime(t)
	db, err := store.Open(filepath.Join(t.TempDir(), "deployment.sqlite3"), 30*24*time.Hour, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	manager := NewManager(db, runtime, time.Minute, time.Now)
	transaction := store.Transaction{ID: "tx-cause", Phase: PhaseStage, State: "staged", TargetSlot: "green", PreviousSlot: "blue", Bundle: "/b", Generation: 1}
	if err := db.Create(t.Context(), transaction, nil); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{"message": "deployment stage failed", "cause": "open <private-path> permission denied"}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	if err := db.RecordFailure(t.Context(), transaction.ID, PhaseStage, "deployment_failed", "deployd", hex.EncodeToString(digest[:]), map[string]any{"message": "deployment stage failed", "cause": "open <private-path> permission denied"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	value, err := manager.Transaction(t.Context(), transaction.ID)
	if err != nil {
		t.Fatal(err)
	}
	failure, ok := value["failure"].(map[string]any)
	if !ok || failure["cause"] != "open <private-path> permission denied" {
		t.Fatalf("transaction exposes no cause: %+v", value["failure"])
	}
	server := httptest.NewServer(NewServer(manager, nil).Handler())
	defer server.Close()
	response, err := http.Post(server.URL+"/v1/stage", "application/json", strings.NewReader(`{"target_slot":"green","bundle":"/b"}`))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Error struct {
			Reason string `json:"reason"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if !strings.Contains(envelope.Error.Reason, "refusing") && envelope.Error.Reason == "" {
		t.Fatalf("error response carries no reason: %+v", envelope.Error)
	}
}
