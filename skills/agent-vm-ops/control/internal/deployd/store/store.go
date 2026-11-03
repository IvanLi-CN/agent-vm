package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = 1

const LatestObservationID = "__latest__"

const schema = `
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS deployment_transactions (
  transaction_id TEXT PRIMARY KEY,
  target_slot TEXT NOT NULL,
  previous_slot TEXT NOT NULL,
  bundle TEXT NOT NULL,
  phase TEXT NOT NULL,
  state TEXT NOT NULL,
  generation INTEGER NOT NULL,
  rollback_used INTEGER NOT NULL DEFAULT 0,
  recovery_required INTEGER NOT NULL DEFAULT 0,
  error_code TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS deployment_transactions_state_updated
  ON deployment_transactions(state, updated_at);
CREATE UNIQUE INDEX IF NOT EXISTS deployment_transactions_one_active
  ON deployment_transactions((1))
  WHERE state NOT IN ('finalized','rolled_back','failed');
CREATE TABLE IF NOT EXISTS deployment_phase_events (
  event_id INTEGER PRIMARY KEY AUTOINCREMENT,
  transaction_id TEXT NOT NULL,
  sequence INTEGER NOT NULL,
  phase TEXT NOT NULL,
  transition TEXT NOT NULL,
  source TEXT NOT NULL,
  evidence_json TEXT NOT NULL,
  created_at TEXT NOT NULL,
  UNIQUE(transaction_id, sequence),
  FOREIGN KEY(transaction_id) REFERENCES deployment_transactions(transaction_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS deployment_failure_evidence (
  evidence_id INTEGER PRIMARY KEY AUTOINCREMENT,
  transaction_id TEXT NOT NULL,
  phase TEXT NOT NULL,
  code TEXT NOT NULL,
  source TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  digest TEXT NOT NULL,
  created_at TEXT NOT NULL,
  FOREIGN KEY(transaction_id) REFERENCES deployment_transactions(transaction_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS deployment_observations (
  transaction_id TEXT PRIMARY KEY,
  observed_at TEXT NOT NULL,
  fresh_until TEXT NOT NULL,
  digest TEXT NOT NULL,
  payload_json TEXT NOT NULL,
  FOREIGN KEY(transaction_id) REFERENCES deployment_transactions(transaction_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS deployment_latest_observation (
  id INTEGER PRIMARY KEY CHECK(id = 1),
  observed_at TEXT NOT NULL,
  fresh_until TEXT NOT NULL,
  digest TEXT NOT NULL,
  payload_json TEXT NOT NULL
);`

type Transaction struct {
	ID               string    `json:"transaction_id"`
	TargetSlot       string    `json:"target_slot"`
	PreviousSlot     string    `json:"previous_slot"`
	Bundle           string    `json:"bundle"`
	Phase            string    `json:"phase"`
	State            string    `json:"state"`
	Generation       int64     `json:"generation"`
	RollbackUsed     bool      `json:"rollback_used"`
	RecoveryRequired bool      `json:"recovery_required"`
	ErrorCode        string    `json:"error_code,omitempty"`
	ErrorMessage     string    `json:"error_message,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type PhaseEvent struct {
	Sequence   int64          `json:"sequence"`
	Phase      string         `json:"phase"`
	Transition string         `json:"transition"`
	Source     string         `json:"source"`
	Evidence   map[string]any `json:"evidence,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
}

type FailureEvidence struct {
	Phase     string         `json:"phase"`
	Code      string         `json:"code"`
	Source    string         `json:"source"`
	Payload   map[string]any `json:"payload"`
	Digest    string         `json:"digest"`
	CreatedAt time.Time      `json:"created_at"`
}

type Observation struct {
	ObservedAt time.Time      `json:"observed_at"`
	FreshUntil time.Time      `json:"fresh_until"`
	Digest     string         `json:"digest"`
	Payload    map[string]any `json:"payload"`
}

