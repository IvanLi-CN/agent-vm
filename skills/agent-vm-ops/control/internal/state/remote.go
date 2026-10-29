package state

// This file contains the rqlite state adapter. The public Store methods keep
// their existing database/sql-shaped implementation, while the adapter uses a
// short-lived SQLite mirror to evaluate those callbacks and submits the
// resulting row/schema delta as one rqlite request-level transaction. The
// mirror is never a second authority: every read refreshes it from a strong
// rqlite query, and every write is fenced by state_epoch in the remote batch.

import (
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
	base   *url.URL
	client *http.Client
	mirror *sql.DB
	mu     sync.Mutex
}

type remoteStatement struct {
	query string
	args  []any
}

type remoteResult struct {
	Columns      []string `json:"columns"`
	Types        []string `json:"types"`
	Values       [][]any  `json:"values"`
	RowsAffected int64    `json:"rows_affected"`
	LastInsertID int64    `json:"last_insert_id"`
	Error        string   `json:"error"`
	ErrorV2      any      `json:"error_v2"`
}

type remoteResponse struct {
	Results []remoteResult `json:"results"`
}

type remoteStatementError struct {
	Index   int
	Message string
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
	Objects map[string]remoteSchemaObject
	Tables  map[string]remoteTableSnapshot
	Epoch   string
}

