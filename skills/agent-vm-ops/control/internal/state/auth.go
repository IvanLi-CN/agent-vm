package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// OIDCTransaction contains only the values required to validate and consume a
// one-time callback. Authorization codes and the raw login cookie are never
// part of this record; the service stores their digests or sealed verifier.
type OIDCTransaction struct {
	TransactionID       string
	StateDigest         string
	NonceDigest         string
	CodeVerifierSealed  string
	BindingCookieDigest string
	ClientID            string
	RedirectURI         string
	CreatedAt           string
	ExpiresAt           string
	ConsumedAt          *string
}

type BrowserSession struct {
	SessionDigest    string
	Issuer           string
	Subject          string
	CSRFSecretDigest string
	Capabilities     []string
	CreatedAt        string
	ExpiresAt        string
	RevokedAt        *string
}

// ManagementConfirmation is metadata for a short-lived, one-use browser
// confirmation. The raw confirmation value is never part of this record.
type ManagementConfirmation struct {
	ConfirmationDigest string
	Subject            string
	Action             string
	Target             string
	TargetVersion      string
	ParamsDigest       string
	CreatedAt          string
	ExpiresAt          string
	ConsumedAt         *string
}

type ManagementConfirmationClaim struct {
	ConfirmationDigest string
	Subject            string
	Action             string
	Target             string
	TargetVersion      string
	ParamsDigest       string
}

// AgentToken is deliberately metadata-only. TokenDigest is a one-way
// verifier, and PlaintextDisclosure records that the bearer was disclosed in
// the creation response without storing that bearer in state.
type AgentToken struct {
	TokenID             string
	TokenDigest         string
	OperationID         string
	RequestDigest       string
	Issuer              string
	Subject             string
	Scopes              []string
	CreatedAt           string
	ExpiresAt           string
	ResourceVersion     string
	RevokedAt           *string
	PlaintextDisclosure string
}

func (s *Store) InsertOIDCTransaction(ctx context.Context, value OIDCTransaction) error {
	if value.TransactionID == "" || value.StateDigest == "" || value.NonceDigest == "" || value.CodeVerifierSealed == "" || value.BindingCookieDigest == "" || value.ClientID == "" || value.RedirectURI == "" || value.CreatedAt == "" || value.ExpiresAt == "" {
		return errors.New("invalid OIDC transaction")
	}
	return s.withWrite(ctx, func(tx *Tx) error {
		_, err := tx.exec(`INSERT INTO oidc_transactions(transaction_id, state_digest, nonce_digest, code_verifier_sealed, binding_cookie_digest, client_id, redirect_uri, created_at, expires_at, consumed_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`, value.TransactionID, value.StateDigest, value.NonceDigest, value.CodeVerifierSealed, value.BindingCookieDigest, value.ClientID, value.RedirectURI, value.CreatedAt, value.ExpiresAt)
		return err
	})
}

func (s *Store) GetOIDCTransaction(ctx context.Context, stateDigest string) (*OIDCTransaction, error) {
	if stateDigest == "" {
		return nil, errors.New("state digest is empty")
	}
	var value *OIDCTransaction
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		candidate, err := scanOIDCTransaction(conn.QueryRowContext(ctx, `SELECT transaction_id, state_digest, nonce_digest, code_verifier_sealed, binding_cookie_digest, client_id, redirect_uri, created_at, expires_at, consumed_at FROM oidc_transactions WHERE state_digest = ?`, stateDigest))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			value = candidate
		}
		return err
	})
	return value, err
}

