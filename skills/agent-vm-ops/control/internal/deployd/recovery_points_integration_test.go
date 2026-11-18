package deployd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// The deployment acceptance script supplies its own TLS authority and binary.
func TestRealRQLiteRecoveryPoint(t *testing.T) {
	endpoint := os.Getenv("AGENT_VM_TEST_RECOVERY_RQLITE_URL")
	if endpoint == "" {
		t.Skip("requires the isolated TLS rqlite acceptance fixture")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	config := Defaults()
	config.Migration.RQLiteURL = endpoint
	config.Migration.RQLiteCAFile = os.Getenv("AGENT_VM_TEST_RECOVERY_RQLITE_CA")
	config.Migration.RQLiteAuthFile = os.Getenv("AGENT_VM_TEST_RECOVERY_RQLITE_AUTH")
	config.Migration.RecoveryDirectory = filepath.Join(t.TempDir(), "recovery")
	runtime := NewHostRuntime(config)
	client, _, _, err := runtime.recoveryClient()
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	control := os.Getenv("AGENT_VM_TEST_RECOVERY_RQLITE_CONTROL")
	execute := func(ctx context.Context, statement string) error {
		_, err := recoverySQL(ctx, client, endpoint, "/db/execute", "agent-vm-control", control, []string{statement})
		return err
	}
	for _, q := range []string{
		"CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)",
		"INSERT INTO meta VALUES('state_epoch','acceptance-epoch') ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		"CREATE TABLE IF NOT EXISTS recovery_acceptance (id INTEGER PRIMARY KEY, value TEXT NOT NULL)",
		"INSERT INTO recovery_acceptance(value) VALUES('trusted-before-backup')",
	} {
		if err := execute(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	writeCtx, stopWrites := context.WithCancel(ctx)
	writes := make(chan error, 1)
	working := make(chan struct{})
	go func() {
		var err error
		defer func() { writes <- err }()
		for n := 0; writeCtx.Err() == nil; n++ {
			err = execute(writeCtx, "INSERT INTO recovery_acceptance(value) VALUES('concurrent-write')")
			if n == 0 {
				close(working)
			}
			if err != nil {
				if writeCtx.Err() != nil {
					err = nil
				}
				return
			}
		}
	}()
	defer func() { stopWrites(); <-writes }()
	<-working
	now := time.Now().UTC()
	authority := runtime.RecoveryAuthority()
	if err := runtime.CreateRecoveryPoint(ctx, "tx-acceptance", "stage", authority, nil, now); err != nil {
		t.Fatal(err)
	}
	points, err := loadRecoveryPoints(runtime.recoveryDirectory())
	if err != nil || len(points) != 1 {
		t.Fatalf("registered recovery points = %v (%v)", len(points), err)
	}
	point := points[0]
	if err := runtime.CreateRecoveryPoint(ctx, "tx-acceptance", "stage", authority, nil, now); err != nil {
		t.Fatalf("resume could not reuse verified point: %v", err)
	}
	oldSQLite, err := os.Create(filepath.Join(config.Migration.RecoveryDirectory, "migration-budget-fixture.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(oldSQLite.Truncate(maxMigrationSourceBytes-point.Size), oldSQLite.Close()); err != nil {
		t.Fatal(err)
	}
	if err := runtime.CreateRecoveryPoint(ctx, "tx-budget", "rollback", authority, nil, now); !errors.Is(err, errBackupStorageExhausted) {
		t.Fatalf("legacy material did not consume capacity: %v", err)
	}
	restoreRecoveryPoint(t, ctx, filepath.Join(runtime.recoveryDirectory(), point.ID, "snapshot.sqlite3"), point.Boundary)
}

func restoreRecoveryPoint(t *testing.T, ctx context.Context, snapshot, boundary string) {
	t.Helper()
	address := func() string {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		return listener.Addr().String()
	}
	httpAddress, raftAddress := address(), address()
	process := exec.CommandContext(ctx, os.Getenv("AGENT_VM_TEST_RECOVERY_RQLITED"), "-node-id", "recovery-target", "-http-addr", httpAddress, "-raft-addr", raftAddress, t.TempDir())
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = process.Process.Kill(); _ = process.Wait() }()
	client := &http.Client{Timeout: 5 * time.Second}
	base := "http://" + httpAddress
	for {
		reply, err := client.Get(base + "/readyz")
		if err == nil {
			_ = reply.Body.Close()
			if reply.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("restore target did not become ready: %v", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	file, err := os.Open(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/db/load", file)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	reply, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(io.LimitReader(reply.Body, 4096))
	_ = reply.Body.Close()
	if reply.StatusCode != http.StatusOK || readErr != nil {
		t.Fatalf("restore failed: HTTP %d (%v): %s", reply.StatusCode, readErr, body)
	}
	for query, want := range map[string]string{
		recoveryMarkerQuery: boundary,
		"SELECT value FROM recovery_acceptance WHERE id=1": "trusted-before-backup",
		"SELECT value FROM recovery_acceptance WHERE id=2": "concurrent-write",
	} {
		result, err := recoverySQL(ctx, client, base, "/db/query?level=linearizable", "", "", []string{query})
		if err != nil || len(result.Values) != 1 || len(result.Values[0]) != 1 || result.Values[0][0] != want {
			t.Fatal(fmt.Errorf("restored coverage query %q: got %v (%v); want %q", query, result.Values, err, want))
		}
	}
}
