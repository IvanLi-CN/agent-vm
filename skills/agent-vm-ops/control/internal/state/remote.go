package state

// This file contains the rqlite state adapter. The public Store methods keep
// their existing database/sql-shaped implementation, while the adapter uses a
// short-lived SQLite mirror to evaluate those callbacks and submits the
// resulting row/schema delta as one rqlite request-level transaction. The
// mirror is never a second authority: every read fences it with strong
// resource and management-auth epoch queries, and every write is fenced by
// both epochs in the remote batch. An unchanged pair may skip the expensive
// schema/table rebuild.

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	remoteWriteAttempts = 8
	remoteSyncAttempts  = 8
	remoteHTTPBodyLimit = 8 << 20
)

type remoteBackend struct {
	base            *url.URL
	client          *http.Client
	mirror          *sql.DB
	mu              sync.Mutex
	syncedEpoch     string
	syncedAuthEpoch string
}

type remoteStatement struct {
	query        string
	args         []any
	epochGuard   bool
	expectedRows int64
}

type remoteResult struct {
	Columns      []string `json:"columns"`
	Types        []string `json:"types"`
	Values       [][]any  `json:"values"`
	RowsAffected int64    `json:"rows_affected"`
	LastInsertID int64    `json:"last_insert_id"`
	Error        string   `json:"error"`
	ErrorV2      any      `json:"error_v2"`
	valuesNull   bool     `json:"-"`
}

func (result *remoteResult) UnmarshalJSON(data []byte) error {
	type plain remoteResult
	var decoded plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*result = remoteResult(decoded)
	raw, present := fields["values"]
	result.valuesNull = present && bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
	return nil
}

type remoteResponse struct {
	Results []remoteResult `json:"results"`
	Error   any            `json:"error"`
	ErrorV2 any            `json:"error_v2"`
}

type remoteStatementError struct {
	Index      int
	Message    string
	EpochGuard bool
}

func (err *remoteStatementError) Error() string {
	return fmt.Sprintf("rqlite statement %d failed: %s", err.Index, err.Message)
}

type remoteSchemaObject struct {
	Type string
	Name string
	SQL  string
}

type remoteColumn struct {
	Name string
	Type string
	PK   int64
}

type remoteRow struct {
	Values   []any
	RowID    int64
	HasRowID bool
}

type remoteTableSnapshot struct {
	Name       string
	Columns    []string
	PrimaryKey []int
	Rows       map[string]remoteRow
}

type remoteSnapshot struct {
	Objects   map[string]remoteSchemaObject
	Tables    map[string]remoteTableSnapshot
	Epoch     string
	AuthEpoch string
}

// SQLiteMigrationGate is the state-layer test boundary for evidence that a
// service/deployment coordinator will eventually provide. These booleans do
// not establish a production fence; the exported migration entry remains
// disabled until that coordinator supplies real writer, worker and native
// quiescence evidence.
type SQLiteMigrationGate struct {
	StateWriteFenced    bool
	WorkersQuiescent    bool
	SQLiteWritersClosed bool
}

func isRemoteDSN(path string) bool {
	parsed, err := url.Parse(path)
	if err != nil {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

func openRemote(dsn string, allowCreate bool) (*Store, error) {
	store, err := newRemoteStore(dsn)
	if err != nil {
		return nil, err
	}
	if err := store.remote.open(context.Background(), allowCreate); err != nil {
		_ = store.Close()
		return nil, err
	}
	return store, nil
}

// newRemoteStore creates only the local mirror and HTTP client. It deliberately
// does not probe or mutate the remote database so migration can perform a
// side-effect-free target preflight first.
func newRemoteStore(dsn string) (*Store, error) {
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("state backend URL is invalid")
	}
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if parsed.Path == "" {
		parsed.Path = "/"
	}

	name := "file:agent_vm_rqlite_mirror_" + randomHex(12) + "?mode=memory&cache=shared"
	mirror, err := sql.Open("sqlite", name)
	if err != nil {
		return nil, fmt.Errorf("open rqlite mirror: %w", err)
	}
	mirror.SetMaxOpenConns(1)
	mirror.SetMaxIdleConns(1)
	backend := &remoteBackend{base: parsed, client: &http.Client{Timeout: 30 * time.Second}, mirror: mirror}
	store := &Store{
		path:   dsn,
		write:  mirror,
		reads:  [2]*sql.DB{mirror, mirror},
		remote: backend,
	}
	return store, nil
}

func randomHex(size int) string {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(value)
}

func (r *remoteBackend) close() error {
	if r.mirror == nil {
		return nil
	}
	return r.mirror.Close()
}

func (r *remoteBackend) invalidateSync() {
	r.syncedEpoch = ""
	r.syncedAuthEpoch = ""
}

func (r *remoteBackend) endpoint(path string, values url.Values) string {
	copyURL := *r.base
	copyURL.Path = strings.TrimRight(copyURL.Path, "/") + path
	copyURL.RawQuery = values.Encode()
	return copyURL.String()
}

func (r *remoteBackend) open(ctx context.Context, allowCreate bool) error {
	objects, err := r.remoteSchema(ctx)
	if err != nil {
		return fmt.Errorf("rqlite schema probe: %w", err)
	}
	if len(objects) != 0 {
		if err := requireSupportedSchema(ctx, objects); err != nil {
			return err
		}
	}
	if allowCreate {
		statements := make([]remoteStatement, 0, 8)
		for _, statement := range splitSQLStatements(schema) {
			statements = append(statements, remoteStatement{query: statement})
		}
		statements = append(statements,
			remoteStatement{query: "INSERT OR IGNORE INTO meta(key, value) VALUES('state_epoch', 'initial')"},
			remoteStatement{query: "INSERT OR IGNORE INTO meta(key, value) VALUES('management_auth_epoch', 'initial')"},
		)
		if _, err := r.request(ctx, statements, true); err != nil {
			return fmt.Errorf("initialize rqlite schema: %w", err)
		}
		objects, err = r.remoteSchema(ctx)
		if err != nil {
			return fmt.Errorf("rqlite schema recheck: %w", err)
		}
	}
	if err := requireSupportedSchema(ctx, objects); err != nil {
		if !allowCreate && len(objects) == 0 {
			return fmt.Errorf("%s: %s", missingDBCode, r.base.String())
		}
		return err
	}
	if err := r.ensureRemoteEpoch(ctx); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.syncLocked(ctx)
}

func requireCoreSchema(objects []remoteSchemaObject) error {
	present := make(map[string]bool, len(objects))
	for _, object := range objects {
		if object.Type == "table" {
			present[object.Name] = true
		}
	}
	for _, table := range []string{"meta", "tasks", "ports", "operations", "audit"} {
		if !present[table] {
			return fmt.Errorf("database schema missing table %s", table)
		}
	}
	return nil
}

func requireCanonicalCoreSchema(ctx context.Context, objects []remoteSchemaObject) error {
	if err := requireCoreSchema(objects); err != nil {
		return err
	}
	canonical, err := canonicalCoreSchema(ctx)
	if err != nil {
		return fmt.Errorf("build canonical core schema: %w", err)
	}
	actual := make(map[string]remoteSchemaObject, len(objects))
	for _, object := range objects {
		key := objectKey(object.Type, object.Name)
		if _, exists := actual[key]; exists {
			return fmt.Errorf("database schema contains duplicate object %s", key)
		}
		actual[key] = object
	}
	for key, expected := range canonical {
		object, exists := actual[key]
		if !exists {
			return fmt.Errorf("database schema missing canonical object %s", key)
		}
		if normalizeSchemaSQL(object.SQL) != normalizeSchemaSQL(expected.SQL) {
			return fmt.Errorf("database schema differs from canonical object %s", key)
		}
	}
	return nil
}

func requireSupportedSchema(ctx context.Context, objects []remoteSchemaObject) error {
	if err := requireCanonicalCoreSchema(ctx, objects); err != nil {
		return err
	}
	core, err := canonicalCoreSchema(ctx)
	if err != nil {
		return fmt.Errorf("build canonical core schema: %w", err)
	}
	management, err := canonicalManagementSchema(ctx)
	if err != nil {
		return fmt.Errorf("build canonical management schema: %w", err)
	}
	managementPresent := false
	authPresent := false
	for _, object := range objects {
		key := objectKey(object.Type, object.Name)
		if expected, exists := core[key]; exists {
			if normalizeSchemaSQL(object.SQL) != normalizeSchemaSQL(expected.SQL) {
				return fmt.Errorf("database schema differs from canonical object %s", key)
			}
			continue
		}
		expected, exists := management[key]
		if !exists {
			return fmt.Errorf("database schema contains unsupported object %s", key)
		}
		managementPresent = true
		if isAuthManagementObject(key) {
			authPresent = true
		}
		if normalizeSchemaSQL(object.SQL) != normalizeSchemaSQL(expected.SQL) {
			return fmt.Errorf("database schema differs from canonical management object %s", key)
		}
	}
	if managementPresent {
		actual := make(map[string]bool, len(objects))
		for _, object := range objects {
			actual[objectKey(object.Type, object.Name)] = true
		}
		for key := range management {
			// The principal/session/Token tables were added after the original
			// management schema. An existing remote database may still expose
			// the complete legacy management set; EnsureManagementSchema adds
			// the new tables in one transaction after this read-only preflight.
			if !authPresent && isAuthManagementObject(key) {
				continue
			}
			// Confirmation records were added after the first durable auth
			// schema. Keep an already-complete OIDC/session/Token database
			// open so EnsureManagementSchema can add this table atomically.
			if authPresent && key == "table:management_confirmations" && !actual[key] {
				continue
			}
			if !actual[key] {
				return fmt.Errorf("database schema missing canonical management object %s", key)
			}
		}
	}
	return nil
}

func isAuthManagementObject(key string) bool {
	switch key {
	case "table:oidc_transactions", "table:browser_sessions", "table:agent_tokens", "table:management_confirmations":
		return true
	default:
		return false
	}
}

func normalizeSchemaSQL(value string) string {
	value = strings.TrimSuffix(strings.TrimSpace(value), ";")
	tokens := tokenizeSchemaSQL(value)
	filtered := make([]schemaSQLToken, 0, len(tokens))
	for index := 0; index < len(tokens); index++ {
		if index+2 < len(tokens) && index > 0 && tokens[index-1].word && tokens[index-1].text != "" &&
			tokens[index].word && tokens[index].text == "if" &&
			tokens[index+1].word && tokens[index+1].text == "not" &&
			tokens[index+2].word && tokens[index+2].text == "exists" &&
			(tokens[index-1].text == "table" || tokens[index-1].text == "index") {
			index += 2
			continue
		}
		filtered = append(filtered, tokens[index])
	}
	var builder strings.Builder
	for index, token := range filtered {
		if index > 0 && filtered[index-1].word && (token.word || token.quoted) {
			builder.WriteByte(' ')
		}
		builder.WriteString(token.text)
	}
	return builder.String()
}

type schemaSQLToken struct {
	text   string
	word   bool
	quoted bool
}

func tokenizeSchemaSQL(value string) []schemaSQLToken {
	var tokens []schemaSQLToken
	for index := 0; index < len(value); {
		switch value[index] {
		case ' ', '\t', '\r', '\n', '\f':
			index++
		case '-', '/':
			if index+1 < len(value) && value[index] == '-' && value[index+1] == '-' {
				index += 2
				for index < len(value) && value[index] != '\n' {
					index++
				}
				continue
			}
			if index+1 < len(value) && value[index] == '/' && value[index+1] == '*' {
				index += 2
				for index+1 < len(value) && !(value[index] == '*' && value[index+1] == '/') {
					index++
				}
				if index+1 < len(value) {
					index += 2
				}
				continue
			}
			fallthrough
		case '(', ')', ',', '.', ';', '=', '<', '>', '+', '*', '|', '&', '!', '?', '%':
			tokens = append(tokens, schemaSQLToken{text: value[index : index+1]})
			index++
		case '\'', '"', '`':
			quote := value[index]
			start := index
			index++
			for index < len(value) {
				if value[index] != quote {
					index++
					continue
				}
				if index+1 < len(value) && value[index+1] == quote {
					index += 2
					continue
				}
				index++
				break
			}
			tokens = append(tokens, schemaSQLToken{text: value[start:index], quoted: true})
		case '[':
			start := index
			index++
			for index < len(value) && value[index] != ']' {
				index++
			}
			if index < len(value) {
				index++
			}
			tokens = append(tokens, schemaSQLToken{text: value[start:index], quoted: true})
		default:
			start := index
			for index < len(value) {
				character := value[index]
				if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '_' || character == '$' {
					index++
					continue
				}
				break
			}
			if start == index {
				tokens = append(tokens, schemaSQLToken{text: value[index : index+1]})
				index++
				continue
			}
			tokens = append(tokens, schemaSQLToken{text: strings.ToLower(value[start:index]), word: true})
		}
	}
	return tokens
}

func canonicalCoreSchema(ctx context.Context) (map[string]remoteSchemaObject, error) {
	return canonicalSchema(ctx, schema)
}

func canonicalManagementSchema(ctx context.Context) (map[string]remoteSchemaObject, error) {
	return canonicalSchema(ctx, managementSchema)
}

func canonicalSchema(ctx context.Context, source string) (map[string]remoteSchemaObject, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	for _, statement := range splitSQLStatements(source) {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return nil, err
		}
	}
	objects, err := localSchemaObjects(ctx, conn)
	if err != nil {
		return nil, err
	}
	result := make(map[string]remoteSchemaObject, len(objects))
	for _, object := range objects {
		result[objectKey(object.Type, object.Name)] = object
	}
	return result, nil
}

