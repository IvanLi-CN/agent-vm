package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const managementSchema = `
CREATE TABLE IF NOT EXISTS state_capabilities (
  name TEXT NOT NULL,
  version INTEGER NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (name, version)
);
CREATE TABLE IF NOT EXISTS managed_images (
  fingerprint TEXT PRIMARY KEY,
  version_label TEXT,
  architecture TEXT NOT NULL,
  instance_type TEXT NOT NULL,
  source_kind TEXT NOT NULL,
  source_id TEXT,
  state TEXT NOT NULL,
  integrity TEXT NOT NULL,
  suitability TEXT NOT NULL,
  findings_json TEXT NOT NULL DEFAULT '[]',
  is_default INTEGER NOT NULL DEFAULT 0,
  default_compatibility TEXT NOT NULL DEFAULT 'none',
  revision INTEGER NOT NULL DEFAULT 1,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS managed_images_one_default ON managed_images(is_default) WHERE is_default = 1;
CREATE TABLE IF NOT EXISTS managed_image_tombstones (
  fingerprint TEXT PRIMARY KEY,
  deleted_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS image_references (
  fingerprint TEXT NOT NULL,
  reference_kind TEXT NOT NULL,
  reference_id TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (fingerprint, reference_kind, reference_id),
  FOREIGN KEY (fingerprint) REFERENCES managed_images(fingerprint) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS management_operations (
  operation_id TEXT PRIMARY KEY,
  action TEXT NOT NULL,
  target_kind TEXT NOT NULL,
  target_id TEXT NOT NULL,
  actor TEXT NOT NULL,
  reason TEXT NOT NULL,
  phase TEXT NOT NULL,
  status TEXT NOT NULL,
  request_json TEXT NOT NULL,
  result_json TEXT,
  error_json TEXT,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS management_audit (
  event_id TEXT PRIMARY KEY,
  operation_id TEXT,
  action TEXT NOT NULL,
  actor TEXT NOT NULL,
  reason TEXT NOT NULL,
  target_json TEXT NOT NULL,
  outcome TEXT NOT NULL,
  phase TEXT NOT NULL,
  detail_json TEXT,
  occurred_at TEXT NOT NULL
);`

var imageFingerprintPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type ImageReferenceCounts struct {
	Default         int `json:"default"`
	Active          int `json:"active"`
	Cached          int `json:"cached"`
	PendingCreation int `json:"pending_creation"`
	Verification    int `json:"verification"`
}

func (c ImageReferenceCounts) Total() int {
	return c.Default + c.Active + c.Cached + c.PendingCreation + c.Verification
}

type ManagedImage struct {
	Fingerprint          string
	VersionLabel         *string
	Architecture         string
	InstanceType         string
	SourceKind           string
	SourceID             *string
	State                string
	Integrity            string
	Suitability          string
	Findings             []string
	IsDefault            bool
	DefaultCompatibility string
	Revision             int64
	CreatedAt            string
	UpdatedAt            string
	References           ImageReferenceCounts
}

type ImageFilter struct {
	State        string
	Architecture string
	Query        string
}

type ManagementCapability struct {
	Name    string `json:"name"`
	Version int64  `json:"version"`
}

type ManagementOperation struct {
	OperationID string
	Action      string
	TargetKind  string
	TargetID    string
	Actor       string
	Reason      string
	Phase       string
	Status      string
	Request     map[string]any
	Result      map[string]any
	Error       map[string]any
	CreatedAt   string
	UpdatedAt   string
}

type ManagementAudit struct {
	EventID     string
	OperationID *string
	Action      string
	Actor       string
	Reason      string
	Target      map[string]any
	Outcome     string
	Phase       string
	Detail      map[string]any
	OccurredAt  string
}

type ImageReferenceError struct {
	Counts  ImageReferenceCounts
	Default bool
}

func (err *ImageReferenceError) Error() string {
	if err.Default {
		return "default_image_required"
	}
	return "image_referenced"
}

