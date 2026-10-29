package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type rqliteTestDescriptor struct {
	SessionID   string `json:"session_id"`
	URL         string `json:"url"`
	PID         int    `json:"pid"`
	DataDir     string `json:"data_dir"`
	CommandLine string `json:"command_line"`
}

func requireSessionOwnedRQLite(t *testing.T) string {
	t.Helper()
	descriptorPath := os.Getenv("AGENT_VM_RQLITE_DESCRIPTOR")
	if descriptorPath == "" {
		t.Skip("AGENT_VM_RQLITE_DESCRIPTOR is required for the isolated rqlite integration contract")
	}
	contents, err := os.ReadFile(descriptorPath)
	if err != nil {
		t.Fatalf("read rqlite descriptor: %v", err)
	}
	var descriptor rqliteTestDescriptor
	if err := json.Unmarshal(contents, &descriptor); err != nil {
		t.Fatalf("decode rqlite descriptor: %v", err)
	}
	if descriptor.SessionID == "" || descriptor.URL == "" || descriptor.PID <= 0 || descriptor.DataDir == "" {
		t.Fatalf("rqlite descriptor is incomplete: %#v", descriptor)
	}
	if expected := os.Getenv("AGENT_VM_SESSION_ID"); expected != "" && expected != descriptor.SessionID {
		t.Fatalf("rqlite session = %q, want current session %q", descriptor.SessionID, expected)
	}
	parsed, err := url.Parse(descriptor.URL)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() == "" {
		t.Fatalf("rqlite descriptor URL is invalid: %q", descriptor.URL)
	}
	switch parsed.Hostname() {
	case "127.0.0.1", "localhost", "::1":
	default:
		t.Fatalf("rqlite descriptor URL is not loopback: %q", descriptor.URL)
	}
	if !filepath.IsAbs(descriptor.DataDir) || filepath.Clean(descriptor.DataDir) == string(filepath.Separator) || !strings.HasPrefix(filepath.Clean(descriptor.DataDir), filepath.Clean(os.TempDir())+string(filepath.Separator)) {
		t.Fatalf("rqlite data directory is not session-temporary: %q", descriptor.DataDir)
	}
	if runtime.GOOS != "linux" {
		t.Skip("isolated rqlite process identity verification requires Linux /proc")
	}
	commandBytes, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", descriptor.PID))
	if err != nil {
		t.Fatalf("read rqlite process identity: %v", err)
	}
	commandLine := strings.ReplaceAll(string(commandBytes), "\x00", " ")
	if !strings.Contains(commandLine, "rqlited") {
		t.Fatalf("descriptor PID is not rqlited: %q", commandLine)
	}
	if !strings.Contains(commandLine, descriptor.DataDir) {
		cwd, cwdErr := os.Readlink(fmt.Sprintf("/proc/%d/cwd", descriptor.PID))
		if cwdErr != nil || (cwd != descriptor.DataDir && !strings.HasPrefix(cwd, filepath.Clean(descriptor.DataDir)+string(filepath.Separator))) {
			t.Fatalf("rqlite process is not bound to descriptor data directory: command=%q cwd=%q data_dir=%q", commandLine, cwd, descriptor.DataDir)
		}
	}
	return descriptor.URL
}