type Store struct {
	db        *sql.DB
	path      string
	retention time.Duration
	clock     func() time.Time
	mu        sync.Mutex
}

func Open(path string, retention time.Duration, clock func() time.Time) (*Store, error) {
	if path == "" {
		return nil, errors.New("deployment database path is empty")
	}
	if clock == nil {
		clock = time.Now
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("deployment database parent: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open deployment sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: path, retention: retention, clock: clock}
	if err := store.configure(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure deployment sqlite: %w", err)
	}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) configure(ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA busy_timeout = 10000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) initialize(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("create deployment schema: %w", err)
	}
	var version string
	err = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'schema_version'").Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO meta(key, value) VALUES('schema_version', ?)", fmt.Sprint(schemaVersion)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("write deployment schema version: %w", err)
		}
	} else if err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("read deployment schema version: %w", err)
	} else if version != fmt.Sprint(schemaVersion) {
		_ = tx.Rollback()
		return fmt.Errorf("deployment schema version %s is unsupported", version)
	}
	return tx.Commit()
}

// NextFenceTerm allocates a term durably so a Deployd restart cannot reuse an
// older fencing token or depend on wall-clock monotonicity.
func (s *Store) NextFenceTerm(ctx context.Context) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	var encoded string
	err = tx.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'fencing_term'").Scan(&encoded)
	current := uint64(0)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	} else if err == nil {
		current, err = strconv.ParseUint(encoded, 10, 64)
	}
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("read fencing term: %w", err)
	}
	if current == ^uint64(0) {
		_ = tx.Rollback()
		return 0, errors.New("fencing term exhausted")
	}
	next := current + 1
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES('fencing_term',?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, strconv.FormatUint(next, 10)); err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("write fencing term: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit fencing term: %w", err)
	}
	return next, nil
}

