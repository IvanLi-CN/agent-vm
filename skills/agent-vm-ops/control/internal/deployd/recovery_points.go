package deployd

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/deployd/store"
	"github.com/IvanLi-CN/agent-vm/skills/agent-vm-ops/control/internal/state"
)

var errBackupStorageExhausted = errors.New("backup_storage_exhausted")

const recoveryRetention = 30 * 24 * time.Hour
const recoveryRecordReserve = 4096

type recoveryPointCreator interface {
	CreateRecoveryPoint(context.Context, string, string, string, map[string]bool, time.Time) error
	RecoveryAuthority() string
}
type recoveryPointValidator interface {
	ValidateRecoveryPoint(context.Context, string, string, string) (recoveryPoint, error)
}

func (manager *Manager) usesRecoveryPoints() bool {
	runtime, ok := manager.Runtime.(recoveryPointCreator)
	return ok && runtime.RecoveryAuthority() != ""
}

func (runtime *HostRuntime) RecoveryAuthority() string {
	endpoint, err := url.Parse(runtime.Config.Migration.RQLiteURL)
	if err != nil || endpoint.Host == "" {
		return ""
	}
	return state.RQLiteAuthorityIdentity(*endpoint)
}

func (manager *Manager) prepareRecoveryPoint(ctx context.Context, transaction store.Transaction, purpose string, active SlotObservation) error {
	runtime, ok := manager.Runtime.(recoveryPointCreator)
	if !ok || runtime.RecoveryAuthority() == "" || transaction.PreviousSlot == "none" {
		return nil
	}
	if active.StateAuthority != runtime.RecoveryAuthority() {
		return errors.New("active slot state authority does not match the configured rqlite recovery authority")
	}
	pending, err := manager.Store.ListActive(ctx)
	if err != nil {
		return err
	}
	protected := map[string]bool{transaction.ID: true}
	for _, operation := range pending {
		protected[operation.ID] = true
	}
	rollbackTarget, err := manager.Store.LatestRollbackTarget(ctx)
	if err != nil {
		return err
	}
	protected[rollbackTarget] = true
	return manager.withControllerRenewal(ctx, active, func(ctx context.Context) error {
		return runtime.CreateRecoveryPoint(ctx, transaction.ID, purpose, active.StateAuthority, protected, manager.Clock().UTC())
	})
}

type recoveryPoint struct {
	ID         string    `json:"id"`
	Operation  string    `json:"deployment_operation_id"`
	Purpose    string    `json:"purpose"`
	Authority  string    `json:"state_authority"`
	SHA256     string    `json:"sha256"`
	Size       int64     `json:"size"`
	Boundary   string    `json:"coverage_marker"`
	Created    time.Time `json:"created_at"`
	LeaseUntil time.Time `json:"lease_until,omitempty"`
}

func (runtime *HostRuntime) recoveryDirectory() string {
	return filepath.Join(runtime.Config.Migration.RecoveryDirectory, "rqlite-recovery")
}

