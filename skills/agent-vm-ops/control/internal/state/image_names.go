package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// ImageSelectionError is a deterministic refusal, never a database failure.
type ImageSelectionError struct{ Code, Message string }

func (e *ImageSelectionError) Error() string { return e.Code + ": " + e.Message }
func imageError(code, message string) error  { return &ImageSelectionError{code, message} }

func NormalizeImageName(name string) (string, error) {
	name = norm.NFC.String(strings.TrimSpace(name))
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) < 1 || utf8.RuneCountInString(name) > 128 || imageFingerprintPattern.MatchString(strings.ToLower(name)) {
		return "", imageError("image_name_invalid", "name must have 1–128 characters and cannot be a fingerprint")
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return "", imageError("image_name_invalid", "name contains unsupported characters")
		}
	}
	return name, nil
}

func imageNameBase(label, fingerprint string) string {
	label = norm.NFC.String(strings.TrimSpace(label))
	label = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return '-'
		}
		return r
	}, label)
	if label == "" || imageFingerprintPattern.MatchString(strings.ToLower(label)) {
		label = "image-" + fingerprint
	}
	runes := []rune(label)
	if len(runes) > 128 {
		label = strings.TrimSpace(string(runes[:128]))
	}
	return label
}

func uniqueImageName(base string, used map[string]bool) string {
	if !used[base] {
		return base
	}
	for n := 2; ; n++ {
		suffix := "-" + strconv.Itoa(n)
		runes := []rune(base)
		if len(runes) > 128-len(suffix) {
			runes = runes[:128-len(suffix)]
		}
		value := string(runes) + suffix
		if !used[value] {
			return value
		}
	}
}

func (s *Store) EnsureManagementSchema(ctx context.Context) error {
	if err := s.ensureManagementBaseSchema(ctx); err != nil {
		return err
	}
	return s.withWrite(ctx, func(tx *Tx) error {
		var marker string
		err := tx.conn.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'image_names_migrated'").Scan(&marker)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		rows, err := tx.conn.QueryContext(ctx, "SELECT fingerprint, COALESCE(version_label, '') FROM managed_images ORDER BY created_at, fingerprint")
		if err != nil {
			return err
		}
		type entry struct{ fp, base string }
		entries := []entry{}
		reserved := map[string]bool{}
		for rows.Next() {
			var fp, label string
			if err := rows.Scan(&fp, &label); err != nil {
				rows.Close()
				return err
			}
			base := imageNameBase(label, fp)
			entries = append(entries, entry{fp, base})
			reserved[base] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		claimed := map[string]bool{}
		for _, item := range entries {
			name := item.base
			if claimed[name] {
				name = uniqueImageName(name, reserved)
			}
			reserved[name] = true
			claimed[item.base] = true
			if _, err := tx.exec("INSERT INTO image_names(name, fingerprint) VALUES(?, ?)", name, item.fp); err != nil {
				return err
			}
		}
		_, err = tx.exec("INSERT INTO meta(key,value) VALUES('image_names_migrated','1')")
		if len(entries) == 0 {
			tx.changed = false
		}
		return err
	})
}

func (s *Store) ImageName(ctx context.Context, fingerprint string) (*string, error) {
	var value *string
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		var name string
		err := conn.QueryRowContext(ctx, "SELECT name FROM image_names WHERE fingerprint = ?", fingerprint).Scan(&name)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			value = &name
		}
		return err
	})
	return value, err
}