func (s *Store) Create(ctx context.Context, transaction Transaction, evidence map[string]any) error {
	if transaction.ID == "" || transaction.TargetSlot == "" || transaction.Bundle == "" || transaction.Generation < 1 {
		return errors.New("deployment transaction identity is incomplete")
	}
	if transaction.CreatedAt.IsZero() {
		transaction.CreatedAt = s.clock().UTC()
	}
	if transaction.UpdatedAt.IsZero() {
		transaction.UpdatedAt = transaction.CreatedAt
	}
	if transaction.State == "" {
		transaction.State = "active"
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var activeID string
		activeErr := tx.QueryRowContext(ctx, `SELECT transaction_id FROM deployment_transactions
			WHERE state NOT IN ('finalized','rolled_back','failed') LIMIT 1`).Scan(&activeID)
		if activeErr == nil {
			return fmt.Errorf("deployment transaction %s is already active", activeID)
		}
		if !errors.Is(activeErr, sql.ErrNoRows) {
			return activeErr
		}
		rollback := boolInt(transaction.RollbackUsed)
		recovery := boolInt(transaction.RecoveryRequired)
		if _, err := tx.ExecContext(ctx, `INSERT INTO deployment_transactions
			(transaction_id,target_slot,previous_slot,bundle,phase,state,generation,rollback_used,recovery_required,error_code,error_message,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, transaction.ID, transaction.TargetSlot, transaction.PreviousSlot, transaction.Bundle, transaction.Phase, transaction.State, transaction.Generation, rollback, recovery, transaction.ErrorCode, transaction.ErrorMessage, formatTime(transaction.CreatedAt), formatTime(transaction.UpdatedAt)); err != nil {
			return err
		}
		return insertEvent(ctx, tx, transaction.ID, 1, transaction.Phase, "created", "deployd", evidence, transaction.CreatedAt)
	})
}

func (s *Store) Get(ctx context.Context, id string) (*Transaction, error) {
	row := s.db.QueryRowContext(ctx, `SELECT transaction_id,target_slot,previous_slot,bundle,phase,state,generation,rollback_used,recovery_required,error_code,error_message,created_at,updated_at FROM deployment_transactions WHERE transaction_id = ?`, id)
	return scanTransaction(row)
}

func (s *Store) ListActive(ctx context.Context) ([]Transaction, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT transaction_id,target_slot,previous_slot,bundle,phase,state,generation,rollback_used,recovery_required,error_code,error_message,created_at,updated_at FROM deployment_transactions WHERE state NOT IN ('finalized','rolled_back','failed') ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Transaction
	for rows.Next() {
		transaction, err := scanTransaction(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *transaction)
	}
	return result, rows.Err()
}

func (s *Store) LatestEvent(ctx context.Context, transactionID, phase, transition string) (*PhaseEvent, error) {
	row := s.db.QueryRowContext(ctx, `SELECT sequence,phase,transition,source,evidence_json,created_at
		FROM deployment_phase_events
		WHERE transaction_id = ? AND phase = ? AND transition = ?
		ORDER BY sequence DESC LIMIT 1`, transactionID, phase, transition)
	return scanPhaseEvent(row)
}

func (s *Store) LatestEventByTransition(ctx context.Context, transactionID, transition string) (*PhaseEvent, error) {
	row := s.db.QueryRowContext(ctx, `SELECT sequence,phase,transition,source,evidence_json,created_at
		FROM deployment_phase_events
		WHERE transaction_id = ? AND transition = ?
		ORDER BY sequence ASC LIMIT 1`, transactionID, transition)
	return scanPhaseEvent(row)
}

func (s *Store) LatestFailureEvidence(ctx context.Context, transactionID string) (*FailureEvidence, error) {
	row := s.db.QueryRowContext(ctx, `SELECT phase,code,source,payload_json,digest,created_at
		FROM deployment_failure_evidence
		WHERE transaction_id = ?
		ORDER BY evidence_id DESC LIMIT 1`, transactionID)
	var evidence FailureEvidence
	var payload, created string
	if err := row.Scan(&evidence.Phase, &evidence.Code, &evidence.Source, &payload, &evidence.Digest, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	digestValue := sha256.Sum256([]byte(payload))
	if evidence.Digest != hex.EncodeToString(digestValue[:]) {
		return nil, errors.New("persisted failure evidence digest mismatch")
	}
	if err := json.Unmarshal([]byte(payload), &evidence.Payload); err != nil {
		return nil, err
	}
	var err error
	evidence.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return nil, err
	}
	return &evidence, nil
}

func scanPhaseEvent(row scanner) (*PhaseEvent, error) {
	var event PhaseEvent
	var evidence, created string
	if err := row.Scan(&event.Sequence, &event.Phase, &event.Transition, &event.Source, &evidence, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(evidence), &event.Evidence); err != nil {
		return nil, err
	}
	var err error
	event.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return nil, err
	}
	return &event, nil
}

func (s *Store) Transition(ctx context.Context, id, expectedPhase, phase, state, transition, source string, evidence map[string]any, now time.Time) (*Transaction, error) {
	if now.IsZero() {
		now = s.clock().UTC()
	}
	var result *Transaction
	err := s.write(ctx, func(tx *sql.Tx) error {
		var currentPhase string
		var sequence int64
		if err := tx.QueryRowContext(ctx, "SELECT phase FROM deployment_transactions WHERE transaction_id = ?", id).Scan(&currentPhase); err != nil {
			return err
		}
		if currentPhase != expectedPhase {
			return fmt.Errorf("deployment phase conflict: expected %s, got %s", expectedPhase, currentPhase)
		}
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence), 0) + 1 FROM deployment_phase_events WHERE transaction_id = ?", id).Scan(&sequence); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE deployment_transactions SET phase = ?, state = ?, updated_at = ?, recovery_required = ? WHERE transaction_id = ?", phase, state, formatTime(now), boolInt(state == "needs_recovery"), id); err != nil {
			return err
		}
		if err := insertEvent(ctx, tx, id, sequence, phase, transition, source, evidence, now); err != nil {
			return err
		}
		var scanErr error
		result, scanErr = scanTransaction(tx.QueryRowContext(ctx, `SELECT transaction_id,target_slot,previous_slot,bundle,phase,state,generation,rollback_used,recovery_required,error_code,error_message,created_at,updated_at FROM deployment_transactions WHERE transaction_id = ?`, id))
		return scanErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) SetRollbackUsed(ctx context.Context, id string, now time.Time) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE deployment_transactions SET rollback_used = 1, updated_at = ? WHERE transaction_id = ?", formatTime(now), id)
		return err
	})
}

// BeginRollback atomically records the one permitted rollback and its phase
// event. A failed event insert must not consume the rollback budget.
func (s *Store) BeginRollback(ctx context.Context, id, expectedPhase, phase, transition, source string, evidence map[string]any, now time.Time) (*Transaction, error) {
	if now.IsZero() {
		now = s.clock().UTC()
	}
	var result *Transaction
	err := s.write(ctx, func(tx *sql.Tx) error {
		var currentPhase string
		var rollbackUsed int
		if err := tx.QueryRowContext(ctx, "SELECT phase, rollback_used FROM deployment_transactions WHERE transaction_id = ?", id).Scan(&currentPhase, &rollbackUsed); err != nil {
			return err
		}
		if currentPhase != expectedPhase {
			return fmt.Errorf("deployment phase conflict: expected %s, got %s", expectedPhase, currentPhase)
		}
		if rollbackUsed != 0 {
			return errors.New("transaction rollback is already used")
		}
		var sequence int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence), 0) + 1 FROM deployment_phase_events WHERE transaction_id = ?", id).Scan(&sequence); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE deployment_transactions
			SET phase = ?, state = 'active', rollback_used = 1, recovery_required = 0, updated_at = ?
			WHERE transaction_id = ?`, phase, formatTime(now), id); err != nil {
			return err
		}
		if err := insertEvent(ctx, tx, id, sequence, phase, transition, source, evidence, now); err != nil {
			return err
		}
		var scanErr error
		result, scanErr = scanTransaction(tx.QueryRowContext(ctx, `SELECT transaction_id,target_slot,previous_slot,bundle,phase,state,generation,rollback_used,recovery_required,error_code,error_message,created_at,updated_at FROM deployment_transactions WHERE transaction_id = ?`, id))
		return scanErr
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) RecordFailure(ctx context.Context, id, phase, code, source, digest string, payload map[string]any, now time.Time) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	digestValue := sha256.Sum256([]byte(encoded))
	if digest != hex.EncodeToString(digestValue[:]) {
		return errors.New("failure evidence digest does not match payload")
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		var currentPhase, currentState string
		if err := tx.QueryRowContext(ctx, "SELECT phase, state FROM deployment_transactions WHERE transaction_id = ?", id).Scan(&currentPhase, &currentState); err != nil {
			return err
		}
		if currentState == "finalized" || currentState == "rolled_back" || currentState == "failed" || currentState == "needs_recovery" {
			return nil
		}
		if currentPhase != phase {
			return fmt.Errorf("deployment failure phase conflict: expected %s, got %s", phase, currentPhase)
		}
		errorMessage := code
		if message, ok := payload["message"].(string); ok && message != "" {
			errorMessage = message
		}
		recoveryRequired := code == "state_unverified"
		state := "failed"
		transition := "failed"
		if recoveryRequired {
			state = "needs_recovery"
			transition = "needs_recovery"
		}
		var sequence int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence), 0) + 1 FROM deployment_phase_events WHERE transaction_id = ?", id).Scan(&sequence); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO deployment_failure_evidence(transaction_id,phase,code,source,payload_json,digest,created_at) VALUES(?,?,?,?,?,?,?)", id, phase, code, source, encoded, digest, formatTime(now)); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE deployment_transactions SET error_code = ?, error_message = ?, recovery_required = ?, state = ?, updated_at = ?
			WHERE transaction_id = ? AND phase = ? AND state NOT IN ('finalized','rolled_back','failed','needs_recovery')`, code, errorMessage, boolInt(recoveryRequired), state, formatTime(now), id, phase)
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return err
		} else if affected != 1 {
			return errors.New("deployment failure state changed concurrently")
		}
		return insertEvent(ctx, tx, id, sequence, phase, transition, source, payload, now)
	})
}

func (s *Store) SaveObservation(ctx context.Context, id string, observation Observation) error {
	payload, err := json.Marshal(observation.Payload)
	if err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		if id == LatestObservationID {
			_, err := tx.ExecContext(ctx, `INSERT INTO deployment_latest_observation(id,observed_at,fresh_until,digest,payload_json)
				VALUES(1,?,?,?,?) ON CONFLICT(id) DO UPDATE SET observed_at=excluded.observed_at,fresh_until=excluded.fresh_until,digest=excluded.digest,payload_json=excluded.payload_json`, formatTime(observation.ObservedAt), formatTime(observation.FreshUntil), observation.Digest, payload)
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO deployment_observations(transaction_id,observed_at,fresh_until,digest,payload_json)
			VALUES(?,?,?,?,?) ON CONFLICT(transaction_id) DO UPDATE SET observed_at=excluded.observed_at,fresh_until=excluded.fresh_until,digest=excluded.digest,payload_json=excluded.payload_json`, id, formatTime(observation.ObservedAt), formatTime(observation.FreshUntil), observation.Digest, payload)
		return err
	})
}