func (runtime *HostRuntime) CreateRecoveryPoint(ctx context.Context, operation, purpose, authority string, protected map[string]bool, now time.Time) error {
	if err := runtime.ensureRecoveryDirectory(); err != nil {
		return err
	}
	root := runtime.recoveryDirectory()
	if err := os.Mkdir(root, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	points, err := loadRecoveryPoints(root)
	if err != nil {
		return err
	}
	all := points
	points = nil
	for _, point := range all {
		if point.Authority != authority {
			continue
		}
		points = append(points, point)
		if point.Operation == operation && point.Purpose == purpose {
			return validateRegisteredRecoveryPoint(ctx, root, point)
		}
	}
	if len(points) > 3 {
		return errBackupStorageExhausted
	}
	if len(points) == 3 {
		oldest := points[0]
		if now.Before(oldest.Created.Add(recoveryRetention)) || now.Before(oldest.LeaseUntil) || protected[oldest.Operation] {
			return errBackupStorageExhausted
		}
		used, err := recoveryMaterialBytes(runtime.Config.Migration.RecoveryDirectory)
		if err != nil {
			return err
		}
		if maxMigrationSourceBytes-used < recoveryRecordReserve {
			return errBackupStorageExhausted
		}
		// Persist the precise deletion target before removing its bytes so an
		// interruption can finish only the deletion that was already approved.
		if err := writeRecoveryRecord(root, "pruned-"+oldest.ID+".json", oldest); err != nil {
			return err
		}
		if err := os.RemoveAll(filepath.Join(root, oldest.ID)); err != nil {
			return err
		}
		if err := syncMigrationDirectory(root); err != nil {
			return err
		}
	}
	used, err := recoveryMaterialBytes(runtime.Config.Migration.RecoveryDirectory)
	if err != nil {
		return err
	}
	free, err := availableMigrationStorageBytes(root)
	if err != nil {
		return err
	}
	limit := min(maxMigrationSourceBytes-used, free) - recoveryRecordReserve
	if limit <= 0 {
		return errBackupStorageExhausted
	}
	point := recoveryPoint{ID: newID("point"), Operation: operation, Purpose: purpose, Authority: authority, Created: now}
	client, endpoint, password, err := runtime.recoveryClient()
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if err := runtime.writeRecoveryMarker(ctx, client, endpoint, point.ID); err != nil {
		return err
	}
	result, err := recoverySQL(ctx, client, endpoint, "/db/query?level=linearizable", "agent-vm-backup", password, []string{recoveryMarkerQuery})
	if err != nil {
		return err
	}
	if len(result.Values) != 1 || len(result.Values[0]) != 1 || !strings.HasSuffix(result.Values[0][0], ":"+point.ID) {
		return errors.New("rqlite recovery coverage marker was not observed")
	}
	point.Boundary = result.Values[0][0]
	tmp, err := os.MkdirTemp(root, ".pending-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	snapshot := filepath.Join(tmp, "snapshot.sqlite3")
	point.Size, point.SHA256, err = downloadRecoverySnapshot(ctx, client, endpoint, password, snapshot, limit)
	if err != nil {
		return err
	}
	if err := verifyRecoverySnapshot(ctx, snapshot, point.Boundary); err != nil {
		return err
	}
	if err := writeRecoveryRecord(tmp, "point.json", point); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(root, point.ID)); err != nil {
		return err
	}
	return syncMigrationDirectory(root)
}

// ValidateRecoveryPoint verifies an existing transaction-bound recovery point without creating,
// deleting, or restoring business state. Recovery actions use it before retrying a staged bundle.
func (runtime *HostRuntime) ValidateRecoveryPoint(ctx context.Context, operation, purpose, authority string) (recoveryPoint, error) {
	var empty recoveryPoint
	if operation == "" || purpose == "" || authority == "" || authority != runtime.RecoveryAuthority() {
		return empty, errors.New("recovery point identity does not match the configured state authority")
	}
	root := runtime.recoveryDirectory()
	info, err := os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return empty, errors.New("rqlite recovery directory is unavailable or not private")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return empty, err
	}
	var match *recoveryPoint
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".pending-") || strings.HasPrefix(name, ".record-") {
			return empty, errors.New("incomplete recovery material prevents stage retry")
		}
		if strings.HasPrefix(name, "pruned-") && strings.HasSuffix(name, ".json") {
			pruned, err := os.Lstat(filepath.Join(root, name))
			if err != nil || pruned.Mode()&os.ModeSymlink != 0 || !pruned.Mode().IsRegular() {
				return empty, errors.New("recovery deletion receipt is invalid")
			}
			continue
		}
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 || !strings.HasPrefix(name, "point-") {
			return empty, errors.New("rqlite recovery directory contains an unrecognized entry")
		}
		data, err := os.ReadFile(filepath.Join(path, "point.json"))
		var point recoveryPoint
		if err != nil || json.Unmarshal(data, &point) != nil || point.ID != name || !validMigrationDigest(point.Authority) ||
			point.Operation == "" || point.Purpose == "" || point.Created.IsZero() || !validMigrationDigest(point.SHA256) ||
			!strings.HasSuffix(point.Boundary, ":"+point.ID) || point.Size <= 0 {
			return empty, errors.New("registered rqlite recovery point is invalid")
		}
		snapshot, err := os.Lstat(filepath.Join(path, "snapshot.sqlite3"))
		if err != nil || snapshot.Mode()&os.ModeSymlink != 0 || !snapshot.Mode().IsRegular() || snapshot.Size() != point.Size {
			return empty, errors.New("registered rqlite recovery material is missing or incomplete")
		}
		if _, err := os.Lstat(filepath.Join(root, "pruned-"+name+".json")); err == nil {
			if point.Operation == operation && point.Purpose == purpose && point.Authority == authority {
				return empty, errors.New("original transaction recovery point is marked for deletion")
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return empty, err
		}
		if point.Operation != operation || point.Purpose != purpose || point.Authority != authority {
			continue
		}
		if match != nil {
			return empty, errors.New("recovery point identity is ambiguous")
		}
		match = &point
	}
	if match == nil {
		return empty, errors.New("original transaction recovery point is missing")
	}
	if err := validateRegisteredRecoveryPoint(ctx, root, *match); err != nil {
		return empty, err
	}
	return *match, nil
}