// ConsumeOIDCTransaction atomically claims an unexpired callback transaction.
// The returned boolean is true only for the caller that changed consumed_at.
func (s *Store) ConsumeOIDCTransaction(ctx context.Context, stateDigest, now string) (*OIDCTransaction, bool, error) {
	if stateDigest == "" || now == "" {
		return nil, false, errors.New("state digest and current time are required")
	}
	var value *OIDCTransaction
	consumed := false
	err := s.withWrite(ctx, func(tx *Tx) error {
		// A remote epoch conflict can replay this callback. Clear the result
		// from the previous speculative attempt before reading the mirror again.
		value = nil
		consumed = false
		candidate, err := scanOIDCTransaction(tx.conn.QueryRowContext(ctx, `SELECT transaction_id, state_digest, nonce_digest, code_verifier_sealed, binding_cookie_digest, client_id, redirect_uri, created_at, expires_at, consumed_at FROM oidc_transactions WHERE state_digest = ?`, stateDigest))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		value = candidate
		if candidate.ConsumedAt != nil || candidate.ExpiresAt <= now {
			return nil
		}
		result, err := tx.exec("UPDATE oidc_transactions SET consumed_at = ? WHERE state_digest = ? AND consumed_at IS NULL AND expires_at > ?", now, stateDigest, now)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return nil
		}
		consumedAt := now
		value.ConsumedAt = &consumedAt
		consumed = true
		return nil
	})
	return value, consumed, err
}

func (s *Store) InsertBrowserSession(ctx context.Context, value BrowserSession) error {
	if value.SessionDigest == "" || value.Issuer == "" || value.Subject == "" || value.CSRFSecretDigest == "" || value.CreatedAt == "" || value.ExpiresAt == "" {
		return errors.New("invalid browser session")
	}
	capabilities, err := json.Marshal(value.Capabilities)
	if err != nil {
		return err
	}
	return s.withWrite(ctx, func(tx *Tx) error {
		_, err := tx.exec(`INSERT INTO browser_sessions(session_digest, issuer, subject, csrf_digest, capabilities_json, created_at, expires_at, revoked_at)
VALUES(?, ?, ?, ?, ?, ?, ?, NULL)`, value.SessionDigest, value.Issuer, value.Subject, value.CSRFSecretDigest, string(capabilities), value.CreatedAt, value.ExpiresAt)
		return err
	})
}

func (s *Store) GetBrowserSession(ctx context.Context, sessionDigest string) (*BrowserSession, error) {
	if sessionDigest == "" {
		return nil, errors.New("session digest is empty")
	}
	var value *BrowserSession
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		candidate, err := scanBrowserSession(conn.QueryRowContext(ctx, `SELECT session_digest, issuer, subject, csrf_digest, capabilities_json, created_at, expires_at, revoked_at FROM browser_sessions WHERE session_digest = ?`, sessionDigest))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			value = candidate
		}
		return err
	})
	return value, err
}

func (s *Store) RevokeBrowserSession(ctx context.Context, sessionDigest, now string) error {
	if sessionDigest == "" || now == "" {
		return errors.New("session digest and current time are required")
	}
	return s.withWrite(ctx, func(tx *Tx) error {
		_, err := tx.exec("UPDATE browser_sessions SET revoked_at = COALESCE(revoked_at, ?) WHERE session_digest = ?", now, sessionDigest)
		return err
	})
}

// InsertManagementConfirmation persists only a digest of the raw browser
// confirmation. A later slot can consume the same record atomically.
func (s *Store) InsertManagementConfirmation(ctx context.Context, value ManagementConfirmation) error {
	if value.ConfirmationDigest == "" || value.Subject == "" || value.Action == "" || value.Target == "" || value.TargetVersion == "" || value.ParamsDigest == "" || value.CreatedAt == "" || value.ExpiresAt == "" {
		return errors.New("invalid management confirmation")
	}
	return s.withWrite(ctx, func(tx *Tx) error {
		_, err := tx.execWithoutEpoch(`INSERT INTO management_confirmations(confirmation_digest, subject, action, target, target_version, params_digest, created_at, expires_at, consumed_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, NULL)`, value.ConfirmationDigest, value.Subject, value.Action, value.Target, value.TargetVersion, value.ParamsDigest, value.CreatedAt, value.ExpiresAt)
		return err
	})
}