func (s *Store) Observation(ctx context.Context, id string) (*Observation, error) {
	var observed, fresh, digest, payload string
	query := "SELECT observed_at,fresh_until,digest,payload_json FROM deployment_observations WHERE transaction_id = ?"
	args := []any{id}
	if id == LatestObservationID {
		query = "SELECT observed_at,fresh_until,digest,payload_json FROM deployment_latest_observation WHERE id = 1"
		args = nil
	}
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&observed, &fresh, &digest, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	digestValue := sha256.Sum256([]byte(payload))
	if digest != hex.EncodeToString(digestValue[:]) {
		return nil, errors.New("persisted observation digest mismatch")
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	observedAt, err := time.Parse(time.RFC3339Nano, observed)
	if err != nil {
		return nil, err
	}
	freshUntil, err := time.Parse(time.RFC3339Nano, fresh)
	if err != nil {
		return nil, err
	}
	return &Observation{ObservedAt: observedAt, FreshUntil: freshUntil, Digest: digest, Payload: value}, nil
}

func (s *Store) Cleanup(ctx context.Context, now time.Time) (int64, error) {
	if s.retention <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-s.retention)
	returnValue := int64(0)
	err := s.write(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM deployment_transactions WHERE state IN ('finalized','rolled_back','failed') AND recovery_required = 0 AND updated_at < ?`, formatTime(cutoff))
		if err != nil {
			return err
		}
		returnValue, err = result.RowsAffected()
		return err
	})
	return returnValue, err
}

func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

func insertEvent(ctx context.Context, tx *sql.Tx, id string, sequence int64, phase, transition, source string, evidence map[string]any, now time.Time) error {
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO deployment_phase_events(transaction_id,sequence,phase,transition,source,evidence_json,created_at) VALUES(?,?,?,?,?,?,?)", id, sequence, phase, transition, source, encoded, formatTime(now))
	return err
}

type scanner interface{ Scan(dest ...any) error }

func scanTransaction(row scanner) (*Transaction, error) {
	var value Transaction
	var rollback, recovery int
	var created, updated string
	if err := row.Scan(&value.ID, &value.TargetSlot, &value.PreviousSlot, &value.Bundle, &value.Phase, &value.State, &value.Generation, &rollback, &recovery, &value.ErrorCode, &value.ErrorMessage, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	var err error
	value.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return nil, err
	}
	value.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return nil, err
	}
	value.RollbackUsed = rollback != 0
	value.RecoveryRequired = recovery != 0
	return &value, nil
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