func TestRemoteMigrationPreservesStateAndSerializesConcurrentOperations(t *testing.T) {
	rqliteURL := requireSessionOwnedRQLite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	runID := randomHex(6)
	sessionID := "migration-session-" + runID
	taskID := "migration-task-" + runID
	operationID := "migration-operation-" + runID
	managementOperationID := "migration-management-operation-" + runID
	instanceName := "agent-vm-migration-task-" + runID
	t.Log("creating SQLite source")
	sourcePath := filepath.Join(t.TempDir(), "state.sqlite3")
	source, err := OpenForTest(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := source.NextGeneration(ctx, sessionID)
	if err != nil {
		source.Close()
		t.Fatal(err)
	}
	if err := source.InsertTask(ctx, Task{
		TaskID:                  taskID,
		SessionID:               sessionID,
		Generation:              generation,
		State:                   "active",
		InstanceName:            instanceName,
		CreatedAt:               "2026-10-29T00:00:00Z",
		UpdatedAt:               "2026-10-29T00:00:00Z",
		ImageFingerprint:        strings.Repeat("a", 64),
		ResourcesJSON:           "{}",
		ResourceReservationJSON: "{}",
	}); err != nil {
		source.Close()
		t.Fatal(err)
	}
	if err := source.SeedLegacyImage(ctx, strings.Repeat("a", 64)); err != nil {
		source.Close()
		t.Fatal(err)
	}
	if _, err := source.ClaimOperation(ctx, operationID, "acquire", &sessionID, &generation, map[string]any{"outcome": "success"}); err != nil {
		source.Close()
		t.Fatal(err)
	}
	if err := source.Audit(ctx, "acquire", taskID, sessionID, "success", &generation, map[string]any{"actor": "legacy_unknown"}); err != nil {
		source.Close()
		t.Fatal(err)
	}
	if _, err := source.ClaimManagementOperation(ctx, ManagementOperation{
		OperationID: managementOperationID,
		Action:      "image-import",
		TargetKind:  "image",
		TargetID:    "migration-image",
		Actor:       "migration-test",
		Reason:      "exercise migration fence",
		Phase:       "queued",
		Status:      "queued",
		Request:     map[string]any{"fingerprint": strings.Repeat("a", 64)},
		CreatedAt:   "2026-10-29T00:00:00Z",
		UpdatedAt:   "2026-10-29T00:00:00Z",
	}); err != nil {
		source.Close()
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	if err := migrateSQLiteToRQLite(ctx, sourcePath, rqliteURL, SQLiteMigrationGate{
		StateWriteFenced:    true,
		WorkersQuiescent:    true,
		SQLiteWritersClosed: true,
	}); err == nil {
		t.Fatal("migration with a queued management operation succeeded")
	}
	source, err = OpenForTest(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.UpdateManagementOperation(ctx, managementOperationID, func(value *ManagementOperation) error {
		value.Phase = "completed"
		value.Status = "succeeded"
		value.Result = map[string]any{"outcome": "success"}
		value.UpdatedAt = "2026-10-29T00:00:01Z"
		return nil
	}); err != nil {
		source.Close()
		t.Fatal(err)
	}
	expectedEpoch, err := source.StateEpoch(ctx)
	if err != nil {
		source.Close()
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}

	if err := migrateSQLiteToRQLite(ctx, sourcePath, rqliteURL, SQLiteMigrationGate{}); err == nil {
		t.Fatal("migration without all fence proofs succeeded")
	}
	if err := migrateSQLiteToRQLite(ctx, sourcePath, rqliteURL, SQLiteMigrationGate{
		StateWriteFenced:    true,
		WorkersQuiescent:    true,
		SQLiteWritersClosed: true,
	}); err != nil {
		t.Fatal(err)
	}
	t.Log("migration complete")

	first, err := OpenForTest(rqliteURL)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	t.Log("first remote store open")
	second, err := OpenForTest(rqliteURL)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	t.Log("second remote store open")

	gotEpoch, err := first.StateEpoch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if gotEpoch != expectedEpoch {
		t.Fatalf("state epoch changed during migration: got %q want %q", gotEpoch, expectedEpoch)
	}
	gotTask, err := second.GetTask(ctx, map[string]any{"task_id": taskID})
	if err != nil {
		t.Fatal(err)
	}
	if gotTask == nil || gotTask.Generation != generation || gotTask.SessionID != sessionID {
		t.Fatalf("migrated task = %#v", gotTask)
	}
	gotOperation, err := first.GetOperation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	if gotOperation == nil || gotOperation.Response["outcome"] != "success" {
		t.Fatalf("migrated operation = %#v", gotOperation)
	}
	t.Log("migration contents verified")
	var managementErrors [2]error
	var managementWait sync.WaitGroup
	managementWait.Add(2)
	go func() {
		defer managementWait.Done()
		managementErrors[0] = first.EnsureManagementSchema(ctx)
	}()
	go func() {
		defer managementWait.Done()
		managementErrors[1] = second.EnsureManagementSchema(ctx)
	}()
	managementWait.Wait()
	for index, err := range managementErrors {
		if err != nil {
			t.Fatalf("concurrent management bootstrap %d: %v", index, err)
		}
	}
	first.remote.mu.Lock()
	beforeGuard, err := snapshotDB(ctx, first.remote.mirror)
	if err != nil {
		first.remote.mu.Unlock()
		t.Fatal(err)
	}
	afterGuard := beforeGuard
	metaTable := beforeGuard.Tables["meta"]
	afterMeta := metaTable
	afterMeta.Rows = make(map[string]remoteRow, len(metaTable.Rows)+1)
	for key, row := range metaTable.Rows {
		afterMeta.Rows[key] = row
	}
	guardMetaKey := "migration-guard-" + runID
	afterMeta.Rows[guardMetaKey] = remoteRow{Values: []any{guardMetaKey, "must-rollback"}}
	afterGuard.Tables = make(map[string]remoteTableSnapshot, len(beforeGuard.Tables))
	for table, snapshot := range beforeGuard.Tables {
		afterGuard.Tables[table] = snapshot
	}
	afterGuard.Tables["meta"] = afterMeta
	_, err = first.remote.applySnapshot(ctx, beforeGuard, afterGuard, "00000000000000000000000000000000", beforeGuard.Epoch)
	first.remote.mu.Unlock()
	if err == nil {
		t.Fatal("stale epoch transaction unexpectedly succeeded")
	}
	if _, exists, err := first.Meta(ctx, guardMetaKey); err != nil {
		t.Fatal(err)
	} else if exists {
		t.Fatal("stale epoch transaction left a partial row")
	}
	if _, err := first.remote.request(ctx, []remoteStatement{
		{query: "INSERT INTO meta(key, value) VALUES('remote-atomicity-test', 'first')"},
		{query: "INSERT INTO meta(key, value) VALUES('remote-atomicity-test', 'second')"},
	}, true); err == nil {
		t.Fatal("failing rqlite transaction succeeded")
	}
	if _, exists, err := first.Meta(ctx, "remote-atomicity-test"); err != nil {
		t.Fatal(err)
	} else if exists {
		t.Fatal("failing rqlite transaction left a partial meta row")
	}

	const concurrentAllocations = 8
	generations := make([]int64, concurrentAllocations)
	errorsByWorker := make([]error, concurrentAllocations)
	stores := []*Store{first, second}
	var wait sync.WaitGroup
	for index := 0; index < concurrentAllocations; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			generations[index], errorsByWorker[index] = stores[index%len(stores)].NextGeneration(ctx, "concurrent-session")
		}(index)
	}
	wait.Wait()
	t.Log("concurrent generations complete")
	for index, err := range errorsByWorker {
		if err != nil {
			t.Fatalf("concurrent generation %d: %v", index, err)
		}
	}
	sort.Slice(generations, func(left, right int) bool { return generations[left] < generations[right] })
	for index, value := range generations {
		want := int64(index + 1)
		if value != want {
			t.Fatalf("concurrent generation[%d] = %d, want %d; all=%v", index, value, want, generations)
		}
	}

	condition := TaskCondition{Generation: &generation, States: []string{"active"}}
	deleted := make(chan bool, 2)
	go func() {
		value, err := first.DeleteTaskIf(ctx, taskID, condition)
		if err != nil {
			t.Errorf("first conditional delete: %v", err)
		}
		deleted <- value
	}()
	go func() {
		value, err := second.DeleteTaskIf(ctx, taskID, condition)
		if err != nil {
			t.Errorf("second conditional delete: %v", err)
		}
		deleted <- value
	}()
	firstDelete, secondDelete := <-deleted, <-deleted
	if firstDelete == secondDelete {
		t.Fatalf("conditional deletes = %v, %v; exactly one writer should win", firstDelete, secondDelete)
	}
	t.Log("conditional deletes complete")
	lastGeneration, exists, err := first.LastGeneration(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || lastGeneration != generation {
		t.Fatalf("generation tombstone = %d, %v; want %d, true", lastGeneration, exists, generation)
	}
}

func TestRemoteRequestRejectsErrorV2(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(remoteResponse{Results: []remoteResult{{ErrorV2: map[string]any{"code": "constraint", "message": "duplicate"}}}})
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client()}
	_, err = backend.request(context.Background(), []remoteStatement{{query: "INSERT INTO tasks(task_id) VALUES('duplicate')"}}, true)
	var statementErr *remoteStatementError
	if !errors.As(err, &statementErr) {
		t.Fatalf("error_v2 response error = %v", err)
	}
	if !strings.Contains(statementErr.Message, "constraint") {
		t.Fatalf("error_v2 message = %q", statementErr.Message)
	}
}

func TestRemoteRequestClassifiesPartialEpochRollback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(remoteResponse{Results: []remoteResult{
			{RowsAffected: 0},
			{Error: "NOT NULL constraint failed: meta.value"},
		}})
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client()}
	_, err = backend.request(context.Background(), []remoteStatement{
		{query: "INSERT INTO meta(key, value) SELECT ?, ? WHERE 0"},
		{query: "INSERT INTO meta(key, value) SELECT ?, NULL", epochGuard: true},
		{query: "UPDATE meta SET value = value"},
		{query: "INSERT INTO meta(key, value) VALUES(?, ?)"},
		{query: "DELETE FROM meta WHERE key = ?"},
	}, true)
	var statementErr *remoteStatementError
	if !errors.As(err, &statementErr) || !statementErr.EpochGuard {
		t.Fatalf("partial epoch rollback error = %#v", err)
	}
}

func TestRemoteRequestRejectsTruncatedSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(remoteResponse{Results: []remoteResult{{RowsAffected: 1}}})
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client()}
	_, err = backend.request(context.Background(), []remoteStatement{
		{query: "INSERT INTO meta(key, value) VALUES(?, ?)"},
		{query: "DELETE FROM meta WHERE key = ?"},
	}, true)
	var statementErr *remoteStatementError
	if err == nil || errors.As(err, &statementErr) {
		t.Fatalf("truncated success error = %#v", err)
	}
}

func TestRemoteRequestLostResponseIsNotRetried(t *testing.T) {
	var posts int
	var postsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		postsMu.Lock()
		posts++
		postsMu.Unlock()
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			t.Fatalf("test server does not support hijacking")
		}
		connection, _, err := hijacker.Hijack()
		if err != nil {
			t.Fatalf("hijack response: %v", err)
		}
		_ = connection.Close()
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client()}
	_, err = backend.request(context.Background(), []remoteStatement{{query: "SELECT 1"}}, true)
	if err == nil {
		t.Fatal("lost response unexpectedly succeeded")
	}
	var statementErr *remoteStatementError
	if errors.As(err, &statementErr) {
		t.Fatalf("lost response was classified as statement failure: %v", err)
	}
	postsMu.Lock()
	defer postsMu.Unlock()
	if posts != 1 {
		t.Fatalf("lost response request count = %d, want 1", posts)
	}
}