func (s *Store) EnsureManagementSchema(ctx context.Context) error {
	return s.withWrite(ctx, func(tx *Tx) error {
		if _, err := tx.exec(managementSchema); err != nil {
			return fmt.Errorf("create management schema: %w", err)
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
		} {
			if _, err := tx.exec("INSERT OR IGNORE INTO state_capabilities(name, version, created_at) VALUES(?, ?, ?)", marker.name, marker.version, now); err != nil {
				return fmt.Errorf("record state capability: %w", err)
			}
		}
		if _, err := tx.exec("INSERT OR IGNORE INTO meta(key, value) VALUES('management_selection_revision', '1')"); err != nil {
			return fmt.Errorf("initialize management selection revision: %w", err)
		}
		if _, err := tx.exec("INSERT OR IGNORE INTO meta(key, value) VALUES('management_catalog_revision', '1')"); err != nil {
			return fmt.Errorf("initialize management catalog revision: %w", err)
		}
		// Schema and capability bootstrap are compatibility metadata, not a
		// business mutation. Preserve the existing observation epoch.
		tx.changed = false
		return nil
	})
}

func (s *Store) SeedLegacyImage(ctx context.Context, fingerprint string) error {
	if !imageFingerprintPattern.MatchString(fingerprint) {
		return errors.New("invalid legacy image fingerprint")
	}
	if err := s.EnsureManagementSchema(ctx); err != nil {
		return err
	}
	unlock := s.LockImageSelection()
	defer unlock()
	return s.withWrite(ctx, func(tx *Tx) error {
		fingerprints := []string{fingerprint}
		rows, err := tx.conn.QueryContext(ctx, "SELECT DISTINCT image_fingerprint FROM tasks")
		if err != nil {
			return err
		}
		for rows.Next() {
			var candidate string
			if err := rows.Scan(&candidate); err != nil {
				rows.Close()
				return err
			}
			if imageFingerprintPattern.MatchString(candidate) && candidate != fingerprint {
				fingerprints = append(fingerprints, candidate)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, candidate := range fingerprints {
			var tombstone int
			err := tx.conn.QueryRowContext(ctx, "SELECT 1 FROM managed_image_tombstones WHERE fingerprint = ?", candidate).Scan(&tombstone)
			if err == nil {
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			var exists int
			err = tx.conn.QueryRowContext(ctx, "SELECT 1 FROM managed_images WHERE fingerprint = ?", candidate).Scan(&exists)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if errors.Is(err, sql.ErrNoRows) {
				now := time.Now().UTC().Format(time.RFC3339Nano)
				label := "legacy-history"
				if candidate == fingerprint {
					label = "legacy-default"
				}
				if _, err := tx.exec(`INSERT INTO managed_images(fingerprint, version_label, architecture, instance_type, source_kind, source_id, state, integrity, suitability, findings_json, is_default, default_compatibility, revision, created_at, updated_at)
VALUES(?, ?, ?, 'container', 'legacy_config', NULL, 'available', 'legacy_unverified', 'not_assessed', '[]', 0, 'legacy_unverified', 1, ?, ?)`, candidate, label, "x86_64", now, now); err != nil {
					return err
				}
			}
		}
		var current string
		err = tx.conn.QueryRowContext(ctx, "SELECT fingerprint FROM managed_images WHERE is_default = 1").Scan(&current)
		if errors.Is(err, sql.ErrNoRows) {
			_, err = tx.exec("UPDATE managed_images SET is_default = 1, default_compatibility = 'legacy_unverified', revision = revision + 1, updated_at = ? WHERE fingerprint = ?", time.Now().UTC().Format(time.RFC3339Nano), fingerprint)
			return err
		}
		return err
	})
}

func (s *Store) CurrentDefaultImage(ctx context.Context) (string, bool, error) {
	var fingerprint string
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		err := conn.QueryRowContext(ctx, "SELECT fingerprint FROM managed_images WHERE is_default = 1").Scan(&fingerprint)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return fingerprint, fingerprint != "" && err == nil, err
}

func (s *Store) ImageSelectionVersion(ctx context.Context) (string, error) {
	var raw string
	if err := s.withRead(ctx, func(conn *sql.Conn) error {
		err := conn.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'management_selection_revision'").Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			raw = "1"
			return nil
		}
		return err
	}); err != nil {
		return "", err
	}
	if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
		return "", fmt.Errorf("invalid management selection revision: %w", err)
	}
	return "image-selection:revision-" + raw, nil
}

func (s *Store) ImageCatalogVersion(ctx context.Context) (string, error) {
	var raw string
	if err := s.withRead(ctx, func(conn *sql.Conn) error {
		err := conn.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'management_catalog_revision'").Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			raw = "1"
			return nil
		}
		return err
	}); err != nil {
		return "", err
	}
	if _, err := strconv.ParseInt(raw, 10, 64); err != nil {
		return "", fmt.Errorf("invalid management catalog revision: %w", err)
	}
	return "image-catalog:revision-" + raw, nil
}