func (runtime *HostRuntime) writeRecoveryMarker(ctx context.Context, client *http.Client, endpoint, id string) error {
	if runtime.recoveryMarker != nil {
		return runtime.recoveryMarker(ctx, id)
	}
	if os.Geteuid() != 0 {
		slot, err := runtime.activeSlot()
		if err != nil {
			return err
		}
		body, status, err := runtime.slotRequest(ctx, runtime.socket(slot), http.MethodPost, recoveryMarkerRoute, map[string]string{"id": id})
		defaults := Defaults()
		if missingRecoveryMarkerRoute(status) && runtime.Config.BlueSocket == defaults.BlueSocket && runtime.Config.GreenSocket == defaults.GreenSocket {
			payload, _ := json.Marshal(map[string]string{"id": id})
			body, status, err = requestSlotRelay(ctx, slotRelayRequest{Slot: slot, Method: http.MethodPost, Path: recoveryMarkerRoute, Body: payload}, 10*time.Second)
		}
		if err != nil || status != http.StatusOK || !responseOK(body) {
			return errors.New("active slot cannot persist the recovery coverage marker")
		}
		return nil
	}
	// The root-run controller still handles installations whose active release
	// predates the marker route. This credential is never opened by the
	// dedicated deployment account.
	writer, err := runtime.rqliteMigrationURL()
	if err != nil {
		return err
	}
	writerURL, err := url.Parse(writer)
	if err != nil {
		return errors.New("rqlite control credential is invalid")
	}
	password, _ := writerURL.User.Password()
	return writeRecoveryMarkerSQL(ctx, client, endpoint, "agent-vm-control", password, id)
}

func writeRecoveryMarkerSQL(ctx context.Context, client *http.Client, endpoint, user, password, id string) error {
	_, err := recoverySQL(ctx, client, endpoint, "/db/execute", user, password, []any{[]any{
		"INSERT INTO meta(key,value) SELECT 'deployment_recovery_marker', value || ':' || ? FROM meta WHERE key='state_epoch' ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		id,
	}})
	return err
}

func loadRecoveryPoints(root string) ([]recoveryPoint, error) {
	// All mutations run under the deployment manager lock. Pending material can
	// belong only to a failed or interrupted operation when reconciliation runs.
	info, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("rqlite recovery directory is not private")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	removed := false
	var points []recoveryPoint
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(root, name)
		if strings.HasPrefix(name, ".pending-") || strings.HasPrefix(name, ".record-") {
			if !entry.IsDir() && !(strings.HasPrefix(name, ".record-") && entry.Type().IsRegular()) {
				return nil, errors.New("invalid pending recovery point")
			}
			if err := os.RemoveAll(path); err != nil {
				return nil, err
			}
			removed = true
			continue
		}
		if strings.HasPrefix(name, "pruned-") && strings.HasSuffix(name, ".json") && entry.Type().IsRegular() {
			continue
		}
		if !entry.IsDir() || !strings.HasPrefix(name, "point-") {
			return nil, errors.New("unexpected entry in rqlite recovery directory")
		}
		receipt := filepath.Join(root, "pruned-"+name+".json")
		if data, err := os.ReadFile(receipt); err == nil {
			var removedPoint recoveryPoint
			if json.Unmarshal(data, &removedPoint) != nil || removedPoint.ID != name {
				return nil, errors.New("invalid recovery deletion receipt")
			}
			if err := os.RemoveAll(path); err != nil {
				return nil, err
			}
			removed = true
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		data, err := os.ReadFile(filepath.Join(path, "point.json"))
		var point recoveryPoint
		if err != nil || json.Unmarshal(data, &point) != nil || point.ID != name || !validMigrationDigest(point.Authority) ||
			point.Operation == "" || point.Created.IsZero() || !validMigrationDigest(point.SHA256) ||
			!strings.HasSuffix(point.Boundary, ":"+point.ID) || point.Size <= 0 {
			return nil, errors.New("invalid registered rqlite recovery point")
		}
		snapshot, err := os.Lstat(filepath.Join(path, "snapshot.sqlite3"))
		if err != nil || !snapshot.Mode().IsRegular() || snapshot.Size() != point.Size {
			return nil, errors.New("registered rqlite recovery material is missing or incomplete")
		}
		points = append(points, point)
	}
	if removed {
		if err := syncMigrationDirectory(root); err != nil {
			return nil, err
		}
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Created.Before(points[j].Created) })
	return points, nil
}