func (r *remoteBackend) ensureRemoteEpoch(ctx context.Context) error {
	result, err := r.query(ctx, "SELECT value FROM meta WHERE key = 'state_epoch'")
	if err != nil {
		return fmt.Errorf("read rqlite state epoch: %w", err)
	}
	if len(result.Values) != 0 {
		if len(result.Values) != 1 || len(result.Values[0]) != 1 {
			return errors.New("rqlite state epoch is not unique")
		}
		value := strings.TrimSpace(fmt.Sprint(result.Values[0][0]))
		if err := validateStateEpoch(value); err != nil {
			return err
		}
	} else {
		r.invalidateSync()
		if _, err := r.request(ctx, []remoteStatement{{query: "INSERT OR IGNORE INTO meta(key, value) VALUES('state_epoch', 'initial')"}}, true); err != nil {
			return fmt.Errorf("initialize rqlite state epoch: %w", err)
		}
	}
	authResult, err := r.query(ctx, "SELECT value FROM meta WHERE key = 'management_auth_epoch'")
	if err != nil {
		return fmt.Errorf("read rqlite management auth epoch: %w", err)
	}
	if len(authResult.Values) > 1 || (len(authResult.Values) == 1 && len(authResult.Values[0]) != 1) {
		return errors.New("rqlite management auth epoch is not unique")
	}
	if len(authResult.Values) == 0 {
		r.invalidateSync()
		if _, err := r.request(ctx, []remoteStatement{{query: "INSERT OR IGNORE INTO meta(key, value) VALUES('management_auth_epoch', 'initial')"}}, true); err != nil {
			return fmt.Errorf("initialize rqlite management auth epoch: %w", err)
		}
		return nil
	}
	if err := validateStateEpoch(strings.TrimSpace(fmt.Sprint(authResult.Values[0][0]))); err != nil {
		return fmt.Errorf("validate rqlite management auth epoch: %w", err)
	}
	return nil
}