func (s *Store) ListManagedImages(ctx context.Context, filter ImageFilter) ([]ManagedImage, error) {
	var values []ManagedImage
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		query := `SELECT i.fingerprint, i.version_label, i.architecture, i.instance_type, i.source_kind, i.source_id,
i.state, i.integrity, i.suitability, i.findings_json, i.is_default, i.default_compatibility, i.revision, i.created_at, i.updated_at,
(SELECT COUNT(*) FROM tasks t WHERE t.image_fingerprint = i.fingerprint AND t.state = 'active'),
(SELECT COUNT(*) FROM tasks t WHERE t.image_fingerprint = i.fingerprint AND t.state = 'cached'),
(SELECT COUNT(*) FROM (
 SELECT t.task_id AS reference_id FROM tasks t WHERE t.image_fingerprint = i.fingerprint AND t.state = 'preparing'
 UNION
 SELECT r.reference_id FROM image_references r WHERE r.fingerprint = i.fingerprint AND r.reference_kind = 'pending_creation'
)),
(SELECT COUNT(*) FROM image_references r WHERE r.fingerprint = i.fingerprint AND r.reference_kind = 'verification')
FROM managed_images i`
		conditions := []string{}
		args := []any{}
		if filter.State != "" {
			conditions = append(conditions, "i.state = ?")
			args = append(args, filter.State)
		}
		if filter.Architecture != "" {
			conditions = append(conditions, "i.architecture = ?")
			args = append(args, filter.Architecture)
		}
		if filter.Query != "" {
			conditions = append(conditions, "(i.fingerprint LIKE ? ESCAPE '\\' OR COALESCE(i.version_label, '') LIKE ? ESCAPE '\\')")
			literal := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(filter.Query)
			like := "%" + literal + "%"
			args = append(args, like, like)
		}
		if len(conditions) > 0 {
			query += " WHERE " + strings.Join(conditions, " AND ")
		}
		query += " ORDER BY i.created_at DESC, i.fingerprint"
		rows, err := conn.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			value, err := scanManagedImage(rows)
			if err != nil {
				return err
			}
			values = append(values, *value)
		}
		return rows.Err()
	})
	return values, err
}

func (s *Store) GetManagedImage(ctx context.Context, fingerprint string) (*ManagedImage, error) {
	var value *ManagedImage
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		row := conn.QueryRowContext(ctx, `SELECT i.fingerprint, i.version_label, i.architecture, i.instance_type, i.source_kind, i.source_id,
i.state, i.integrity, i.suitability, i.findings_json, i.is_default, i.default_compatibility, i.revision, i.created_at, i.updated_at,
(SELECT COUNT(*) FROM tasks t WHERE t.image_fingerprint = i.fingerprint AND t.state = 'active'),
(SELECT COUNT(*) FROM tasks t WHERE t.image_fingerprint = i.fingerprint AND t.state = 'cached'),
(SELECT COUNT(*) FROM (
 SELECT t.task_id AS reference_id FROM tasks t WHERE t.image_fingerprint = i.fingerprint AND t.state = 'preparing'
 UNION
 SELECT r.reference_id FROM image_references r WHERE r.fingerprint = i.fingerprint AND r.reference_kind = 'pending_creation'
)),
(SELECT COUNT(*) FROM image_references r WHERE r.fingerprint = i.fingerprint AND r.reference_kind = 'verification')
FROM managed_images i WHERE i.fingerprint = ?`, fingerprint)
		image, err := scanManagedImage(row)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			value = image
		}
		return err
	})
	return value, err
}