func validateRegisteredRecoveryPoint(ctx context.Context, root string, point recoveryPoint) error {
	path := filepath.Join(root, point.ID, "snapshot.sqlite3")
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	sum := sha256.New()
	size, err := io.Copy(sum, io.LimitReader(file, point.Size+1))
	if err != nil || hex.EncodeToString(sum.Sum(nil)) != point.SHA256 || size != point.Size {
		return errors.New("registered rqlite recovery material does not match its digest")
	}
	return verifyRecoverySnapshot(ctx, path, point.Boundary)
}

func (runtime *HostRuntime) ReconcileRecoveryPoints() error {
	_, err := loadRecoveryPoints(runtime.recoveryDirectory())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func writeRecoveryRecord(directory, name string, point recoveryPoint) error {
	var out *os.File
	out, err := os.CreateTemp(directory, ".record-")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	writeErr := json.NewEncoder(out).Encode(point)
	syncErr := out.Sync()
	if err := errors.Join(writeErr, syncErr, out.Close()); err != nil {
		return err
	}
	if err := os.Rename(out.Name(), filepath.Join(directory, name)); err != nil {
		return err
	}
	return syncMigrationDirectory(directory)
}

// Both one-time SQLite migration snapshots and rqlite recovery material
// consume the same byte budget, including files still being written.
func recoveryMaterialBytes(directory string) (int64, error) {
	var total int64
	err := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("recovery storage contains a non-regular file")
		}
		if info.Size() > maxMigrationSourceBytes-total {
			return errBackupStorageExhausted
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func (runtime *HostRuntime) recoveryClient() (*http.Client, string, string, error) {
	config := runtime.Config.Migration
	endpoint, err := url.Parse(config.RQLiteURL)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		(endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return nil, "", "", errors.New("rqlite recovery requires the configured HTTPS authority address")
	}
	ca, err := readRecoveryConfig(config.RQLiteBackupCA, false)
	if err != nil {
		return nil, "", "", err
	}
	auth, err := readRecoveryConfig(config.RQLiteBackupAuth, true)
	if err != nil {
		return nil, "", "", err
	}
	client, password, err := newRecoveryClient(ca, auth)
	return client, strings.TrimRight(endpoint.String(), "/"), password, err
}

// ProbeInstalledBackupReader verifies that the supplied credentials and CA
// work against the live, local authority without changing business state.
func ProbeInstalledBackupReader(ctx context.Context, ca, auth []byte) error {
	client, password, err := newRecoveryClient(ca, auth)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	const expected = "agent-vm-backup-preflight"
	got, err := recoverySQL(ctx, client, "https://127.0.0.1:4001", "/db/query?level=linearizable",
		"agent-vm-backup", password, []string{"SELECT '" + expected + "'"})
	if err != nil || len(got.Values) != 1 || len(got.Values[0]) != 1 || got.Values[0][0] != expected {
		return errors.New("live rqlite did not confirm the restricted backup identity over TLS")
	}
	return nil
}

func newRecoveryClient(ca, auth []byte) (*http.Client, string, error) {
	client, err := recoveryTLSClient(ca)
	if err != nil {
		return nil, "", err
	}
	var credential backupCredential
	if json.Unmarshal(auth, &credential) != nil || !validBackupPassword(credential.Password) {
		return nil, "", errors.New("rqlite recovery credential is invalid")
	}
	return client, credential.Password, nil
}

func recoveryTLSClient(ca []byte) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("rqlite recovery CA is invalid")
	}
	return &http.Client{
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       5 * time.Minute,
	}, nil
}

type backupCredential struct {
	Password string `json:"password"`
}

func validBackupPassword(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.HasPrefix(value, "<")
}