func allocateImageName(ctx context.Context, tx *Tx, fingerprint, label string) error {
	rows, err := tx.conn.QueryContext(ctx, "SELECT name FROM image_names")
	if err != nil {
		return err
	}
	used := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		used[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	_, err = tx.exec("INSERT INTO image_names(name,fingerprint) VALUES(?,?)", uniqueImageName(imageNameBase(label, fingerprint), used), fingerprint)
	return err
}

// SetImageName is one durable management operation, including replay and audit.
func (s *Store) SetImageName(ctx context.Context, fingerprint string, name *string, expected, operationID, actor, reason string) (map[string]any, error) {
	if name != nil {
		normalized, err := NormalizeImageName(*name)
		if err != nil {
			return nil, err
		}
		name = &normalized
	}
	intent, _ := json.Marshal(map[string]any{"fingerprint": fingerprint, "name": name, "target_version": expected, "actor": actor, "reason": reason})
	var result map[string]any
	err := s.withWrite(ctx, func(tx *Tx) error {
		prior, err := scanManagementOperation(ctx, tx.conn, operationID)
		if err == nil {
			raw, _ := json.Marshal(prior.Request)
			if prior.Action != "image_name" || string(raw) != string(intent) {
				return imageError("operation_conflict", "operation ID was used with another intent")
			}
			result = prior.Result
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var revision string
		if err = tx.conn.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='management_catalog_revision'").Scan(&revision); err != nil {
			return err
		}
		if expected != "image-catalog:revision-"+revision {
			return imageError("stale_target", "image catalog changed")
		}
		var target string
		if err = tx.conn.QueryRowContext(ctx, "SELECT fingerprint FROM managed_images WHERE fingerprint=?", fingerprint).Scan(&target); errors.Is(err, sql.ErrNoRows) {
			return imageError("image_not_found", "target image does not exist")
		}
		if err != nil {
			return err
		}
		var current string
		err = tx.conn.QueryRowContext(ctx, "SELECT name FROM image_names WHERE fingerprint=?", fingerprint).Scan(&current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var previous *string
		if current != "" {
			value := fingerprint
			previous = &value
		}
		if name != nil {
			if current != "" && current != *name {
				return imageError("image_name_conflict", "clear the target image name before rebinding")
			}
			var old string
			err = tx.conn.QueryRowContext(ctx, "SELECT fingerprint FROM image_names WHERE name=?", *name).Scan(&old)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if old != "" {
				previous = &old
			}
			if _, err = tx.exec("DELETE FROM image_names WHERE name=?", *name); err != nil {
				return err
			}
			if _, err = tx.exec("INSERT INTO image_names(name,fingerprint) VALUES(?,?)", *name, fingerprint); err != nil {
				return err
			}
		} else {
			if _, err = tx.exec("DELETE FROM image_names WHERE fingerprint=?", fingerprint); err != nil {
				return err
			}
		}
		if err = bumpImageCatalogRevision(tx); err != nil {
			return err
		}
		result = map[string]any{"fingerprint": fingerprint, "name": name, "previous_fingerprint": previous, "previous_name": current, "operation_id": operationID}
		raw, _ := json.Marshal(result)
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if _, err = tx.exec("INSERT INTO management_operations(operation_id,action,target_kind,target_id,actor,reason,phase,status,request_json,result_json,created_at,updated_at) VALUES(?,'image_name','image',?,?,?,'completed','succeeded',?,?,?,?)", operationID, fingerprint, actor, reason, string(intent), string(raw), now, now); err != nil {
			return err
		}
		// Keep actor/reason and the before/after binding in the existing durable audit stream.
		return tx.appendImageNameAudit(operationID, actor, reason, result)
	})
	if err == nil {
		s.auditSummaryMu.Lock()
		s.auditSummaryReady = false
		s.auditSummaryMu.Unlock()
	}
	return result, err
}

func (tx *Tx) appendImageNameAudit(operationID, actor, reason string, result map[string]any) error {
	payload, _ := json.Marshal(map[string]any{"action": "image_name", "operation_id": operationID, "actor": actor, "reason": reason, "result": result})
	_, err := tx.exec("INSERT INTO management_audit(event_id,operation_id,action,actor,reason,target_json,outcome,phase,detail_json,occurred_at) VALUES(?,?,'image_name',?,?,?,'success','completed',?,?)", "image-name:"+operationID, operationID, actor, reason, string(payload), string(payload), time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

// AcquireImageSelection is consumed inside the same transaction as journal claim.
type AcquireImageSelection struct {
	DefaultFingerprint string
	AllowLegacy        bool
}

func pinAcquireImage(ctx context.Context, tx *Tx, sessionID *string, response map[string]any, selection AcquireImageSelection) error {
	request, _ := response["_request"].(map[string]any)
	selector, explicit := request["image"].(string)
	var existing string
	if sessionID != nil {
		err := tx.conn.QueryRowContext(ctx, "SELECT image_fingerprint FROM tasks WHERE session_id=?", *sessionID).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	fp := selection.DefaultFingerprint
	if existing != "" && !explicit {
		fp = existing
	} else {
		if explicit {
			if imageFingerprintPattern.MatchString(selector) {
				fp = selector
			} else {
				normalized, err := NormalizeImageName(selector)
				if err != nil {
					return err
				}
				err = tx.conn.QueryRowContext(ctx, "SELECT fingerprint FROM image_names WHERE name=?", normalized).Scan(&fp)
				if errors.Is(err, sql.ErrNoRows) {
					return imageError("image_not_found", "image name does not exist")
				}
				if err != nil {
					return err
				}
			}
		} else {
			var managed string
			err := tx.conn.QueryRowContext(ctx, "SELECT fingerprint FROM managed_images WHERE is_default=1").Scan(&managed)
			if err == nil {
				fp = managed
			} else if !errors.Is(err, sql.ErrNoRows) && !(selection.AllowLegacy && strings.Contains(err.Error(), "no such table")) {
				return err
			}
		}
		if explicit && existing != "" {
			if fp != existing {
				return imageError("image_conflict", "session is pinned to another image; destroy explicitly before replacing")
			}
			response["_image_fingerprint"] = existing
			return nil
		}
		var status, integrity, suitability, compat, source, instanceType string
		err := tx.conn.QueryRowContext(ctx, "SELECT state,integrity,suitability,default_compatibility,source_kind,instance_type FROM managed_images WHERE fingerprint=?", fp).Scan(&status, &integrity, &suitability, &compat, &source, &instanceType)
		if err != nil {
			if !(selection.AllowLegacy && !explicit && (errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "no such table"))) {
				if errors.Is(err, sql.ErrNoRows) {
					return imageError("image_not_found", "image fingerprint is not registered")
				}
				return err
			}
		} else if instanceType != "container" {
			return imageError("image_unavailable", "only system container images can be selected")
		} else if status != "available" || integrity != "passed" || suitability != "passed" {
			if !(selection.AllowLegacy && !explicit && status == "available" && integrity == "legacy_unverified" && suitability == "not_assessed" && compat == "legacy_unverified" && source == "legacy_config") {
				return imageError("image_unavailable", "image is retired or not verified for creation")
			}
		}
	}
	if explicit && existing != "" && fp != existing {
		return imageError("image_conflict", "session is pinned to another image; destroy explicitly before replacing")
	}
	response["_image_fingerprint"] = fp
	return nil
}

// CreatableImages exposes only the public selection catalog, never provenance.
func (s *Store) CreatableImages(ctx context.Context) ([]map[string]any, error) {
	result := []map[string]any{}
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx, `SELECT n.name,i.fingerprint,i.architecture,i.is_default FROM managed_images i LEFT JOIN image_names n ON n.fingerprint=i.fingerprint WHERE i.state='available' AND i.integrity='passed' AND i.suitability='passed' AND i.instance_type='container' ORDER BY i.fingerprint`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name sql.NullString
			var fp, arch string
			var def int
			if err := rows.Scan(&name, &fp, &arch, &def); err != nil {
				return err
			}
			result = append(result, map[string]any{"name": nullableString(name), "fingerprint": fp, "architecture": arch, "is_default": def == 1})
		}
		return rows.Err()
	})
	return result, err
}

func (s *Store) ImageFingerprintForName(ctx context.Context, name string) (*string, error) {
	var result *string
	err := s.withRead(ctx, func(conn *sql.Conn) error {
		var fp string
		err := conn.QueryRowContext(ctx, "SELECT fingerprint FROM image_names WHERE name=?", name).Scan(&fp)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err == nil {
			result = &fp
		}
		return err
	})
	return result, err
}
