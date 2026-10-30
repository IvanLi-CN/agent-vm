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

type rqliteStatusIdentity struct {
	HTTP struct {
		BindAddr string `json:"bind_addr"`
	} `json:"http"`
	OS struct {
		PID int `json:"pid"`
	} `json:"os"`
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
	expectedSession := os.Getenv("AGENT_VM_SESSION_ID")
	if expectedSession == "" {
		t.Fatal("AGENT_VM_SESSION_ID is required when an rqlite descriptor is provided")
	}
	if expectedSession != descriptor.SessionID {
		t.Fatalf("rqlite session = %q, want current session %q", descriptor.SessionID, expectedSession)
	}
	parsed, err := url.Parse(descriptor.URL)
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() == "" || parsed.Port() == "" {
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
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(strings.TrimRight(descriptor.URL, "/") + "/status")
	if err != nil {
		t.Fatalf("read rqlite status identity: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("rqlite status identity HTTP status = %d", response.StatusCode)
	}
	var status rqliteStatusIdentity
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatalf("decode rqlite status identity: %v", err)
	}
	if status.OS.PID != descriptor.PID || status.HTTP.BindAddr != parsed.Host {
		t.Fatalf("rqlite status identity = pid %d bind %q, want pid %d bind %q", status.OS.PID, status.HTTP.BindAddr, descriptor.PID, parsed.Host)
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
	authStateDigest := "remote-state-" + runID
	if err := first.InsertOIDCTransaction(ctx, OIDCTransaction{
		TransactionID: "remote-transaction-" + runID, StateDigest: authStateDigest, NonceDigest: "remote-nonce-" + runID,
		CodeVerifierSealed: "sealed", BindingCookieDigest: "remote-binding-" + runID, ClientID: "client", RedirectURI: "https://console.example/api/v1/auth/oidc/callback",
		CreatedAt: "2026-10-29T00:00:00Z", ExpiresAt: "2099-10-29T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if transaction, err := second.GetOIDCTransaction(ctx, authStateDigest); err != nil || transaction == nil {
		t.Fatalf("remote OIDC transaction=%#v err=%v", transaction, err)
	}
	if _, consumed, err := second.ConsumeOIDCTransaction(ctx, authStateDigest, "2026-10-29T00:01:00Z"); err != nil || !consumed {
		t.Fatalf("remote OIDC consume consumed=%v err=%v", consumed, err)
	}
	if _, consumed, err := first.ConsumeOIDCTransaction(ctx, authStateDigest, "2026-10-29T00:01:00Z"); err != nil || consumed {
		t.Fatalf("remote OIDC replay consumed=%v err=%v", consumed, err)
	}
	remoteToken := AgentToken{
		TokenID: "remote-token-" + runID, TokenDigest: "remote-token-digest-" + runID, OperationID: "remote-token-operation-" + runID, RequestDigest: "remote-token-request-" + runID,
		Issuer: "https://issuer.example", Subject: "admin", Scopes: []string{"agent:read"}, CreatedAt: "2026-10-29T00:00:00Z", ExpiresAt: "2099-10-29T00:00:00Z", ResourceVersion: "token:initial-" + runID, PlaintextDisclosure: "returned",
	}
	remoteOperation := ManagementOperation{
		OperationID: remoteToken.OperationID, Action: "token_create", TargetKind: "token", TargetID: remoteToken.TokenID, Actor: "https://issuer.example\x00admin", Phase: "durable_state", Status: "succeeded",
		Request: map[string]any{"_request_digest": remoteToken.RequestDigest}, Result: map[string]any{"token_metadata": map[string]any{"token_id": remoteToken.TokenID}, "plaintext_disclosure": "returned"}, CreatedAt: remoteToken.CreatedAt, UpdatedAt: remoteToken.CreatedAt,
	}
	remoteAudit := ManagementAudit{EventID: "remote-token-event-" + runID, OperationID: stringPointer(remoteToken.OperationID), Action: "token_create", Actor: remoteOperation.Actor, Target: map[string]any{"kind": "token", "id": remoteToken.TokenID}, Outcome: "succeeded", Phase: "durable_state", OccurredAt: remoteToken.CreatedAt}
	if _, _, created, err := first.ClaimAgentTokenWithOperation(ctx, remoteToken, remoteOperation, remoteAudit); err != nil || !created {
		t.Fatalf("remote Token create created=%v err=%v", created, err)
	}
	if _, replayed, created, err := second.ClaimAgentTokenWithOperation(ctx, remoteToken, remoteOperation, remoteAudit); err != nil || created || replayed == nil {
		t.Fatalf("remote Token replay token=%#v created=%v err=%v", replayed, created, err)
	}
	if token, err := second.GetAgentTokenByDigest(ctx, remoteToken.TokenDigest); err != nil || token == nil {
		t.Fatalf("remote Token read token=%#v err=%v", token, err)
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

func TestRemoteOIDCConsumptionReplaysEpochConflict(t *testing.T) {
	rqliteURL := requireSessionOwnedRQLite(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	runID := randomHex(6)
	first, err := OpenForTest(rqliteURL)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenForTest(rqliteURL)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := first.EnsureManagementSchema(ctx); err != nil {
		t.Fatal(err)
	}
	stateDigest := "remote-oidc-conflict-" + runID
	if err := first.InsertOIDCTransaction(ctx, OIDCTransaction{
		TransactionID: "remote-oidc-transaction-" + runID, StateDigest: stateDigest, NonceDigest: "remote-oidc-nonce-" + runID,
		CodeVerifierSealed: "sealed", BindingCookieDigest: "remote-oidc-binding-" + runID, ClientID: "client", RedirectURI: "https://console.example/api/v1/auth/oidc/callback",
		CreatedAt: "2026-10-29T00:00:00Z", ExpiresAt: "2099-10-29T00:00:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	if transaction, err := first.GetOIDCTransaction(ctx, stateDigest); err != nil || transaction == nil {
		t.Fatalf("first remote OIDC transaction=%#v err=%v", transaction, err)
	}
	if transaction, err := second.GetOIDCTransaction(ctx, stateDigest); err != nil || transaction == nil {
		t.Fatalf("second remote OIDC transaction=%#v err=%v", transaction, err)
	}

	type result struct {
		consumed bool
		err      error
	}
	results := make(chan result, 2)
	for _, store := range []*Store{first, second} {
		go func(store *Store) {
			_, consumed, err := store.ConsumeOIDCTransaction(ctx, stateDigest, "2026-10-29T00:01:00Z")
			results <- result{consumed: consumed, err: err}
		}(store)
	}
	consumed := 0
	for range 2 {
		outcome := <-results
		if outcome.err != nil {
			t.Fatalf("remote OIDC consume: %v", outcome.err)
		}
		if outcome.consumed {
			consumed++
		}
	}
	if consumed != 1 {
		t.Fatalf("successful remote OIDC consumers = %d, want 1", consumed)
	}
}

func TestRemoteSyncUsesStrongEpochFenceForCachedMirror(t *testing.T) {
	var queries int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		queries++
		if request.Method != http.MethodGet || request.URL.Path != "/db/query" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		if request.URL.Query().Get("level") != "strong" {
			http.Error(writer, "epoch query was not strong", http.StatusBadRequest)
			return
		}
		if !strings.Contains(request.URL.Query().Get("q"), "state_epoch") {
			http.Error(writer, "query did not read state_epoch", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(remoteResponse{Results: []remoteResult{{
			Columns: []string{"value"}, Types: []string{"text"}, Values: [][]any{{"initial"}},
		}}})
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client(), syncedEpoch: "initial"}
	if err := backend.syncLocked(context.Background()); err != nil {
		t.Fatal(err)
	}
	if queries != 1 {
		t.Fatalf("cached sync issued %d strong epoch queries, want 1", queries)
	}
}

func TestRemoteSyncDoesNotAdvanceCacheWhenRefreshFailsAfterEpochChange(t *testing.T) {
	var queries int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		queries++
		if request.Method != http.MethodGet || request.URL.Path != "/db/query" {
			http.Error(writer, "unexpected request", http.StatusBadRequest)
			return
		}
		if strings.Contains(request.URL.Query().Get("q"), "state_epoch") {
			writer.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(writer).Encode(remoteResponse{Results: []remoteResult{{
				Columns: []string{"value"}, Types: []string{"text"}, Values: [][]any{{"0123456789abcdef0123456789abcdef"}},
			}}})
			return
		}
		http.Error(writer, "schema temporarily unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client(), syncedEpoch: "initial"}
	if err := backend.syncLocked(context.Background()); err == nil {
		t.Fatal("epoch-changed refresh unexpectedly succeeded")
	}
	if backend.syncedEpoch != "initial" {
		t.Fatalf("failed refresh advanced cached epoch to %q", backend.syncedEpoch)
	}
	if queries != 2 {
		t.Fatalf("failed refresh issued %d queries, want strong epoch plus schema", queries)
	}
}

func TestRemoteQueryRejectsMalformedResultShape(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		results []remoteResult
	}{
		{name: "empty", results: nil},
		{name: "missing fields", results: []remoteResult{{}}},
		{name: "multiple", results: []remoteResult{{}, {}}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(writer).Encode(remoteResponse{Results: testCase.results})
			}))
			defer server.Close()
			parsed, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			backend := &remoteBackend{base: parsed, client: server.Client()}
			if _, err := backend.query(context.Background(), "SELECT 1"); err == nil {
				t.Fatal("malformed query response was accepted")
			}
		})
	}
}

func TestRemoteQueryAcceptsEmptyResultWithoutValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"results":[{"columns":["value"],"types":["text"]}]}`))
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client()}
	result, err := backend.query(context.Background(), "SELECT value FROM empty")
	if err != nil {
		t.Fatal(err)
	}
	if result.Values == nil {
		t.Fatal("empty result values were not normalized")
	}
	if len(result.Values) != 0 {
		t.Fatalf("empty result values = %#v", result.Values)
	}
}

func TestRemoteQueryRejectsNullValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"results":[{"columns":["value"],"types":["text"],"values":null}]}`))
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client()}
	if _, err := backend.query(context.Background(), "SELECT value FROM empty"); err == nil {
		t.Fatal("null query values were accepted")
	}
}

func TestRemoteQueryPreservesLargeIntegerValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"results":[{"columns":["value"],"types":["INTEGER"],"values":[[9007199254740993]]}]}`))
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client()}
	result, err := backend.query(context.Background(), "SELECT value FROM large_values")
	if err != nil {
		t.Fatal(err)
	}
	number, ok := result.Values[0][0].(json.Number)
	if !ok || number.String() != "9007199254740993" {
		t.Fatalf("large integer value = %#v (%T)", result.Values[0][0], result.Values[0][0])
	}
	normalized, ok := normalizeRemoteValue(number, "INTEGER").(int64)
	if !ok || normalized != 9007199254740993 {
		t.Fatalf("normalized large integer = %#v (%T)", normalizeRemoteValue(number, "INTEGER"), normalizeRemoteValue(number, "INTEGER"))
	}
}

func TestRemoteQueryRejectsTopLevelError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(remoteResponse{Error: "query failed"})
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client()}
	if _, err := backend.query(context.Background(), "SELECT 1"); err == nil || !strings.Contains(err.Error(), "query failed") {
		t.Fatalf("top-level query error = %v", err)
	}
}

func TestRemoteResponseRejectsEmptyErrorWithErrorV2(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"error":"","error_v2":{"message":"query failed"},"results":[{"columns":["value"],"types":["text"],"values":[["ok"]]}]}`))
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client()}
	if _, err := backend.query(context.Background(), "SELECT 1"); err == nil || !strings.Contains(err.Error(), "error_v2") {
		t.Fatalf("empty top-level error with error_v2 = %v", err)
	}
}

func TestRemoteResponseRejectsTrailingJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"results":[{"columns":["value"],"types":["text"],"values":[["ok"]]}]}{"error":"trailing"}`))
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client()}
	if _, err := backend.query(context.Background(), "SELECT 1"); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing JSON response = %v", err)
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

func TestRemoteRequestRejectsExtraErrorResultAsUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(remoteResponse{Results: []remoteResult{
			{RowsAffected: 1},
			{Error: "unexpected extra result"},
		}})
	}))
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backend := &remoteBackend{base: parsed, client: server.Client()}
	_, err = backend.request(context.Background(), []remoteStatement{{query: "INSERT INTO meta(key, value) VALUES(?, ?)"}}, true)
	var statementErr *remoteStatementError
	if err == nil || errors.As(err, &statementErr) {
		t.Fatalf("extra result error = %#v", err)
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
		_ = json.NewEncoder(writer).Encode(remoteResponse{Results: []remoteResult{{
			Columns: []string{"value"},
			Types:   []string{"text"},
			Values:  [][]any{{"not-an-epoch"}},
		}}})
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

func TestMigrationRejectsMalformedTargetQueryWithoutWrite(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "source.sqlite3")
	source, err := OpenForTest(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			posts++
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(remoteResponse{Results: []remoteResult{{}}})
	}))
	defer server.Close()
	if err := migrateSQLiteToRQLite(context.Background(), sourcePath, server.URL, SQLiteMigrationGate{
		StateWriteFenced:    true,
		WorkersQuiescent:    true,
		SQLiteWritersClosed: true,
	}); err == nil {
		t.Fatal("malformed target query response was accepted")
	}
	if posts != 0 {
		t.Fatalf("malformed target query issued %d remote writes", posts)
	}
}

func TestMigrationAcceptsLegacyCoreSchemaFormatting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite3")
	db, err := openLegacyFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := requireSQLiteSchema(context.Background(), db); err != nil {
		t.Fatalf("legacy fixture schema rejected: %v", err)
	}
}

func TestLegacyMigrationSnapshotSynthesizesInitialEpoch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.sqlite3")
	db, err := openLegacyFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM operations WHERE operation_id = 'baseline-pending'"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotDB(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if snapshotHasStateEpoch(snapshot) {
		t.Fatal("legacy snapshot unexpectedly contained a state epoch")
	}
	if err := ensureSnapshotStateEpoch(&snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshotHasStateEpoch(snapshot) || snapshot.Epoch != "initial" {
		t.Fatalf("legacy snapshot epoch = %q, has_epoch=%v", snapshot.Epoch, snapshotHasStateEpoch(snapshot))
	}
	meta := snapshot.Tables["meta"]
	if len(meta.Rows) != 2 {
		t.Fatalf("legacy meta rows = %d, want 2", len(meta.Rows))
	}
}

func TestLegacyMigrationWithoutEpochReachesTargetPreflight(t *testing.T) {
	sourcePath := filepath.Join(t.TempDir(), "legacy.sqlite3")
	db, err := openLegacyFixture(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("DELETE FROM operations WHERE operation_id = 'baseline-pending'"); err != nil {
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
		if request.Method == http.MethodGet {
			_ = json.NewEncoder(writer).Encode(remoteResponse{Results: []remoteResult{{
				Columns: []string{"type", "name", "sql"},
				Types:   []string{"text", "text", "text"},
				Values:  [][]any{},
			}}})
			return
		}
		_ = json.NewEncoder(writer).Encode(remoteResponse{Results: []remoteResult{}})
	}))
	defer server.Close()
	err = migrateSQLiteToRQLite(context.Background(), sourcePath, server.URL, SQLiteMigrationGate{
		StateWriteFenced:    true,
		WorkersQuiescent:    true,
		SQLiteWritersClosed: true,
	})
	if err == nil || strings.Contains(err.Error(), "state epoch is missing") {
		t.Fatalf("legacy migration without epoch error = %v", err)
	}
	if posts != 1 {
		t.Fatalf("legacy migration without epoch POST count = %d, want 1", posts)
	}
}

func TestSchemaSQLNormalizationPreservesLiteralSemantics(t *testing.T) {
	canonical := "CREATE TABLE IF NOT EXISTS sample (value TEXT DEFAULT 'none')"
	if normalizeSchemaSQL(canonical) == normalizeSchemaSQL("CREATE TABLE sample (value TEXT DEFAULT 'NONE')") {
		t.Fatal("schema normalization changed a string literal")
	}
	if normalizeSchemaSQL(canonical) == normalizeSchemaSQL("CREATE TABLE sample (value TEXT DEFAULT 'IF NOT EXISTSnone')") {
		t.Fatal("schema normalization removed text inside a string literal")
	}
}

func TestRemoteOpenRejectsMalformedManagementSchemaBeforeWrite(t *testing.T) {
	canonical, err := canonicalCoreSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	values := make([][]any, 0, len(canonical)+1)
	for _, object := range canonical {
		values = append(values, []any{object.Type, object.Name, object.SQL})
	}
	values = append(values, []any{"table", "state_capabilities", "CREATE TABLE state_capabilities(incompatible TEXT)"})
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost {
			posts++
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(remoteResponse{Results: []remoteResult{{
			Columns: []string{"type", "name", "sql"},
			Types:   []string{"text", "text", "text"},
			Values:  values,
		}}})
	}))
	defer server.Close()
	if _, err := openRemote(server.URL, false); err == nil || !strings.Contains(err.Error(), "canonical management object") {
		t.Fatalf("malformed management schema error = %v", err)
	}
	if posts != 0 {
		t.Fatalf("malformed management schema issued %d remote writes", posts)
	}
}

func TestSupportedSchemaAcceptsLegacyManagementBeforeAuthBootstrap(t *testing.T) {
	core, err := canonicalCoreSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	management, err := canonicalManagementSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	objects := make([]remoteSchemaObject, 0, len(core)+len(management))
	for _, object := range core {
		objects = append(objects, object)
	}
	for key, object := range management {
		if isAuthManagementObject(key) {
			continue
		}
		objects = append(objects, object)
	}
	if err := requireSupportedSchema(context.Background(), objects); err != nil {
		t.Fatalf("legacy management schema rejected before additive auth bootstrap: %v", err)
	}
}

func TestSupportedSchemaAcceptsDurableAuthBeforeConfirmationBootstrap(t *testing.T) {
	core, err := canonicalCoreSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	management, err := canonicalManagementSchema(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	objects := make([]remoteSchemaObject, 0, len(core)+len(management)-1)
	for _, object := range core {
		objects = append(objects, object)
	}
	for key, object := range management {
		if key == "table:management_confirmations" {
			continue
		}
		objects = append(objects, object)
	}
	if err := requireSupportedSchema(context.Background(), objects); err != nil {
		t.Fatalf("durable auth schema rejected before confirmation bootstrap: %v", err)
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

func TestMigrationRejectsPartialManagementSchemaBeforeRemoteWrite(t *testing.T) {
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
	if _, err := db.Exec("DROP INDEX managed_images_one_default"); err != nil {
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
	if err := migrateSQLiteToRQLite(context.Background(), sourcePath, server.URL, SQLiteMigrationGate{
		StateWriteFenced:    true,
		WorkersQuiescent:    true,
		SQLiteWritersClosed: true,
	}); err == nil || !strings.Contains(err.Error(), "missing canonical management object") {
		t.Fatalf("partial management schema error = %v", err)
	}
	if posts != 0 {
		t.Fatalf("partial management schema issued %d remote writes", posts)
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

func TestApplySnapshotProvesEpochWhenOnlyEpochChanges(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var batch [][]any
		if err := json.NewDecoder(request.Body).Decode(&batch); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		foundEpochUpdate := false
		results := make([]remoteResult, len(batch))
		for index, statement := range batch {
			if len(statement) > 0 {
				if query, ok := statement[0].(string); ok && strings.HasPrefix(query, "UPDATE meta SET value") {
					foundEpochUpdate = true
				}
			}
			results[index].RowsAffected = 1
		}
		if !foundEpochUpdate {
			t.Errorf("epoch-only snapshot omitted guarded epoch update: %#v", batch)
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
	before := remoteSnapshot{
		Objects: map[string]remoteSchemaObject{},
		Tables: map[string]remoteTableSnapshot{
			"meta": {
				Name:       "meta",
				Columns:    []string{"key", "value"},
				PrimaryKey: []int{0},
				Rows:       map[string]remoteRow{"state_epoch": {Values: []any{"state_epoch", "initial"}}},
			},
		},
		Epoch: "initial",
	}
	after := before
	after.Tables = map[string]remoteTableSnapshot{
		"meta": {
			Name:       "meta",
			Columns:    []string{"key", "value"},
			PrimaryKey: []int{0},
			Rows:       map[string]remoteRow{"state_epoch": {Values: []any{"state_epoch", "0123456789abcdef0123456789abcdef"}}},
		},
	}
	after.Epoch = "0123456789abcdef0123456789abcdef"
	applied, err := backend.applySnapshot(context.Background(), before, after, before.Epoch, after.Epoch)
	if err != nil || !applied {
		t.Fatalf("epoch-only snapshot applied=%v err=%v", applied, err)
	}
}

func TestApplySnapshotAcceptsEquivalentSchemaFormatting(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var batch [][]any
		if err := json.NewDecoder(request.Body).Decode(&batch); err != nil {
			http.Error(writer, err.Error(), http.StatusBadRequest)
			return
		}
		results := make([]remoteResult, len(batch))
		for index := range results {
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
	before := remoteSnapshot{
		Objects: map[string]remoteSchemaObject{
			objectKey("table", "meta"): {Type: "table", Name: "meta", SQL: "CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)"},
		},
		Tables: map[string]remoteTableSnapshot{
			"meta": {
				Name:       "meta",
				Columns:    []string{"key", "value"},
				PrimaryKey: []int{0},
				Rows:       map[string]remoteRow{"state_epoch": {Values: []any{"state_epoch", "initial"}}},
			},
		},
		Epoch: "initial",
	}
	after := before
	after.Objects = map[string]remoteSchemaObject{
		objectKey("table", "meta"): {Type: "table", Name: "meta", SQL: "CREATE TABLE meta (\n  key TEXT PRIMARY KEY,\n  value TEXT NOT NULL\n)"},
	}
	applied, err := backend.applySnapshot(context.Background(), before, after, "initial", "initial")
	if err != nil || !applied {
		t.Fatalf("equivalent schema snapshot applied=%v err=%v", applied, err)
	}
}

func TestMigrationRejectsUnknownOperationStateBeforeRemoteWrite(t *testing.T) {
	testCases := []struct {
		name string
		seed func(context.Context, *Store) error
	}{
		{
			name: "operation outcome",
			seed: func(ctx context.Context, store *Store) error {
				_, err := store.ClaimOperation(ctx, "unknown-outcome", "acquire", nil, nil, map[string]any{"outcome": "mystery"})
				return err
			},
		},
		{
			name: "management status",
			seed: func(ctx context.Context, store *Store) error {
				if err := store.EnsureManagementSchema(ctx); err != nil {
					return err
				}
				_, err := store.ClaimManagementOperation(ctx, ManagementOperation{
					OperationID: "unknown-status",
					Action:      "test",
					TargetKind:  "test",
					TargetID:    "test",
					Actor:       "test",
					Reason:      "test",
					Phase:       "test",
					Status:      "mystery",
					Request:     map[string]any{},
				})
				return err
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "source.sqlite3")
			store, err := OpenForTest(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := testCase.seed(context.Background(), store); err != nil {
				store.Close()
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
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
			err = migrateSQLiteToRQLite(context.Background(), path, server.URL, SQLiteMigrationGate{
				StateWriteFenced:    true,
				WorkersQuiescent:    true,
				SQLiteWritersClosed: true,
			})
			if err == nil || !strings.Contains(err.Error(), "unsupported") {
				t.Fatalf("unknown state error = %v", err)
			}
			if posts != 0 {
				t.Fatalf("unknown state issued %d remote writes", posts)
			}
		})
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