// ExportBackupCredential selects only the identity used by the backup client.
func ExportBackupCredential(auth []byte) ([]byte, error) {
	var users []struct {
		Username string   `json:"username"`
		Password string   `json:"password"`
		Perms    []string `json:"perms"`
	}
	if json.Unmarshal(auth, &users) != nil {
		return nil, errors.New("invalid rqlite authentication input")
	}
	var selected string
	for _, user := range users {
		if user.Username != "agent-vm-backup" {
			continue
		}
		if selected != "" || !validBackupPassword(user.Password) {
			return nil, errors.New("invalid or duplicate rqlite backup credential")
		}
		perms := make(map[string]bool)
		for _, perm := range user.Perms {
			switch perm {
			case "backup", "query", "ready", "status":
				perms[perm] = true
			default:
				return nil, errors.New("rqlite backup identity exceeds permitted access")
			}
		}
		if !perms["backup"] || !perms["query"] {
			return nil, errors.New("rqlite backup identity needs backup and query permissions")
		}
		selected = user.Password
	}
	if selected == "" {
		return nil, errors.New("rqlite backup credential is missing")
	}
	for _, user := range users {
		if user.Username != "agent-vm-backup" && user.Password == selected {
			return nil, errors.New("rqlite backup password must not be shared with another identity")
		}
	}
	return json.Marshal(backupCredential{Password: selected})
}

func readRecoveryConfig(path string, secret bool) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("cannot open protected rqlite recovery configuration")
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 ||
		(secret && info.Mode().Perm()&0o037 != 0) || info.Size() > 1<<20 {
		return nil, errors.New("rqlite recovery configuration is not a protected regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 && stat.Uid != uint32(os.Geteuid()) {
		return nil, errors.New("rqlite recovery configuration has an untrusted owner")
	}
	data, err := io.ReadAll(io.LimitReader(file, 1<<20+1))
	if err == nil && len(data) > 1<<20 {
		return nil, errors.New("rqlite recovery configuration is too large")
	}
	return data, err
}

const recoveryMarkerQuery = "SELECT value FROM meta WHERE key='deployment_recovery_marker'"

type recoveryQueryResult struct {
	Values [][]string `json:"values"`
	Error  string     `json:"error"`
}

func recoverySQL(ctx context.Context, client *http.Client, endpoint, path, user, pass string, statements any) (recoveryQueryResult, error) {
	body, err := json.Marshal(statements)
	if err != nil {
		return recoveryQueryResult{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, strings.NewReader(string(body)))
	if err != nil {
		return recoveryQueryResult{}, err
	}
	request.SetBasicAuth(user, pass)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return recoveryQueryResult{}, fmt.Errorf("rqlite recovery SQL: %w", err)
	}
	defer response.Body.Close()
	var result struct {
		Results []recoveryQueryResult `json:"results"`
		Error   string                `json:"error"`
	}
	if response.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&result) != nil ||
		result.Error != "" || len(result.Results) != 1 || result.Results[0].Error != "" {
		return recoveryQueryResult{}, errors.New("rqlite recovery SQL did not complete")
	}
	return result.Results[0], nil
}

func downloadRecoverySnapshot(ctx context.Context, client *http.Client, endpoint, password, path string, limit int64) (int64, string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/db/backup", nil)
	if err != nil {
		return 0, "", err
	}
	request.SetBasicAuth("agent-vm-backup", password)
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, "", fmt.Errorf("rqlite backup returned HTTP %d", response.StatusCode)
	}
	if response.ContentLength > limit {
		return 0, "", errBackupStorageExhausted
	}
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, "", err
	}
	defer out.Close()
	sum := sha256.New()
	size, err := io.Copy(io.MultiWriter(out, sum), io.LimitReader(response.Body, limit))
	if err != nil {
		if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
			return 0, "", errBackupStorageExhausted
		}
		return 0, "", err
	}
	var extra [1]byte
	n, err := io.ReadFull(response.Body, extra[:])
	if n > 0 {
		return 0, "", errBackupStorageExhausted
	}
	if err != io.EOF || size == 0 || response.Header.Get("X-Stream-Error") != "" || response.Trailer.Get("X-Stream-Error") != "" {
		return 0, "", errors.New("rqlite backup stream did not complete")
	}
	if err := out.Sync(); err != nil {
		return 0, "", err
	}
	if err := out.Close(); err != nil {
		return 0, "", err
	}
	return size, hex.EncodeToString(sum.Sum(nil)), nil
}

func verifyRecoverySnapshot(ctx context.Context, path, boundary string) error {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	var integrity, marker string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
		return errors.New("rqlite backup failed SQLite integrity check")
	}
	if err := db.QueryRowContext(ctx, recoveryMarkerQuery).Scan(&marker); err != nil || marker != boundary {
		return errors.New("rqlite backup does not cover the verified lower boundary")
	}
	return nil
}