func (r *remoteBackend) ensureManagementSchema(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	statements := make([]remoteStatement, 0, 12)
	for _, statement := range splitSQLStatements(managementSchema) {
		statements = append(statements, remoteStatement{query: statement})
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, marker := range []struct {
		name    string
		version int64
	}{
		{name: "legacy-agent-vm-core", version: 1},
		{name: "managed-image-catalog", version: 1},
		{name: "session-image-binding", version: 1},
		{name: "management-operation-audit", version: 1},
		{name: "principal-identity", version: 1},
		{name: "token-revocation", version: 1},
	} {
		statements = append(statements, remoteStatement{
			query: "INSERT OR IGNORE INTO state_capabilities(name, version, created_at) VALUES(?, ?, ?)",
			args:  []any{marker.name, marker.version, now},
		})
	}
	statements = append(statements,
		remoteStatement{query: "INSERT OR IGNORE INTO meta(key, value) VALUES('management_selection_revision', '1')"},
		remoteStatement{query: "INSERT OR IGNORE INTO meta(key, value) VALUES('management_catalog_revision', '1')"},
		remoteStatement{query: "INSERT OR IGNORE INTO meta(key, value) VALUES('management_auth_epoch', 'initial')"},
	)
	// Management bootstrap changes schema and metadata without advancing the
	// core state_epoch. Force the next sync to rebuild the mirror even when the
	// epoch itself is unchanged. This also covers an ambiguous request result.
	r.invalidateSync()
	if _, err := r.request(ctx, statements, true); err != nil {
		var statementErr *remoteStatementError
		stateCommitted := (*bool)(nil)
		if errors.As(err, &statementErr) {
			stateCommitted = committed(false)
		}
		return &TransactionError{Phase: "commit", SQLOperation: "rqlite_management_schema", StateCommitted: stateCommitted, Cause: err}
	}
	if err := r.syncLocked(ctx); err != nil {
		return &TransactionError{Phase: "verify", SQLOperation: "rqlite_management_schema", StateCommitted: nil, Cause: err}
	}
	return nil
}

func (r *remoteBackend) withRead(ctx context.Context, fn func(*sql.Conn) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.syncLocked(ctx); err != nil {
		return &TransactionError{Phase: "connection", SQLOperation: "read", StateCommitted: committed(false), Cause: err}
	}
	conn, err := r.mirror.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := configureMirror(conn, ctx); err != nil {
		return err
	}
	return fn(conn)
}

func (r *remoteBackend) withWrite(ctx context.Context, fn func(*Tx) error) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	for attempt := 0; attempt < remoteWriteAttempts; attempt++ {
		if err := r.syncLocked(ctx); err != nil {
			return &TransactionError{Phase: "connection", SQLOperation: "write", StateCommitted: committed(false), Cause: err}
		}
		before, err := snapshotDB(ctx, r.mirror)
		if err != nil {
			return &TransactionError{Phase: "connection", SQLOperation: "snapshot", StateCommitted: committed(false), Cause: err}
		}

		conn, err := r.mirror.Conn(ctx)
		if err != nil {
			return &TransactionError{Phase: "connection", SQLOperation: "write", StateCommitted: committed(false), Cause: err}
		}
		tx := &Tx{conn: conn}
		if err := configureMirror(conn, ctx); err != nil {
			_ = conn.Close()
			return &TransactionError{Phase: "connection", SQLOperation: "configure", StateCommitted: committed(false), Cause: err}
		}
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			_ = conn.Close()
			return &TransactionError{Phase: "begin", SQLOperation: "write", StateCommitted: committed(false), Cause: err}
		}
		if err := fn(tx); err != nil {
			_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
			_ = conn.Close()
			if rollbackErr != nil {
				r.invalidateSync()
			}
			stateCommitted := committed(false)
			if rollbackErr != nil {
				stateCommitted = nil
			}
			return &TransactionError{Phase: "write", SQLOperation: "write", StateCommitted: stateCommitted, RollbackError: rollbackErr, Cause: err}
		}

		if tx.changed {
			if _, err := tx.conn.ExecContext(ctx, "INSERT INTO meta(key, value) VALUES('state_epoch', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", newEpoch()); err != nil {
				_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
				_ = conn.Close()
				if rollbackErr != nil {
					r.invalidateSync()
				}
				stateCommitted := committed(false)
				if rollbackErr != nil {
					stateCommitted = nil
				}
				return &TransactionError{Phase: "commit", SQLOperation: "state_epoch", StateCommitted: stateCommitted, RollbackError: rollbackErr, Cause: err}
			}
		}
		if tx.authChanged {
			if _, err := tx.conn.ExecContext(ctx, "INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", managementAuthEpochKey, newEpoch()); err != nil {
				_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
				_ = conn.Close()
				if rollbackErr != nil {
					r.invalidateSync()
				}
				stateCommitted := committed(false)
				if rollbackErr != nil {
					stateCommitted = nil
				}
				return &TransactionError{Phase: "commit", SQLOperation: managementAuthEpochKey, StateCommitted: stateCommitted, RollbackError: rollbackErr, Cause: err}
			}
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
			_ = conn.Close()
			r.invalidateSync()
			return &TransactionError{Phase: "commit", SQLOperation: "commit", StateCommitted: nil, RollbackError: rollbackErr, Cause: err}
		}
		tx.closed = true
		_ = conn.Close()
		// The local mirror now contains a speculative commit. Keep it invalid
		// until the remote compare-and-act succeeds or a fresh sync rebuilds it.
		r.invalidateSync()

		after, err := snapshotDB(ctx, r.mirror)
		if err != nil {
			return &TransactionError{Phase: "commit", SQLOperation: "snapshot", StateCommitted: nil, Cause: err}
		}
		if snapshotsEqual(before, after) {
			r.syncedEpoch = after.Epoch
			r.syncedAuthEpoch = after.AuthEpoch
			return nil
		}
		applied, err := r.applySnapshot(ctx, before, after, before.Epoch, after.Epoch)
		if err != nil {
			r.invalidateSync()
			// Once the HTTP request has been sent, a transport failure cannot
			// establish whether rqlite committed. Preserve the operation as
			// unknown instead of retrying it under a new request.
			var statementErr *remoteStatementError
			if errors.As(err, &statementErr) && statementErr.EpochGuard {
				// The epoch sentinel is part of the same request-level
				// transaction. Its constraint failure proves that rqlite
				// rolled the batch back, so refresh and replay the callback.
				continue
			}
			if errors.As(err, &statementErr) {
				return &TransactionError{Phase: "commit", SQLOperation: "rqlite_transaction", StateCommitted: committed(false), Cause: err}
			}
			return &TransactionError{Phase: "commit", SQLOperation: "rqlite_transaction", StateCommitted: nil, Cause: err}
		}
		if applied {
			r.syncedEpoch = after.Epoch
			r.syncedAuthEpoch = after.AuthEpoch
			return nil
		}
		// Another slot committed after this mirror was refreshed. The
		// callback is replayed against the new authoritative state, but only
		// for a proven compare-and-act conflict; transport ambiguity never
		// reaches this path.
	}
	return &TransactionError{Phase: "commit", SQLOperation: "state_epoch", StateCommitted: committed(false), Cause: errors.New("rqlite state changed concurrently")}
}

func configureMirror(conn *sql.Conn, ctx context.Context) error {
	for _, statement := range []string{
		"PRAGMA busy_timeout = 10000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (r *remoteBackend) request(ctx context.Context, statements []remoteStatement, transaction bool) (remoteResponse, error) {
	encoded := make([][]any, 0, len(statements))
	for _, statement := range statements {
		values := make([]any, 0, len(statement.args)+1)
		values = append(values, statement.query)
		values = append(values, statement.args...)
		encoded = append(encoded, values)
	}
	body, err := json.Marshal(encoded)
	if err != nil {
		return remoteResponse{}, err
	}
	query := url.Values{}
	if transaction {
		query.Set("transaction", "")
	}
	response, err := r.do(ctx, http.MethodPost, "/db/execute", query, strings.NewReader(string(body)))
	if err != nil {
		return remoteResponse{}, err
	}
	defer response.Close()
	result, err := decodeRemoteResponse(response, "rqlite response")
	if err != nil {
		return remoteResponse{}, err
	}
	if message := remoteResponseError(result); message != "" {
		return result, errors.New(message)
	}
	if len(result.Results) > len(statements) {
		return result, fmt.Errorf("rqlite response returned %d results for %d statements", len(result.Results), len(statements))
	}
	for index, item := range result.Results {
		if message := remoteResultError(item); message != "" {
			return result, &remoteStatementError{Index: index, Message: message, EpochGuard: statements[index].epochGuard}
		}
	}
	if len(result.Results) != len(statements) {
		return result, fmt.Errorf("rqlite response returned %d results for %d statements", len(result.Results), len(statements))
	}
	return result, nil
}

func (r *remoteBackend) query(ctx context.Context, statement string) (remoteResult, error) {
	query := url.Values{}
	query.Set("level", "strong")
	query.Set("q", statement)
	response, err := r.do(ctx, http.MethodGet, "/db/query", query, nil)
	if err != nil {
		return remoteResult{}, err
	}
	defer response.Close()
	result, err := decodeRemoteResponse(response, "rqlite query")
	if err != nil {
		return remoteResult{}, err
	}
	if message := remoteResponseError(result); message != "" {
		return remoteResult{}, errors.New(message)
	}
	if len(result.Results) != 1 {
		return remoteResult{}, fmt.Errorf("rqlite query returned %d results, want 1", len(result.Results))
	}
	if message := remoteResultError(result.Results[0]); message != "" {
		return remoteResult{}, &remoteStatementError{Index: 0, Message: message}
	}
	if err := validateRemoteQueryResult(result.Results[0]); err != nil {
		return remoteResult{}, err
	}
	if result.Results[0].Values == nil {
		// rqlite omits values when a valid query returns no rows.
		result.Results[0].Values = [][]any{}
	}
	return result.Results[0], nil
}

func validateRemoteQueryResult(result remoteResult) error {
	if result.Columns == nil || result.Types == nil {
		return errors.New("rqlite query result is missing columns or types")
	}
	if result.valuesNull {
		return errors.New("rqlite query result values is null")
	}
	if len(result.Columns) != len(result.Types) {
		return fmt.Errorf("rqlite query result has %d columns and %d types", len(result.Columns), len(result.Types))
	}
	for index, row := range result.Values {
		if len(row) != len(result.Columns) {
			return fmt.Errorf("rqlite query row %d has %d values, want %d", index, len(row), len(result.Columns))
		}
	}
	return nil
}

func decodeRemoteResponse(reader io.Reader, label string) (remoteResponse, error) {
	var result remoteResponse
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return remoteResponse{}, fmt.Errorf("decode %s: %w", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return remoteResponse{}, fmt.Errorf("decode %s: trailing JSON value", label)
		}
		return remoteResponse{}, fmt.Errorf("decode %s trailing data: %w", label, err)
	}
	return result, nil
}

