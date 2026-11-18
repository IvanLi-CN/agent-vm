package install

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
)

// copyAccountJournal runs while the caller holds the deployment singleton
// lock. The old file stays in place until the host handoff has been accepted.
func copyAccountJournal(ctx context.Context, source, destination string, uid, gid int) error {
	owner := uint32(os.Geteuid())
	for _, suffix := range []string{"", "-wal", "-shm"} {
		path := source + suffix
		info, err := os.Lstat(path)
		if suffix != "" && errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect legacy deployment journal: %w", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != owner || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 {
			return errors.New("legacy deployment journal is not protected")
		}
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Lstat(destination + suffix); !errors.Is(err, os.ErrNotExist) {
			return errors.New("dedicated deployment journal already exists or cannot be inspected")
		}
	}
	stage, err := os.MkdirTemp(filepath.Dir(source), ".account-journal-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	snapshot := filepath.Join(stage, "deployment.sqlite3")

	old, err := openAccountJournal(source)
	if err != nil {
		return err
	}
	defer old.Close()
	if _, err := old.ExecContext(ctx, "VACUUM INTO ?", snapshot); err != nil {
		return fmt.Errorf("snapshot deployment journal: %w", err)
	}
	candidate, err := openAccountJournal(snapshot)
	if err != nil {
		return err
	}
	defer candidate.Close()
	var check string
	if err := candidate.QueryRowContext(ctx, "PRAGMA integrity_check(1)").Scan(&check); err != nil || check != "ok" {
		return errors.New("deployment journal snapshot integrity check failed")
	}
	var version string
	if err := candidate.QueryRowContext(ctx, "SELECT value FROM meta WHERE key='schema_version'").Scan(&version); err != nil || version == "" {
		return errors.New("deployment journal snapshot has no schema version")
	}
	if err := candidate.QueryRowContext(ctx, "SELECT 1 FROM deployment_transactions WHERE state NOT IN ('finalized','rolled_back','failed') LIMIT 1").Scan(new(int)); !errors.Is(err, sql.ErrNoRows) {
		return errors.New("deployment journal has an unresolved transaction or cannot be verified")
	}
	var violations int
	if err := candidate.QueryRowContext(ctx, "SELECT COUNT(*) FROM pragma_foreign_key_check").Scan(&violations); err != nil || violations != 0 {
		return errors.New("deployment journal snapshot foreign key check failed")
	}
	if err := candidate.Close(); err != nil {
		return err
	}
	file, err := os.Open(snapshot)
	if err != nil {
		return err
	}
	err = errors.Join(file.Chmod(0600), file.Chown(uid, gid), file.Sync(), file.Close())
	if err != nil {
		return err
	}
	// The private staging directory and the runtime directory share a
	// filesystem. Linking publishes the complete file without replacing one.
	if err := os.Link(snapshot, destination); err != nil {
		return err
	}
	return syncDir(filepath.Dir(destination))
}

func openAccountJournal(path string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}
	db, err := sql.Open("sqlite", u.String())
	if err == nil {
		db.SetMaxOpenConns(1)
	}
	return db, err
}