func TestRemoteEpochRejectsMalformedValue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(remoteResponse{Results: []remoteResult{{Values: [][]any{{"not-an-epoch"}}}}})
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client()}
	_, err = backend.remoteEpoch(context.Background())
	var epochErr *StateEpochError
	if !errors.As(err, &epochErr) {
		t.Fatalf("malformed epoch error = %v", err)
	}
}

func TestSQLiteReadOnlyURIOverridesWritableMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readonly.sqlite3")
	store, err := OpenForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := openSQLiteReadOnly("file:" + path + "?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE write_probe(value TEXT)"); err == nil {
		t.Fatal("read-only SQLite source accepted a write")
	}
}

func TestMigrationTargetPreflightDoesNotWrite(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.sqlite3")
	source, err := OpenForTest(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	var posts int
	var postsMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			postsMu.Lock()
			posts++
			postsMu.Unlock()
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(remoteResponse{Results: []remoteResult{{
			Columns: []string{"type", "name", "sql"},
			Values:  [][]any{{"table", "unexpected", "CREATE TABLE unexpected(value TEXT)"}},
		}}})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := migrateSQLiteToRQLite(ctx, sourcePath, server.URL, SQLiteMigrationGate{
		StateWriteFenced:    true,
		WorkersQuiescent:    true,
		SQLiteWritersClosed: true,
	}); err == nil {
		t.Fatal("migration accepted a non-canonical target")
	}
	postsMu.Lock()
	defer postsMu.Unlock()
	if posts != 0 {
		t.Fatalf("target preflight issued %d write requests", posts)
	}
}

func TestMigrationRejectsUnsupportedSourceSchemaBeforeRemoteWrite(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.sqlite3")
	source, err := OpenForTest(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE unsupported_source_state(value TEXT)"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			posts++
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(remoteResponse{Results: []remoteResult{}})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := migrateSQLiteToRQLite(ctx, sourcePath, server.URL, SQLiteMigrationGate{
		StateWriteFenced:    true,
		WorkersQuiescent:    true,
		SQLiteWritersClosed: true,
	}); err == nil || !strings.Contains(err.Error(), "unsupported object") {
		t.Fatalf("unsupported source schema error = %v", err)
	}
	if posts != 0 {
		t.Fatalf("unsupported source schema issued %d remote writes", posts)
	}
}

func TestApplySnapshotRequiresEpochProof(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var batch [][]any
		if err := json.NewDecoder(request.Body).Decode(&batch); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		results := make([]remoteResult, len(batch))
		for index, statement := range batch {
			if len(statement) > 0 {
				if query, ok := statement[0].(string); ok && strings.HasPrefix(query, "UPDATE meta") {
					results[index].RowsAffected = 0
					continue
				}
			}
			results[index].RowsAffected = 1
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(remoteResponse{Results: results})
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client()}
	before := remoteSnapshot{Objects: map[string]remoteSchemaObject{}, Tables: map[string]remoteTableSnapshot{}, Epoch: "initial"}
	after := remoteSnapshot{
		Objects: map[string]remoteSchemaObject{
			objectKey("table", "meta"): {Type: "table", Name: "meta", SQL: "CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)"},
		},
		Tables: map[string]remoteTableSnapshot{
			"meta": {
				Name:       "meta",
				Columns:    []string{"key", "value"},
				PrimaryKey: []int{0},
				Rows: map[string]remoteRow{
					"state_epoch": {Values: []any{"state_epoch", "initial"}},
				},
			},
		},
		Epoch: "initial",
	}
	if applied, err := backend.applySnapshot(context.Background(), before, after, "initial", "initial"); err == nil || applied {
		t.Fatalf("epoch proof failure = applied=%v err=%v", applied, err)
	}
}

func TestExportedMigrationEntryFailsClosed(t *testing.T) {
	if err := MigrateSQLiteToRQLite(context.Background(), "source.sqlite3", "http://127.0.0.1:4001", SQLiteMigrationGate{
		StateWriteFenced:    true,
		WorkersQuiescent:    true,
		SQLiteWritersClosed: true,
	}); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("exported migration entry error = %v", err)
	}
}