func scanManagedImage(row scanner) (*ManagedImage, error) {
	var value ManagedImage
	var label, sourceID sql.NullString
	var findings string
	var isDefault int
	var active, cached, pending, verification int
	if err := row.Scan(&value.Fingerprint, &label, &value.Architecture, &value.InstanceType, &value.SourceKind, &sourceID,
		&value.State, &value.Integrity, &value.Suitability, &findings, &isDefault, &value.DefaultCompatibility, &value.Revision, &value.CreatedAt, &value.UpdatedAt,
		&active, &cached, &pending, &verification); err != nil {
		return nil, err
	}
	value.VersionLabel = nullableString(label)
	value.SourceID = nullableString(sourceID)
	value.IsDefault = isDefault != 0
	if err := json.Unmarshal([]byte(findings), &value.Findings); err != nil {
		return nil, fmt.Errorf("managed image findings: %w", err)
	}
	value.References = ImageReferenceCounts{Default: isDefault, Active: active, Cached: cached, PendingCreation: pending, Verification: verification}
	return &value, nil
}

func (s *Store) InsertManagedImage(ctx context.Context, value ManagedImage) error {
	if !imageFingerprintPattern.MatchString(value.Fingerprint) {
		return errors.New("invalid image fingerprint")
	}
	findingsValue := value.Findings
	if findingsValue == nil {
		findingsValue = []string{}
	}
	findings, err := json.Marshal(findingsValue)
	if err != nil {
		return err
	}
	now := value.CreatedAt
	if now == "" {
		now = time.Now().UTC().Format(time.RFC3339Nano)
	}
	updated := value.UpdatedAt
	if updated == "" {
		updated = now
	}
	if value.Revision <= 0 {
		value.Revision = 1
	}
	return s.withWrite(ctx, func(tx *Tx) error {
		_, err := tx.exec(`INSERT INTO managed_images(fingerprint, version_label, architecture, instance_type, source_kind, source_id, state, integrity, suitability, findings_json, is_default, default_compatibility, revision, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, value.Fingerprint, nullableStringArg(value.VersionLabel), value.Architecture, value.InstanceType, value.SourceKind, nullableStringArg(value.SourceID), value.State, value.Integrity, value.Suitability, string(findings), boolInt(value.IsDefault), value.DefaultCompatibility, value.Revision, now, updated)
		if err != nil {
			return err
		}
		return bumpImageCatalogRevision(tx)
	})
}

func (s *Store) UpdateManagedImageVerification(ctx context.Context, fingerprint, integrity, suitability string, findings []string, nextState string) error {
	findingsValue := findings
	if findingsValue == nil {
		findingsValue = []string{}
	}
	raw, err := json.Marshal(findingsValue)
	if err != nil {
		return err
	}
	return s.withWrite(ctx, func(tx *Tx) error {
		result, err := tx.exec("UPDATE managed_images SET integrity = ?, suitability = ?, findings_json = ?, state = ?, revision = revision + 1, updated_at = ? WHERE fingerprint = ?", integrity, suitability, string(raw), nextState, time.Now().UTC().Format(time.RFC3339Nano), fingerprint)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows > 0 {
			return bumpImageCatalogRevision(tx)
		}
		return nil
	})
}

func (s *Store) SelectManagedImage(ctx context.Context, fingerprint, expectedVersion string) (string, error) {
	if err := s.EnsureManagementSchema(ctx); err != nil {
		return "", err
	}
	unlock := s.LockImageSelection()
	defer unlock()
	var version string
	err := s.withWrite(ctx, func(tx *Tx) error {
		var currentRevision string
		if err := tx.conn.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'management_selection_revision'").Scan(&currentRevision); err != nil {
			return err
		}
		currentNumber, err := strconv.ParseInt(currentRevision, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid management selection revision: %w", err)
		}
		currentVersion := "image-selection:revision-" + currentRevision
		if expectedVersion == "" {
			return errors.New("invalid_request: selection target_version is required")
		}
		if expectedVersion != currentVersion {
			return fmt.Errorf("stale_target: expected %s, current %s", expectedVersion, currentVersion)
		}
		var stateValue, integrity, suitability string
		if err := tx.conn.QueryRowContext(ctx, "SELECT state, integrity, suitability FROM managed_images WHERE fingerprint = ?", fingerprint).Scan(&stateValue, &integrity, &suitability); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("not_found: managed image does not exist")
			}
			return err
		}
		if stateValue != "available" || integrity != "passed" || suitability != "passed" {
			return errors.New("image_not_applicable: image is not fully verified")
		}
		if _, err := tx.exec("UPDATE managed_images SET is_default = 0, revision = revision + 1, updated_at = ? WHERE is_default = 1", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
		newNumber := currentNumber + 1
		if _, err := tx.exec("UPDATE managed_images SET is_default = 1, default_compatibility = 'none', revision = revision + 1, updated_at = ? WHERE fingerprint = ?", time.Now().UTC().Format(time.RFC3339Nano), fingerprint); err != nil {
			return err
		}
		if _, err := tx.exec("UPDATE meta SET value = ? WHERE key = 'management_selection_revision'", strconv.FormatInt(newNumber, 10)); err != nil {
			return err
		}
		version = "image-selection:revision-" + strconv.FormatInt(newNumber, 10)
		return nil
	})
	return version, err
}

func (s *Store) RetireManagedImage(ctx context.Context, fingerprint string) error {
	return s.withWrite(ctx, func(tx *Tx) error {
		var isDefault int
		if err := tx.conn.QueryRowContext(ctx, "SELECT is_default FROM managed_images WHERE fingerprint = ?", fingerprint).Scan(&isDefault); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("not_found: managed image does not exist")
			}
			return err
		}
		if isDefault != 0 {
			return errors.New("default_image_required: select another default before retiring this image")
		}
		result, err := tx.exec("UPDATE managed_images SET state = 'retired', revision = revision + 1, updated_at = ? WHERE fingerprint = ? AND state != 'retired'", time.Now().UTC().Format(time.RFC3339Nano), fingerprint)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows > 0 {
			return bumpImageCatalogRevision(tx)
		}
		return nil
	})
}

func (s *Store) DeleteManagedImage(ctx context.Context, fingerprint string) error {
	return s.withWrite(ctx, func(tx *Tx) error {
		var isDefault int
		if err := tx.conn.QueryRowContext(ctx, "SELECT is_default FROM managed_images WHERE fingerprint = ?", fingerprint).Scan(&isDefault); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errors.New("not_found: managed image does not exist")
			}
			return err
		}
		if isDefault != 0 {
			return &ImageReferenceError{Default: true, Counts: ImageReferenceCounts{Default: 1}}
		}
		var counts ImageReferenceCounts
		if err := tx.conn.QueryRowContext(ctx, `SELECT
(SELECT COUNT(*) FROM tasks WHERE image_fingerprint = ? AND state = 'active'),
(SELECT COUNT(*) FROM tasks WHERE image_fingerprint = ? AND state = 'cached'),
(SELECT COUNT(*) FROM (
 SELECT task_id AS reference_id FROM tasks WHERE image_fingerprint = ? AND state = 'preparing'
 UNION
 SELECT reference_id FROM image_references WHERE fingerprint = ? AND reference_kind = 'pending_creation'
)),
			(SELECT COUNT(*) FROM image_references WHERE fingerprint = ? AND reference_kind = 'verification')`, fingerprint, fingerprint, fingerprint, fingerprint, fingerprint).Scan(&counts.Active, &counts.Cached, &counts.PendingCreation, &counts.Verification); err != nil {
			return err
		}
		if counts.Total() > 0 {
			return &ImageReferenceError{Counts: counts}
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.exec("INSERT OR IGNORE INTO managed_image_tombstones(fingerprint, deleted_at) VALUES(?, ?)", fingerprint, now); err != nil {
			return err
		}
		result, err := tx.exec("DELETE FROM managed_images WHERE fingerprint = ?", fingerprint)
		if err != nil {
			return err
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows > 0 {
			return bumpImageCatalogRevision(tx)
		}
		return nil
	})
}

func bumpImageCatalogRevision(tx *Tx) error {
	var raw string
	if err := tx.conn.QueryRowContext(context.Background(), "SELECT value FROM meta WHERE key = 'management_catalog_revision'").Scan(&raw); err != nil {
		return err
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid management catalog revision: %w", err)
	}
	_, err = tx.exec("UPDATE meta SET value = ? WHERE key = 'management_catalog_revision'", strconv.FormatInt(value+1, 10))
	return err
}

func (s *Store) AddImageReference(ctx context.Context, fingerprint, kind, referenceID string) error {
	if fingerprint == "" || referenceID == "" {
		return errors.New("image reference requires fingerprint and id")
	}
	if kind != "pending_creation" && kind != "verification" {
		return errors.New("unsupported image reference kind")
	}
	unlock := s.LockImageSelection()
	defer unlock()
	return s.withWrite(ctx, func(tx *Tx) error {
		_, err := tx.exec("INSERT OR IGNORE INTO image_references(fingerprint, reference_kind, reference_id, created_at) VALUES(?, ?, ?, ?)", fingerprint, kind, referenceID, time.Now().UTC().Format(time.RFC3339Nano))
		return err
	})
}

func (s *Store) RemoveImageReference(ctx context.Context, fingerprint, kind, referenceID string) error {
	unlock := s.LockImageSelection()
	defer unlock()
	return s.withWrite(ctx, func(tx *Tx) error {
		_, err := tx.exec("DELETE FROM image_references WHERE fingerprint = ? AND reference_kind = ? AND reference_id = ?", fingerprint, kind, referenceID)
		return err
	})
}

func (s *Store) TransitionManagementOperation(ctx context.Context, operationID, expectedStatus, phase, status string) (*ManagementOperation, bool, error) {
	var value *ManagementOperation
	claimed := false
	err := s.withWrite(ctx, func(tx *Tx) error {
		current, err := scanManagementOperation(ctx, tx.conn, operationID)
		if err != nil {
			return err
		}
		value = current
		if current.Status != expectedStatus {
			return nil
		}
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := tx.exec("UPDATE management_operations SET phase = ?, status = ?, updated_at = ? WHERE operation_id = ? AND status = ?", phase, status, now, operationID, expectedStatus); err != nil {
			return err
		}
		value.Phase, value.Status, value.UpdatedAt = phase, status, now
		claimed = true
		return nil
	})
	return value, claimed, err
}

func (s *Store) Capabilities(ctx context.Context) ([]ManagementCapability, error) {
	var values []ManagementCapability
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, "SELECT name, version FROM state_capabilities ORDER BY name, version")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var value ManagementCapability
			if err := rows.Scan(&value.Name, &value.Version); err != nil {
				return err
			}
			values = append(values, value)
		}
		return rows.Err()
	})
	return values, err
}

func (s *Store) ClaimManagementOperation(ctx context.Context, value ManagementOperation) (*ManagementOperation, error) {
	request, err := json.Marshal(value.Request)
	if err != nil {
		return nil, err
	}
	var existing *ManagementOperation
	err = s.withWrite(ctx, func(tx *Tx) error {
		current, scanErr := scanManagementOperation(ctx, tx.conn, value.OperationID)
		if scanErr == nil {
			existing = current
			return nil
		}
		if !errors.Is(scanErr, sql.ErrNoRows) {
			return scanErr
		}
		now := value.CreatedAt
		if now == "" {
			now = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if value.UpdatedAt == "" {
			value.UpdatedAt = now
		}
		_, err := tx.exec(`INSERT INTO management_operations(operation_id, action, target_kind, target_id, actor, reason, phase, status, request_json, result_json, error_json, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?)`, value.OperationID, value.Action, value.TargetKind, value.TargetID, value.Actor, value.Reason, value.Phase, value.Status, string(request), now, value.UpdatedAt)
		return err
	})
	return existing, err
}

func (s *Store) ClaimManagementOperationWithAudit(ctx context.Context, value ManagementOperation, audit ManagementAudit) (*ManagementOperation, error) {
	request, err := json.Marshal(value.Request)
	if err != nil {
		return nil, err
	}
	target, err := json.Marshal(audit.Target)
	if err != nil {
		return nil, err
	}
	detail, err := jsonNullable(audit.Detail)
	if err != nil {
		return nil, err
	}
	var existing *ManagementOperation
	err = s.withWrite(ctx, func(tx *Tx) error {
		current, scanErr := scanManagementOperation(ctx, tx.conn, value.OperationID)
		if scanErr == nil {
			existing = current
			return nil
		}
		if !errors.Is(scanErr, sql.ErrNoRows) {
			return scanErr
		}
		now := value.CreatedAt
		if now == "" {
			now = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if value.UpdatedAt == "" {
			value.UpdatedAt = now
		}
		if _, err := tx.exec(`INSERT INTO management_operations(operation_id, action, target_kind, target_id, actor, reason, phase, status, request_json, result_json, error_json, created_at, updated_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?)`, value.OperationID, value.Action, value.TargetKind, value.TargetID, value.Actor, value.Reason, value.Phase, value.Status, string(request), now, value.UpdatedAt); err != nil {
			return err
		}
		return recordManagementAuditTx(tx, audit, string(target), detail, now)
	})
	return existing, err
}

func (s *Store) GetManagementOperation(ctx context.Context, operationID string) (*ManagementOperation, error) {
	var value *ManagementOperation
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		result, err := scanManagementOperation(ctx, conn, operationID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			value = result
		}
		return err
	})
	return value, err
}

func (s *Store) ListManagementOperations(ctx context.Context, limit int) ([]ManagementOperation, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	values := make([]ManagementOperation, 0, limit)
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, `SELECT operation_id, action, target_kind, target_id, actor, reason, phase, status, request_json, result_json, error_json, created_at, updated_at
FROM management_operations ORDER BY created_at DESC, operation_id DESC LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			value, err := scanManagementOperationRow(rows)
			if err != nil {
				return err
			}
			values = append(values, *value)
		}
		return rows.Err()
	})
	return values, err
}

