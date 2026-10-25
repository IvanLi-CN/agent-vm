// Package state owns the durable Agent VM state boundary.
package state

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tasks (
  task_id TEXT PRIMARY KEY,
  session_id TEXT NOT NULL UNIQUE,
  generation INTEGER NOT NULL,
  state TEXT NOT NULL,
  instance_name TEXT NOT NULL UNIQUE,
  vm_ip TEXT,
  ssh_host_port INTEGER,
  lease_expires_at TEXT,
  inactive_since TEXT,
  cleanup_after TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  last_error TEXT,
  image_fingerprint TEXT NOT NULL,
  resources_json TEXT NOT NULL DEFAULT '{}',
  reservation_bytes INTEGER NOT NULL DEFAULT 0,
  previous_generation INTEGER,
  resource_reservation_json TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS ports (
  task_id TEXT NOT NULL,
  generation INTEGER NOT NULL,
  port_name TEXT NOT NULL,
  protocol TEXT NOT NULL,
  host_port INTEGER NOT NULL,
  vm_port INTEGER NOT NULL,
  device_name TEXT NOT NULL,
  state TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (task_id, port_name, protocol),
  UNIQUE (host_port, protocol),
  FOREIGN KEY (task_id) REFERENCES tasks(task_id)
);
CREATE TABLE IF NOT EXISTS operations (
  operation_id TEXT PRIMARY KEY,
  action TEXT NOT NULL,
  session_id TEXT,
  generation INTEGER,
  response_json TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  action TEXT NOT NULL,
  task_id TEXT,
  session_id TEXT,
  generation INTEGER,
  outcome TEXT NOT NULL,
  detail_json TEXT,
  created_at TEXT NOT NULL
);`

const missingDBCode = "database_missing"

type Task struct {
	TaskID                  string
	SessionID               string
	Generation              int64
	State                   string
	InstanceName            string
	VMIP                    *string
	SSHHostPort             *int64
	LeaseExpiresAt          *string
	InactiveSince           *string
	CleanupAfter            *string
	CreatedAt               string
	UpdatedAt               string
	LastError               *string
	ImageFingerprint        string
	ResourcesJSON           string
	ReservationBytes        int64
	PreviousGeneration      *int64
	ResourceReservationJSON string
}

type Port struct {
	TaskID     string `json:"task_id"`
	Generation int64  `json:"generation"`
	PortName   string `json:"port_name"`
	Protocol   string `json:"protocol"`
	HostPort   int64  `json:"host_port"`
	VMPort     int64  `json:"vm_port"`
	DeviceName string `json:"device_name"`
	State      string `json:"state"`
	CreatedAt  string `json:"created_at"`
}

type Operation struct {
	OperationID string
	Action      string
	SessionID   *string
	Generation  *int64
	Response    map[string]any
	CreatedAt   string
}

type Store struct {
	path      string
	write     *sql.DB
	reads     [2]*sql.DB
	readIndex atomic.Uint32
}

type Tx struct {
	conn    *sql.Conn
	changed bool
	closed  bool
}

// TaskCondition describes the durable facts that must still hold when a
// lifecycle transition is committed. It keeps lease expiry and generation
// decisions inside the same BEGIN IMMEDIATE transaction as the state change.
type TaskCondition struct {
	Generation       *int64
	States           []string
	LeaseActiveAfter *time.Time
	LeaseExpiredAt   *time.Time
}

func Open(path string, allowCreate bool) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("%s: empty path", missingDBCode)
	}
	if info, err := os.Stat(path); err != nil {
		if !allowCreate && errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %s", missingDBCode, path)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("database stat: %w", err)
		}
	} else if info.IsDir() {
		return nil, fmt.Errorf("database path is a directory: %s", path)
	}
	if allowCreate {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return nil, fmt.Errorf("database parent: %w", err)
		}
	}
	write, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	write.SetMaxOpenConns(1)
	write.SetMaxIdleConns(1)
	store := &Store{path: path, write: write}
	for index := range store.reads {
		read, openErr := sql.Open("sqlite", path)
		if openErr != nil {
			store.Close()
			return nil, fmt.Errorf("open sqlite read connection: %w", openErr)
		}
		read.SetMaxOpenConns(1)
		read.SetMaxIdleConns(1)
		store.reads[index] = read
	}
	if allowCreate {
		if err := store.initialize(context.Background()); err != nil {
			store.Close()
			return nil, err
		}
	} else if err := store.checkReadable(context.Background()); err != nil {
		store.Close()
		return nil, err
	}
	return store, nil
}

func OpenForTest(path string) (*Store, error) { return Open(path, true) }

func (s *Store) Path() string { return s.path }

func (s *Store) Close() error {
	var first error
	for _, db := range s.reads {
		if db != nil {
			if err := db.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	if s.write != nil {
		if err := s.write.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Store) initialize(ctx context.Context) error {
	return s.withWrite(ctx, func(tx *Tx) error {
		if _, err := tx.exec(schema); err != nil {
			return fmt.Errorf("create schema: %w", err)
		}
		var value string
		err := tx.conn.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'state_epoch'").Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			if _, err := tx.exec("INSERT INTO meta(key, value) VALUES('state_epoch', 'initial')"); err != nil {
				return fmt.Errorf("initialize state epoch: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("read state epoch: %w", err)
		}
		// Schema/bootstrap writes establish the known initial sentinel; the
		// first business mutation receives an opaque token on its commit.
		tx.changed = false
		return nil
	})
}

func (s *Store) checkReadable(ctx context.Context) error {
	return s.withRead(ctx, func(conn *sql.Conn) error {
		var value int
		if err := conn.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'tasks'").Scan(&value); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("database schema missing: %s", s.path)
			}
			return fmt.Errorf("database check: %w", err)
		}
		return nil
	})
}

func configure(conn *sql.Conn, ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA busy_timeout = 10000",
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) withRead(ctx context.Context, fn func(*sql.Conn) error) error {
	db := s.reads[s.readIndex.Add(1)%uint32(len(s.reads))]
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := configure(conn, ctx); err != nil {
		return err
	}
	return fn(conn)
}

func (s *Store) withWrite(ctx context.Context, fn func(*Tx) error) error {
	conn, err := s.write.Conn(ctx)
	if err != nil {
		return err
	}
	tx := &Tx{conn: conn}
	defer conn.Close()
	if err := configure(conn, ctx); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	if tx.changed {
		if _, err := tx.conn.ExecContext(ctx, "INSERT INTO meta(key, value) VALUES('state_epoch', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", newEpoch()); err != nil {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		return err
	}
	tx.closed = true
	return nil
}

func (tx *Tx) exec(query string, args ...any) (sql.Result, error) {
	if tx.closed {
		return nil, errors.New("transaction is closed")
	}
	result, err := tx.conn.ExecContext(context.Background(), query, args...)
	if err == nil {
		if rows, rowsErr := result.RowsAffected(); rowsErr != nil || rows > 0 {
			tx.changed = true
		}
	}
	return result, err
}

func (s *Store) StateEpoch(ctx context.Context) (string, error) {
	var value string
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		return conn.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'state_epoch'").Scan(&value)
	})
	return value, err
}

func (s *Store) Meta(ctx context.Context, key string) (string, bool, error) {
	var value string
	found := false
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		err := conn.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&value)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			found = true
		}
		return err
	})
	return value, found, err
}

func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	return s.withWrite(ctx, func(tx *Tx) error {
		var current string
		err := tx.conn.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = ?", key).Scan(&current)
		if err == nil && current == value {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = tx.exec("INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
		return err
	})
}

func (s *Store) GetTask(ctx context.Context, lookup map[string]any) (*Task, error) {
	where, arg, ok := "", any(nil), false
	if value, exists := lookup["task_id"].(string); exists {
		where, arg, ok = "task_id = ?", value, true
	} else if value, exists := lookup["session_id"].(string); exists {
		where, arg, ok = "session_id = ?", value, true
	}
	if !ok {
		return nil, errors.New("task lookup requires task_id or session_id")
	}
	var task *Task
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		row := conn.QueryRowContext(ctx, "SELECT task_id, session_id, generation, state, instance_name, vm_ip, ssh_host_port, lease_expires_at, inactive_since, cleanup_after, created_at, updated_at, last_error, image_fingerprint, resources_json, reservation_bytes, previous_generation, resource_reservation_json FROM tasks WHERE "+where, arg)
		value, err := scanTask(row)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			task = value
		}
		return err
	})
	return task, err
}

func (s *Store) ListTasks(ctx context.Context) ([]Task, error) {
	var tasks []Task
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, "SELECT task_id, session_id, generation, state, instance_name, vm_ip, ssh_host_port, lease_expires_at, inactive_since, cleanup_after, created_at, updated_at, last_error, image_fingerprint, resources_json, reservation_bytes, previous_generation, resource_reservation_json FROM tasks ORDER BY task_id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			value, err := scanTask(rows)
			if err != nil {
				return err
			}
			tasks = append(tasks, *value)
		}
		return rows.Err()
	})
	return tasks, err
}

type scanner interface{ Scan(...any) error }

func scanTask(row scanner) (*Task, error) {
	var value Task
	var vmIP, lease, inactive, cleanup, lastError sql.NullString
	var sshPort, previous sql.NullInt64
	err := row.Scan(&value.TaskID, &value.SessionID, &value.Generation, &value.State, &value.InstanceName, &vmIP, &sshPort, &lease, &inactive, &cleanup, &value.CreatedAt, &value.UpdatedAt, &lastError, &value.ImageFingerprint, &value.ResourcesJSON, &value.ReservationBytes, &previous, &value.ResourceReservationJSON)
	if err != nil {
		return nil, err
	}
	value.VMIP, value.LeaseExpiresAt, value.InactiveSince, value.CleanupAfter, value.LastError = nullableString(vmIP), nullableString(lease), nullableString(inactive), nullableString(cleanup), nullableString(lastError)
	value.SSHHostPort, value.PreviousGeneration = nullableInt(sshPort), nullableInt(previous)
	return &value, nil
}

func nullableString(value sql.NullString) *string {
	if !value.Valid {
		return nil
	}
	return &value.String
}

func nullableInt(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return &value.Int64
}

func (s *Store) InsertTask(ctx context.Context, task Task) error {
	return s.withWrite(ctx, func(tx *Tx) error {
		_, err := tx.exec("INSERT INTO tasks(task_id, session_id, generation, state, instance_name, vm_ip, ssh_host_port, lease_expires_at, inactive_since, cleanup_after, created_at, updated_at, last_error, image_fingerprint, resources_json, reservation_bytes, previous_generation, resource_reservation_json) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)", task.TaskID, task.SessionID, task.Generation, task.State, task.InstanceName, nullableStringArg(task.VMIP), nullableIntArg(task.SSHHostPort), nullableStringArg(task.LeaseExpiresAt), nullableStringArg(task.InactiveSince), nullableStringArg(task.CleanupAfter), task.CreatedAt, task.UpdatedAt, nullableStringArg(task.LastError), task.ImageFingerprint, task.ResourcesJSON, task.ReservationBytes, nullableIntArg(task.PreviousGeneration), task.ResourceReservationJSON)
		return err
	})
}

func (s *Store) UpdateTask(ctx context.Context, taskID string, updates map[string]any) error {
	_, err := s.updateTask(ctx, taskID, nil, updates)
	return err
}

// UpdateTaskIf applies updates only when the supplied durable preconditions
// still match. The returned bool is false when another lifecycle operation won
// the transition race.
func (s *Store) UpdateTaskIf(ctx context.Context, taskID string, condition TaskCondition, updates map[string]any) (bool, error) {
	return s.updateTask(ctx, taskID, &condition, updates)
}

func (s *Store) updateTask(ctx context.Context, taskID string, condition *TaskCondition, updates map[string]any) (bool, error) {
	allowed := map[string]bool{"generation": true, "state": true, "vm_ip": true, "ssh_host_port": true, "lease_expires_at": true, "inactive_since": true, "cleanup_after": true, "updated_at": true, "last_error": true, "resources_json": true, "reservation_bytes": true, "previous_generation": true, "resource_reservation_json": true, "image_fingerprint": true}
	keys := make([]string, 0, len(updates))
	for key := range updates {
		if !allowed[key] {
			return false, fmt.Errorf("unsupported task update: %s", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return false, nil
	}
	changed := false
	err := s.withWrite(ctx, func(tx *Tx) error {
		var updateChanged bool
		updateChanged, err := taskUpdatesChanged(ctx, tx, taskID, keys, updates)
		if err != nil {
			return err
		}
		changed = updateChanged
		if condition != nil {
			where := []string{"task_id = ?"}
			args := []any{taskID}
			if condition.Generation != nil {
				where = append(where, "generation = ?")
				args = append(args, *condition.Generation)
			}
			if len(condition.States) > 0 {
				placeholders := make([]string, len(condition.States))
				for index, state := range condition.States {
					placeholders[index] = "?"
					args = append(args, state)
				}
				where = append(where, "state IN ("+strings.Join(placeholders, ", ")+")")
			}
			if condition.LeaseActiveAfter != nil {
				where = append(where, "datetime(lease_expires_at) > datetime(?)")
				args = append(args, condition.LeaseActiveAfter.UTC().Format(time.RFC3339Nano))
			}
			if condition.LeaseExpiredAt != nil {
				where = append(where, "(lease_expires_at IS NULL OR datetime(lease_expires_at) <= datetime(?))")
				args = append(args, condition.LeaseExpiredAt.UTC().Format(time.RFC3339Nano))
			}
			var exists int
			if err := tx.conn.QueryRowContext(ctx, "SELECT 1 FROM tasks WHERE "+strings.Join(where, " AND "), args...).Scan(&exists); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					changed = false
					return nil
				}
				return err
			}
		}
		if !changed {
			return nil
		}
		args := make([]any, 0, len(keys)+1)
		sets := make([]string, 0, len(keys))
		for _, key := range keys {
			sets = append(sets, key+" = ?")
			args = append(args, updates[key])
		}
		args = append(args, taskID)
		_, err = tx.exec("UPDATE tasks SET "+strings.Join(sets, ", ")+" WHERE task_id = ?", args...)
		return err
	})
	return changed, err
}

func taskUpdatesChanged(ctx context.Context, tx *Tx, taskID string, keys []string, updates map[string]any) (bool, error) {
	for _, key := range keys {
		var current any
		err := tx.conn.QueryRowContext(ctx, "SELECT "+key+" FROM tasks WHERE task_id = ?", taskID).Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if !sameSQLValue(current, updates[key]) {
			return true, nil
		}
	}
	return false, nil
}

func sameSQLValue(left, right any) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	if bytes, ok := left.([]byte); ok {
		left = string(bytes)
	}
	if bytes, ok := right.([]byte); ok {
		right = string(bytes)
	}
	return fmt.Sprint(left) == fmt.Sprint(right)
}

func (s *Store) DeleteTask(ctx context.Context, taskID string) error {
	return s.withWrite(ctx, func(tx *Tx) error { _, err := tx.exec("DELETE FROM tasks WHERE task_id = ?", taskID); return err })
}

func (s *Store) GetOperation(ctx context.Context, operationID string) (*Operation, error) {
	var value *Operation
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		var result Operation
		var session sql.NullString
		var generation sql.NullInt64
		var raw string
		err := conn.QueryRowContext(ctx, "SELECT operation_id, action, session_id, generation, response_json, created_at FROM operations WHERE operation_id = ?", operationID).Scan(&result.OperationID, &result.Action, &session, &generation, &raw, &result.CreatedAt)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		result.SessionID, result.Generation = nullableString(session), nullableInt(generation)
		if err := json.Unmarshal([]byte(raw), &result.Response); err != nil {
			return fmt.Errorf("operation JSON: %w", err)
		}
		value = &result
		return nil
	})
	return value, err
}

func (s *Store) RecordOperation(ctx context.Context, operationID, action string, sessionID *string, generation *int64, response map[string]any) error {
	raw, err := json.Marshal(response)
	if err != nil {
		return err
	}
	return s.withWrite(ctx, func(tx *Tx) error {
		_, err := tx.exec("INSERT INTO operations(operation_id, action, session_id, generation, response_json, created_at) VALUES(?, ?, ?, ?, ?, ?) ON CONFLICT(operation_id) DO UPDATE SET action = excluded.action, session_id = excluded.session_id, generation = excluded.generation, response_json = excluded.response_json", operationID, action, nullableStringArg(sessionID), nullableIntArg(generation), string(raw), time.Now().UTC().Format(time.RFC3339))
		return err
	})
}

func (s *Store) PendingOperations(ctx context.Context) ([]Operation, error) {
	var values []Operation
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, "SELECT operation_id, action, session_id, generation, response_json, created_at FROM operations WHERE json_extract(response_json, '$.outcome') = 'pending' ORDER BY created_at, operation_id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var value Operation
			var session sql.NullString
			var generation sql.NullInt64
			var raw string
			if err := rows.Scan(&value.OperationID, &value.Action, &session, &generation, &raw, &value.CreatedAt); err != nil {
				return err
			}
			value.SessionID, value.Generation = nullableString(session), nullableInt(generation)
			if err := json.Unmarshal([]byte(raw), &value.Response); err != nil {
				return err
			}
			values = append(values, value)
		}
		return rows.Err()
	})
	return values, err
}

func (s *Store) ListPorts(ctx context.Context, taskID string) ([]Port, error) {
	var values []Port
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, "SELECT task_id, generation, port_name, protocol, host_port, vm_port, device_name, state, created_at FROM ports WHERE task_id = ? ORDER BY port_name, protocol", taskID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var value Port
			if err := rows.Scan(&value.TaskID, &value.Generation, &value.PortName, &value.Protocol, &value.HostPort, &value.VMPort, &value.DeviceName, &value.State, &value.CreatedAt); err != nil {
				return err
			}
			values = append(values, value)
		}
		return rows.Err()
	})
	return values, err
}

func (s *Store) UpsertPort(ctx context.Context, value Port) error {
	return s.withWrite(ctx, func(tx *Tx) error {
		_, err := tx.exec("INSERT INTO ports(task_id, generation, port_name, protocol, host_port, vm_port, device_name, state, created_at) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(task_id, port_name, protocol) DO UPDATE SET generation = excluded.generation, host_port = excluded.host_port, vm_port = excluded.vm_port, device_name = excluded.device_name, state = excluded.state WHERE generation IS NOT excluded.generation OR host_port IS NOT excluded.host_port OR vm_port IS NOT excluded.vm_port OR device_name IS NOT excluded.device_name OR state IS NOT excluded.state", value.TaskID, value.Generation, value.PortName, value.Protocol, value.HostPort, value.VMPort, value.DeviceName, value.State, value.CreatedAt)
		return err
	})
}

func (s *Store) DeletePort(ctx context.Context, taskID, name, protocol string) error {
	return s.withWrite(ctx, func(tx *Tx) error {
		_, err := tx.exec("DELETE FROM ports WHERE task_id = ? AND port_name = ? AND protocol = ?", taskID, name, protocol)
		return err
	})
}

func (s *Store) Audit(ctx context.Context, action, taskID, sessionID, outcome string, generation *int64, detail any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	return s.withWrite(ctx, func(tx *Tx) error {
		_, err := tx.exec("INSERT INTO audit(action, task_id, session_id, generation, outcome, detail_json, created_at) VALUES(?, ?, ?, ?, ?, ?, ?)", action, nullableStringArg(stringPointerIfNonEmpty(taskID)), nullableStringArg(stringPointerIfNonEmpty(sessionID)), nullableIntArg(generation), outcome, string(raw), time.Now().UTC().Format(time.RFC3339))
		return err
	})
}

func nullableStringArg(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableIntArg(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func stringPointerIfNonEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func newEpoch() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("%032x", uint64(time.Now().UnixNano()))
	}
	return hex.EncodeToString(value[:])
}