// SQLiteMigrationGate is the proof supplied by the caller that the one-time
// migration fence has already stopped new durable writes, drained native
// workers, and closed all SQLite writers. The adapter deliberately fails
// closed when any one of those facts is absent.
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
	if err := backend.open(context.Background(), allowCreate); err != nil {
		_ = backend.close()
		return nil, err
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
	if allowCreate {
		statements := make([]remoteStatement, 0, 8)
		for _, statement := range splitSQLStatements(schema) {
			statements = append(statements, remoteStatement{query: statement})
		}
		statements = append(statements, remoteStatement{query: "INSERT OR IGNORE INTO meta(key, value) VALUES('state_epoch', 'initial')"})
		if _, err := r.request(ctx, statements, true); err != nil {
			return fmt.Errorf("initialize rqlite schema: %w", err)
		}
		objects, err = r.remoteSchema(ctx)
		if err != nil {
			return fmt.Errorf("rqlite schema recheck: %w", err)
		}
	}
	if err := requireCoreSchema(objects); err != nil {
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

func (r *remoteBackend) ensureRemoteEpoch(ctx context.Context) error {
	result, err := r.query(ctx, "SELECT value FROM meta WHERE key = 'state_epoch'")
	if err != nil {
		return fmt.Errorf("read rqlite state epoch: %w", err)
	}
	if len(result.Values) != 0 {
		return nil
	}
	if _, err := r.request(ctx, []remoteStatement{{query: "INSERT OR IGNORE INTO meta(key, value) VALUES('state_epoch', 'initial')"}}, true); err != nil {
		return fmt.Errorf("initialize rqlite state epoch: %w", err)
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
				stateCommitted := committed(false)
				if rollbackErr != nil {
					stateCommitted = nil
				}
				return &TransactionError{Phase: "commit", SQLOperation: "state_epoch", StateCommitted: stateCommitted, RollbackError: rollbackErr, Cause: err}
			}
		}
		if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
			_, rollbackErr := conn.ExecContext(context.Background(), "ROLLBACK")
			_ = conn.Close()
			return &TransactionError{Phase: "commit", SQLOperation: "commit", StateCommitted: nil, RollbackError: rollbackErr, Cause: err}
		}
		tx.closed = true
		_ = conn.Close()

		after, err := snapshotDB(ctx, r.mirror)
		if err != nil {
			return &TransactionError{Phase: "commit", SQLOperation: "snapshot", StateCommitted: nil, Cause: err}
		}
		if snapshotsEqual(before, after) {
			return nil
		}
		applied, err := r.applySnapshot(ctx, before, after, before.Epoch, after.Epoch)
		if err != nil {
			// Once the HTTP request has been sent, a transport failure cannot
			// establish whether rqlite committed. Preserve the operation as
			// unknown instead of retrying it under a new request.
			var statementErr *remoteStatementError
			if errors.As(err, &statementErr) {
				return &TransactionError{Phase: "commit", SQLOperation: "rqlite_transaction", StateCommitted: committed(false), Cause: err}
			}
			return &TransactionError{Phase: "commit", SQLOperation: "rqlite_transaction", StateCommitted: nil, Cause: err}
		}
		if applied {
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
	var result remoteResponse
	decoder := json.NewDecoder(response)
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return remoteResponse{}, fmt.Errorf("decode rqlite response: %w", err)
	}
	for index, item := range result.Results {
		if item.Error != "" {
			return result, &remoteStatementError{Index: index, Message: item.Error}
		}
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
	var result remoteResponse
	decoder := json.NewDecoder(response)
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return remoteResult{}, fmt.Errorf("decode rqlite query: %w", err)
	}
	if len(result.Results) == 0 {
		return remoteResult{}, nil
	}
	if result.Results[0].Error != "" {
		return remoteResult{}, &remoteStatementError{Index: 0, Message: result.Results[0].Error}
	}
	return result.Results[0], nil
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
		if len(row) < 3 {
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
		objects, err := r.remoteSchema(ctx)
		if err != nil {
			return err
		}
		if err := requireCoreSchema(objects); err != nil {
			return err
		}
		if err := r.rebuildMirror(ctx, objects); err != nil {
			return fmt.Errorf("refresh rqlite mirror: %w", err)
		}
		endEpoch, err := r.remoteEpoch(ctx)
		if err != nil {
			return err
		}
		if startEpoch == endEpoch {
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
	if len(result.Values) != 1 || len(result.Values[0]) < 1 {
		return "", errors.New("rqlite state epoch is missing")
	}
	return fmt.Sprint(result.Values[0][0]), nil
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
	order := []string{"meta", "tasks", "ports", "operations", "audit", "state_capabilities", "managed_images", "managed_image_tombstones", "image_references", "management_operations", "management_audit"}
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
		Objects: make(map[string]remoteSchemaObject, len(objects)),
		Tables:  make(map[string]remoteTableSnapshot),
		Epoch:   "initial",
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
				if fmt.Sprint(row.Values[keyIndex]) == "state_epoch" {
					snapshot.Epoch = fmt.Sprint(row.Values[valueIndex])
					break
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
	if left.Epoch != right.Epoch || len(left.Objects) != len(right.Objects) || len(left.Tables) != len(right.Tables) {
		return false
	}
	for key, object := range left.Objects {
		other, exists := right.Objects[key]
		if !exists || object.SQL != other.SQL {
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
	if targetEpoch == "" {
		targetEpoch = after.Epoch
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
		if !exists || other.SQL != object.SQL {
			return false, fmt.Errorf("rqlite schema changed for %s", key)
		}
	}

	for _, tableName := range orderedSnapshotTables(after.Tables) {
		afterTable := after.Tables[tableName]
		beforeTable, exists := before.Tables[tableName]
		if exists && (!equalStrings(beforeTable.Columns, afterTable.Columns) || !equalIntSlices(beforeTable.PrimaryKey, afterTable.PrimaryKey)) {
			return false, fmt.Errorf("rqlite table layout changed for %s", tableName)
		}
		if err := appendTableDiff(&statements, beforeTable, exists, afterTable, expectedEpoch); err != nil {
			return false, err
		}
	}
	if len(statements) == 0 {
		return true, nil
	}
	statements = append(statements,
		remoteStatement{
			query: "UPDATE meta SET value = ? WHERE key = 'state_epoch' AND value = ?",
			args:  []any{targetEpoch, expectedEpoch},
		},
	)
	response, err := r.request(ctx, statements, true)
	if err != nil {
		return false, err
	}
	if len(response.Results) == 0 {
		return false, errors.New("rqlite transaction did not return its epoch update")
	}
	// /db/execute returns statement metadata rather than row values, so the
	// guarded epoch update's rows_affected field is the commit proof. A zero
	// count means another writer changed the epoch before this transaction.
	update := response.Results[len(response.Results)-1]
	if update.RowsAffected != 1 {
		return false, nil
	}
	return true, nil
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

func appendTableDiff(statements *[]remoteStatement, before remoteTableSnapshot, beforeExists bool, after remoteTableSnapshot, expectedEpoch string) error {
	if !beforeExists {
		before = remoteTableSnapshot{Name: after.Name, Columns: after.Columns, PrimaryKey: after.PrimaryKey, Rows: map[string]remoteRow{}}
	}
	for _, key := range sortedSnapshotRowKeys(before) {
		row := before.Rows[key]
		if _, exists := after.Rows[key]; exists {
			continue
		}
		if isStateEpochRow(after, row) {
			continue
		}
		appendDelete(statements, after, row, expectedEpoch)
	}
	for _, key := range sortedSnapshotRowKeys(after) {
		row := after.Rows[key]
		previous, exists := before.Rows[key]
		if !exists {
			if isStateEpochRow(after, row) {
				continue
			}
			appendInsert(statements, after, row, expectedEpoch)
			continue
		}
		if isStateEpochRow(after, row) {
			continue
		}
		if rowsEqual(previous, row) {
			continue
		}
		if previous.HasRowID != row.HasRowID || (previous.HasRowID && previous.RowID != row.RowID) {
			appendDelete(statements, after, previous, expectedEpoch)
			appendInsert(statements, after, row, expectedEpoch)
			continue
		}
		if err := appendUpdate(statements, after, previous, row, expectedEpoch); err != nil {
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

func epochGuard(expectedEpoch string) (string, []any) {
	return `EXISTS (SELECT 1 FROM "meta" WHERE "key" = 'state_epoch' AND "value" = ?)`, []any{expectedEpoch}
}

func appendDelete(statements *[]remoteStatement, table remoteTableSnapshot, row remoteRow, expectedEpoch string) {
	where, args := rowPredicate(table, row)
	guard, guardArgs := epochGuard(expectedEpoch)
	*statements = append(*statements, remoteStatement{query: "DELETE FROM " + quoteIdentifier(table.Name) + " WHERE " + where + " AND " + guard, args: append(args, guardArgs...)})
}

func appendInsert(statements *[]remoteStatement, table remoteTableSnapshot, row remoteRow, expectedEpoch string) {
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
	guard, guardArgs := epochGuard(expectedEpoch)
	query := "INSERT INTO " + quoteIdentifier(table.Name) + "(" + joinIdentifiers(columns) + ") SELECT " + strings.Join(placeholders, ", ") + " WHERE " + guard
	*statements = append(*statements, remoteStatement{query: query, args: append(values, guardArgs...)})
}

func appendUpdate(statements *[]remoteStatement, table remoteTableSnapshot, previous, current remoteRow, expectedEpoch string) error {
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
	guard, guardArgs := epochGuard(expectedEpoch)
	args = append(args, whereArgs...)
	args = append(args, guardArgs...)
	*statements = append(*statements, remoteStatement{query: "UPDATE " + quoteIdentifier(table.Name) + " SET " + strings.Join(sets, ", ") + " WHERE " + where + " AND " + guard, args: args})
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
	if len(left.Objects) != len(right.Objects) || len(left.Tables) != len(right.Tables) || left.Epoch != right.Epoch {
		return false
	}
	for key := range left.Objects {
		if _, exists := right.Objects[key]; !exists {
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

// MigrateSQLiteToRQLite imports a fenced SQLite snapshot into a pristine
// rqlite database. It never opens the source as a writer and refuses to
// overwrite an existing target. A failed rqlite transaction leaves the target
// unchanged, while a lost response is returned as an unknown commit.
func MigrateSQLiteToRQLite(ctx context.Context, sqlitePath, rqliteURL string, gate SQLiteMigrationGate) error {
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

	target, err := openRemote(rqliteURL, true)
	if err != nil {
		return err
	}
	defer target.Close()
	target.remote.mu.Lock()
	defer target.remote.mu.Unlock()
	targetSnapshot, err := snapshotDB(ctx, target.remote.mirror)
	if err != nil {
		return fmt.Errorf("snapshot rqlite target: %w", err)
	}
	if !pristineRemoteTarget(targetSnapshot) {
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
		return errors.New("rqlite migration target changed concurrently")
	}
	if err := target.remote.syncLocked(ctx); err != nil {
		return fmt.Errorf("refresh imported rqlite state: %w", err)
	}
	imported, err := snapshotDB(ctx, target.remote.mirror)
	if err != nil {
		return fmt.Errorf("verify imported rqlite state: %w", err)
	}
	if !migrationSnapshotsEqual(sourceSnapshot, imported) {
		return errors.New("rqlite import verification mismatch")
	}
	return nil
}

func openSQLiteReadOnly(path string) (*sql.DB, error) {
	if path == "" {
		return nil, fmt.Errorf("%s: empty path", missingDBCode)
	}
	readonlyPath := path
	if !strings.HasPrefix(path, "file:") && !strings.Contains(path, "?") {
		readonlyPath = "file:" + path + "?mode=ro"
	}
	db, err := sql.Open("sqlite", readonlyPath)
	if err != nil {
		return nil, fmt.Errorf("open SQLite read-only: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
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
	return requireCoreSchema(objects)
}

func countMigrationBlockingOperations(ctx context.Context, db *sql.DB) (int, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	var pending, unknown int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM operations WHERE json_extract(response_json, '$.outcome') = 'pending'`).Scan(&pending); err != nil {
		return 0, err
	}
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM operations WHERE json_extract(response_json, '$.outcome') = 'error' AND json_extract(response_json, '$.error.code') = 'operation_outcome_unknown'`).Scan(&unknown); err != nil {
		return 0, err
	}
	managementBlocking := 0
	var managementTable string
	if err := conn.QueryRowContext(ctx, "SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'management_operations'").Scan(&managementTable); err == nil {
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM management_operations WHERE status IN ('awaiting_upload', 'queued', 'running', 'reconciling', 'outcome_unknown')`).Scan(&managementBlocking); err != nil {
			return 0, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return pending + unknown + managementBlocking, nil
}

func pristineRemoteTarget(snapshot remoteSnapshot) bool {
	for table := range snapshot.Tables {
		if table != "meta" && table != "tasks" && table != "ports" && table != "operations" && table != "audit" {
			return false
		}
	}
	for table, value := range snapshot.Tables {
		if table == "meta" {
			for key, row := range value.Rows {
				if !isStateEpochRow(value, row) || key == "" {
					return false
				}
			}
			continue
		}
		if len(value.Rows) != 0 {
			return false
		}
	}
	return true
}