// ListAllManagementOperations is reserved for resident recovery. Public API
// reads stay bounded; startup recovery must inspect every durable operation.
func (s *Store) ListAllManagementOperations(ctx context.Context) ([]ManagementOperation, error) {
	values := []ManagementOperation{}
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, `SELECT operation_id, action, target_kind, target_id, actor, reason, phase, status, request_json, result_json, error_json, created_at, updated_at
FROM management_operations ORDER BY created_at DESC, operation_id DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			value, err := scanManagementOperationRow(rows)
			if err != nil {
				return err
			}
			values = append(values, *value)
		}
		return rows.Err()
	})
	return values, err
}

func (s *Store) UpdateManagementOperation(ctx context.Context, operationID string, update func(*ManagementOperation) error) error {
	return s.withWrite(ctx, func(tx *Tx) error {
		value, err := scanManagementOperation(ctx, tx.conn, operationID)
		if err != nil {
			return err
		}
		if value.Status == "succeeded" || value.Status == "failed" || value.Status == "outcome_unknown" {
			return errors.New("management operation is already finalized")
		}
		if err := update(value); err != nil {
			return err
		}
		request, err := json.Marshal(value.Request)
		if err != nil {
			return err
		}
		result, err := jsonNullable(value.Result)
		if err != nil {
			return err
		}
		errorValue, err := jsonNullable(value.Error)
		if err != nil {
			return err
		}
		_, err = tx.exec("UPDATE management_operations SET phase = ?, status = ?, request_json = ?, result_json = ?, error_json = ?, updated_at = ? WHERE operation_id = ?", value.Phase, value.Status, string(request), result, errorValue, value.UpdatedAt, operationID)
		return err
	})
}

func (s *Store) FinishManagementOperation(ctx context.Context, operationID, phase, status string, result, failure map[string]any, audit ManagementAudit) (*ManagementOperation, error) {
	target, err := json.Marshal(audit.Target)
	if err != nil {
		return nil, err
	}
	detail, err := jsonNullable(audit.Detail)
	if err != nil {
		return nil, err
	}
	err = s.withWrite(ctx, func(tx *Tx) error {
		value, err := scanManagementOperation(ctx, tx.conn, operationID)
		if err != nil {
			return err
		}
		if value.Status == "succeeded" || value.Status == "failed" || value.Status == "outcome_unknown" {
			return errors.New("management operation is already finalized")
		}
		if value.Status != "running" {
			return errors.New("management operation is not owned for execution")
		}
		resultJSON, err := jsonNullable(result)
		if err != nil {
			return err
		}
		errorJSON, err := jsonNullable(failure)
		if err != nil {
			return err
		}
		now := audit.OccurredAt
		if now == "" {
			now = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if _, err := tx.exec("UPDATE management_operations SET phase = ?, status = ?, result_json = ?, error_json = ?, updated_at = ? WHERE operation_id = ?", phase, status, resultJSON, errorJSON, now, operationID); err != nil {
			return err
		}
		return recordManagementAuditTx(tx, audit, string(target), detail, now)
	})
	if err != nil {
		return nil, err
	}
	return s.GetManagementOperation(ctx, operationID)
}

func scanManagementOperation(ctx context.Context, conn *sql.Conn, operationID string) (*ManagementOperation, error) {
	return scanManagementOperationRow(conn.QueryRowContext(ctx, `SELECT operation_id, action, target_kind, target_id, actor, reason, phase, status, request_json, result_json, error_json, created_at, updated_at FROM management_operations WHERE operation_id = ?`, operationID))
}

func scanManagementOperationRow(row scanner) (*ManagementOperation, error) {
	var value ManagementOperation
	var request, result, errorValue sql.NullString
	err := row.Scan(&value.OperationID, &value.Action, &value.TargetKind, &value.TargetID, &value.Actor, &value.Reason, &value.Phase, &value.Status, &request, &result, &errorValue, &value.CreatedAt, &value.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if err := decodeNullableJSON(request, &value.Request); err != nil {
		return nil, err
	}
	if err := decodeNullableJSON(result, &value.Result); err != nil {
		return nil, err
	}
	if err := decodeNullableJSON(errorValue, &value.Error); err != nil {
		return nil, err
	}
	return &value, nil
}

func (s *Store) RecordManagementAudit(ctx context.Context, value ManagementAudit) error {
	target, err := json.Marshal(value.Target)
	if err != nil {
		return err
	}
	detail, err := jsonNullable(value.Detail)
	if err != nil {
		return err
	}
	now := value.OccurredAt
	if now == "" {
		now = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return s.withWrite(ctx, func(tx *Tx) error {
		return recordManagementAuditTx(tx, value, string(target), detail, now)
	})
}

func recordManagementAuditTx(tx *Tx, value ManagementAudit, target string, detail any, occurredAt string) error {
	_, err := tx.exec(`INSERT OR IGNORE INTO management_audit(event_id, operation_id, action, actor, reason, target_json, outcome, phase, detail_json, occurred_at)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, value.EventID, nullableStringArg(value.OperationID), value.Action, value.Actor, value.Reason, target, value.Outcome, value.Phase, detail, occurredAt)
	return err
}

