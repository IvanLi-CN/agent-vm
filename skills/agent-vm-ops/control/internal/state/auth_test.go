package state

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
)

func TestOIDCTransactionIsConsumedExactlyOnce(t *testing.T) {
	store, err := OpenForTest(t.TempDir() + "/state.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.InsertOIDCTransaction(ctx, OIDCTransaction{
		TransactionID: "transaction-1", StateDigest: strings.Repeat("a", 64), NonceDigest: strings.Repeat("b", 64),
		CodeVerifierSealed: "sealed", BindingCookieDigest: strings.Repeat("c", 64), ClientID: "client", RedirectURI: "https://console.example/api/v1/auth/oidc/callback",
		CreatedAt: "2026-10-30T00:00:00Z", ExpiresAt: "2026-10-30T00:05:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	var mu sync.Mutex
	consumed := 0
	wait.Add(16)
	for i := 0; i < 16; i++ {
		go func() {
			defer wait.Done()
			_, ok, err := store.ConsumeOIDCTransaction(ctx, strings.Repeat("a", 64), "2026-10-30T00:01:00Z")
			if err != nil {
				t.Errorf("consume: %v", err)
				return
			}
			if ok {
				mu.Lock()
				consumed++
				mu.Unlock()
			}
		}()
	}
	wait.Wait()
	if consumed != 1 {
		t.Fatalf("successful OIDC consumers = %d, want 1", consumed)
	}
}

func TestManagementConfirmationIsCrossStoreSingleUse(t *testing.T) {
	path := t.TempDir() + "/state.sqlite3"
	first, err := OpenForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenForTest(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	ctx := context.Background()
	if err := first.InsertManagementConfirmation(ctx, ManagementConfirmation{
		ConfirmationDigest: strings.Repeat("a", 64), Subject: "issuer\x00admin", Action: "token_revoke", Target: "token:token-1", TargetVersion: "token:initial", ParamsDigest: strings.Repeat("b", 64), CreatedAt: "2026-10-30T00:00:00Z", ExpiresAt: "2026-10-30T00:05:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	var mu sync.Mutex
	consumed := 0
	wait.Add(2)
	for _, store := range []*Store{first, second} {
		go func(store *Store) {
			defer wait.Done()
			ok, err := store.ConsumeManagementConfirmation(ctx, strings.Repeat("a", 64), "issuer\x00admin", "token_revoke", "token:token-1", "token:initial", strings.Repeat("b", 64), "2026-10-30T00:01:00Z")
			if err != nil {
				t.Errorf("consume: %v", err)
				return
			}
			if ok {
				mu.Lock()
				consumed++
				mu.Unlock()
			}
		}(store)
	}
	wait.Wait()
	if consumed != 1 {
		t.Fatalf("successful cross-store consumers = %d, want 1", consumed)
	}
	if ok, err := first.ConsumeManagementConfirmation(ctx, strings.Repeat("a", 64), "issuer\x00admin", "token_revoke", "token:token-1", "token:initial", strings.Repeat("b", 64), "2026-10-30T00:02:00Z"); err != nil || ok {
		t.Fatalf("replay consume = %v, err=%v", ok, err)
	}
}

func TestAgentTokenLifecycleDoesNotPersistBearer(t *testing.T) {
	store, err := OpenForTest(t.TempDir() + "/state.sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	value := AgentToken{
		TokenID: "token-1234567890123456", TokenDigest: strings.Repeat("a", 64), OperationID: "operation-1234567890123456", RequestDigest: strings.Repeat("b", 64),
		Issuer: "https://issuer.example", Subject: "admin", Scopes: []string{"agent:read"}, CreatedAt: "2026-10-30T00:00:00Z", ExpiresAt: "2026-10-30T01:00:00Z", ResourceVersion: "token:initial", PlaintextDisclosure: "returned",
	}
	operation := ManagementOperation{
		OperationID: value.OperationID, Action: "token_create", TargetKind: "token", TargetID: value.TokenID, Actor: "https://issuer.example\x00admin", Phase: "durable_state", Status: "succeeded",
		Request: map[string]any{"_request_digest": value.RequestDigest}, Result: map[string]any{"token_metadata": map[string]any{"token_id": value.TokenID}, "plaintext_disclosure": "returned"}, CreatedAt: value.CreatedAt, UpdatedAt: value.CreatedAt,
	}
	audit := ManagementAudit{EventID: "event-1", OperationID: stringPointerForTest(value.OperationID), Action: "token_create", Actor: operation.Actor, Target: map[string]any{"kind": "token", "id": value.TokenID}, Outcome: "succeeded", Phase: "durable_state", OccurredAt: value.CreatedAt}
	createdOperation, createdToken, created, err := store.ClaimAgentTokenWithOperation(ctx, value, operation, audit)
	if err != nil || !created || createdOperation == nil || createdToken == nil {
		t.Fatalf("create operation=%#v token=%#v created=%v err=%v", createdOperation, createdToken, created, err)
	}
	replayedOperation, replayedToken, replayed, err := store.ClaimAgentTokenWithOperation(ctx, value, operation, audit)
	if err != nil || replayed || replayedOperation == nil || replayedToken == nil {
		t.Fatalf("replay operation=%#v token=%#v created=%v err=%v", replayedOperation, replayedToken, replayed, err)
	}
	if replayedToken.TokenDigest != value.TokenDigest || replayedToken.PlaintextDisclosure != "returned" {
		t.Fatalf("replayed token = %#v", replayedToken)
	}
	var schemaSQL string
	if err := store.withRead(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'agent_tokens'").Scan(&schemaSQL)
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(schemaSQL, "token_plaintext") || strings.Contains(schemaSQL, "bearer") {
		t.Fatalf("agent token schema contains plaintext storage: %s", schemaSQL)
	}
	if err := store.InsertManagementConfirmation(ctx, ManagementConfirmation{
		ConfirmationDigest: strings.Repeat("c", 64), Subject: operation.Actor, Action: "token_revoke", Target: "token:" + value.TokenID, TargetVersion: value.ResourceVersion, ParamsDigest: strings.Repeat("d", 64), CreatedAt: "2026-10-30T00:01:00Z", ExpiresAt: "2026-10-30T00:05:00Z",
	}); err != nil {
		t.Fatal(err)
	}
	const nextVersion = "token:revoked"
	revokedOperation, revokedToken, changed, err := store.RevokeAgentTokenWithOperationAndConfirmation(ctx, value.TokenID, value.ResourceVersion, nextVersion, "2026-10-30T00:02:00Z", ManagementConfirmationClaim{
		ConfirmationDigest: strings.Repeat("c", 64), Subject: operation.Actor, Action: "token_revoke", Target: "token:" + value.TokenID, TargetVersion: value.ResourceVersion, ParamsDigest: strings.Repeat("d", 64),
	}, ManagementOperation{
		OperationID: "operation-revoke-123456789012", Action: "token_revoke", TargetKind: "token", TargetID: value.TokenID, Actor: operation.Actor, Phase: "durable_state", Request: map[string]any{"_request_digest": "revoke"}, CreatedAt: "2026-10-30T00:02:00Z", UpdatedAt: "2026-10-30T00:02:00Z",
	}, ManagementAudit{EventID: "event-2", OperationID: stringPointerForTest("operation-revoke-123456789012"), Action: "token_revoke", Actor: operation.Actor, Target: map[string]any{"kind": "token", "id": value.TokenID}, Outcome: "succeeded", Phase: "durable_state", OccurredAt: "2026-10-30T00:02:00Z"})
	if err != nil || !changed || revokedOperation == nil || revokedToken == nil || revokedToken.RevokedAt == nil || revokedToken.ResourceVersion != nextVersion {
		t.Fatalf("revoke operation=%#v token=%#v changed=%v err=%v", revokedOperation, revokedToken, changed, err)
	}
}

func stringPointerForTest(value string) *string { return &value }