func remoteResultError(result remoteResult) string {
	if result.Error != "" {
		return result.Error
	}
	if result.ErrorV2 == nil {
		return ""
	}
	if message, ok := result.ErrorV2.(string); ok && message != "" {
		return message
	}
	encoded, err := json.Marshal(result.ErrorV2)
	if err == nil && string(encoded) != "null" {
		return "rqlite error_v2: " + string(encoded)
	}
	return "rqlite error_v2"
}

func remoteResponseError(result remoteResponse) string {
	if result.Error != nil {
		if message, ok := result.Error.(string); ok {
			if message != "" {
				return message
			}
		} else {
			encoded, err := json.Marshal(result.Error)
			if err == nil && string(encoded) != "null" {
				return "rqlite error: " + string(encoded)
			}
			return "rqlite error"
		}
	}
	if result.ErrorV2 != nil {
		return remoteResultError(remoteResult{ErrorV2: result.ErrorV2})
	}
	return ""
}

func (r *remoteBackend) do(ctx context.Context, method, path string, values url.Values, body io.Reader) (io.ReadCloser, error) {
	request, err := http.NewRequestWithContext(ctx, method, r.endpoint(path, values), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := r.client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		defer response.Body.Close()
		message, _ := io.ReadAll(io.LimitReader(response.Body, remoteHTTPBodyLimit))
		return nil, fmt.Errorf("rqlite HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	return response.Body, nil
}

func (r *remoteBackend) remoteSchema(ctx context.Context) ([]remoteSchemaObject, error) {
	result, err := r.query(ctx, `SELECT type, name, sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 WHEN 'trigger' THEN 2 ELSE 3 END, name`)
	if err != nil {
		return nil, err
	}
	objects := make([]remoteSchemaObject, 0, len(result.Values))
	for index, row := range result.Values {
		if len(row) != 3 {
			return nil, fmt.Errorf("rqlite schema row %d is incomplete", index)
		}
		typeName, ok := row[0].(string)
		if !ok {
			return nil, fmt.Errorf("rqlite schema row %d has invalid type", index)
		}
		name, ok := row[1].(string)
		if !ok {
			return nil, fmt.Errorf("rqlite schema row %d has invalid name", index)
		}
		sqlText, ok := row[2].(string)
		if !ok {
			return nil, fmt.Errorf("rqlite schema row %d has invalid SQL", index)
		}
		objects = append(objects, remoteSchemaObject{Type: typeName, Name: name, SQL: sqlText})
	}
	return objects, nil
}

func (r *remoteBackend) syncLocked(ctx context.Context) error {
	for attempt := 0; attempt < remoteSyncAttempts; attempt++ {
		startEpoch, err := r.remoteEpoch(ctx)
		if err != nil {
			return err
		}
		startAuthEpoch, err := r.remoteAuthEpoch(ctx)
		if err != nil {
			return err
		}
		if startEpoch == r.syncedEpoch && startAuthEpoch == r.syncedAuthEpoch {
			return nil
		}
		objects, err := r.remoteSchema(ctx)
		if err != nil {
			return err
		}
		if err := requireSupportedSchema(ctx, objects); err != nil {
			return err
		}
		if err := r.rebuildMirror(ctx, objects); err != nil {
			return fmt.Errorf("refresh rqlite mirror: %w", err)
		}
		endEpoch, err := r.remoteEpoch(ctx)
		if err != nil {
			return err
		}
		endAuthEpoch, err := r.remoteAuthEpoch(ctx)
		if err != nil {
			return err
		}
		if startEpoch == endEpoch && startAuthEpoch == endAuthEpoch {
			r.syncedEpoch = endEpoch
			r.syncedAuthEpoch = endAuthEpoch
			return nil
		}
	}
	return errors.New("rqlite state changed while refreshing mirror")
}

func (r *remoteBackend) remoteEpoch(ctx context.Context) (string, error) {
	result, err := r.query(ctx, "SELECT value FROM meta WHERE key = 'state_epoch'")
	if err != nil {
		return "", err
	}
	if len(result.Values) != 1 || len(result.Values[0]) != 1 {
		return "", errors.New("rqlite state epoch is missing")
	}
	value := strings.TrimSpace(fmt.Sprint(result.Values[0][0]))
	if err := validateStateEpoch(value); err != nil {
		return "", err
	}
	return value, nil
}

func (r *remoteBackend) remoteAuthEpoch(ctx context.Context) (string, error) {
	result, err := r.query(ctx, "SELECT value FROM meta WHERE key = 'management_auth_epoch'")
	if err != nil {
		return "", err
	}
	if len(result.Values) == 0 {
		return "initial", nil
	}
	if len(result.Values) != 1 || len(result.Values[0]) != 1 {
		return "", errors.New("rqlite management auth epoch is missing or not unique")
	}
	value := strings.TrimSpace(fmt.Sprint(result.Values[0][0]))
	if err := validateStateEpoch(value); err != nil {
		return "", fmt.Errorf("management auth epoch: %w", err)
	}
	return value, nil
}

func (r *remoteBackend) rebuildMirror(ctx context.Context, objects []remoteSchemaObject) error {
	conn, err := r.mirror.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return err
	}
	localObjects, err := localSchemaObjects(ctx, conn)
	if err != nil {
		return err
	}
	transaction, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	rollback := func(cause error) error {
		_ = transaction.Rollback()
		return cause
	}
	for _, object := range localObjects {
		if object.Type == "trigger" {
			if _, err := transaction.ExecContext(ctx, "DROP TRIGGER "+quoteIdentifier(object.Name)); err != nil {
				return rollback(err)
			}
		}
	}
	for _, object := range localObjects {
		if object.Type == "index" && !strings.HasPrefix(object.Name, "sqlite_autoindex_") {
			if _, err := transaction.ExecContext(ctx, "DROP INDEX "+quoteIdentifier(object.Name)); err != nil {
				return rollback(err)
			}
		}
	}
	for index := len(localObjects) - 1; index >= 0; index-- {
		object := localObjects[index]
		if object.Type == "table" {
			if _, err := transaction.ExecContext(ctx, "DROP TABLE "+quoteIdentifier(object.Name)); err != nil {
				return rollback(err)
			}
		}
	}
	for _, object := range sortedSchemaObjects(objects) {
		if _, err := transaction.ExecContext(ctx, object.SQL); err != nil {
			return rollback(err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		return err
	}

	loadTx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, table := range orderedTables(objects) {
		if err := r.loadTable(ctx, loadTx, table); err != nil {
			_ = loadTx.Rollback()
			return err
		}
	}
	return loadTx.Commit()
}

func sortedSchemaObjects(objects []remoteSchemaObject) []remoteSchemaObject {
	values := append([]remoteSchemaObject(nil), objects...)
	sort.SliceStable(values, func(left, right int) bool {
		rank := func(typeName string) int {
			switch typeName {
			case "table":
				return 0
			case "index":
				return 1
			case "trigger":
				return 2
			default:
				return 3
			}
		}
		if rank(values[left].Type) != rank(values[right].Type) {
			return rank(values[left].Type) < rank(values[right].Type)
		}
		return values[left].Name < values[right].Name
	})
	return values
}

func orderedTables(objects []remoteSchemaObject) []string {
	present := make(map[string]bool)
	for _, object := range objects {
		if object.Type == "table" {
			present[object.Name] = true
		}
	}
	order := []string{"meta", "tasks", "ports", "operations", "audit", "state_capabilities", "managed_images", "managed_image_tombstones", "image_references", "management_operations", "management_audit", "oidc_transactions", "browser_sessions", "agent_tokens", "management_confirmations"}
	values := make([]string, 0, len(present))
	for _, table := range order {
		if present[table] {
			values = append(values, table)
			delete(present, table)
		}
	}
	for table := range present {
		values = append(values, table)
	}
	knownCount := len(values) - len(present)
	sort.Strings(values[knownCount:])
	return values
}

func (r *remoteBackend) loadTable(ctx context.Context, transaction *sql.Tx, table string) error {
	columns, err := tableColumns(ctx, transaction, table)
	if err != nil {
		return err
	}
	selectColumns := "*"
	insertColumns := make([]string, 0, len(columns)+1)
	if table == "operations" {
		selectColumns = "rowid, *"
		insertColumns = append(insertColumns, "rowid")
	}
	for _, column := range columns {
		insertColumns = append(insertColumns, column.Name)
	}
	result, err := r.query(ctx, "SELECT "+selectColumns+" FROM "+quoteIdentifier(table)+" ORDER BY rowid")
	if err != nil {
		return err
	}
	if len(result.Values) == 0 {
		return nil
	}
	placeholders := make([]string, len(insertColumns))
	for index := range placeholders {
		placeholders[index] = "?"
	}
	statement := "INSERT INTO " + quoteIdentifier(table) + "(" + joinIdentifiers(insertColumns) + ") VALUES (" + strings.Join(placeholders, ", ") + ")"
	for rowIndex, row := range result.Values {
		if len(row) != len(insertColumns) {
			return fmt.Errorf("rqlite table %s row %d has %d values, want %d", table, rowIndex, len(row), len(insertColumns))
		}
		args := make([]any, len(row))
		for index, value := range row {
			if table == "operations" && index == 0 {
				args[index] = normalizeRemoteRowID(value)
				continue
			}
			columnIndex := index
			if table == "operations" {
				columnIndex--
			}
			args[index] = normalizeRemoteValue(value, columns[columnIndex].Type)
		}
		if _, err := transaction.ExecContext(ctx, statement, args...); err != nil {
			return fmt.Errorf("load rqlite table %s row %d: %w", table, rowIndex, err)
		}
	}
	return nil
}

func tableColumns(ctx context.Context, transaction *sql.Tx, table string) ([]remoteColumn, error) {
	rows, err := transaction.QueryContext(ctx, "PRAGMA table_info("+quoteString(table)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []remoteColumn
	for rows.Next() {
		var cid int64
		var column remoteColumn
		var notNull int64
		var defaultValue any
		if err := rows.Scan(&cid, &column.Name, &column.Type, &notNull, &defaultValue, &column.PK); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func normalizeRemoteValue(value any, declaredType string) any {
	number, ok := value.(json.Number)
	if !ok {
		return value
	}
	upper := strings.ToUpper(declaredType)
	if strings.Contains(upper, "INT") {
		if parsed, err := strconv.ParseInt(string(number), 10, 64); err == nil {
			return parsed
		}
	}
	if strings.Contains(upper, "REAL") || strings.Contains(upper, "FLOA") || strings.Contains(upper, "DOUB") {
		if parsed, err := strconv.ParseFloat(string(number), 64); err == nil {
			return parsed
		}
	}
	return string(number)
}

func normalizeRemoteRowID(value any) any {
	if number, ok := value.(json.Number); ok {
		if parsed, err := strconv.ParseInt(string(number), 10, 64); err == nil {
			return parsed
		}
	}
	return value
}

func splitSQLStatements(source string) []string {
	var statements []string
	start := 0
	quote := byte(0)
	for index := 0; index < len(source); index++ {
		current := source[index]
		if quote != 0 {
			if current == quote {
				if index+1 < len(source) && source[index+1] == quote {
					index++
					continue
				}
				quote = 0
			}
			continue
		}
		switch current {
		case '\'', '"', '`':
			quote = current
		case ';':
			if statement := strings.TrimSpace(source[start:index]); statement != "" {
				statements = append(statements, statement)
			}
			start = index + 1
		}
	}
	if statement := strings.TrimSpace(source[start:]); statement != "" {
		statements = append(statements, statement)
	}
	return statements
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func quoteString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func joinIdentifiers(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = quoteIdentifier(value)
	}
	return strings.Join(quoted, ", ")
}

func localSchemaObjects(ctx context.Context, conn *sql.Conn) ([]remoteSchemaObject, error) {
	rows, err := conn.QueryContext(ctx, `SELECT type, name, sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%' ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 WHEN 'trigger' THEN 2 ELSE 3 END, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var objects []remoteSchemaObject
	for rows.Next() {
		var object remoteSchemaObject
		if err := rows.Scan(&object.Type, &object.Name, &object.SQL); err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, rows.Err()
}

func tableColumnsConn(ctx context.Context, conn *sql.Conn, table string) ([]remoteColumn, error) {
	rows, err := conn.QueryContext(ctx, "PRAGMA table_info("+quoteString(table)+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []remoteColumn
	for rows.Next() {
		var cid int64
		var column remoteColumn
		var notNull int64
		var defaultValue any
		if err := rows.Scan(&cid, &column.Name, &column.Type, &notNull, &defaultValue, &column.PK); err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func snapshotDB(ctx context.Context, db *sql.DB) (remoteSnapshot, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return remoteSnapshot{}, err
	}
	defer conn.Close()
	if err := configureMirror(conn, ctx); err != nil {
		return remoteSnapshot{}, err
	}
	return snapshotConn(ctx, conn)
}

func snapshotConn(ctx context.Context, conn *sql.Conn) (remoteSnapshot, error) {
	objects, err := localSchemaObjects(ctx, conn)
	if err != nil {
		return remoteSnapshot{}, err
	}
	snapshot := remoteSnapshot{
		Objects:   make(map[string]remoteSchemaObject, len(objects)),
		Tables:    make(map[string]remoteTableSnapshot),
		Epoch:     "initial",
		AuthEpoch: "initial",
	}
	for _, object := range objects {
		snapshot.Objects[objectKey(object.Type, object.Name)] = object
	}
	for _, table := range orderedTables(objects) {
		columns, err := tableColumnsConn(ctx, conn, table)
		if err != nil {
			return remoteSnapshot{}, err
		}
		primaryKey := make([]int, 0)
		columnNames := make([]string, len(columns))
		for index, column := range columns {
			columnNames[index] = column.Name
			if column.PK > 0 {
				primaryKey = append(primaryKey, index)
			}
		}
		sort.Slice(primaryKey, func(left, right int) bool {
			return columns[primaryKey[left]].PK < columns[primaryKey[right]].PK
		})
		query := "SELECT * FROM " + quoteIdentifier(table)
		if table == "operations" {
			query = "SELECT rowid, * FROM " + quoteIdentifier(table)
		}
		rows, err := conn.QueryContext(ctx, query)
		if err != nil {
			return remoteSnapshot{}, err
		}
		valueRows, err := scanSnapshotRows(rows, table, len(columns), primaryKey)
		rows.Close()
		if err != nil {
			return remoteSnapshot{}, err
		}
		snapshot.Tables[table] = remoteTableSnapshot{Name: table, Columns: columnNames, PrimaryKey: primaryKey, Rows: valueRows}
	}
	if table, exists := snapshot.Tables["meta"]; exists {
		keyIndex := indexOf(table.Columns, "key")
		valueIndex := indexOf(table.Columns, "value")
		if keyIndex >= 0 && valueIndex >= 0 {
			for _, row := range table.Rows {
				switch fmt.Sprint(row.Values[keyIndex]) {
				case "state_epoch":
					snapshot.Epoch = strings.TrimSpace(fmt.Sprint(row.Values[valueIndex]))
				case managementAuthEpochKey:
					snapshot.AuthEpoch = strings.TrimSpace(fmt.Sprint(row.Values[valueIndex]))
				}
			}
		}
	}
	return snapshot, nil
}

func scanSnapshotRows(rows *sql.Rows, table string, columnCount int, primaryKey []int) (map[string]remoteRow, error) {
	result := make(map[string]remoteRow)
	for rows.Next() {
		values := make([]any, columnCount)
		dest := make([]any, len(values))
		for index := range values {
			dest[index] = &values[index]
		}
		var rowID int64
		if table == "operations" {
			if err := rows.Scan(append([]any{&rowID}, dest...)...); err != nil {
				return nil, err
			}
		} else if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		for index, value := range values {
			values[index] = cloneSQLValue(value)
		}
		key := snapshotRowKey(values, rowID, table, primaryKey)
		if _, exists := result[key]; exists {
			return nil, fmt.Errorf("duplicate snapshot key %s in table %s", key, table)
		}
		result[key] = remoteRow{Values: values, RowID: rowID, HasRowID: table == "operations" || len(primaryKey) == 0}
	}
	return result, rows.Err()
}

func cloneSQLValue(value any) any {
	if bytes, ok := value.([]byte); ok {
		return append([]byte(nil), bytes...)
	}
	return value
}

func objectKey(typeName, name string) string { return typeName + "\x00" + name }

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}

func snapshotRowKey(values []any, rowID int64, table string, primaryKey []int) string {
	if len(primaryKey) == 0 {
		return "rowid:" + strconv.FormatInt(rowID, 10)
	}
	var builder strings.Builder
	for _, index := range primaryKey {
		value := values[index]
		encoded := fmt.Sprintf("%T:%v", value, value)
		builder.WriteString(strconv.Itoa(len(encoded)))
		builder.WriteByte(':')
		builder.WriteString(encoded)
		builder.WriteByte('|')
	}
	if table == "operations" {
		builder.WriteString("operation")
	}
	return builder.String()
}

func snapshotsEqual(left, right remoteSnapshot) bool {
	if left.Epoch != right.Epoch || left.AuthEpoch != right.AuthEpoch || len(left.Objects) != len(right.Objects) || len(left.Tables) != len(right.Tables) {
		return false
	}
	for key, object := range left.Objects {
		other, exists := right.Objects[key]
		if !exists || normalizeSchemaSQL(object.SQL) != normalizeSchemaSQL(other.SQL) {
			return false
		}
	}
	for table, value := range left.Tables {
		other, exists := right.Tables[table]
		if !exists || !tableSnapshotsEqual(value, other) {
			return false
		}
	}
	return true
}

func tableSnapshotsEqual(left, right remoteTableSnapshot) bool {
	if !equalStrings(left.Columns, right.Columns) || len(left.PrimaryKey) != len(right.PrimaryKey) || len(left.Rows) != len(right.Rows) {
		return false
	}
	for index, value := range left.PrimaryKey {
		if value != right.PrimaryKey[index] {
			return false
		}
	}
	for key, value := range left.Rows {
		other, exists := right.Rows[key]
		if !exists || !rowsEqual(value, other) {
			return false
		}
	}
	return true
}

func rowsEqual(left, right remoteRow) bool {
	if left.RowID != right.RowID || left.HasRowID != right.HasRowID || len(left.Values) != len(right.Values) {
		return false
	}
	for index := range left.Values {
		if !sqlValuesEqual(left.Values[index], right.Values[index]) {
			return false
		}
	}
	return true
}

func sqlValuesEqual(left, right any) bool {
	if leftBytes, ok := left.([]byte); ok {
		rightBytes, rightOK := right.([]byte)
		return rightOK && string(leftBytes) == string(rightBytes)
	}
	return fmt.Sprintf("%T:%v", left, left) == fmt.Sprintf("%T:%v", right, right)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func (r *remoteBackend) applySnapshot(ctx context.Context, before, after remoteSnapshot, expectedEpoch, targetEpoch string) (bool, error) {
	if expectedEpoch == "" {
		expectedEpoch = before.Epoch
	}
	if targetEpoch == "" {
		targetEpoch = after.Epoch
	}
	if before.AuthEpoch == "" {
		before.AuthEpoch = "initial"
	}
	if after.AuthEpoch == "" {
		after.AuthEpoch = "initial"
	}
	expectedAuthEpoch := before.AuthEpoch
	targetAuthEpoch := after.AuthEpoch
	if err := validateStateEpoch(expectedEpoch); err != nil {
		return false, fmt.Errorf("invalid expected state epoch: %w", err)
	}
	if err := validateStateEpoch(targetEpoch); err != nil {
		return false, fmt.Errorf("invalid target state epoch: %w", err)
	}
	if err := validateStateEpoch(expectedAuthEpoch); err != nil {
		return false, fmt.Errorf("invalid expected management auth epoch: %w", err)
	}
	if err := validateStateEpoch(targetAuthEpoch); err != nil {
		return false, fmt.Errorf("invalid target management auth epoch: %w", err)
	}
	statements := make([]remoteStatement, 0)

	for key := range before.Objects {
		if _, exists := after.Objects[key]; !exists {
			return false, fmt.Errorf("rqlite schema removal is not supported for %s", key)
		}
	}
	for _, object := range sortedSchemaObjects(afterObjectsMissing(before.Objects, after.Objects)) {
		statements = append(statements, remoteStatement{query: object.SQL})
	}
	for key, object := range before.Objects {
		other, exists := after.Objects[key]
		if !exists || normalizeSchemaSQL(other.SQL) != normalizeSchemaSQL(object.SQL) {
			return false, fmt.Errorf("rqlite schema changed for %s", key)
		}
	}
	if !snapshotHasStateEpoch(before) && snapshotHasStateEpoch(after) {
		statements = append(statements, remoteStatement{query: "INSERT OR IGNORE INTO meta(key, value) VALUES('state_epoch', 'initial')"})
	}
	if !snapshotHasManagementAuthEpoch(before) {
		statements = append(statements, remoteStatement{query: "INSERT OR IGNORE INTO meta(key, value) VALUES('management_auth_epoch', 'initial')"})
	}

	for _, tableName := range orderedSnapshotTables(after.Tables) {
		afterTable := after.Tables[tableName]
		beforeTable, exists := before.Tables[tableName]
		if exists && (!equalStrings(beforeTable.Columns, afterTable.Columns) || !equalIntSlices(beforeTable.PrimaryKey, afterTable.PrimaryKey)) {
			return false, fmt.Errorf("rqlite table layout changed for %s", tableName)
		}
		if err := appendTableDiff(&statements, beforeTable, exists, afterTable, expectedEpoch, before.AuthEpoch); err != nil {
			return false, err
		}
	}
	guardKey := "__agent_vm_epoch_guard_" + randomHex(12)
	proofKey := guardKey + "_proof"
	guardIndex := len(statements)
	statements = append(statements,
		remoteStatement{
			query:      "INSERT INTO meta(key, value) SELECT ?, CASE WHEN EXISTS (SELECT 1 FROM meta WHERE key = 'state_epoch' AND value = ?) AND EXISTS (SELECT 1 FROM meta WHERE key = 'management_auth_epoch' AND value = ?) THEN 'ok' ELSE NULL END",
			args:       []any{guardKey, expectedEpoch, expectedAuthEpoch},
			epochGuard: true,
		},
		remoteStatement{
			query:        "UPDATE meta SET value = ? WHERE key = 'state_epoch' AND value = ?",
			args:         []any{targetEpoch, expectedEpoch},
			epochGuard:   true,
			expectedRows: 1,
		},
		remoteStatement{
			query:        "UPDATE meta SET value = ? WHERE key = 'management_auth_epoch' AND value = ?",
			args:         []any{targetAuthEpoch, expectedAuthEpoch},
			epochGuard:   true,
			expectedRows: 1,
		},
		remoteStatement{
			query:      "INSERT INTO meta(key, value) SELECT ?, CASE WHEN changes() = 1 THEN 'ok' ELSE NULL END",
			args:       []any{proofKey},
			epochGuard: true,
		},
		remoteStatement{query: "DELETE FROM meta WHERE key IN (?, ?)", args: []any{guardKey, proofKey}},
	)
	response, err := r.request(ctx, statements, true)
	if err != nil {
		return false, err
	}
	if len(response.Results) <= guardIndex+3 {
		return false, errors.New("rqlite transaction did not return its epoch updates")
	}
	for index, statement := range statements {
		if statement.expectedRows == 0 || response.Results[index].RowsAffected == statement.expectedRows {
			continue
		}
		return false, &remoteStatementError{
			Index:      index,
			Message:    fmt.Sprintf("rqlite statement affected %d rows, want %d", response.Results[index].RowsAffected, statement.expectedRows),
			EpochGuard: statement.epochGuard,
		}
	}
	return true, nil
}

func snapshotHasStateEpoch(snapshot remoteSnapshot) bool {
	table, exists := snapshot.Tables["meta"]
	if !exists {
		return false
	}
	for _, row := range table.Rows {
		if isStateEpochRow(table, row) {
			return true
		}
	}
	return false
}

func snapshotHasManagementAuthEpoch(snapshot remoteSnapshot) bool {
	table, exists := snapshot.Tables["meta"]
	if !exists {
		return false
	}
	for _, row := range table.Rows {
		if isManagementAuthEpochRow(table, row) {
			return true
		}
	}
	return false
}

func ensureSnapshotStateEpoch(snapshot *remoteSnapshot) error {
	if snapshotHasStateEpoch(*snapshot) {
		return nil
	}
	table, exists := snapshot.Tables["meta"]
	if !exists {
		return errors.New("state epoch requires a meta table")
	}
	keyIndex := indexOf(table.Columns, "key")
	valueIndex := indexOf(table.Columns, "value")
	if keyIndex < 0 || valueIndex < 0 {
		return errors.New("state epoch requires key and value columns")
	}
	values := make([]any, len(table.Columns))
	values[keyIndex] = "state_epoch"
	values[valueIndex] = "initial"
	if table.Rows == nil {
		table.Rows = make(map[string]remoteRow)
	}
	key := snapshotRowKey(values, 0, table.Name, table.PrimaryKey)
	table.Rows[key] = remoteRow{Values: values, HasRowID: len(table.PrimaryKey) == 0}
	snapshot.Tables["meta"] = table
	snapshot.Epoch = "initial"
	return nil
}

func ensureSnapshotManagementAuthEpoch(snapshot *remoteSnapshot) error {
	if snapshotHasManagementAuthEpoch(*snapshot) {
		if snapshot.AuthEpoch == "" {
			snapshot.AuthEpoch = "initial"
		}
		return nil
	}
	table, exists := snapshot.Tables["meta"]
	if !exists {
		return errors.New("management auth epoch requires a meta table")
	}
	keyIndex := indexOf(table.Columns, "key")
	valueIndex := indexOf(table.Columns, "value")
	if keyIndex < 0 || valueIndex < 0 {
		return errors.New("management auth epoch requires key and value columns")
	}
	values := make([]any, len(table.Columns))
	values[keyIndex] = managementAuthEpochKey
	values[valueIndex] = "initial"
	if table.Rows == nil {
		table.Rows = make(map[string]remoteRow)
	}
	key := snapshotRowKey(values, 0, table.Name, table.PrimaryKey)
	table.Rows[key] = remoteRow{Values: values, HasRowID: len(table.PrimaryKey) == 0}
	snapshot.Tables["meta"] = table
	snapshot.AuthEpoch = "initial"
	return nil
}

func afterObjectsMissing(before, after map[string]remoteSchemaObject) []remoteSchemaObject {
	values := make([]remoteSchemaObject, 0)
	for key, object := range after {
		if _, exists := before[key]; !exists {
			values = append(values, object)
		}
	}
	return values
}

func orderedSnapshotTables(tables map[string]remoteTableSnapshot) []string {
	objects := make([]remoteSchemaObject, 0, len(tables))
	for name := range tables {
		objects = append(objects, remoteSchemaObject{Type: "table", Name: name})
	}
	values := orderedTables(objects)
	return values
}

func appendTableDiff(statements *[]remoteStatement, before remoteTableSnapshot, beforeExists bool, after remoteTableSnapshot, expectedEpoch, expectedAuthEpoch string) error {
	if !beforeExists {
		before = remoteTableSnapshot{Name: after.Name, Columns: after.Columns, PrimaryKey: after.PrimaryKey, Rows: map[string]remoteRow{}}
	}
	for _, key := range sortedSnapshotRowKeys(before) {
		row := before.Rows[key]
		if _, exists := after.Rows[key]; exists {
			continue
		}
		if isEpochRow(after, row) {
			continue
		}
		appendDelete(statements, after, row, expectedEpoch, expectedAuthEpoch)
	}
	for _, key := range sortedSnapshotRowKeys(after) {
		row := after.Rows[key]
		previous, exists := before.Rows[key]
		if !exists {
			if isEpochRow(after, row) {
				continue
			}
			appendInsert(statements, after, row, expectedEpoch, expectedAuthEpoch)
			continue
		}
		if isEpochRow(after, row) {
			continue
		}
		if rowsEqual(previous, row) {
			continue
		}
		if previous.HasRowID != row.HasRowID || (previous.HasRowID && previous.RowID != row.RowID) {
			appendDelete(statements, after, previous, expectedEpoch, expectedAuthEpoch)
			appendInsert(statements, after, row, expectedEpoch, expectedAuthEpoch)
			continue
		}
		if err := appendUpdate(statements, after, previous, row, expectedEpoch, expectedAuthEpoch); err != nil {
			return err
		}
	}
	return nil
}

func sortedSnapshotRowKeys(table remoteTableSnapshot) []string {
	keys := make([]string, 0, len(table.Rows))
	for key := range table.Rows {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		leftRow, rightRow := table.Rows[keys[left]], table.Rows[keys[right]]
		if table.Name == "operations" && leftRow.RowID != rightRow.RowID {
			return leftRow.RowID < rightRow.RowID
		}
		return keys[left] < keys[right]
	})
	return keys
}

func isStateEpochRow(table remoteTableSnapshot, row remoteRow) bool {
	if table.Name != "meta" {
		return false
	}
	keyIndex := indexOf(table.Columns, "key")
	return keyIndex >= 0 && keyIndex < len(row.Values) && fmt.Sprint(row.Values[keyIndex]) == "state_epoch"
}

func isManagementAuthEpochRow(table remoteTableSnapshot, row remoteRow) bool {
	if table.Name != "meta" {
		return false
	}
	keyIndex := indexOf(table.Columns, "key")
	return keyIndex >= 0 && keyIndex < len(row.Values) && fmt.Sprint(row.Values[keyIndex]) == managementAuthEpochKey
}

func isEpochRow(table remoteTableSnapshot, row remoteRow) bool {
	return isStateEpochRow(table, row) || isManagementAuthEpochRow(table, row)
}

func epochGuard(expectedEpoch, expectedAuthEpoch string) (string, []any) {
	return `EXISTS (SELECT 1 FROM "meta" WHERE "key" = 'state_epoch' AND "value" = ?) AND EXISTS (SELECT 1 FROM "meta" WHERE "key" = 'management_auth_epoch' AND "value" = ?)`, []any{expectedEpoch, expectedAuthEpoch}
}

func appendDelete(statements *[]remoteStatement, table remoteTableSnapshot, row remoteRow, expectedEpoch, expectedAuthEpoch string) {
	where, args := rowPredicate(table, row)
	guard, guardArgs := epochGuard(expectedEpoch, expectedAuthEpoch)
	*statements = append(*statements, remoteStatement{query: "DELETE FROM " + quoteIdentifier(table.Name) + " WHERE " + where + " AND " + guard, args: append(args, guardArgs...), epochGuard: true, expectedRows: 1})
}

func appendInsert(statements *[]remoteStatement, table remoteTableSnapshot, row remoteRow, expectedEpoch, expectedAuthEpoch string) {
	columns := append([]string(nil), table.Columns...)
	values := append([]any(nil), row.Values...)
	if row.HasRowID {
		columns = append([]string{"rowid"}, columns...)
		values = append([]any{row.RowID}, values...)
	}
	placeholders := make([]string, len(columns))
	for index := range placeholders {
		placeholders[index] = "?"
	}
	guard, guardArgs := epochGuard(expectedEpoch, expectedAuthEpoch)
	query := "INSERT INTO " + quoteIdentifier(table.Name) + "(" + joinIdentifiers(columns) + ") SELECT " + strings.Join(placeholders, ", ") + " WHERE " + guard
	*statements = append(*statements, remoteStatement{query: query, args: append(values, guardArgs...), epochGuard: true, expectedRows: 1})
}

func appendUpdate(statements *[]remoteStatement, table remoteTableSnapshot, previous, current remoteRow, expectedEpoch, expectedAuthEpoch string) error {
	primary := make(map[int]bool, len(table.PrimaryKey))
	for _, index := range table.PrimaryKey {
		primary[index] = true
	}
	sets := make([]string, 0, len(table.Columns))
	args := make([]any, 0, len(table.Columns))
	for index, column := range table.Columns {
		if primary[index] {
			continue
		}
		sets = append(sets, quoteIdentifier(column)+" = ?")
		args = append(args, current.Values[index])
	}
	if len(sets) == 0 {
		return fmt.Errorf("row changed without an updateable column in %s", table.Name)
	}
	where, whereArgs := rowPredicate(table, previous)
	guard, guardArgs := epochGuard(expectedEpoch, expectedAuthEpoch)
	args = append(args, whereArgs...)
	args = append(args, guardArgs...)
	*statements = append(*statements, remoteStatement{query: "UPDATE " + quoteIdentifier(table.Name) + " SET " + strings.Join(sets, ", ") + " WHERE " + where + " AND " + guard, args: args, epochGuard: true, expectedRows: 1})
	return nil
}

func rowPredicate(table remoteTableSnapshot, row remoteRow) (string, []any) {
	if len(table.PrimaryKey) == 0 {
		return "rowid = ?", []any{row.RowID}
	}
	parts := make([]string, 0, len(table.PrimaryKey))
	args := make([]any, 0, len(table.PrimaryKey))
	for _, index := range table.PrimaryKey {
		parts = append(parts, quoteIdentifier(table.Columns[index])+" = ?")
		args = append(args, row.Values[index])
	}
	return strings.Join(parts, " AND "), args
}

func equalIntSlices(left, right []int) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func migrationSnapshotsEqual(left, right remoteSnapshot) bool {
	return snapshotsEqual(left, right)
}

// MigrateSQLiteToRQLite is intentionally fail-closed until the service and
// deployment layers provide a real migration fence and quiescence proof.
func MigrateSQLiteToRQLite(context.Context, string, string, SQLiteMigrationGate) error {
	return errors.New("state migration is disabled until service/deployment fence evidence is integrated")
}

// migrateSQLiteToRQLite imports a fenced SQLite snapshot into a pristine
// rqlite database for state-package validation. It is deliberately unexported
// so production callers cannot mistake caller-supplied booleans for a real
// migration fence. A failed rqlite transaction leaves the target unchanged,
// while a lost response is returned as an unknown commit.
func migrateSQLiteToRQLite(ctx context.Context, sqlitePath, rqliteURL string, gate SQLiteMigrationGate) error {
	if !gate.StateWriteFenced || !gate.WorkersQuiescent || !gate.SQLiteWritersClosed {
		return errors.New("state migration requires a write fence, quiescent workers, and closed SQLite writers")
	}
	source, err := openSQLiteReadOnly(sqlitePath)
	if err != nil {
		return err
	}
	defer source.Close()
	if err := requireSQLiteSchema(ctx, source); err != nil {
		return err
	}
	if count, err := countMigrationBlockingOperations(ctx, source); err != nil {
		return err
	} else if count != 0 {
		return fmt.Errorf("state migration blocked by %d pending or unknown operations", count)
	}
	sourceSnapshot, err := snapshotDB(ctx, source)
	if err != nil {
		return fmt.Errorf("snapshot SQLite state: %w", err)
	}
	if err := ensureSnapshotStateEpoch(&sourceSnapshot); err != nil {
		return fmt.Errorf("initialize SQLite state epoch: %w", err)
	}
	if err := ensureSnapshotManagementAuthEpoch(&sourceSnapshot); err != nil {
		return fmt.Errorf("initialize SQLite management auth epoch: %w", err)
	}
	if err := validateStateEpoch(sourceSnapshot.Epoch); err != nil {
		return fmt.Errorf("validate SQLite state epoch: %w", err)
	}

	target, err := newRemoteStore(rqliteURL)
	if err != nil {
		return err
	}
	defer target.Close()
	target.remote.mu.Lock()
	defer target.remote.mu.Unlock()
	targetObjects, err := target.remote.remoteSchema(ctx)
	if err != nil {
		return fmt.Errorf("probe rqlite target: %w", err)
	}
	targetSnapshot := remoteSnapshot{
		Objects:   make(map[string]remoteSchemaObject),
		Tables:    make(map[string]remoteTableSnapshot),
		Epoch:     "initial",
		AuthEpoch: "initial",
	}
	if len(targetObjects) != 0 {
		if err := requireSupportedSchema(ctx, targetObjects); err != nil {
			return fmt.Errorf("validate rqlite target schema: %w", err)
		}
		if err := target.remote.ensureRemoteEpoch(ctx); err != nil {
			return fmt.Errorf("validate rqlite epochs: %w", err)
		}
		if err := target.remote.syncLocked(ctx); err != nil {
			return fmt.Errorf("refresh rqlite target: %w", err)
		}
		targetSnapshot, err = snapshotDB(ctx, target.remote.mirror)
		if err != nil {
			return fmt.Errorf("snapshot rqlite target: %w", err)
		}
	}
	pristine, err := pristineRemoteTarget(ctx, targetSnapshot)
	if err != nil {
		return fmt.Errorf("validate rqlite target contents: %w", err)
	}
	if !pristine {
		return errors.New("rqlite migration target is not pristine")
	}
	applied, err := target.remote.applySnapshot(ctx, targetSnapshot, sourceSnapshot, targetSnapshot.Epoch, sourceSnapshot.Epoch)
	if err != nil {
		var statementErr *remoteStatementError
		cause := fmt.Errorf("import SQLite state into rqlite: %w", err)
		if errors.As(err, &statementErr) {
			return &TransactionError{Phase: "commit", SQLOperation: "rqlite_migration", StateCommitted: committed(false), Cause: cause}
		}
		return &TransactionError{Phase: "commit", SQLOperation: "rqlite_migration", StateCommitted: nil, Cause: cause}
	}
	if !applied {
		return &TransactionError{Phase: "commit", SQLOperation: "rqlite_migration", StateCommitted: committed(false), Cause: errors.New("rqlite migration target changed concurrently")}
	}
	if err := target.remote.syncLocked(ctx); err != nil {
		return &TransactionError{Phase: "verify", SQLOperation: "rqlite_migration", StateCommitted: nil, Cause: fmt.Errorf("refresh imported rqlite state: %w", err)}
	}
	imported, err := snapshotDB(ctx, target.remote.mirror)
	if err != nil {
		return &TransactionError{Phase: "verify", SQLOperation: "rqlite_migration", StateCommitted: nil, Cause: fmt.Errorf("verify imported rqlite state: %w", err)}
	}
	if !migrationSnapshotsEqual(sourceSnapshot, imported) {
		return &TransactionError{Phase: "verify", SQLOperation: "rqlite_migration", StateCommitted: nil, Cause: errors.New("rqlite import verification mismatch")}
	}
	return nil
}

func openSQLiteReadOnly(path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("%s: empty path", missingDBCode)
	}
	readonlyPath, err := sqliteReadOnlyDSN(path)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", readonlyPath)
	if err != nil {
		return nil, fmt.Errorf("open SQLite read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open SQLite read-only: %w", err)
	}
	return db, nil
}

func sqliteReadOnlyDSN(path string) (string, error) {
	if strings.HasPrefix(path, "file:") {
		parsed, err := url.Parse(path)
		if err != nil {
			return "", fmt.Errorf("parse SQLite URI: %w", err)
		}
		query := parsed.Query()
		query.Set("mode", "ro")
		parsed.RawQuery = query.Encode()
		return parsed.String(), nil
	}
	if strings.Contains(path, "?") {
		return "", errors.New("SQLite paths with query parameters must use a file: URI")
	}
	return "file:" + path + "?mode=ro", nil
}

func requireSQLiteSchema(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	objects, err := localSchemaObjects(ctx, conn)
	if err != nil {
		return err
	}
	return requireSupportedSchema(ctx, objects)
}

func countMigrationBlockingOperations(ctx context.Context, db *sql.DB) (int, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	blocking := 0
	rows, err := conn.QueryContext(ctx, "SELECT operation_id, response_json FROM operations")
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var operationID, raw string
		if err := rows.Scan(&operationID, &raw); err != nil {
			_ = rows.Close()
			return 0, err
		}
		var response map[string]any
		if err := json.Unmarshal([]byte(raw), &response); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("operation %s has invalid response JSON: %w", operationID, err)
		}
		outcome, ok := response["outcome"].(string)
		if !ok || (outcome != "success" && outcome != "pending" && outcome != "error") {
			_ = rows.Close()
			return 0, fmt.Errorf("operation %s has unsupported outcome %q", operationID, outcome)
		}
		if outcome == "pending" {
			blocking++
			continue
		}
		if outcome == "error" {
			errorValue, _ := response["error"].(map[string]any)
			if code, _ := errorValue["code"].(string); code == "operation_outcome_unknown" {
				blocking++
			}
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	managementBlocking := 0
	var managementTable string
	if err := conn.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'management_operations'").Scan(&managementTable); err == nil {
		managementRows, err := conn.QueryContext(ctx, "SELECT operation_id, status FROM management_operations")
		if err != nil {
			return 0, err
		}
		for managementRows.Next() {
			var operationID, status string
			if err := managementRows.Scan(&operationID, &status); err != nil {
				_ = managementRows.Close()
				return 0, err
			}
			switch status {
			case "awaiting_upload", "queued", "running", "reconciling", "outcome_unknown":
				managementBlocking++
			case "succeeded", "failed":
			default:
				_ = managementRows.Close()
				return 0, fmt.Errorf("management operation %s has unsupported status %q", operationID, status)
			}
		}
		if err := managementRows.Err(); err != nil {
			_ = managementRows.Close()
			return 0, err
		}
		if err := managementRows.Close(); err != nil {
			return 0, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return blocking + managementBlocking, nil
}

func pristineRemoteTarget(ctx context.Context, snapshot remoteSnapshot) (bool, error) {
	if len(snapshot.Objects) == 0 {
		return len(snapshot.Tables) == 0 && snapshot.Epoch == "initial", nil
	}
	canonical, err := canonicalCoreSchema(ctx)
	if err != nil {
		return false, err
	}
	if len(snapshot.Objects) != len(canonical) {
		return false, nil
	}
	for key, expected := range canonical {
		actual, exists := snapshot.Objects[key]
		if !exists || normalizeSchemaSQL(actual.SQL) != normalizeSchemaSQL(expected.SQL) {
			return false, nil
		}
	}
	if snapshot.Epoch != "initial" {
		return false, nil
	}
	for table := range snapshot.Tables {
		if table != "meta" && table != "tasks" && table != "ports" && table != "operations" && table != "audit" {
			return false, nil
		}
	}
	for table, value := range snapshot.Tables {
		if table == "meta" {
			if len(value.Rows) != 1 && len(value.Rows) != 2 {
				return false, nil
			}
			valueIndex := indexOf(value.Columns, "value")
			if valueIndex < 0 {
				return false, nil
			}
			for key, row := range value.Rows {
				if key == "" || valueIndex >= len(row.Values) || fmt.Sprint(row.Values[valueIndex]) != "initial" || (!isStateEpochRow(value, row) && !isManagementAuthEpochRow(value, row)) {
					return false, nil
				}
			}
			continue
		}
		if len(value.Rows) != 0 {
			return false, nil
		}
	}
	return true, nil
}