func (s *Store) ListManagementAudit(ctx context.Context, limit int) ([]ManagementAudit, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	values := make([]ManagementAudit, 0, limit)
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, `SELECT event_id, operation_id, action, actor, reason, target_json, outcome, phase, detail_json, occurred_at
FROM management_audit ORDER BY occurred_at DESC, event_id DESC LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var value ManagementAudit
			var operationID sql.NullString
			var target, detail sql.NullString
			if err := rows.Scan(&value.EventID, &operationID, &value.Action, &value.Actor, &value.Reason, &target, &value.Outcome, &value.Phase, &detail, &value.OccurredAt); err != nil {
				return err
			}
			value.OperationID = nullableString(operationID)
			if !target.Valid {
				return errors.New("management audit target is null")
			}
			if err := json.Unmarshal([]byte(target.String), &value.Target); err != nil {
				return err
			}
			if detail.Valid && detail.String != "" {
				if err := json.Unmarshal([]byte(detail.String), &value.Detail); err != nil {
					return err
				}
			}
			values = append(values, value)
		}
		return rows.Err()
	})
	return values, err
}

// ListAllManagementAudit is used by the API boundary before applying its
// filters and bounded cursor page. It prevents a recent-record LIMIT from
// hiding older records that match an actor, target, or date filter.
func (s *Store) ListAllManagementAudit(ctx context.Context) ([]ManagementAudit, error) {
	values := make([]ManagementAudit, 0)
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, `SELECT event_id, operation_id, action, actor, reason, target_json, outcome, phase, detail_json, occurred_at
FROM management_audit ORDER BY occurred_at DESC, event_id DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var value ManagementAudit
			var operationID sql.NullString
			var target, detail sql.NullString
			if err := rows.Scan(&value.EventID, &operationID, &value.Action, &value.Actor, &value.Reason, &target, &value.Outcome, &value.Phase, &detail, &value.OccurredAt); err != nil {
				return err
			}
			value.OperationID = nullableString(operationID)
			if !target.Valid {
				return errors.New("management audit target is null")
			}
			if err := json.Unmarshal([]byte(target.String), &value.Target); err != nil {
				return err
			}
			if detail.Valid && detail.String != "" {
				if err := json.Unmarshal([]byte(detail.String), &value.Detail); err != nil {
					return err
				}
			}
			values = append(values, value)
		}
		return rows.Err()
	})
	return values, err
}

func jsonNullable(value map[string]any) (any, error) {
	if value == nil {
		return nil, nil
	}
	raw, err := json.Marshal(value)
	return string(raw), err
}

func decodeNullableJSON(value sql.NullString, target *map[string]any) error {
	if !value.Valid || value.String == "" {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(value.String))
	decoder.UseNumber()
	return decoder.Decode(target)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