// ConsumeManagementConfirmation atomically validates and consumes a
// confirmation. Exactly one concurrent caller can change consumed_at.
func (s *Store) ConsumeManagementConfirmation(ctx context.Context, confirmationDigest, subject, action, target, targetVersion, paramsDigest, now string) (bool, error) {
	claim := ManagementConfirmationClaim{ConfirmationDigest: confirmationDigest, Subject: subject, Action: action, Target: target, TargetVersion: targetVersion, ParamsDigest: paramsDigest}
	if err := validateManagementConfirmationClaim(claim, now); err != nil {
		return false, err
	}
	consumed := false
	err := s.withWrite(ctx, func(tx *Tx) error {
		var err error
		consumed, err = consumeManagementConfirmationTx(tx, claim, now)
		return err
	})
	return consumed, err
}

func validateManagementConfirmationClaim(claim ManagementConfirmationClaim, now string) error {
	if claim.ConfirmationDigest == "" || claim.Subject == "" || claim.Action == "" || claim.Target == "" || claim.TargetVersion == "" || claim.ParamsDigest == "" || now == "" {
		return errors.New("confirmation identity and current time are required")
	}
	return nil
}

func consumeManagementConfirmationTx(tx *Tx, claim ManagementConfirmationClaim, now string) (bool, error) {
	if err := validateManagementConfirmationClaim(claim, now); err != nil {
		return false, err
	}
	result, err := tx.execWithoutEpoch(`UPDATE management_confirmations
SET consumed_at = ?
WHERE confirmation_digest = ? AND subject = ? AND action = ? AND target = ? AND target_version = ? AND params_digest = ? AND consumed_at IS NULL AND expires_at > ?`, now, claim.ConfirmationDigest, claim.Subject, claim.Action, claim.Target, claim.TargetVersion, claim.ParamsDigest, now)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

func (s *Store) InsertAgentToken(ctx context.Context, value AgentToken) error {
	if value.TokenID == "" || value.TokenDigest == "" || value.OperationID == "" || value.RequestDigest == "" || value.Issuer == "" || value.Subject == "" || len(value.Scopes) == 0 || value.CreatedAt == "" || value.ExpiresAt == "" || value.ResourceVersion == "" {
		return errors.New("invalid agent token")
	}
	if value.PlaintextDisclosure == "" {
		value.PlaintextDisclosure = "returned"
	}
	scopes, err := json.Marshal(value.Scopes)
	if err != nil {
		return err
	}
	return s.withWrite(ctx, func(tx *Tx) error {
		_, err := tx.exec(`INSERT INTO agent_tokens(token_id, token_digest, operation_id, request_digest, issuer, subject, scopes_json, created_at, expires_at, resource_version, revoked_at, plaintext_disclosure)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`, value.TokenID, value.TokenDigest, value.OperationID, value.RequestDigest, value.Issuer, value.Subject, string(scopes), value.CreatedAt, value.ExpiresAt, value.ResourceVersion, value.PlaintextDisclosure)
		return err
	})
}

// ClaimAgentTokenWithOperation persists a newly issued Token and its terminal
// management operation in one state transaction. The caller keeps the bearer
// only in memory; this method accepts metadata and never has a plaintext field
// to persist.
func (s *Store) ClaimAgentTokenWithOperation(ctx context.Context, token AgentToken, operation ManagementOperation, audit ManagementAudit) (*ManagementOperation, *AgentToken, bool, error) {
	if token.TokenID == "" || token.TokenDigest == "" || token.OperationID == "" || token.RequestDigest == "" || token.Issuer == "" || token.Subject == "" || len(token.Scopes) == 0 || token.CreatedAt == "" || token.ExpiresAt == "" || token.ResourceVersion == "" {
		return nil, nil, false, errors.New("invalid agent token")
	}
	if operation.OperationID == "" || operation.Action == "" || operation.TargetKind == "" || operation.TargetID == "" || operation.Actor == "" || operation.Status == "" || operation.Phase == "" {
		return nil, nil, false, errors.New("invalid token operation")
	}
	if operation.OperationID != token.OperationID || operation.Action != "token_create" || operation.TargetKind != "token" || operation.TargetID != token.TokenID {
		return nil, nil, false, errors.New("token operation identity does not match token metadata")
	}
	if audit.OperationID != nil && *audit.OperationID != operation.OperationID {
		return nil, nil, false, errors.New("token audit identity does not match token operation")
	}
	if operation.CreatedAt == "" {
		operation.CreatedAt = token.CreatedAt
	}
	if operation.UpdatedAt == "" {
		operation.UpdatedAt = operation.CreatedAt
	}
	if token.PlaintextDisclosure == "" {
		token.PlaintextDisclosure = "returned"
	}
	requestJSON, err := json.Marshal(operation.Request)
	if err != nil {
		return nil, nil, false, err
	}
	resultJSON, err := jsonNullable(operation.Result)
	if err != nil {
		return nil, nil, false, err
	}
	errorJSON, err := jsonNullable(operation.Error)
	if err != nil {
		return nil, nil, false, err
	}
	auditTarget, err := json.Marshal(audit.Target)
	if err != nil {
		return nil, nil, false, err
	}
	auditDetail, err := jsonNullable(audit.Detail)
	if err != nil {
		return nil, nil, false, err
	}
	scopes, err := json.Marshal(token.Scopes)
	if err != nil {
		return nil, nil, false, err
	}
	var existing *ManagementOperation
	var existingToken *AgentToken
	created := false
	err = s.withWrite(ctx, func(tx *Tx) error {
		// A remote epoch conflict can replay this callback. Do not return a
		// speculative result from the failed attempt when the mirror is refreshed.
		existing = nil
		existingToken = nil
		created = false
		current, scanErr := scanManagementOperation(ctx, tx.conn, operation.OperationID)
		if scanErr == nil {
			existing = current
			candidate, tokenErr := scanAgentToken(tx.conn.QueryRowContext(ctx, agentTokenSelect+" WHERE operation_id = ?", operation.OperationID))
			if tokenErr == nil {
				existingToken = candidate
				return nil
			}
			if errors.Is(tokenErr, sql.ErrNoRows) {
				return nil
			}
			return tokenErr
		}
		if !errors.Is(scanErr, sql.ErrNoRows) {
			return scanErr
		}
		if _, err := tx.exec(`INSERT INTO management_operations(operation_id, action, target_kind, target_id, actor, reason, phase, status, request_json, result_json, error_json, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, operation.OperationID, operation.Action, operation.TargetKind, operation.TargetID, operation.Actor, operation.Reason, operation.Phase, operation.Status, string(requestJSON), resultJSON, errorJSON, operation.CreatedAt, operation.UpdatedAt); err != nil {
			return err
		}
		if err := recordManagementAuditTx(tx, audit, string(auditTarget), auditDetail, audit.OccurredAtOr(operation.CreatedAt)); err != nil {
			return err
		}
		if _, err := tx.exec(`INSERT INTO agent_tokens(token_id, token_digest, operation_id, request_digest, issuer, subject, scopes_json, created_at, expires_at, resource_version, revoked_at, plaintext_disclosure)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`, token.TokenID, token.TokenDigest, token.OperationID, token.RequestDigest, token.Issuer, token.Subject, string(scopes), token.CreatedAt, token.ExpiresAt, token.ResourceVersion, token.PlaintextDisclosure); err != nil {
			return err
		}
		created = true
		return nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	if existing != nil {
		return existing, existingToken, false, nil
	}
	return &operation, &token, created, nil
}

func (s *Store) GetAgentToken(ctx context.Context, tokenID string) (*AgentToken, error) {
	if tokenID == "" {
		return nil, errors.New("token ID is empty")
	}
	var value *AgentToken
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		candidate, err := scanAgentToken(conn.QueryRowContext(ctx, agentTokenSelect+" WHERE token_id = ?", tokenID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			value = candidate
		}
		return err
	})
	return value, err
}

func (s *Store) GetAgentTokenByDigest(ctx context.Context, digest string) (*AgentToken, error) {
	if digest == "" {
		return nil, errors.New("token digest is empty")
	}
	var value *AgentToken
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		candidate, err := scanAgentToken(conn.QueryRowContext(ctx, agentTokenSelect+" WHERE token_digest = ?", digest))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			value = candidate
		}
		return err
	})
	return value, err
}

func (s *Store) GetAgentTokenByOperation(ctx context.Context, operationID string) (*AgentToken, error) {
	if operationID == "" {
		return nil, errors.New("operation ID is empty")
	}
	var value *AgentToken
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		candidate, err := scanAgentToken(conn.QueryRowContext(ctx, agentTokenSelect+" WHERE operation_id = ?", operationID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			value = candidate
		}
		return err
	})
	return value, err
}

func (s *Store) ListAgentTokens(ctx context.Context, limit int) ([]AgentToken, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	values := make([]AgentToken, 0, limit)
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, agentTokenSelect+" ORDER BY created_at DESC, token_id DESC LIMIT ?", limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			value, err := scanAgentToken(rows)
			if err != nil {
				return err
			}
			values = append(values, *value)
		}
		return rows.Err()
	})
	return values, err
}

// ListAgentTokensPage returns the stable descending Token inventory page. The
// cursor is the last (created_at, token_id) pair from the preceding page.
func (s *Store) ListAgentTokensPage(ctx context.Context, afterCreatedAt, afterTokenID string, limit int) ([]AgentToken, bool, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	values := make([]AgentToken, 0, limit)
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		query := agentTokenSelect + " ORDER BY created_at DESC, token_id DESC LIMIT ?"
		args := []any{limit + 1}
		if afterCreatedAt != "" || afterTokenID != "" {
			query = agentTokenSelect + " WHERE created_at < ? OR (created_at = ? AND token_id < ?) ORDER BY created_at DESC, token_id DESC LIMIT ?"
			args = []any{afterCreatedAt, afterCreatedAt, afterTokenID, limit + 1}
		}
		rows, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			value, err := scanAgentToken(rows)
			if err != nil {
				return err
			}
			values = append(values, *value)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, false, err
	}
	more := len(values) > limit
	if more {
		values = values[:limit]
	}
	return values, more, nil
}

// RevokeAgentToken performs compare-and-act on the current resource version.
// A false result means the target was already revoked or its version changed.
func (s *Store) RevokeAgentToken(ctx context.Context, tokenID, expectedVersion, nextVersion, now string) (*AgentToken, bool, error) {
	if tokenID == "" || expectedVersion == "" || nextVersion == "" || now == "" {
		return nil, false, errors.New("token ID, versions and current time are required")
	}
	var value *AgentToken
	changed := false
	err := s.withWrite(ctx, func(tx *Tx) error {
		// A remote epoch conflict can replay this callback after another slot
		// revoked or changed the token.
		value = nil
		changed = false
		candidate, err := scanAgentToken(tx.conn.QueryRowContext(ctx, agentTokenSelect+" WHERE token_id = ?", tokenID))
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		value = candidate
		if candidate.RevokedAt != nil || candidate.ResourceVersion != expectedVersion {
			return nil
		}
		result, err := tx.exec("UPDATE agent_tokens SET revoked_at = ?, resource_version = ? WHERE token_id = ? AND resource_version = ? AND revoked_at IS NULL", now, nextVersion, tokenID, expectedVersion)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows == 1 {
			revokedAt := now
			value.RevokedAt = &revokedAt
			value.ResourceVersion = nextVersion
			changed = true
		}
		return nil
	})
	return value, changed, err
}

// RevokeAgentTokenWithOperation commits the revocation and its terminal
// operation/audit record in one transaction. The conditional version update is
// the authorization fence used by every caller.
func (s *Store) RevokeAgentTokenWithOperation(ctx context.Context, tokenID, expectedVersion, nextVersion, now string, operation ManagementOperation, audit ManagementAudit) (*ManagementOperation, *AgentToken, bool, error) {
	return s.revokeAgentTokenWithOperation(ctx, tokenID, expectedVersion, nextVersion, now, nil, operation, audit)
}

// RevokeAgentTokenWithOperationAndConfirmation commits the one-use browser
// confirmation and Token revocation in the same state transaction.
func (s *Store) RevokeAgentTokenWithOperationAndConfirmation(ctx context.Context, tokenID, expectedVersion, nextVersion, now string, confirmation ManagementConfirmationClaim, operation ManagementOperation, audit ManagementAudit) (*ManagementOperation, *AgentToken, bool, error) {
	return s.revokeAgentTokenWithOperation(ctx, tokenID, expectedVersion, nextVersion, now, &confirmation, operation, audit)
}

func (s *Store) revokeAgentTokenWithOperation(ctx context.Context, tokenID, expectedVersion, nextVersion, now string, confirmation *ManagementConfirmationClaim, operation ManagementOperation, audit ManagementAudit) (*ManagementOperation, *AgentToken, bool, error) {
	if tokenID == "" || expectedVersion == "" || nextVersion == "" || now == "" || operation.OperationID == "" || operation.Action == "" || operation.TargetKind == "" || operation.TargetID == "" || operation.Actor == "" {
		return nil, nil, false, errors.New("token, version, time and operation are required")
	}
	if operation.Action != "token_revoke" || operation.TargetKind != "token" || operation.TargetID != tokenID {
		return nil, nil, false, errors.New("token revoke operation identity does not match target")
	}
	if audit.OperationID != nil && *audit.OperationID != operation.OperationID {
		return nil, nil, false, errors.New("token audit identity does not match revoke operation")
	}
	if confirmation != nil {
		if err := validateManagementConfirmationClaim(*confirmation, now); err != nil {
			return nil, nil, false, err
		}
		if confirmation.Action != operation.Action || confirmation.Target != operation.TargetKind+":"+operation.TargetID || confirmation.TargetVersion != expectedVersion {
			return nil, nil, false, errors.New("confirmation identity does not match token revoke operation")
		}
	}
	operation.Status = "succeeded"
	if operation.Phase == "" {
		operation.Phase = "durable_state"
	}
	if operation.CreatedAt == "" {
		operation.CreatedAt = now
	}
	if operation.UpdatedAt == "" {
		operation.UpdatedAt = now
	}
	if operation.Result == nil {
		operation.Result = map[string]any{"effect": "revoked", "token_id": tokenID}
	}
	requestJSON, err := json.Marshal(operation.Request)
	if err != nil {
		return nil, nil, false, err
	}
	resultJSON, err := jsonNullable(operation.Result)
	if err != nil {
		return nil, nil, false, err
	}
	errorJSON, err := jsonNullable(operation.Error)
	if err != nil {
		return nil, nil, false, err
	}
	auditTarget, err := json.Marshal(audit.Target)
	if err != nil {
		return nil, nil, false, err
	}
	auditDetail, err := jsonNullable(audit.Detail)
	if err != nil {
		return nil, nil, false, err
	}
	var existing *ManagementOperation
	var value *AgentToken
	changed := false
	err = s.withWrite(ctx, func(tx *Tx) error {
		// A remote epoch conflict can replay this callback. Clear the result from
		// the previous speculative attempt before reading the refreshed mirror.
		existing = nil
		value = nil
		changed = false
		current, scanErr := scanManagementOperation(ctx, tx.conn, operation.OperationID)
		if scanErr == nil {
			existing = current
			candidate, tokenErr := scanAgentToken(tx.conn.QueryRowContext(ctx, agentTokenSelect+" WHERE token_id = ?", tokenID))
			if tokenErr == nil {
				value = candidate
				return nil
			}
			if errors.Is(tokenErr, sql.ErrNoRows) {
				return nil
			}
			return tokenErr
		}
		if !errors.Is(scanErr, sql.ErrNoRows) {
			return scanErr
		}
		candidate, tokenErr := scanAgentToken(tx.conn.QueryRowContext(ctx, agentTokenSelect+" WHERE token_id = ?", tokenID))
		if errors.Is(tokenErr, sql.ErrNoRows) {
			return errors.New("not_found: agent token was not found")
		}
		if tokenErr != nil {
			return tokenErr
		}
		value = candidate
		if candidate.RevokedAt != nil {
			return errors.New("operation_conflict: agent token is already revoked")
		}
		if candidate.ResourceVersion != expectedVersion {
			return fmt.Errorf("stale_target: expected %s, current %s", expectedVersion, candidate.ResourceVersion)
		}
		if confirmation != nil {
			consumed, err := consumeManagementConfirmationTx(tx, *confirmation, now)
			if err != nil {
				return err
			}
			if !consumed {
				return errors.New("precondition_required: confirmation token is invalid or expired")
			}
		}
		if _, err := tx.exec(`INSERT INTO management_operations(operation_id, action, target_kind, target_id, actor, reason, phase, status, request_json, result_json, error_json, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, operation.OperationID, operation.Action, operation.TargetKind, operation.TargetID, operation.Actor, operation.Reason, operation.Phase, operation.Status, string(requestJSON), resultJSON, errorJSON, operation.CreatedAt, operation.UpdatedAt); err != nil {
			return err
		}
		if _, err := tx.exec("UPDATE agent_tokens SET revoked_at = ?, resource_version = ? WHERE token_id = ? AND resource_version = ? AND revoked_at IS NULL", now, nextVersion, tokenID, expectedVersion); err != nil {
			return err
		}
		revokedAt := now
		value.RevokedAt = &revokedAt
		value.ResourceVersion = nextVersion
		if err := recordManagementAuditTx(tx, audit, string(auditTarget), auditDetail, audit.OccurredAtOr(now)); err != nil {
			return err
		}
		changed = true
		return nil
	})
	if err != nil {
		return nil, nil, false, err
	}
	if existing != nil {
		return existing, value, false, nil
	}
	return &operation, value, changed, nil
}

const agentTokenSelect = `SELECT token_id, token_digest, operation_id, request_digest, issuer, subject, scopes_json, created_at, expires_at, resource_version, revoked_at, plaintext_disclosure FROM agent_tokens`

func scanOIDCTransaction(row scanner) (*OIDCTransaction, error) {
	var value OIDCTransaction
	var consumed sql.NullString
	if err := row.Scan(&value.TransactionID, &value.StateDigest, &value.NonceDigest, &value.CodeVerifierSealed, &value.BindingCookieDigest, &value.ClientID, &value.RedirectURI, &value.CreatedAt, &value.ExpiresAt, &consumed); err != nil {
		return nil, err
	}
	value.ConsumedAt = nullableString(consumed)
	return &value, nil
}

func scanBrowserSession(row scanner) (*BrowserSession, error) {
	var value BrowserSession
	var capabilities, revoked sql.NullString
	if err := row.Scan(&value.SessionDigest, &value.Issuer, &value.Subject, &value.CSRFSecretDigest, &capabilities, &value.CreatedAt, &value.ExpiresAt, &revoked); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(capabilities.String), &value.Capabilities); err != nil {
		return nil, fmt.Errorf("decode browser session capabilities: %w", err)
	}
	value.RevokedAt = nullableString(revoked)
	return &value, nil
}

func scanAgentToken(row scanner) (*AgentToken, error) {
	var value AgentToken
	var scopes string
	var revoked sql.NullString
	if err := row.Scan(&value.TokenID, &value.TokenDigest, &value.OperationID, &value.RequestDigest, &value.Issuer, &value.Subject, &scopes, &value.CreatedAt, &value.ExpiresAt, &value.ResourceVersion, &revoked, &value.PlaintextDisclosure); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(scopes), &value.Scopes); err != nil {
		return nil, fmt.Errorf("decode agent token scopes: %w", err)
	}
	value.RevokedAt = nullableString(revoked)
	return &value, nil
}

func (audit ManagementAudit) OccurredAtOr(fallback string) string {
	if audit.OccurredAt != "" {
		return audit.OccurredAt
	}
	return fallback
}
